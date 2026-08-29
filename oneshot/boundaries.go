package oneshot

import (
	"context"
	"errors"
	"iter"
	"sync"
	"sync/atomic"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

type runStatistics struct {
	modelCalls   atomic.Int64
	toolCalls    atomic.Int64
	inputTokens  atomic.Int64
	outputTokens atomic.Int64
	totalTokens  atomic.Int64
}

func (s *runStatistics) observeResponse(response *model.LLMResponse) {
	if response == nil || response.UsageMetadata == nil {
		return
	}
	s.inputTokens.Add(int64(response.UsageMetadata.PromptTokenCount))
	s.outputTokens.Add(int64(response.UsageMetadata.CandidatesTokenCount))
	s.totalTokens.Add(int64(response.UsageMetadata.TotalTokenCount))
}

func (s *runStatistics) metadata() Metadata {
	return Metadata{
		ModelCalls: int(s.modelCalls.Load()),
		ToolCalls:  int(s.toolCalls.Load()),
		Usage: Usage{
			InputTokens: s.inputTokens.Load(), OutputTokens: s.outputTokens.Load(), TotalTokens: s.totalTokens.Load(),
		},
	}
}

type failureRecorder struct {
	mu    sync.Mutex
	value error
}

func (r *failureRecorder) record(value error) {
	if value == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.value == nil {
		r.value = value
	}
}

func (r *failureRecorder) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.value
}

type protectedModel struct {
	name       string
	source     model.LLM
	statistics *runStatistics
	failures   *failureRecorder
}

func protectModel(source model.LLM, statistics *runStatistics, failures *failureRecorder) (model.LLM, error) {
	name, err := callModelName(source)
	if err != nil {
		return nil, err
	}
	return &protectedModel{name: name, source: source, statistics: statistics, failures: failures}, nil
}

func (m *protectedModel) Name() string { return m.name }

func (m *protectedModel) GenerateContent(ctx context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	m.statistics.modelCalls.Add(1)
	return func(yield func(*model.LLMResponse, error) bool) {
		var downstreamPanic any
		defer func() {
			recovered := recover()
			if downstreamPanic != nil {
				panic(downstreamPanic)
			}
			if recovered == nil {
				return
			}
			failure := codedError(CodeModelPanic, "call model", ErrModelPanic, nil)
			m.failures.record(failure)
			yield(nil, failure)
		}()

		sequence := m.source.GenerateContent(ctx, request, stream)
		sequence(func(response *model.LLMResponse, err error) (keepGoing bool) {
			defer func() {
				if recovered := recover(); recovered != nil {
					downstreamPanic = recovered
					panic(recovered)
				}
			}()
			if response == nil && err == nil {
				err = errors.New("caller model yielded a nil response without an error")
			}
			m.statistics.observeResponse(response)
			return yield(response, err)
		})
		if downstreamPanic != nil {
			panic(downstreamPanic)
		}
	}
}

func callModelName(source model.LLM) (name string, err error) {
	defer func() {
		if recover() != nil {
			err = codedError(CodeModelPanic, "read model name", ErrModelPanic, nil)
		}
	}()
	return source.Name(), nil
}

type requestProcessor interface {
	ProcessRequest(agent.Context, *model.LLMRequest) error
}

type declarer interface {
	Declaration() *genai.FunctionDeclaration
}

type functionTool interface {
	tool.Tool
	declarer
	Run(agent.Context, any) (map[string]any, error)
}

type streamingTool interface {
	tool.Tool
	declarer
	RunStream(agent.Context, any) iter.Seq2[string, error]
}

type responseDeferrer interface {
	DefersResponse() bool
}

type toolDescriptor struct {
	name        string
	description string
	longRunning bool
	declaration *genai.FunctionDeclaration
	defers      bool
	source      tool.Tool
	processor   requestProcessor
	statistics  *runStatistics
	failures    *failureRecorder
	identities  *identityStripper
}

func protectTools(
	values []tool.Tool,
	statistics *runStatistics,
	failures *failureRecorder,
	identities *identityStripper,
) ([]tool.Tool, error) {
	result := make([]tool.Tool, len(values))
	for index, value := range values {
		if nilInterface(value) {
			return nil, codedError(CodeInvalidArgument, "protect tools", ErrInvalidArgument, errors.New("tool is nil"))
		}
		descriptor, err := inspectTool(value, statistics, failures, identities)
		if err != nil {
			return nil, err
		}
		switch source := value.(type) {
		case streamingTool:
			result[index] = &protectedStreamingTool{toolDescriptor: descriptor, source: source}
		case functionTool:
			result[index] = &protectedFunctionTool{toolDescriptor: descriptor, source: source}
		default:
			if descriptor.processor == nil {
				return nil, codedError(CodeInvalidArgument, "protect tools", ErrInvalidArgument, errors.New("tool cannot be packed into an ADK request"))
			}
			result[index] = &protectedRequestTool{toolDescriptor: descriptor}
		}
	}
	return result, nil
}

func inspectTool(
	value tool.Tool,
	statistics *runStatistics,
	failures *failureRecorder,
	identities *identityStripper,
) (descriptor *toolDescriptor, err error) {
	name, err := callToolMetadata("read tool name", value.Name)
	if err != nil {
		return nil, err
	}
	description, err := callToolMetadata("read tool description", value.Description)
	if err != nil {
		return nil, err
	}
	longRunning, err := callToolMetadata("read tool long-running state", value.IsLongRunning)
	if err != nil {
		return nil, err
	}
	descriptor = &toolDescriptor{
		name: name, description: description, longRunning: longRunning,
		source: value, statistics: statistics, failures: failures, identities: identities,
	}
	if declaration, ok := value.(declarer); ok {
		descriptor.declaration, err = callToolMetadata("read tool declaration", declaration.Declaration)
		if err != nil {
			return nil, err
		}
	}
	if processor, ok := value.(requestProcessor); ok {
		descriptor.processor = processor
	}
	if deferrer, ok := value.(responseDeferrer); ok {
		descriptor.defers, err = callToolMetadata("read tool response policy", deferrer.DefersResponse)
		if err != nil {
			return nil, err
		}
	}
	return descriptor, nil
}

func callToolMetadata[T any](operation string, call func() T) (value T, err error) {
	defer func() {
		if recover() != nil {
			err = codedError(CodeToolPanic, operation, ErrToolPanic, nil)
		}
	}()
	return call(), nil
}

func (t *toolDescriptor) Name() string                            { return t.name }
func (t *toolDescriptor) Description() string                     { return t.description }
func (t *toolDescriptor) IsLongRunning() bool                     { return t.longRunning }
func (t *toolDescriptor) Declaration() *genai.FunctionDeclaration { return t.declaration }
func (t *toolDescriptor) DefersResponse() bool                    { return t.defers }

func (t *toolDescriptor) processRequest(ctx agent.Context, request *model.LLMRequest, packed toolutils.Tool) error {
	t.identities.beforeTool(request)
	if t.processor == nil {
		return toolutils.PackTool(request, packed)
	}
	existed := request.Tools != nil && request.Tools[t.name] != nil
	if err, panicked := callToolProcessor(t.processor, ctx, request); err != nil {
		if panicked {
			t.failures.record(err)
		}
		return err
	}
	if !existed && request.Tools != nil && request.Tools[t.name] != nil {
		request.Tools[t.name] = packed
		return nil
	}
	if !existed && t.declaration != nil {
		return toolutils.PackTool(request, packed)
	}
	return nil
}

func callToolProcessor(processor requestProcessor, ctx agent.Context, request *model.LLMRequest) (err error, panicked bool) {
	defer func() {
		if recover() != nil {
			err = codedError(CodeToolPanic, "prepare tool request", ErrToolPanic, nil)
			panicked = true
		}
	}()
	return processor.ProcessRequest(ctx, request), false
}

type protectedRequestTool struct{ *toolDescriptor }

func (t *protectedRequestTool) ProcessRequest(ctx agent.Context, request *model.LLMRequest) error {
	return t.processRequest(ctx, request, t)
}

type protectedFunctionTool struct {
	*toolDescriptor
	source functionTool
}

func (t *protectedFunctionTool) ProcessRequest(ctx agent.Context, request *model.LLMRequest) error {
	return t.processRequest(ctx, request, t)
}

func (t *protectedFunctionTool) Run(ctx agent.Context, arguments any) (result map[string]any, err error) {
	t.statistics.toolCalls.Add(1)
	result, err, panicked := callFunctionTool(t.source, ctx, arguments)
	if panicked {
		failure := codedError(CodeToolPanic, "call tool "+t.name, ErrToolPanic, nil)
		t.failures.record(failure)
		return nil, failure
	}
	return result, err
}

func callFunctionTool(
	source functionTool,
	ctx agent.Context,
	arguments any,
) (result map[string]any, err error, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	result, err = source.Run(ctx, arguments)
	return result, err, false
}

type protectedStreamingTool struct {
	*toolDescriptor
	source streamingTool
}

func (t *protectedStreamingTool) ProcessRequest(ctx agent.Context, request *model.LLMRequest) error {
	return t.processRequest(ctx, request, t)
}

func (t *protectedStreamingTool) RunStream(ctx agent.Context, arguments any) iter.Seq2[string, error] {
	t.statistics.toolCalls.Add(1)
	return func(yield func(string, error) bool) {
		var downstreamPanic any
		defer func() {
			recovered := recover()
			if downstreamPanic != nil {
				panic(downstreamPanic)
			}
			if recovered == nil {
				return
			}
			failure := codedError(CodeToolPanic, "call tool "+t.name, ErrToolPanic, nil)
			t.failures.record(failure)
			yield("", failure)
		}()
		sequence := t.source.RunStream(ctx, arguments)
		sequence(func(value string, err error) (keepGoing bool) {
			defer func() {
				if recovered := recover(); recovered != nil {
					downstreamPanic = recovered
					panic(recovered)
				}
			}()
			return yield(value, err)
		})
		if downstreamPanic != nil {
			panic(downstreamPanic)
		}
	}
}

var (
	_ model.LLM        = (*protectedModel)(nil)
	_ requestProcessor = (*protectedRequestTool)(nil)
	_ requestProcessor = (*protectedFunctionTool)(nil)
	_ requestProcessor = (*protectedStreamingTool)(nil)
	_ functionTool     = (*protectedFunctionTool)(nil)
	_ streamingTool    = (*protectedStreamingTool)(nil)
)

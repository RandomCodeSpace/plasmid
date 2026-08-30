package oneshot

import (
	"context"
	"errors"
	"iter"
	"maps"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/platform"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

type runStatistics struct {
	modelCalls       atomic.Int64
	toolCalls        atomic.Int64
	inputTokens      atomic.Int64
	outputTokens     atomic.Int64
	totalTokens      atomic.Int64
	completedToolsMu sync.Mutex
	completedToolIDs map[string]struct{}
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

func (s *runStatistics) completeToolCall(id string) {
	if id == "" {
		panic("oneshot: completed tool call has an empty ID")
	}
	s.completedToolsMu.Lock()
	defer s.completedToolsMu.Unlock()
	if s.completedToolIDs == nil {
		s.completedToolIDs = make(map[string]struct{})
	}
	s.completedToolIDs[id] = struct{}{}
}

func (s *runStatistics) consumeCompletedToolCall(id string) bool {
	s.completedToolsMu.Lock()
	defer s.completedToolsMu.Unlock()
	if _, completed := s.completedToolIDs[id]; !completed {
		return false
	}
	delete(s.completedToolIDs, id)
	return true
}

func (s *runStatistics) clearCompletedToolCalls() {
	s.completedToolsMu.Lock()
	clear(s.completedToolIDs)
	s.completedToolsMu.Unlock()
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
	responses  *responseRecorder
	controls   executionControls
}

func protectModel(
	source model.LLM,
	statistics *runStatistics,
	failures *failureRecorder,
	responses *responseRecorder,
	controls executionControls,
) (model.LLM, error) {
	name, err := callModelName(source)
	if err != nil {
		return nil, err
	}
	return &protectedModel{
		name: name, source: source, statistics: statistics, failures: failures, responses: responses, controls: controls,
	}, nil
}

func (m *protectedModel) Name() string { return m.name }

func (m *protectedModel) GenerateContent(ctx context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if failure := m.failures.failure(); failure != nil {
			yield(nil, failure)
			return
		}
		if !m.statistics.startModelCall(m.controls.maxModelCalls) {
			failure := codedError(CodeModelCallLimit, "call model", ErrModelCallLimit, nil)
			m.failures.record(failure)
			yield(nil, failure)
			return
		}
		m.responses.startModelCall()
		if request.Config == nil {
			request.Config = &genai.GenerateContentConfig{}
		}
		request.Config.MaxOutputTokens = m.controls.maxOutputTokens
		m.generate(ctx, request, stream, yield)
	}
}

func (m *protectedModel) generate(
	ctx context.Context,
	request *model.LLMRequest,
	stream bool,
	yield func(*model.LLMResponse, error) bool,
) {
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

	sequence := m.source.GenerateContent(platform.WithTaskRunner(ctx, nestedTaskRunner), request, stream)
	sequence(func(response *model.LLMResponse, err error) (keepGoing bool) {
		defer func() {
			if recovered := recover(); recovered != nil {
				downstreamPanic = recovered
				panic(recovered)
			}
		}()
		m.statistics.clearCompletedToolCalls()
		if response == nil && err == nil {
			err = errors.New("caller model yielded a nil response without an error")
		}
		err, panicked := untrustedCallerError(err)
		if panicked {
			failure := codedError(CodeModelPanic, "call model", ErrModelPanic, nil)
			m.failures.record(failure)
			err = failure
		}
		m.statistics.observeResponse(response)
		if response != nil {
			textOverflow := m.responses.record(response.Content, response.Partial, m.controls.maxReturnedTextBytes)
			if err != nil {
				return yield(response, err)
			}
			normalizeFunctionCallIDs(response.Content)
			var failure error
			switch {
			case response.FinishReason == genai.FinishReasonMaxTokens:
				failure = codedError(CodeOutputTruncated, "call model", ErrOutputTruncated, nil)
			case textOverflow:
				failure = codedError(CodeTextTruncated, "call model", ErrTextTruncated, nil)
			case functionCallCount(response.Content) > m.controls.maxToolCallsPerResponse:
				failure = codedError(CodeToolCallLimit, "call model", ErrToolCallLimit, nil)
			}
			if failure != nil {
				m.failures.record(failure)
				yield(nil, failure)
				return false
			}
		}
		return yield(response, err)
	})
	if downstreamPanic != nil {
		panic(downstreamPanic)
	}
}

func (s *runStatistics) startModelCall(limit int) bool {
	for {
		current := s.modelCalls.Load()
		if current >= int64(limit) {
			return false
		}
		if s.modelCalls.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

type responseRecorder struct {
	mu       sync.Mutex
	text     string
	callText string
}

func (r *responseRecorder) startModelCall() {
	r.mu.Lock()
	r.callText = ""
	r.mu.Unlock()
}

func (r *responseRecorder) record(content *genai.Content, partial bool, limit int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !partial {
		value, overflow := boundedText(content, limit)
		r.callText = value
		if value != "" {
			r.text = value
		}
		return overflow
	}

	remaining := limit - len(r.callText)
	value, overflow := boundedText(content, remaining)
	r.callText += value
	if r.callText != "" {
		r.text = r.callText
	}
	return overflow
}

func (r *responseRecorder) textValue() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.text
}

func boundedText(content *genai.Content, limit int) (string, bool) {
	if content == nil {
		return "", false
	}
	var result strings.Builder
	remaining := limit
	for _, part := range content.Parts {
		if part == nil || part.Thought || part.Text == "" {
			continue
		}
		if len(part.Text) > remaining {
			result.WriteString(part.Text[:remaining])
			return result.String(), true
		}
		result.WriteString(part.Text)
		remaining -= len(part.Text)
	}
	return result.String(), false
}

func functionCallCount(content *genai.Content) int {
	if content == nil {
		return 0
	}
	count := 0
	for _, part := range content.Parts {
		if part != nil && part.FunctionCall != nil {
			count++
		}
	}
	return count
}

func normalizeFunctionCallIDs(content *genai.Content) {
	if content == nil {
		return
	}
	reserved := make(map[string]struct{})
	for _, part := range content.Parts {
		if part == nil || part.FunctionCall == nil || part.FunctionCall.ID == "" {
			continue
		}
		reserved[part.FunctionCall.ID] = struct{}{}
	}
	nextID := 1
	used := make(map[string]struct{})
	for _, part := range content.Parts {
		if part == nil || part.FunctionCall == nil {
			continue
		}
		id := part.FunctionCall.ID
		if _, duplicate := used[id]; id == "" || duplicate {
			for {
				candidate := "plasmid-oneshot-call-" + strconv.Itoa(nextID)
				nextID++
				if _, exists := reserved[candidate]; exists {
					continue
				}
				if _, exists := used[candidate]; exists {
					continue
				}
				id = candidate
				break
			}
		}
		part.FunctionCall.ID = id
		used[id] = struct{}{}
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
	source     tool.Tool
	processor  requestProcessor
	statistics *runStatistics
	failures   *failureRecorder
	identities *identityStripper
}

func protectTools(
	values []tool.Tool,
	statistics *runStatistics,
	failures *failureRecorder,
	identities *identityStripper,
) ([]tool.Tool, error) {
	result := make([]tool.Tool, len(values))
	for index, value := range values {
		protected, err := protectTool(value, statistics, failures, identities)
		if err != nil {
			return nil, err
		}
		result[index] = protected
	}
	return result, nil
}

func protectTool(
	value tool.Tool,
	statistics *runStatistics,
	failures *failureRecorder,
	identities *identityStripper,
) (tool.Tool, error) {
	if nilInterface(value) {
		return nil, codedError(CodeInvalidArgument, "protect tools", ErrInvalidArgument, errors.New("tool is nil"))
	}
	switch protected := value.(type) {
	case *protectedRequestTool:
		if protected.ownedBy(statistics, failures, identities) {
			return protected, nil
		}
		value = protected.source
	case *protectedFunctionTool:
		if protected.ownedBy(statistics, failures, identities) {
			return protected, nil
		}
		value = protected.toolDescriptor.source
	case *protectedStreamingTool:
		if protected.ownedBy(statistics, failures, identities) {
			return protected, nil
		}
		value = protected.toolDescriptor.source
	}
	descriptor, err := inspectTool(value, statistics, failures, identities)
	if err != nil {
		return nil, err
	}
	switch source := value.(type) {
	case streamingTool:
		return &protectedStreamingTool{toolDescriptor: descriptor, source: source}, nil
	case functionTool:
		return &protectedFunctionTool{toolDescriptor: descriptor, source: source}, nil
	default:
		if descriptor.processor == nil {
			return nil, codedError(CodeInvalidArgument, "protect tools", ErrInvalidArgument, errors.New("tool cannot be packed into an ADK request"))
		}
		return &protectedRequestTool{toolDescriptor: descriptor}, nil
	}
}

func inspectTool(
	value tool.Tool,
	statistics *runStatistics,
	failures *failureRecorder,
	identities *identityStripper,
) (descriptor *toolDescriptor, err error) {
	descriptor = &toolDescriptor{
		source: value, statistics: statistics, failures: failures, identities: identities,
	}
	if processor, ok := value.(requestProcessor); ok {
		descriptor.processor = processor
	}
	return descriptor, nil
}

func (t *toolDescriptor) Name() string {
	return protectedToolMetadata(t, "read tool name", t.source.Name)
}

func (t *toolDescriptor) ownedBy(
	statistics *runStatistics,
	failures *failureRecorder,
	identities *identityStripper,
) bool {
	return t.statistics == statistics && t.failures == failures && t.identities == identities
}
func (t *toolDescriptor) Description() string {
	return protectedToolMetadata(t, "read tool description", t.source.Description)
}
func (t *toolDescriptor) IsLongRunning() bool {
	return protectedToolMetadata(t, "read tool long-running state", t.source.IsLongRunning)
}
func (t *toolDescriptor) Declaration() *genai.FunctionDeclaration {
	declaration, ok := t.source.(declarer)
	if !ok {
		return nil
	}
	return protectedToolMetadata(t, "read tool declaration", declaration.Declaration)
}
func (t *toolDescriptor) DefersResponse() bool {
	deferrer, ok := t.source.(responseDeferrer)
	if !ok {
		return false
	}
	return protectedToolMetadata(t, "read tool response policy", deferrer.DefersResponse)
}

func protectedToolMetadata[T any](descriptor *toolDescriptor, operation string, call func() T) (value T) {
	defer func() {
		if recover() == nil {
			return
		}
		descriptor.failures.record(codedError(CodeToolPanic, operation, ErrToolPanic, nil))
	}()
	return call()
}

func (t *toolDescriptor) processRequest(ctx agent.Context, request *model.LLMRequest, packed toolutils.Tool) error {
	t.identities.beforeTool(request)
	if t.processor == nil {
		return toolutils.PackTool(request, packed)
	}
	callerContext := contextWithoutTaskRunner(ctx)
	if err, panicked := callToolProcessor(t.processor, callerContext, request); err != nil {
		if panicked {
			t.failures.record(err)
		}
		return err
	}
	return protectRegisteredTools(request, t.statistics, t.failures, t.identities)
}

func protectRegisteredTools(
	request *model.LLMRequest,
	statistics *runStatistics,
	failures *failureRecorder,
	identities *identityStripper,
) error {
	for name, registered := range request.Tools {
		registeredTool, ok := registered.(tool.Tool)
		if !ok {
			continue
		}
		protected, err := protectTool(registeredTool, statistics, failures, identities)
		if err != nil {
			return err
		}
		request.Tools[name] = protected
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
	err, panicked = untrustedCallerError(processor.ProcessRequest(ctx, request))
	if panicked {
		return codedError(CodeToolPanic, "prepare tool request", ErrToolPanic, nil), true
	}
	return err, false
}

func contextWithoutTaskRunner(ctx agent.Context) agent.Context {
	nativeContext := ctx.WithDelta(nil)
	return nativeContext.WithAgentContext(platform.WithTaskRunner(nativeContext, nestedTaskRunner))
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
	callID := ctx.FunctionCallID()
	defer t.statistics.completeToolCall(callID)
	result, err, panicked := callFunctionTool(t.source, ctx, arguments)
	if panicked {
		failure := codedError(CodeToolPanic, "call tool", ErrToolPanic, nil)
		t.failures.record(failure)
		return nil, failure
	}
	err, panicked = untrustedCallerError(err)
	if panicked {
		failure := codedError(CodeToolPanic, "call tool", ErrToolPanic, nil)
		t.failures.record(failure)
		return nil, failure
	}
	if err == nil {
		result, panicked = sanitizeToolResult(result)
		if panicked {
			failure := codedError(CodeToolPanic, "call tool", ErrToolPanic, nil)
			t.failures.record(failure)
			return nil, failure
		}
	}
	return result, err
}

func sanitizeToolResult(result map[string]any) (map[string]any, bool) {
	result = maps.Clone(result)
	callerError, ok := result["error"].(error)
	if !ok {
		return result, false
	}
	safe, panicked := untrustedCallerError(callerError)
	if panicked {
		return nil, true
	}
	result["error"] = safe.Error()
	return result, false
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
	callID := ctx.FunctionCallID()
	return func(yield func(string, error) bool) {
		defer t.statistics.completeToolCall(callID)
		var downstreamPanic any
		defer func() {
			recovered := recover()
			if downstreamPanic != nil {
				panic(downstreamPanic)
			}
			if recovered == nil {
				return
			}
			failure := codedError(CodeToolPanic, "call tool", ErrToolPanic, nil)
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
			err, panicked := untrustedCallerError(err)
			if panicked {
				failure := codedError(CodeToolPanic, "call tool", ErrToolPanic, nil)
				t.failures.record(failure)
				err = failure
			}
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

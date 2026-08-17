package plasmid

import (
	"fmt"
	"iter"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/contextresolver"
	"github.com/plasmid-dev/plasmid/internal/syntax"
)

type nativeFunctionTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(agent.Context, any) (map[string]any, error)
}

type nativeStreamingTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	RunStream(agent.Context, any) iter.Seq2[string, error]
}

type responseDeferrer interface {
	DefersResponse() bool
}

type guardedFunctionTool struct {
	source    nativeFunctionTool
	confirmed nativeFunctionTool
	contexts  *contextresolver.Resolver
	defers    bool
}

func (t *guardedFunctionTool) Name() string        { return t.source.Name() }
func (t *guardedFunctionTool) Description() string { return t.source.Description() }
func (t *guardedFunctionTool) IsLongRunning() bool { return t.source.IsLongRunning() }
func (t *guardedFunctionTool) Declaration() *genai.FunctionDeclaration {
	return t.source.Declaration()
}
func (t *guardedFunctionTool) DefersResponse() bool {
	return t.defers
}
func (t *guardedFunctionTool) ProcessRequest(ctx agent.Context, request *model.LLMRequest) error {
	return processGuardedToolRequest(ctx, request, t.source, t, t.contexts, t.confirmed != nil)
}
func (t *guardedFunctionTool) Run(ctx agent.Context, arguments any) (map[string]any, error) {
	args, ok := arguments.(map[string]any)
	if !ok {
		return nil, toolPolicyError(ctx, t.contexts, t.Name(), nil)
	}
	if err := toolPolicyError(ctx, t.contexts, t.Name(), args); err != nil {
		return nil, err
	}
	executor := t.source
	if t.confirmed != nil {
		executor = t.confirmed
	}
	return executor.Run(ctx, arguments)
}

type guardedStreamingTool struct {
	source   nativeStreamingTool
	contexts *contextresolver.Resolver
	defers   bool
}

func (t *guardedStreamingTool) Name() string        { return t.source.Name() }
func (t *guardedStreamingTool) Description() string { return t.source.Description() }
func (t *guardedStreamingTool) IsLongRunning() bool { return t.source.IsLongRunning() }
func (t *guardedStreamingTool) Declaration() *genai.FunctionDeclaration {
	return t.source.Declaration()
}
func (t *guardedStreamingTool) DefersResponse() bool {
	return t.defers
}
func (t *guardedStreamingTool) ProcessRequest(ctx agent.Context, request *model.LLMRequest) error {
	return processGuardedToolRequest(ctx, request, t.source, t, t.contexts, false)
}
func (t *guardedStreamingTool) RunStream(ctx agent.Context, arguments any) iter.Seq2[string, error] {
	args, ok := arguments.(map[string]any)
	if !ok {
		return failedToolStream(toolPolicyError(ctx, t.contexts, t.Name(), nil))
	}
	if err := toolPolicyError(ctx, t.contexts, t.Name(), args); err != nil {
		return failedToolStream(err)
	}
	return t.source.RunStream(ctx, arguments)
}

func processGuardedToolRequest(
	ctx agent.Context,
	request *model.LLMRequest,
	source tool.Tool,
	wrapper toolutils.Tool,
	contexts *contextresolver.Resolver,
	confirmation bool,
) error {
	_, existedBefore := request.Tools[source.Name()]
	if processor, ok := source.(toolsetRequestProcessor); ok {
		if err := processor.ProcessRequest(ctx, request); err != nil {
			return err
		}
		if !existedBefore {
			if packed, ok := request.Tools[source.Name()]; ok {
				actual, ok := packed.(tool.Tool)
				if !ok {
					return fmt.Errorf("tool %q request processor packed a non-tool value", source.Name())
				}
				guarded, err := guardToolExecution(actual, contexts, confirmation)
				if err != nil {
					return err
				}
				request.Tools[source.Name()] = guarded
				return nil
			}
		}
	}
	return toolutils.PackTool(request, wrapper)
}

func guardToolExecution(value tool.Tool, contexts *contextresolver.Resolver, confirmation bool) (tool.Tool, error) {
	switch current := value.(type) {
	case *guardedFunctionTool:
		if (current.confirmed != nil) == confirmation && current.contexts == contexts {
			return current, nil
		}
		return guardToolExecution(current.source, contexts, confirmation)
	case *guardedStreamingTool:
		if confirmation {
			return nil, fmt.Errorf("tool %q is streaming and does not support native confirmation", current.Name())
		}
		if current.contexts == contexts {
			return current, nil
		}
		return guardToolExecution(current.source, contexts, false)
	case nativeStreamingTool:
		if confirmation {
			return nil, fmt.Errorf("tool %q is streaming and does not support native confirmation", current.Name())
		}
		return &guardedStreamingTool{source: current, contexts: contexts, defers: defersResponse(current)}, nil
	case nativeFunctionTool:
		guarded := &guardedFunctionTool{source: current, contexts: contexts, defers: defersResponse(current)}
		if confirmation {
			confirmed, err := nativeConfirmationTool(current)
			if err != nil {
				return nil, err
			}
			guarded.confirmed = confirmed
		}
		return guarded, nil
	default:
		return value, nil
	}
}

func nativeConfirmationTool(value nativeFunctionTool) (nativeFunctionTool, error) {
	values, err := tool.WithConfirmation(singleToolset{value: value}, true, nil).Tools(nil)
	if err != nil {
		return nil, err
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("wrap tool %q with native confirmation: got %d tools", value.Name(), len(values))
	}
	confirmed, ok := values[0].(nativeFunctionTool)
	if !ok {
		return nil, fmt.Errorf("wrap tool %q with native confirmation: wrapper is not runnable", value.Name())
	}
	return confirmed, nil
}

type singleToolset struct{ value tool.Tool }

func (s singleToolset) Name() string { return s.value.Name() }
func (s singleToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) {
	return []tool.Tool{s.value}, nil
}

func defersResponse(value tool.Tool) bool {
	deferrer, ok := value.(responseDeferrer)
	return ok && deferrer.DefersResponse()
}

func toolPolicyError(ctx agent.Context, contexts *contextresolver.Resolver, name string, args map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if contexts.Closed() {
		return fmt.Errorf("%w: context resolver is closed", syntax.ErrToolDenied)
	}
	if contexts.Allows(ctx.SessionID(), ctx.InvocationID(), name, args) {
		return nil
	}
	return fmt.Errorf("%w: %s", syntax.ErrToolDenied, name)
}

func failedToolStream(err error) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		yield("", err)
	}
}

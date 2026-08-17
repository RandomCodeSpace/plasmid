package adkloop

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/loop"
)

type hookADKTool struct{ name string }

func (t hookADKTool) Name() string      { return t.name }
func (hookADKTool) Description() string { return "test tool" }
func (hookADKTool) IsLongRunning() bool { return false }

func TestHookBridgeChainsPropagateMutationsAndShortCircuit(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "before model", run: testBeforeModelHookChain},
		{name: "after model", run: testAfterModelHookChain},
		{name: "before tool", run: testBeforeToolHookChain},
		{name: "after tool", run: testAfterToolHookChain},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func testBeforeModelHookChain(t *testing.T) {
	native := &model.LLMRequest{
		Model:    "old-model",
		Contents: []*genai.Content{genai.NewContentFromText("old prompt", genai.RoleUser)},
		Config:   &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromText("old system", genai.Role("system"))},
	}
	var order []string
	hooks := loop.Hooks{BeforeModel: []loop.BeforeModelHook{
		func(_ context.Context, request *loop.ModelRequest) (*loop.ModelResponse, error) {
			order = append(order, "first")
			if request.Raw != native {
				t.Fatalf("Raw = %T %p, want native %p", request.Raw, request.Raw, native)
			}
			request.Model = "new-model"
			request.System = "new system"
			request.Messages[0].Text = "new prompt"
			return nil, nil
		},
		func(_ context.Context, request *loop.ModelRequest) (*loop.ModelResponse, error) {
			order = append(order, "second")
			if request.Model != "new-model" || request.System != "new system" || request.Messages[0].Text != "new prompt" {
				t.Fatalf("request = %#v", request)
			}
			return &loop.ModelResponse{Message: loop.Message{Role: loop.RoleAssistant, Text: "cached"}}, nil
		},
		func(context.Context, *loop.ModelRequest) (*loop.ModelResponse, error) {
			order = append(order, "third")
			return nil, nil
		},
	}}
	config := &llmagent.Config{}
	applyHookConfig(config, hooks)
	response, err := config.BeforeModelCallbacks[0](&bridgeContext{}, native)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatalf("order = %#v", order)
	}
	if native.Model != "new-model" || native.Config.SystemInstruction.Parts[0].Text != "new system" || native.Contents[0].Parts[0].Text != "new prompt" {
		t.Fatalf("native request = %#v", native)
	}
	if response == nil || response.Content.Parts[0].Text != "cached" || response.Content.Role != genai.RoleModel {
		t.Fatalf("response = %#v", response)
	}
}

func testAfterModelHookChain(t *testing.T) {
	sentinel := errors.New("model warning")
	native := &model.LLMResponse{
		Content:        genai.NewContentFromText("initial", genai.RoleModel),
		CustomMetadata: map[string]any{"provider": "preserved"},
		ModelVersion:   "model-v1",
	}
	var order []string
	hooks := loop.Hooks{AfterModel: []loop.AfterModelHook{
		func(_ context.Context, response *loop.ModelResponse, err error) (*loop.ModelResponse, error) {
			order = append(order, "first")
			if !errors.Is(err, sentinel) || response.Raw != native {
				t.Fatalf("response = %#v, error = %v", response, err)
			}
			response.Message.Text = "mutated"
			return nil, nil
		},
		func(_ context.Context, response *loop.ModelResponse, err error) (*loop.ModelResponse, error) {
			order = append(order, "second")
			if !errors.Is(err, sentinel) || response.Message.Text != "mutated" {
				t.Fatalf("response = %#v, error = %v", response, err)
			}
			response.Message.Text = "final"
			return response, nil
		},
	}}
	config := &llmagent.Config{}
	applyHookConfig(config, hooks)
	response, err := config.AfterModelCallbacks[0](&bridgeContext{}, native, sentinel)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatalf("order = %#v", order)
	}
	if response == native || response.Content.Parts[0].Text != "final" || response.CustomMetadata["provider"] != "preserved" || response.ModelVersion != "model-v1" {
		t.Fatalf("response = %#v", response)
	}
}

func testBeforeToolHookChain(t *testing.T) {
	var order []string
	hooks := loop.Hooks{BeforeTool: []loop.BeforeToolHook{
		func(_ context.Context, call *loop.ToolCall) (*loop.ToolResult, error) {
			order = append(order, "first")
			call.Args["key"] = "beta"
			return nil, nil
		},
		func(_ context.Context, call *loop.ToolCall) (*loop.ToolResult, error) {
			order = append(order, "second")
			if call.ID != "call" || call.Name != "tool" || call.SessionID != "session" || call.InvocationID != "invocation" || call.Args["key"] != "beta" {
				t.Fatalf("call = %#v", call)
			}
			return &loop.ToolResult{CallID: "call", Content: map[string]any{"value": "short"}}, nil
		},
		func(context.Context, *loop.ToolCall) (*loop.ToolResult, error) {
			order = append(order, "third")
			return nil, nil
		},
	}}
	config := &llmagent.Config{}
	applyHookConfig(config, hooks)
	args := map[string]any{"key": "alpha"}
	result, err := config.BeforeToolCallbacks[0](&bridgeContext{callID: "call", sessionID: "session", invocationID: "invocation"}, hookADKTool{name: "tool"}, args)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"first", "second"}) || args["key"] != "beta" || result["value"] != "short" {
		t.Fatalf("order = %#v, args = %#v, result = %#v", order, args, result)
	}
}

func testAfterToolHookChain(t *testing.T) {
	sentinel := errors.New("tool failed")
	var order []string
	hooks := loop.Hooks{AfterTool: []loop.AfterToolHook{
		func(_ context.Context, call *loop.ToolCall, result *loop.ToolResult, err error) (*loop.ToolResult, error) {
			order = append(order, "first")
			if !errors.Is(err, sentinel) || call.Args["key"] != "alpha" || result.Content["value"] != "initial" || !result.IsError {
				t.Fatalf("call = %#v, result = %#v, error = %v", call, result, err)
			}
			return &loop.ToolResult{CallID: "call", Content: map[string]any{"value": "replacement"}, IsError: true}, nil
		},
		func(_ context.Context, _ *loop.ToolCall, result *loop.ToolResult, err error) (*loop.ToolResult, error) {
			order = append(order, "second")
			if !errors.Is(err, sentinel) || result.Content["value"] != "replacement" {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
			return &loop.ToolResult{CallID: "call", Content: map[string]any{"value": "final"}, IsError: true}, nil
		},
	}}
	config := &llmagent.Config{}
	applyHookConfig(config, hooks)
	result, err := config.AfterToolCallbacks[0](
		&bridgeContext{callID: "call", sessionID: "session", invocationID: "invocation"},
		hookADKTool{name: "tool"}, map[string]any{"key": "alpha"}, map[string]any{"value": "initial"}, sentinel,
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(order, []string{"first", "second"}) || result["value"] != "final" {
		t.Fatalf("order = %#v, result = %#v", order, result)
	}
}

func TestHookBridgeStructuredToolErrors(t *testing.T) {
	t.Run("before-tool result", func(t *testing.T) {
		config := &llmagent.Config{}
		applyHookConfig(config, loop.Hooks{BeforeTool: []loop.BeforeToolHook{
			func(context.Context, *loop.ToolCall) (*loop.ToolResult, error) {
				return &loop.ToolResult{
					CallID:  "call",
					Content: map[string]any{"retryable": true},
					IsError: true,
				}, nil
			},
		}})
		result, err := config.BeforeToolCallbacks[0](
			&bridgeContext{callID: "call"},
			hookADKTool{name: "tool"},
			map[string]any{},
		)
		if err != nil {
			t.Fatal(err)
		}
		if result["error"] != true || result["retryable"] != true {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("after-tool input", func(t *testing.T) {
		config := &llmagent.Config{}
		applyHookConfig(config, loop.Hooks{AfterTool: []loop.AfterToolHook{
			func(_ context.Context, _ *loop.ToolCall, result *loop.ToolResult, runErr error) (*loop.ToolResult, error) {
				if runErr != nil {
					t.Fatalf("run error = %v", runErr)
				}
				if !result.IsError || result.Content["error"] != "failed" {
					t.Fatalf("result = %#v", result)
				}
				return result, nil
			},
		}})
		result, err := config.AfterToolCallbacks[0](
			&bridgeContext{callID: "call"},
			hookADKTool{name: "tool"},
			map[string]any{},
			map[string]any{"error": "failed", "retryable": true},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result["error"] != "failed" || result["retryable"] != true {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("after-tool replacement", func(t *testing.T) {
		config := &llmagent.Config{}
		applyHookConfig(config, loop.Hooks{AfterTool: []loop.AfterToolHook{
			func(context.Context, *loop.ToolCall, *loop.ToolResult, error) (*loop.ToolResult, error) {
				return &loop.ToolResult{
					CallID:  "call",
					Content: map[string]any{"retryable": true},
					IsError: true,
				}, nil
			},
		}})
		result, err := config.AfterToolCallbacks[0](
			&bridgeContext{callID: "call"},
			hookADKTool{name: "tool"},
			map[string]any{},
			map[string]any{"value": "initial"},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result["error"] != true || result["retryable"] != true {
			t.Fatalf("result = %#v", result)
		}
	})
}

func TestHookBridgeRejectsImmutableIdentityMutations(t *testing.T) {
	identityMutations := []struct {
		name   string
		mutate func(*loop.ToolCall)
	}{
		{name: "call ID", mutate: func(call *loop.ToolCall) { call.ID = "other" }},
		{name: "tool name", mutate: func(call *loop.ToolCall) { call.Name = "other" }},
		{name: "session ID", mutate: func(call *loop.ToolCall) { call.SessionID = "other" }},
		{name: "invocation ID", mutate: func(call *loop.ToolCall) { call.InvocationID = "other" }},
	}
	for _, phase := range []string{"before", "after"} {
		for _, test := range identityMutations {
			t.Run(phase+" "+test.name, func(t *testing.T) {
				config := &llmagent.Config{}
				hooks := loop.Hooks{}
				if phase == "before" {
					hooks.BeforeTool = []loop.BeforeToolHook{func(_ context.Context, call *loop.ToolCall) (*loop.ToolResult, error) {
						test.mutate(call)
						return nil, nil
					}}
				} else {
					hooks.AfterTool = []loop.AfterToolHook{func(_ context.Context, call *loop.ToolCall, _ *loop.ToolResult, _ error) (*loop.ToolResult, error) {
						test.mutate(call)
						return nil, nil
					}}
				}
				applyHookConfig(config, hooks)
				ctx := &bridgeContext{callID: "call", sessionID: "session", invocationID: "invocation"}
				var err error
				if phase == "before" {
					_, err = config.BeforeToolCallbacks[0](ctx, hookADKTool{name: "tool"}, map[string]any{})
				} else {
					_, err = config.AfterToolCallbacks[0](ctx, hookADKTool{name: "tool"}, map[string]any{}, map[string]any{}, nil)
				}
				if !errors.Is(err, ErrImmutableHookField) {
					t.Fatalf("error = %v", err)
				}
			})
		}
	}
}

func TestHookBridgeRejectsLossyMutationsAndResults(t *testing.T) {
	tests := []struct {
		name string
		run  func(*llmagent.Config) error
	}{
		{
			name: "model Raw replaced",
			run: func(config *llmagent.Config) error {
				configHooks := loop.Hooks{BeforeModel: []loop.BeforeModelHook{func(_ context.Context, request *loop.ModelRequest) (*loop.ModelResponse, error) {
					request.Raw = &model.LLMRequest{}
					return nil, nil
				}}}
				applyHookConfig(config, configHooks)
				_, err := config.BeforeModelCallbacks[0](&bridgeContext{}, &model.LLMRequest{})
				return err
			},
		},
		{
			name: "tool declarations changed",
			run: func(config *llmagent.Config) error {
				configHooks := loop.Hooks{BeforeModel: []loop.BeforeModelHook{func(_ context.Context, request *loop.ModelRequest) (*loop.ModelResponse, error) {
					request.Tools = append(request.Tools, loop.ToolSchema{Name: "new"})
					return nil, nil
				}}}
				applyHookConfig(config, configHooks)
				request := &model.LLMRequest{Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "old"}}}}}}
				_, err := config.BeforeModelCallbacks[0](&bridgeContext{}, request)
				return err
			},
		},
		{
			name: "multimodal request content changed",
			run: func(config *llmagent.Config) error {
				applyHookConfig(config, loop.Hooks{BeforeModel: []loop.BeforeModelHook{func(_ context.Context, request *loop.ModelRequest) (*loop.ModelResponse, error) {
					request.Messages[0].Text = "changed"
					return nil, nil
				}}})
				request := &model.LLMRequest{Contents: []*genai.Content{{
					Role: genai.RoleUser,
					Parts: []*genai.Part{
						genai.NewPartFromText("original"),
						{InlineData: &genai.Blob{MIMEType: "application/octet-stream", Data: []byte{1}}},
					},
				}}}
				_, err := config.BeforeModelCallbacks[0](&bridgeContext{}, request)
				return err
			},
		},
		{
			name: "before-tool call ID changed",
			run: func(config *llmagent.Config) error {
				applyHookConfig(config, loop.Hooks{BeforeTool: []loop.BeforeToolHook{func(context.Context, *loop.ToolCall) (*loop.ToolResult, error) {
					return &loop.ToolResult{CallID: "other", Content: map[string]any{}}, nil
				}}})
				_, err := config.BeforeToolCallbacks[0](&bridgeContext{callID: "call"}, hookADKTool{name: "tool"}, map[string]any{})
				return err
			},
		},
		{
			name: "before-tool empty IsError without error",
			run: func(config *llmagent.Config) error {
				applyHookConfig(config, loop.Hooks{BeforeTool: []loop.BeforeToolHook{func(context.Context, *loop.ToolCall) (*loop.ToolResult, error) {
					return &loop.ToolResult{CallID: "call", Content: map[string]any{}, IsError: true}, nil
				}}})
				_, err := config.BeforeToolCallbacks[0](&bridgeContext{callID: "call"}, hookADKTool{name: "tool"}, map[string]any{})
				return err
			},
		},
		{
			name: "after-tool call ID changed",
			run: func(config *llmagent.Config) error {
				applyHookConfig(config, loop.Hooks{AfterTool: []loop.AfterToolHook{func(context.Context, *loop.ToolCall, *loop.ToolResult, error) (*loop.ToolResult, error) {
					return &loop.ToolResult{CallID: "other", Content: map[string]any{}}, nil
				}}})
				_, err := config.AfterToolCallbacks[0](&bridgeContext{callID: "call"}, hookADKTool{name: "tool"}, map[string]any{}, map[string]any{}, nil)
				return err
			},
		},
		{
			name: "after-tool empty IsError without error",
			run: func(config *llmagent.Config) error {
				applyHookConfig(config, loop.Hooks{AfterTool: []loop.AfterToolHook{func(context.Context, *loop.ToolCall, *loop.ToolResult, error) (*loop.ToolResult, error) {
					return &loop.ToolResult{CallID: "call", Content: map[string]any{}, IsError: true}, nil
				}}})
				_, err := config.AfterToolCallbacks[0](&bridgeContext{callID: "call"}, hookADKTool{name: "tool"}, map[string]any{}, map[string]any{}, nil)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run(&llmagent.Config{})
			if !errors.Is(err, ErrFidelity) && !errors.Is(err, ErrImmutableHookField) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestHookBridgeRawRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "before model request",
			run: func(t *testing.T) {
				native := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("prompt", genai.RoleUser)}}
				config := &llmagent.Config{}
				applyHookConfig(config, loop.Hooks{BeforeModel: []loop.BeforeModelHook{func(_ context.Context, request *loop.ModelRequest) (*loop.ModelResponse, error) {
					if request.Raw != native {
						t.Fatalf("Raw = %#v, want native request", request.Raw)
					}
					return nil, nil
				}}})
				response, err := config.BeforeModelCallbacks[0](&bridgeContext{}, native)
				if err != nil || response != nil {
					t.Fatalf("response = %#v, error = %v", response, err)
				}
			},
		},
		{
			name: "after model response",
			run: func(t *testing.T) {
				native := &model.LLMResponse{Content: genai.NewContentFromText("response", genai.RoleModel)}
				config := &llmagent.Config{}
				applyHookConfig(config, loop.Hooks{AfterModel: []loop.AfterModelHook{func(_ context.Context, response *loop.ModelResponse, _ error) (*loop.ModelResponse, error) {
					if response.Raw != native {
						t.Fatalf("Raw = %#v, want native response", response.Raw)
					}
					return nil, nil
				}}})
				response, err := config.AfterModelCallbacks[0](&bridgeContext{}, native, nil)
				if err != nil || response != native {
					t.Fatalf("response = %#v, error = %v", response, err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func TestHookErrorsStopLaterHooks(t *testing.T) {
	sentinel := errors.New("hook stopped")
	tests := []struct {
		name string
		run  func(loop.Hooks) error
	}{
		{
			name: "before model",
			run: func(hooks loop.Hooks) error {
				config := &llmagent.Config{}
				applyHookConfig(config, hooks)
				_, err := config.BeforeModelCallbacks[0](&bridgeContext{}, &model.LLMRequest{})
				return err
			},
		},
		{
			name: "after model",
			run: func(hooks loop.Hooks) error {
				config := &llmagent.Config{}
				applyHookConfig(config, hooks)
				_, err := config.AfterModelCallbacks[0](&bridgeContext{}, &model.LLMResponse{}, nil)
				return err
			},
		},
		{
			name: "before tool",
			run: func(hooks loop.Hooks) error {
				config := &llmagent.Config{}
				applyHookConfig(config, hooks)
				_, err := config.BeforeToolCallbacks[0](&bridgeContext{}, hookADKTool{name: "tool"}, map[string]any{})
				return err
			},
		},
		{
			name: "after tool",
			run: func(hooks loop.Hooks) error {
				config := &llmagent.Config{}
				applyHookConfig(config, hooks)
				_, err := config.AfterToolCallbacks[0](&bridgeContext{}, hookADKTool{name: "tool"}, map[string]any{}, map[string]any{}, nil)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			hooks := loop.Hooks{}
			switch test.name {
			case "before model":
				hooks.BeforeModel = []loop.BeforeModelHook{
					func(context.Context, *loop.ModelRequest) (*loop.ModelResponse, error) { return nil, sentinel },
					func(context.Context, *loop.ModelRequest) (*loop.ModelResponse, error) { calls++; return nil, nil },
				}
			case "after model":
				hooks.AfterModel = []loop.AfterModelHook{
					func(context.Context, *loop.ModelResponse, error) (*loop.ModelResponse, error) { return nil, sentinel },
					func(context.Context, *loop.ModelResponse, error) (*loop.ModelResponse, error) {
						calls++
						return nil, nil
					},
				}
			case "before tool":
				hooks.BeforeTool = []loop.BeforeToolHook{
					func(context.Context, *loop.ToolCall) (*loop.ToolResult, error) { return nil, sentinel },
					func(context.Context, *loop.ToolCall) (*loop.ToolResult, error) { calls++; return nil, nil },
				}
			case "after tool":
				hooks.AfterTool = []loop.AfterToolHook{
					func(context.Context, *loop.ToolCall, *loop.ToolResult, error) (*loop.ToolResult, error) {
						return nil, sentinel
					},
					func(context.Context, *loop.ToolCall, *loop.ToolResult, error) (*loop.ToolResult, error) {
						calls++
						return nil, nil
					},
				}
			}
			if err := test.run(hooks); !errors.Is(err, sentinel) || calls != 0 {
				t.Fatalf("error = %v, later calls = %d", err, calls)
			}
		})
	}
}

var _ adktool.Tool = hookADKTool{}

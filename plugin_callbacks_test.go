package plasmid

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	adkplugin "google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/warning"
)

func TestGuardPluginCallbacksIsolatesPanics(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		configure func(*adkplugin.Config)
		invoke    func(*adkplugin.Plugin) error
	}{
		{
			name: "on user message", path: "on user message",
			configure: func(config *adkplugin.Config) {
				config.OnUserMessageCallback = func(agent.InvocationContext, *genai.Content) (*genai.Content, error) { panic("TOPSECRET") }
			},
			invoke: func(plugin *adkplugin.Plugin) error {
				_, err := plugin.OnUserMessageCallback()(nil, nil)
				return err
			},
		},
		{
			name: "on event", path: "on event",
			configure: func(config *adkplugin.Config) {
				config.OnEventCallback = func(agent.InvocationContext, *session.Event) (*session.Event, error) { panic("TOPSECRET") }
			},
			invoke: func(plugin *adkplugin.Plugin) error {
				_, err := plugin.OnEventCallback()(nil, nil)
				return err
			},
		},
		{
			name: "before run", path: "before run",
			configure: func(config *adkplugin.Config) {
				config.BeforeRunCallback = func(agent.InvocationContext) (*genai.Content, error) { panic("TOPSECRET") }
			},
			invoke: func(plugin *adkplugin.Plugin) error {
				_, err := plugin.BeforeRunCallback()(nil)
				return err
			},
		},
		{
			name: "after run", path: "after run",
			configure: func(config *adkplugin.Config) {
				config.AfterRunCallback = func(agent.InvocationContext) { panic("TOPSECRET") }
			},
			invoke: func(plugin *adkplugin.Plugin) error {
				plugin.AfterRunCallback()(nil)
				return nil
			},
		},
		{
			name: "before agent", path: "before agent",
			configure: func(config *adkplugin.Config) {
				config.BeforeAgentCallback = func(agent.Context) (*genai.Content, error) { panic("TOPSECRET") }
			},
			invoke: func(plugin *adkplugin.Plugin) error {
				_, err := plugin.BeforeAgentCallback()(nil)
				return err
			},
		},
		{
			name: "after agent", path: "after agent",
			configure: func(config *adkplugin.Config) {
				config.AfterAgentCallback = func(agent.Context) (*genai.Content, error) { panic("TOPSECRET") }
			},
			invoke: func(plugin *adkplugin.Plugin) error {
				_, err := plugin.AfterAgentCallback()(nil)
				return err
			},
		},
		{
			name: "before model", path: "before model",
			configure: func(config *adkplugin.Config) {
				config.BeforeModelCallback = func(agent.Context, *model.LLMRequest) (*model.LLMResponse, error) { panic("TOPSECRET") }
			},
			invoke: func(plugin *adkplugin.Plugin) error {
				_, err := plugin.BeforeModelCallback()(nil, nil)
				return err
			},
		},
		{
			name: "after model", path: "after model",
			configure: func(config *adkplugin.Config) {
				config.AfterModelCallback = func(agent.Context, *model.LLMResponse, error) (*model.LLMResponse, error) { panic("TOPSECRET") }
			},
			invoke: func(plugin *adkplugin.Plugin) error {
				_, err := plugin.AfterModelCallback()(nil, nil, errors.New("cause"))
				return err
			},
		},
		{
			name: "on model error", path: "on model error",
			configure: func(config *adkplugin.Config) {
				config.OnModelErrorCallback = func(agent.Context, *model.LLMRequest, error) (*model.LLMResponse, error) { panic("TOPSECRET") }
			},
			invoke: func(plugin *adkplugin.Plugin) error {
				_, err := plugin.OnModelErrorCallback()(nil, nil, errors.New("cause"))
				return err
			},
		},
		{
			name: "before tool", path: "before tool",
			configure: func(config *adkplugin.Config) {
				config.BeforeToolCallback = func(agent.Context, tool.Tool, map[string]any) (map[string]any, error) { panic("TOPSECRET") }
			},
			invoke: func(plugin *adkplugin.Plugin) error {
				_, err := plugin.BeforeToolCallback()(nil, nil, nil)
				return err
			},
		},
		{
			name: "after tool", path: "after tool",
			configure: func(config *adkplugin.Config) {
				config.AfterToolCallback = func(agent.Context, tool.Tool, map[string]any, map[string]any, error) (map[string]any, error) {
					panic("TOPSECRET")
				}
			},
			invoke: func(plugin *adkplugin.Plugin) error {
				_, err := plugin.AfterToolCallback()(nil, nil, nil, nil, errors.New("cause"))
				return err
			},
		},
		{
			name: "on tool error", path: "on tool error",
			configure: func(config *adkplugin.Config) {
				config.OnToolErrorCallback = func(agent.Context, tool.Tool, map[string]any, error) (map[string]any, error) { panic("TOPSECRET") }
			},
			invoke: func(plugin *adkplugin.Plugin) error {
				_, err := plugin.OnToolErrorCallback()(nil, nil, nil, errors.New("cause"))
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := adkplugin.Config{Name: "panic-callback"}
			test.configure(&config)
			plugin, err := adkplugin.New(config)
			if err != nil {
				t.Fatal(err)
			}
			warnings := &warning.SliceSink{}
			guarded, err := guardPluginCallbacks(plugin, warnings)
			if err != nil {
				t.Fatal(err)
			}
			callbackErr := test.invoke(guarded)
			if test.name != "after run" && callbackErr == nil {
				t.Fatal("panicking callback returned nil error")
			}
			if callbackErr != nil && strings.Contains(callbackErr.Error(), "TOPSECRET") {
				t.Fatalf("callback error leaked panic value: %v", callbackErr)
			}
			notices := warnings.Warnings()
			if len(notices) != 1 || notices[0].Code != warning.WarnPluginCallbackPanic || notices[0].Source != "plugin:panic-callback" || notices[0].Path != test.path || strings.Contains(notices[0].Message, "TOPSECRET") {
				t.Fatalf("warnings = %#v", notices)
			}
		})
	}
}

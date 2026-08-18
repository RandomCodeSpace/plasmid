package plasmid

import (
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	adkplugin "google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/warning"
)

func guardPluginCallbacks(value *adkplugin.Plugin, sink warning.Warner) (*adkplugin.Plugin, error) {
	name := value.Name()
	config := adkplugin.Config{Name: name}
	if callback := value.OnUserMessageCallback(); callback != nil {
		config.OnUserMessageCallback = func(ctx agent.InvocationContext, content *genai.Content) (result *genai.Content, err error) {
			defer guardPluginPanic(sink, name, "on user message", &err)
			return callback(ctx, content)
		}
	}
	if callback := value.OnEventCallback(); callback != nil {
		config.OnEventCallback = func(ctx agent.InvocationContext, event *session.Event) (result *session.Event, err error) {
			defer guardPluginPanic(sink, name, "on event", &err)
			return callback(ctx, event)
		}
	}
	if callback := value.BeforeRunCallback(); callback != nil {
		config.BeforeRunCallback = func(ctx agent.InvocationContext) (result *genai.Content, err error) {
			defer guardPluginPanic(sink, name, "before run", &err)
			return callback(ctx)
		}
	}
	if callback := value.AfterRunCallback(); callback != nil {
		config.AfterRunCallback = func(ctx agent.InvocationContext) {
			defer guardPluginPanicOnly(sink, name, "after run")
			callback(ctx)
		}
	}
	if callback := value.BeforeAgentCallback(); callback != nil {
		config.BeforeAgentCallback = func(ctx agent.Context) (result *genai.Content, err error) {
			defer guardPluginPanic(sink, name, "before agent", &err)
			return callback(ctx)
		}
	}
	if callback := value.AfterAgentCallback(); callback != nil {
		config.AfterAgentCallback = func(ctx agent.Context) (result *genai.Content, err error) {
			defer guardPluginPanic(sink, name, "after agent", &err)
			return callback(ctx)
		}
	}
	if callback := value.BeforeModelCallback(); callback != nil {
		config.BeforeModelCallback = func(ctx agent.Context, request *model.LLMRequest) (result *model.LLMResponse, err error) {
			defer guardPluginPanic(sink, name, "before model", &err)
			return callback(ctx, request)
		}
	}
	if callback := value.AfterModelCallback(); callback != nil {
		config.AfterModelCallback = func(ctx agent.Context, response *model.LLMResponse, cause error) (result *model.LLMResponse, err error) {
			defer guardPluginPanic(sink, name, "after model", &err)
			return callback(ctx, response, cause)
		}
	}
	if callback := value.OnModelErrorCallback(); callback != nil {
		config.OnModelErrorCallback = func(ctx agent.Context, request *model.LLMRequest, cause error) (result *model.LLMResponse, err error) {
			defer guardPluginPanic(sink, name, "on model error", &err)
			return callback(ctx, request, cause)
		}
	}
	if callback := value.BeforeToolCallback(); callback != nil {
		config.BeforeToolCallback = func(ctx agent.Context, current tool.Tool, arguments map[string]any) (result map[string]any, err error) {
			defer guardPluginPanic(sink, name, "before tool", &err)
			return callback(ctx, current, arguments)
		}
	}
	if callback := value.AfterToolCallback(); callback != nil {
		config.AfterToolCallback = func(ctx agent.Context, current tool.Tool, arguments, output map[string]any, cause error) (result map[string]any, err error) {
			defer guardPluginPanic(sink, name, "after tool", &err)
			return callback(ctx, current, arguments, output, cause)
		}
	}
	if callback := value.OnToolErrorCallback(); callback != nil {
		config.OnToolErrorCallback = func(ctx agent.Context, current tool.Tool, arguments map[string]any, cause error) (result map[string]any, err error) {
			defer guardPluginPanic(sink, name, "on tool error", &err)
			return callback(ctx, current, arguments, cause)
		}
	}
	return adkplugin.New(config)
}

func guardPluginPanic(sink warning.Warner, pluginName, callback string, err *error) {
	if recover() == nil {
		return
	}
	sink.Warn(warning.Warning{Code: warning.WarnPluginCallbackPanic, Source: "plugin:" + pluginName, Path: callback, Message: "compiled plugin callback panicked"})
	*err = fmt.Errorf("plugin %q %s callback panicked", pluginName, callback)
}

func guardPluginPanicOnly(sink warning.Warner, pluginName, callback string) {
	if recover() == nil {
		return
	}
	sink.Warn(warning.Warning{Code: warning.WarnPluginCallbackPanic, Source: "plugin:" + pluginName, Path: callback, Message: "compiled plugin callback panicked"})
}

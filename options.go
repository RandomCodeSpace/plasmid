package plasmid

import (
	"log/slog"

	"google.golang.org/adk/v2/model"
	adkplugin "google.golang.org/adk/v2/plugin"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/RandomCodeSpace/plasmid/config"
)

// LSPMode selects automatic detection or complete LSP disablement.
type LSPMode = config.LSPMode

const (
	// LSPAuto enables lazy configured LSP detection.
	LSPAuto = config.LSPAuto
	// LSPOff disables LSP behavior.
	LSPOff = config.LSPOff
)

// ForeignResolution configures which installed foreign hosts are scanned.
type ForeignResolution = config.Foreign

// Option configures a Harness before construction begins.
type Option func(*options) error

type options struct {
	model      model.LLM
	config     config.Options
	logger     *slog.Logger
	tools      []adktool.Tool
	plugins    []Plugin
	adkPlugins []*adkplugin.Plugin
}

// WithModel supplies the required native ADK model.
func WithModel(value model.LLM) Option {
	return func(o *options) error {
		o.model = value
		return nil
	}
}

// WithWorkingDir sets the workspace root.
func WithWorkingDir(path string) Option {
	return func(o *options) error {
		o.config.WorkingDir = path
		return nil
	}
}

// WithSessionDir sets the durable session storage directory.
func WithSessionDir(path string) Option {
	return func(o *options) error {
		o.config.SessionDir = path
		return nil
	}
}

// WithAppName sets the ADK application name.
func WithAppName(name string) Option {
	return func(o *options) error {
		o.config.AppName = &name
		return nil
	}
}

// WithUserID sets the durable session user identity.
func WithUserID(id string) Option {
	return func(o *options) error {
		o.config.UserID = id
		return nil
	}
}

// WithTools appends host-provided native ADK tools after built-in tools.
func WithTools(values ...adktool.Tool) Option {
	return func(o *options) error {
		o.tools = append(o.tools, values...)
		return nil
	}
}

// WithPlugins appends host-compiled Plasmid plugins in initialization order.
func WithPlugins(values ...Plugin) Option {
	return func(o *options) error {
		o.plugins = append(o.plugins, values...)
		return nil
	}
}

// WithADKPlugins appends native ADK plugins after compiled-registered plugins.
func WithADKPlugins(values ...*adkplugin.Plugin) Option {
	return func(o *options) error {
		o.adkPlugins = append(o.adkPlugins, values...)
		return nil
	}
}

// WithLSP overrides the configured LSP mode.
func WithLSP(mode LSPMode) Option {
	return func(o *options) error {
		o.config.LSPMode = &mode
		return nil
	}
}

// WithConfig selects one explicit versioned configuration file.
func WithConfig(path string) Option {
	return func(o *options) error {
		o.config.ConfigPath = path
		return nil
	}
}

// WithLogger sets the structured logger. The default discards logs.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) error {
		o.logger = logger
		return nil
	}
}

// WithForeignResolution overrides foreign discovery settings.
func WithForeignResolution(value ForeignResolution) Option {
	return func(o *options) error {
		copy := value
		o.config.Foreign = &copy
		return nil
	}
}

// WithToolConfirmation enables native ADK confirmation wrappers for tools.
func WithToolConfirmation(enabled bool) Option {
	return func(o *options) error {
		o.config.ToolConfirmation = &enabled
		return nil
	}
}

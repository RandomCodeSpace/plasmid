package plasmid

import (
	"fmt"
	"sync"

	adkplugin "google.golang.org/adk/v2/plugin"
	adktool "google.golang.org/adk/v2/tool"
)

type registry struct {
	mu                 sync.Mutex
	sealed             bool
	tools              []adktool.Tool
	toolsets           []adktool.Toolset
	compiledADKPlugins []*adkplugin.Plugin
	nativeADKPlugins   []*adkplugin.Plugin
}

// RegisterTools adds static native ADK tools during compiled-plugin Init.
func (h *Harness) RegisterTools(values ...adktool.Tool) error {
	if h == nil || h.registry == nil {
		return codedError(CodeClosed, "register tools", ErrClosed, nil)
	}
	return h.registry.addTools(values...)
}

// RegisterToolsets adds native ADK toolsets during compiled-plugin Init.
func (h *Harness) RegisterToolsets(values ...adktool.Toolset) error {
	if h == nil || h.registry == nil {
		return codedError(CodeClosed, "register toolsets", ErrClosed, nil)
	}
	return h.registry.addToolsets(values...)
}

// RegisterADKPlugins adds native ADK plugins during compiled-plugin Init.
func (h *Harness) RegisterADKPlugins(values ...*adkplugin.Plugin) error {
	if h == nil || h.registry == nil {
		return codedError(CodeClosed, "register ADK plugins", ErrClosed, nil)
	}
	return h.registry.addADKPlugins(values...)
}

func (r *registry) addTools(values ...adktool.Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return codedError(CodeRegistrationSealed, "register tools", ErrRegistrationSealed, nil)
	}
	r.tools = append(r.tools, values...)
	return nil
}

func (r *registry) addToolsets(values ...adktool.Toolset) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return codedError(CodeRegistrationSealed, "register toolsets", ErrRegistrationSealed, nil)
	}
	r.toolsets = append(r.toolsets, values...)
	return nil
}

func (r *registry) addADKPlugins(values ...*adkplugin.Plugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return codedError(CodeRegistrationSealed, "register ADK plugins", ErrRegistrationSealed, nil)
	}
	r.compiledADKPlugins = append(r.compiledADKPlugins, values...)
	return nil
}

func (r *registry) addNativeADKPlugins(values ...*adkplugin.Plugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return codedError(CodeRegistrationSealed, "register native ADK plugins", ErrRegistrationSealed, nil)
	}
	r.nativeADKPlugins = append(r.nativeADKPlugins, values...)
	return nil
}

type registrySnapshot struct {
	tools              []adktool.Tool
	toolsets           []adktool.Toolset
	compiledADKPlugins []*adkplugin.Plugin
	nativeADKPlugins   []*adkplugin.Plugin
}

func (r *registry) seal() (registrySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return registrySnapshot{}, codedError(CodeRegistrationSealed, "seal registry", ErrRegistrationSealed, nil)
	}
	r.sealed = true
	if err := validateStaticNames(r.tools, func(value adktool.Tool) string {
		if nilInterface(value) {
			return ""
		}
		return value.Name()
	}, "tool"); err != nil {
		return registrySnapshot{}, err
	}
	if err := validateStaticNames(r.toolsets, func(value adktool.Toolset) string {
		if nilInterface(value) {
			return ""
		}
		return value.Name()
	}, "toolset"); err != nil {
		return registrySnapshot{}, err
	}
	allPlugins := append(append([]*adkplugin.Plugin(nil), r.compiledADKPlugins...), r.nativeADKPlugins...)
	if err := validateStaticNames(allPlugins, func(value *adkplugin.Plugin) string {
		if value == nil {
			return ""
		}
		return value.Name()
	}, "ADK plugin"); err != nil {
		return registrySnapshot{}, err
	}
	return registrySnapshot{
		tools:              append([]adktool.Tool(nil), r.tools...),
		toolsets:           append([]adktool.Toolset(nil), r.toolsets...),
		compiledADKPlugins: append([]*adkplugin.Plugin(nil), r.compiledADKPlugins...),
		nativeADKPlugins:   append([]*adkplugin.Plugin(nil), r.nativeADKPlugins...),
	}, nil
}

func (r *registry) ownedADKPlugins() []*adkplugin.Plugin {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := append([]*adkplugin.Plugin(nil), r.compiledADKPlugins...)
	return append(result, r.nativeADKPlugins...)
}

func validateStaticNames[T any](values []T, name func(T) string, kind string) error {
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		current, err := guardedStaticName(value, name)
		if err != nil {
			return codedError(CodeConstructionFailed, "validate registry", ErrConstructionFailed, fmt.Errorf("%s %d name: %w", kind, index, err))
		}
		if current == "" {
			return codedError(CodeInvalidArgument, "validate registry", ErrInvalidArgument, fmt.Errorf("%s %d has an empty name or is nil", kind, index))
		}
		if _, exists := seen[current]; exists {
			return codedError(CodeDuplicate, "validate registry", ErrDuplicate, fmt.Errorf("duplicate %s name %q", kind, current))
		}
		seen[current] = struct{}{}
	}
	return nil
}

func guardedStaticName[T any](value T, name func(T) string) (result string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panicked: %v", recovered)
		}
	}()
	return name(value), nil
}

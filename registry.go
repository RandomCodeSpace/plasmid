package plasmid

import (
	"fmt"
	"strings"
	"sync"

	adkplugin "google.golang.org/adk/v2/plugin"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/warning"
)

type registeredPromptFragment struct {
	plugin string
	value  PromptFragment
}

type registry struct {
	mu                 sync.Mutex
	sealed             bool
	tools              []adktool.Tool
	builtinToolsets    []adktool.Toolset
	toolsets           []adktool.Toolset
	compiledADKPlugins []*adkplugin.Plugin
	nativeADKPlugins   []*adkplugin.Plugin
	reservedToolNames  []string
	promptFragments    []registeredPromptFragment
	warnings           []warning.Warning
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

// RegisterPromptFragments appends plugin instructions after Plasmid's built-in
// instructions. It is valid only during the calling plugin's Init.
func (h *Harness) RegisterPromptFragments(values ...PromptFragment) error {
	if h == nil || h.registry == nil {
		return codedError(CodeClosed, "register prompt fragments", ErrClosed, nil)
	}
	return h.registry.addPromptFragments(h.registrationPlugin(), values...)
}

// RegisterWarnings publishes stable warnings from the calling plugin's Init.
func (h *Harness) RegisterWarnings(values ...warning.Warning) error {
	if h == nil || h.registry == nil {
		return codedError(CodeClosed, "register warnings", ErrClosed, nil)
	}
	return h.registry.addWarnings(h.registrationPlugin(), values...)
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

func (r *registry) addBuiltinToolsets(values ...adktool.Toolset) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return codedError(CodeRegistrationSealed, "register built-in toolsets", ErrRegistrationSealed, nil)
	}
	r.builtinToolsets = append(r.builtinToolsets, values...)
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

func (r *registry) addReservedToolNames(values ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return codedError(CodeRegistrationSealed, "reserve tool names", ErrRegistrationSealed, nil)
	}
	r.reservedToolNames = append(r.reservedToolNames, values...)
	return nil
}

func (r *registry) addPromptFragments(source string, values ...PromptFragment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return codedError(CodeRegistrationSealed, "register prompt fragments", ErrRegistrationSealed, nil)
	}
	if source == "" {
		return codedError(CodeInvalidArgument, "register prompt fragments", ErrInvalidArgument, fmt.Errorf("registration is only valid during plugin Init"))
	}
	for index, value := range values {
		if strings.TrimSpace(value.Name) == "" || strings.TrimSpace(value.Content) == "" {
			return codedError(CodeInvalidArgument, "register prompt fragments", ErrInvalidArgument, fmt.Errorf("prompt fragment %d requires name and content", index))
		}
		r.promptFragments = append(r.promptFragments, registeredPromptFragment{plugin: source, value: value})
	}
	return nil
}

func (r *registry) addWarnings(source string, values ...warning.Warning) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return codedError(CodeRegistrationSealed, "register warnings", ErrRegistrationSealed, nil)
	}
	if source == "" {
		return codedError(CodeInvalidArgument, "register warnings", ErrInvalidArgument, fmt.Errorf("registration is only valid during plugin Init"))
	}
	for index, value := range values {
		if strings.TrimSpace(value.Code) == "" {
			return codedError(CodeInvalidArgument, "register warnings", ErrInvalidArgument, fmt.Errorf("warning %d requires a code", index))
		}
		if value.Source == "" {
			value.Source = "plugin:" + source
		}
		r.warnings = append(r.warnings, value)
	}
	return nil
}

func (r *registry) extensionMetadata() ([]registeredPromptFragment, []warning.Warning) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]registeredPromptFragment(nil), r.promptFragments...), append([]warning.Warning(nil), r.warnings...)
}

type registrySnapshot struct {
	tools              []adktool.Tool
	toolsets           []adktool.Toolset
	compiledADKPlugins []*adkplugin.Plugin
	nativeADKPlugins   []*adkplugin.Plugin
	promptFragments    []registeredPromptFragment
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
	seenToolNames := make(map[string]bool, len(r.tools)+len(r.reservedToolNames))
	for _, value := range r.tools {
		seenToolNames[value.Name()] = true
	}
	for index, name := range r.reservedToolNames {
		if name == "" {
			return registrySnapshot{}, codedError(CodeInvalidArgument, "validate registry", ErrInvalidArgument, fmt.Errorf("reserved tool name %d is empty", index))
		}
		if seenToolNames[name] {
			return registrySnapshot{}, codedError(CodeDuplicate, "validate registry", ErrDuplicate, fmt.Errorf("duplicate tool name %q", name))
		}
		seenToolNames[name] = true
	}
	seenFragments := make(map[string]bool, len(r.promptFragments))
	for _, fragment := range r.promptFragments {
		if seenFragments[fragment.value.Name] {
			return registrySnapshot{}, codedError(CodeDuplicate, "validate registry", ErrDuplicate, fmt.Errorf("duplicate prompt fragment name %q", fragment.value.Name))
		}
		seenFragments[fragment.value.Name] = true
	}
	allToolsets := append(append([]adktool.Toolset(nil), r.builtinToolsets...), r.toolsets...)
	if err := validateStaticNames(allToolsets, func(value adktool.Toolset) string {
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
		toolsets:           allToolsets,
		compiledADKPlugins: append([]*adkplugin.Plugin(nil), r.compiledADKPlugins...),
		nativeADKPlugins:   append([]*adkplugin.Plugin(nil), r.nativeADKPlugins...),
		promptFragments:    append([]registeredPromptFragment(nil), r.promptFragments...),
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

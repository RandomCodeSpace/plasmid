package codingtools

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/warning"
)

const (
	defaultRegistryFileBytes   int64 = 5 << 20
	defaultRegistryGrepBytes   int64 = 10 << 20
	defaultRegistryBashTimeout       = 120 * time.Second
)

// Set is the fixed, ordered collection of built-in coding tools.
type Set struct {
	tools  []adktool.Tool
	byName map[string]adktool.Tool
}

// New constructs the built-in coding tools in their stable wire order.
func New(cfg Config) (*Set, error) {
	if cfg.Root == nil {
		return nil, errors.New("construct coding tools: workspace root is required")
	}
	if cfg.Queue == nil {
		return nil, errors.New("construct coding tools: mutation queue is required")
	}
	if cfg.Ledger == nil {
		return nil, errors.New("construct coding tools: workspace ledger is required")
	}
	if cfg.Touch == nil {
		return nil, errors.New("construct coding tools: touch bus is required")
	}

	cfg = defaultConfig(cfg)
	constructors := []func(Config) (adktool.Tool, error){
		NewReadTool,
		NewWriteTool,
		NewEditTool,
	}
	if cfg.Shell != nil {
		constructors = append(constructors, NewBashTool)
	} else {
		cfg.WarningSink.Warn(warning.Warning{
			Code:    warning.WarnCodingtoolsBashOmitted,
			Source:  "codingtools",
			Message: "bash tool omitted because no shell executor is configured",
		})
	}
	constructors = append(constructors, NewGrepTool, NewFindTool, NewListTool)

	tools := make([]adktool.Tool, 0, len(constructors))
	for _, construct := range constructors {
		tool, err := construct(cfg)
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return newSet(tools)
}

func defaultConfig(cfg Config) Config {
	if cfg.MaxReadBytes <= 0 {
		cfg.MaxReadBytes = defaultRegistryFileBytes
	}
	if cfg.MaxWriteBytes <= 0 {
		cfg.MaxWriteBytes = defaultRegistryFileBytes
	}
	if cfg.MaxGrepFileBytes <= 0 {
		cfg.MaxGrepFileBytes = defaultRegistryGrepBytes
	}
	if cfg.MaxTouchEvents <= 0 {
		cfg.MaxTouchEvents = MaxTouchEvents
	}
	if cfg.DefaultBashTimeout <= 0 {
		cfg.DefaultBashTimeout = defaultRegistryBashTimeout
	}
	if cfg.Output == (outputlimit.Policy{}) {
		cfg.Output = outputlimit.Defaults()
	}
	if cfg.Budget == nil {
		cfg.Budget = outputlimit.NewBudget(outputlimit.DefaultPerSession)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.WarningSink == nil {
		cfg.WarningSink = configWarningSink(cfg)
	}
	return cfg
}

func configWarningSink(cfg Config) warning.Sink {
	if cfg.WarningSink != nil {
		return cfg.WarningSink
	}
	return warning.SlogSink{Logger: cfg.Logger}
}

func newSet(tools []adktool.Tool) (*Set, error) {
	set := &Set{
		tools:  make([]adktool.Tool, 0, len(tools)),
		byName: make(map[string]adktool.Tool, len(tools)),
	}
	for _, tool := range tools {
		name := tool.Name()
		if _, exists := set.byName[name]; exists {
			return nil, fmt.Errorf("construct coding tools: duplicate tool name %q", name)
		}
		set.tools = append(set.tools, tool)
		set.byName[name] = tool
	}
	return set, nil
}

// Tools returns the tools in stable wire order. The returned slice is owned by
// the caller.
func (s *Set) Tools() []adktool.Tool {
	if s == nil {
		return nil
	}
	return append([]adktool.Tool(nil), s.tools...)
}

// Names returns stable tool names in tool order. The returned slice is owned
// by the caller.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	names := make([]string, len(s.tools))
	for index, tool := range s.tools {
		names[index] = tool.Name()
	}
	return names
}

// Tool returns the tool with an exact wire name.
func (s *Set) Tool(name string) (adktool.Tool, bool) {
	if s == nil {
		return nil, false
	}
	tool, ok := s.byName[name]
	return tool, ok
}

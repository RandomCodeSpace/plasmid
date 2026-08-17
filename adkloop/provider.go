// Package adkloop adapts Google ADK to Plasmid's framework-free loop contract.
package adkloop

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"reflect"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/loop"
)

// Config contains provider-specific dependencies. Turn configuration belongs
// to loop.RunnerConfig.
type Config struct {
	Model  model.LLM
	Logger *slog.Logger
}

type provider struct {
	mu        sync.Mutex
	model     model.LLM
	logger    *slog.Logger
	runner    *runner.Runner
	streaming bool
	closed    bool
	nextRun   uint64
	active    map[uint64]context.CancelFunc
}

// New creates an unconfigured ADK-backed provider.
func New(config Config) (loop.Provider, error) {
	if nilInterface(config.Model) {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidConfig)
	}
	return &provider{model: config.Model, logger: config.Logger, active: make(map[uint64]context.CancelFunc)}, nil
}

func (*provider) Name() string { return "google-adk" }

func (p *provider) Configure(ctx context.Context, config loop.RunnerConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrClosed
	}
	if p.runner != nil {
		return ErrAlreadyConfigured
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if config.AppName == "" {
		return fmt.Errorf("%w: app name is required", ErrInvalidConfig)
	}
	if config.AgentName == "" {
		return fmt.Errorf("%w: agent name is required", ErrInvalidConfig)
	}
	if nilInterface(config.Sessions) {
		return fmt.Errorf("%w: session store is required", ErrInvalidConfig)
	}
	configuredLogger := p.logger
	if config.Logger != nil {
		configuredLogger = config.Logger
	}

	staticNames := make(map[string]struct{}, len(config.Tools))
	adkTools := make([]adktool.Tool, 0, len(config.Tools))
	for index, coreTool := range config.Tools {
		if nilInterface(coreTool) {
			return fmt.Errorf("%w: tool %d is nil", ErrInvalidConfig, index)
		}
		name := coreTool.Name()
		if name == "" {
			return fmt.Errorf("%w: tool %d has an empty name", ErrInvalidConfig, index)
		}
		if _, duplicate := staticNames[name]; duplicate {
			return fmt.Errorf("%w: duplicate tool name %q", ErrInvalidConfig, name)
		}
		staticNames[name] = struct{}{}
		bridged, err := newToolBridge(coreTool)
		if err != nil {
			return fmt.Errorf("%w: tool %q: %v", ErrInvalidConfig, name, err)
		}
		adkTools = append(adkTools, bridged)
	}

	toolsetNames := make(map[string]struct{}, len(config.Toolsets))
	adkToolsets := make([]*toolsetBridge, 0, len(config.Toolsets))
	for index, coreToolset := range config.Toolsets {
		if nilInterface(coreToolset) {
			return fmt.Errorf("%w: toolset %d is nil", ErrInvalidConfig, index)
		}
		name := coreToolset.Name()
		if name == "" {
			return fmt.Errorf("%w: toolset %d has an empty name", ErrInvalidConfig, index)
		}
		if _, duplicate := toolsetNames[name]; duplicate {
			return fmt.Errorf("%w: duplicate toolset name %q", ErrInvalidConfig, name)
		}
		toolsetNames[name] = struct{}{}
		adkToolsets = append(adkToolsets, &toolsetBridge{core: coreToolset, reserved: staticNames})
	}

	llmConfig := llmagent.Config{
		Name:  config.AgentName,
		Model: p.model,
		Mode:  llmagent.ModeChat,
	}
	llmConfig.Tools = adkTools
	llmConfig.Toolsets = make([]adktool.Toolset, len(adkToolsets))
	for index, toolset := range adkToolsets {
		llmConfig.Toolsets[index] = toolset
	}
	applyHookConfig(&llmConfig, config.Hooks)
	if config.Instruction != nil {
		instruction := config.Instruction
		store := config.Sessions
		llmConfig.InstructionProvider = func(adkContext agent.ReadonlyContext) (string, error) {
			ref, _, err := store.Get(adkContext, adkContext.AppName(), adkContext.UserID(), adkContext.SessionID())
			if err != nil {
				return "", fmt.Errorf("load instruction session: %w", err)
			}
			return instruction(adkContext, ref)
		}
	}

	adkAgent, err := llmagent.New(llmConfig)
	if err != nil {
		return fmt.Errorf("create ADK agent: %w", err)
	}
	adkRunner, err := runner.New(runner.Config{
		AppName:           config.AppName,
		Agent:             adkAgent,
		SessionService:    NewSessionService(config.Sessions),
		AutoCreateSession: false,
	})
	if err != nil {
		return fmt.Errorf("create ADK runner: %w", err)
	}

	p.runner = adkRunner
	p.logger = configuredLogger
	p.streaming = config.Streaming
	return nil
}

func (p *provider) Run(ctx context.Context, request loop.RunRequest) iter.Seq2[loop.Event, error] {
	return func(yield func(loop.Event, error) bool) {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			yield(loop.Event{}, ErrClosed)
			return
		}
		adkRunner := p.runner
		streaming := p.streaming
		if adkRunner == nil {
			p.mu.Unlock()
			yield(loop.Event{}, ErrNotConfigured)
			return
		}
		runCtx, cancel := context.WithCancel(ctx)
		p.nextRun++
		runID := p.nextRun
		p.active[runID] = cancel
		p.mu.Unlock()
		defer func() {
			cancel()
			p.mu.Lock()
			delete(p.active, runID)
			p.mu.Unlock()
		}()

		content, err := messageToContent(request.Input)
		if err != nil {
			yield(loop.Event{}, fmt.Errorf("convert run input: %w", err))
			return
		}
		mode := agent.StreamingModeNone
		if streaming || request.Stream {
			mode = agent.StreamingModeSSE
		}
		for adkEvent, runErr := range adkRunner.Run(runCtx, request.UserID, request.SessionID, content, agent.RunConfig{StreamingMode: mode}) {
			if runErr != nil {
				if !yield(loop.Event{}, runErr) {
					return
				}
				continue
			}
			events, conversionErr := toLoopEvents(request.SessionID, adkEvent)
			if conversionErr != nil {
				if !yield(loop.Event{}, conversionErr) {
					return
				}
				continue
			}
			for _, event := range events {
				if !yield(event, nil) {
					return
				}
			}
		}
	}
}

func (p *provider) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	cancels := make([]context.CancelFunc, 0, len(p.active))
	for _, cancel := range p.active {
		cancels = append(cancels, cancel)
	}
	p.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

package plasmid

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/codingtools"
	"github.com/plasmid-dev/plasmid/compaction"
	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/contextresolver"
	"github.com/plasmid-dev/plasmid/extensions"
	"github.com/plasmid-dev/plasmid/foreign"
	"github.com/plasmid-dev/plasmid/lsp"
	plasmidmcp "github.com/plasmid-dev/plasmid/mcp"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/sessionstore"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/skills"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

type harnessConstruction struct {
	opts               options
	loaded             config.Result
	harness            *Harness
	warnings           warning.Sink
	root               *workspace.Root
	queue              *workspace.MutationQueue
	ledger             *workspace.Ledger
	touches            *workspace.TouchBus
	policy             outputlimit.Policy
	budget             *outputlimit.Budget
	store              *sessionstore.Store
	shell              *shellexec.Executor
	builtins           *codingtools.Set
	contexts           *contextresolver.Resolver
	homeDir            string
	hosts              *contextresolver.HostSelection
	compiledRecords    []extensions.CompiledPlugin
	instructionRecords []extensions.Instruction
	extensionStore     *extensions.Store
	skillToolset       *skills.Toolset
	registered         registrySnapshot
	scopedToolsets     []tool.Toolset
	guardedCallbacks   []*plugin.Plugin
}

type constructionStep struct {
	operation string
	run       func() error
}

func loadHarnessOptions(ctx context.Context, supplied []Option) (options, config.Result, error) {
	if ctx == nil {
		return options{}, config.Result{}, codedError(CodeInvalidArgument, opConstructHarness, ErrInvalidArgument, errors.New("context is nil"))
	}
	var opts options
	if err := applyHarnessOptions(&opts, supplied); err != nil {
		return options{}, config.Result{}, err
	}
	if nilInterface(opts.model) {
		return options{}, config.Result{}, codedError(CodeInvalidArgument, opConstructHarness, ErrInvalidArgument, errors.New("model is required"))
	}
	loaded, err := config.Load(ctx, opts.config)
	if err != nil {
		return options{}, config.Result{}, codedError(CodeConstructionFailed, "load configuration", ErrConstructionFailed, err)
	}
	return opts, loaded, nil
}

func applyHarnessOptions(opts *options, supplied []Option) error {
	return firstError(supplied, func(index int, apply Option) error {
		if apply == nil {
			return invalidOptionError(opConstructHarness, fmt.Errorf("option %d is nil", index))
		}
		return invalidOptionError("apply option", apply(opts))
	})
}

func invalidOptionError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return codedError(CodeInvalidArgument, operation, ErrInvalidArgument, err)
}

func firstError[T any](values []T, visit func(int, T) error) error {
	for index, value := range values {
		if err := visit(index, value); err != nil {
			return err
		}
	}
	return nil
}

func newHarnessConstruction(ctx context.Context, opts options, loaded config.Result) (*harnessConstruction, context.Context) {
	logger := opts.logger
	logWarnings := true
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		logWarnings = false
	}
	collector := &warning.SliceSink{}
	for _, notice := range loaded.Warnings {
		collector.Warn(notice)
	}
	warnings := warning.Sink(collector)
	if logWarnings {
		warnings = multiWarningSink{collector, warning.SlogSink{Logger: logger}}
	}
	rootContext, cancelRoot := context.WithCancel(context.WithoutCancel(ctx))
	harness := &Harness{
		configuration:    loaded.Config,
		logger:           logger,
		warnings:         collector,
		registry:         &registry{},
		agentName:        rootAgentName,
		rootDone:         rootContext.Done(),
		cancelRoot:       cancelRoot,
		active:           make(map[string]context.CancelFunc),
		closeWaitTimeout: closeTimeout,
	}
	return &harnessConstruction{opts: opts, loaded: loaded, harness: harness, warnings: warnings}, rootContext
}

func (c *harnessConstruction) build(ctx, rootContext context.Context) error {
	if err := c.runConstructionSteps(
		constructionStep{run: c.openWorkspace},
		constructionStep{run: func() error { return c.configureLSP(rootContext) }},
		constructionStep{run: c.configureToolsAndContexts},
		constructionStep{run: c.configureCompiledPlugins},
		constructionStep{run: c.configureExtensions},
	); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return c.fail(opConstructHarness, err)
	}
	return c.runConstructionSteps(
		constructionStep{run: c.sealRegistry},
		constructionStep{run: func() error { return c.configureAgentAndRunner(ctx) }},
	)
}

func (c *harnessConstruction) runConstructionSteps(steps ...constructionStep) error {
	for _, step := range steps {
		if err := step.run(); err != nil {
			if step.operation != "" {
				return c.fail(step.operation, err)
			}
			return err
		}
	}
	return nil
}

func (c *harnessConstruction) fail(op string, cause error) error {
	c.harness.cancelRoot()
	if c.harness.registry != nil {
		c.harness.adkPlugins = c.harness.registry.ownedADKPlugins()
	}
	cleanupErr := c.harness.closeResources()
	code := CodeOf(cause)
	if code == "" {
		code = CodeConstructionFailed
	}
	return codedError(code, op, ErrConstructionFailed, errors.Join(cause, cleanupErr))
}

func (c *harnessConstruction) openWorkspace() error {
	return c.runConstructionSteps(
		constructionStep{operation: "construct workspace", run: func() error {
			root, err := workspace.NewRoot(c.loaded.Config.WorkingDir)
			if err == nil {
				c.root = root
			}
			return err
		}},
		constructionStep{operation: "open session store", run: func() error {
			store, err := sessionstore.OpenWith(sessionstore.Options{
				Dir: c.loaded.Config.SessionDir, Logger: c.harness.logger, WarningSink: c.warnings,
			})
			if err == nil {
				c.store = store
				c.harness.sessions = store
			}
			return err
		}},
		constructionStep{run: func() error {
			c.queue = workspace.NewMutationQueue()
			c.ledger = workspace.NewLedger()
			c.touches = workspace.NewTouchBus()
			c.policy = outputlimit.Defaults()
			c.policy.MaxBytes = c.loaded.Config.Tools.CallOutputBytes
			c.budget = outputlimit.NewBudget(c.loaded.Config.Tools.SessionOutputBytes)
			return nil
		}},
	)
}

func (c *harnessConstruction) configureLSP(rootContext context.Context) error {
	if c.loaded.Config.LSP.Mode == config.LSPOff {
		return nil
	}
	registryEntries := make([]lsp.Server, 0, len(c.loaded.Config.LSP.Servers))
	for _, server := range c.loaded.Config.LSP.Servers {
		registryEntries = append(registryEntries, lsp.Server{
			ID: server.ID, Command: server.Command, Args: append([]string(nil), server.Args...),
			Extensions: append([]string(nil), server.Extensions...), RootMarkers: append([]string(nil), server.RootMarkers...),
			Disabled: server.Disabled,
		})
	}
	lspRegistry := lsp.MergeRegistry(registryEntries, c.warnings)
	return c.runConstructionSteps(
		constructionStep{operation: "construct LSP manager", run: func() error {
			manager, err := lsp.NewManager(rootContext, lspRegistry, lsp.ManagerOptions{
				Warnings: c.warnings, InitializeTimeout: c.loaded.Config.LSP.InitializeTimeout,
				RequestTimeout: c.loaded.Config.LSP.RequestTimeout, FailureLimit: c.loaded.Config.LSP.FailureThreshold,
				DiagnosticsPerFile: c.loaded.Config.LSP.MaxDiagnosticsPerFile,
			})
			if err == nil {
				c.harness.lspManager = manager
			}
			return err
		}},
		constructionStep{operation: "construct LSP enforcement", run: func() error {
			enforcer, err := lsp.NewEnforcer(lsp.EnforcerOptions{
				WorkspaceDir: c.loaded.Config.WorkingDir, Touches: c.touches, Registry: lspRegistry, Manager: c.harness.lspManager,
				SettleTimeout: c.loaded.Config.LSP.SettleTimeout, Output: c.policy, Warnings: c.warnings,
				Maximum: c.loaded.Config.LSP.MaxDiagnosticsPerFile,
			})
			if err == nil {
				c.harness.lspEnforcer = enforcer
			}
			return err
		}},
	)
}

func (c *harnessConstruction) configureToolsAndContexts() error {
	return c.runConstructionSteps(
		constructionStep{operation: "construct shell executor", run: func() error {
			shell, err := shellexec.New(shellexec.Config{
				Root: c.root, DefaultTimeout: c.loaded.Config.Tools.BashTimeout,
				MaxTimeout: c.loaded.Config.Tools.BashMaxTimeout, OutputLimit: c.policy,
			})
			if errors.Is(err, shellexec.ErrNoShell) {
				err = nil
			}
			if err == nil {
				c.shell = shell
			}
			return err
		}},
		constructionStep{operation: "construct coding tools", run: func() error {
			builtins, err := codingtools.New(codingtools.Config{
				Root: c.root, Queue: c.queue, Ledger: c.ledger, Touch: c.touches, Shell: c.shell,
				Output: c.policy, Budget: c.budget,
				Logger: c.harness.logger, WarningSink: c.warnings, DefaultBashTimeout: c.loaded.Config.Tools.BashTimeout,
				MaxTouchEvents: c.loaded.Config.Context.TouchesPerToolCall,
			})
			if err == nil {
				c.builtins = builtins
			}
			return err
		}},
		constructionStep{run: func() error {
			c.homeDir, _ = os.UserHomeDir()
			c.hosts = &contextresolver.HostSelection{}
			if c.loaded.Config.Foreign.Enabled {
				c.hosts.Claude = c.loaded.Config.Foreign.Claude
				c.hosts.Codex = c.loaded.Config.Foreign.Codex
				c.hosts.Copilot = c.loaded.Config.Foreign.Copilot
			}
			return nil
		}},
		constructionStep{operation: "construct context resolver", run: func() error {
			contexts, err := contextresolver.New(contextresolver.Options{
				Root: c.root, HomeDir: c.homeDir, ImportRoots: c.loaded.Config.Context.ImportRoots,
				TrustedRoots: c.loaded.Config.Foreign.TrustedRoots, MaxFileBytes: c.loaded.Config.Context.MaxFileBytes,
				MaxBytes: c.loaded.Config.Context.MaxBytes, MaxImportDepth: c.loaded.Config.Context.MaxImportDepth,
				MaxImportDepthSet: true,
				PromptCommands:    c.loaded.Config.Syntax.PromptCommands, CommandTimeout: c.loaded.Config.Syntax.CommandTimeout,
				DocumentTimeout: c.loaded.Config.Syntax.DocumentTimeout, CommandOutputBytes: c.loaded.Config.Syntax.CommandOutputBytes,
				DocumentOutputBytes: c.loaded.Config.Syntax.DocumentOutputBytes, Executor: c.shell, WarningSink: c.warnings,
				Hosts: c.hosts,
			})
			if err == nil {
				c.contexts = contexts
				c.harness.contexts = contexts
				c.harness.unsubscribeContext = c.touches.Subscribe(contexts)
			}
			return err
		}},
		constructionStep{operation: "register coding tools", run: func() error {
			return c.harness.registry.addTools(c.builtins.Tools()...)
		}},
		constructionStep{operation: "register host tools", run: func() error {
			return c.harness.registry.addTools(c.opts.tools...)
		}},
	)
}

func (c *harnessConstruction) configureCompiledPlugins() error {
	if err := validatePlugins(c.opts.plugins); err != nil {
		return c.fail(opValidateCompiledPlugin, err)
	}
	c.compiledRecords = make([]extensions.CompiledPlugin, 0, len(c.opts.plugins))
	for _, compiled := range c.opts.plugins {
		name, _ := compiledPluginName(compiled)
		c.compiledRecords = append(c.compiledRecords, extensions.CompiledPlugin{
			Name:       name,
			Provenance: []extensions.Provenance{{Host: "plasmid", Scope: "compiled", PluginID: name, Enabled: true, Trusted: true, Classification: "compiled"}},
		})
	}
	c.harness.plugins = make([]Plugin, 0, len(c.opts.plugins))
	for _, compiled := range c.opts.plugins {
		c.harness.plugins = append(c.harness.plugins, compiled)
		if err := initializePlugin(compiled, c.harness); err != nil {
			name, _ := compiledPluginName(compiled)
			return c.fail("initialize compiled plugin "+name, err)
		}
	}
	fragments, pluginWarnings := c.harness.registry.extensionMetadata()
	for _, notice := range pluginWarnings {
		c.warnings.Warn(notice)
	}
	c.instructionRecords = make([]extensions.Instruction, 0, len(fragments))
	for _, fragment := range fragments {
		c.instructionRecords = append(c.instructionRecords, extensions.Instruction{
			Name:       fragment.value.Name,
			Provenance: []extensions.Provenance{{Host: "plasmid", Scope: "compiled", PluginID: fragment.plugin, Enabled: true, Trusted: true, Classification: "compiled"}},
		})
	}
	return nil
}

func (c *harnessConstruction) configureExtensions() error {
	projectTrusted := pathWithinAny(c.loaded.Config.WorkingDir, c.loaded.Config.Foreign.TrustedRoots)
	return c.runConstructionSteps(
		constructionStep{operation: "construct extension catalog", run: func() error {
			extensionStore, err := extensions.NewStore(extensions.Options{
				WorkingDir: c.loaded.Config.WorkingDir, HomeDir: c.homeDir, SkillRoots: c.loaded.Config.Skills.Roots,
				Foreign: foreign.Options{HomeDir: c.homeDir, WorkingDir: c.loaded.Config.WorkingDir, RepositoryRoot: c.loaded.Config.WorkingDir, ProjectTrusted: projectTrusted, MaxFileBytes: int64(c.loaded.Config.Context.MaxFileBytes)},
				Claude:  c.hosts.Claude, Codex: c.hosts.Codex, Copilot: c.hosts.Copilot, MCP: c.loaded.Config.MCP,
				Instructions: c.instructionRecords, CompiledPlugins: c.compiledRecords, MaxResourceBytes: int64(c.loaded.Config.Context.MaxFileBytes), WarningSink: c.warnings,
			})
			if err == nil {
				c.extensionStore = extensionStore
				c.harness.extensions = extensionStore
				c.harness.unsubscribeSkills = c.touches.Subscribe(extensionStore)
			}
			return err
		}},
		constructionStep{operation: "construct MCP manager", run: func() error {
			mcpManager, err := plasmidmcp.New(plasmidmcp.Options{
				Catalogs: c.extensionStore, WorkingDir: c.loaded.Config.WorkingDir, Warnings: c.warnings,
				Output: c.policy, Budget: c.budget,
			})
			if err == nil {
				c.harness.mcpManager = mcpManager
			}
			return err
		}},
		constructionStep{operation: "construct skill toolset", run: func() error {
			skillToolset, err := skills.New(skills.Config{
				Catalogs: c.extensionStore, Contexts: c.contexts, ProjectDir: c.loaded.Config.WorkingDir,
				Warnings: c.warnings, Output: c.policy, Budget: c.budget,
			})
			if err == nil {
				c.skillToolset = skillToolset
			}
			return err
		}},
		constructionStep{operation: "reserve skill tool names", run: func() error {
			return c.harness.registry.addReservedToolNames("list_skills", "load_skill", "load_skill_resource")
		}},
		constructionStep{operation: "register extension toolsets", run: func() error {
			return c.harness.registry.addBuiltinToolsets(c.skillToolset, c.harness.mcpManager)
		}},
	)
}

func (c *harnessConstruction) sealRegistry() error {
	return c.runConstructionSteps(
		constructionStep{operation: "register native ADK plugins", run: func() error {
			return c.harness.registry.addNativeADKPlugins(c.opts.adkPlugins...)
		}},
		constructionStep{operation: "seal registry", run: func() error {
			registered, err := c.harness.registry.seal()
			if err == nil {
				c.registered = registered
				c.harness.adkPlugins = append(append([]*plugin.Plugin(nil), registered.compiledADKPlugins...), registered.nativeADKPlugins...)
				c.scopedToolsets = []tool.Toolset{scopedToolset{name: "plasmid", tools: registered.tools, contexts: c.contexts}}
				for _, source := range registered.toolsets {
					c.scopedToolsets = append(c.scopedToolsets, scopedToolset{name: source.Name(), source: source, contexts: c.contexts})
				}
			}
			return err
		}},
	)
}

func (c *harnessConstruction) configureAgentAndRunner(ctx context.Context) error {
	agentConfig := c.agentConfig()
	var rootAgent agent.Agent
	var runnerPlugins []*plugin.Plugin
	return c.runConstructionSteps(
		constructionStep{operation: "guard plugin callbacks", run: func() error {
			guardedCallbacks, err := c.guardCallbacks()
			if err == nil {
				c.guardedCallbacks = guardedCallbacks
				for _, registeredPlugin := range guardedCallbacks {
					appendAgentPluginCallbacks(&agentConfig, registeredPlugin)
				}
				if c.loaded.Config.Tools.Confirmation {
					agentConfig.Toolsets = confirmationToolsets(c.scopedToolsets)
				}
			}
			return err
		}},
		constructionStep{operation: "construct native agent", run: func() error {
			value, err := llmagent.New(agentConfig)
			if err == nil {
				rootAgent = value
			}
			return err
		}},
		constructionStep{operation: "project compiled plugin callbacks", run: func() error {
			values, err := runnerCallbackPlugins(c.guardedCallbacks)
			if err == nil {
				runnerPlugins = values
			}
			return err
		}},
		constructionStep{operation: "construct native runner", run: func() error {
			nativeRunner, err := runner.New(runner.Config{
				AppName: c.loaded.Config.AppName, Agent: rootAgent, SessionService: c.store,
				AutoCreateSession: false,
				PluginConfig:      runner.PluginConfig{Plugins: runnerPlugins, CloseTimeout: closeTimeout},
			})
			if err == nil {
				c.harness.runner = nativeRunner
			}
			return err
		}},
		constructionStep{operation: opConstructHarness, run: ctx.Err},
	)
}

func (c *harnessConstruction) agentConfig() llmagent.Config {
	result := llmagent.Config{
		Name: rootAgentName, Model: c.opts.model, Mode: llmagent.ModeChat,
		Toolsets: c.scopedToolsets,
		InstructionProvider: instructionProvider{
			contexts:  c.contexts,
			enforcer:  c.harness.lspEnforcer,
			fragments: c.registered.promptFragments,
		}.Provide,
	}
	if enforcer := c.harness.lspEnforcer; enforcer != nil {
		result.AfterToolCallbacks = append(result.AfterToolCallbacks, lspAfterToolCallback(enforcer, c.warnings))
	}
	compactor := compaction.New(compaction.Config{
		Policy: c.loaded.Config.Compaction, Store: c.store, Budget: c.budget, WarningSink: c.warnings,
	})
	result.BeforeModelCallbacks = append(result.BeforeModelCallbacks, compactor.BeforeModel)
	result.AfterModelCallbacks = append(result.AfterModelCallbacks, compactor.AfterModel)
	result.BeforeToolCallbacks = append(result.BeforeToolCallbacks, func(ctx agent.Context, current tool.Tool, args map[string]any) (map[string]any, error) {
		return nil, toolPolicyError(ctx, c.contexts, current.Name(), args)
	})
	return result
}

func (c *harnessConstruction) guardCallbacks() ([]*plugin.Plugin, error) {
	ordered := append(append([]*plugin.Plugin(nil), c.registered.compiledADKPlugins...), c.registered.nativeADKPlugins...)
	guarded := make([]*plugin.Plugin, 0, len(ordered))
	err := firstError(ordered, func(_ int, registeredPlugin *plugin.Plugin) error {
		value, err := guardPluginCallbacks(registeredPlugin, c.warnings)
		if err == nil {
			guarded = append(guarded, value)
		}
		return err
	})
	return guarded, err
}

func appendAgentPluginCallbacks(config *llmagent.Config, registeredPlugin *plugin.Plugin) {
	if callback := registeredPlugin.BeforeAgentCallback(); callback != nil {
		config.BeforeAgentCallbacks = append(config.BeforeAgentCallbacks, callback)
	}
	if callback := registeredPlugin.AfterAgentCallback(); callback != nil {
		config.AfterAgentCallbacks = append(config.AfterAgentCallbacks, callback)
	}
	if callback := registeredPlugin.BeforeModelCallback(); callback != nil {
		config.BeforeModelCallbacks = append(config.BeforeModelCallbacks, callback)
	}
	if callback := registeredPlugin.AfterModelCallback(); callback != nil {
		config.AfterModelCallbacks = append(config.AfterModelCallbacks, callback)
	}
	if callback := registeredPlugin.OnModelErrorCallback(); callback != nil {
		config.OnModelErrorCallbacks = append(config.OnModelErrorCallbacks, callback)
	}
	if callback := registeredPlugin.BeforeToolCallback(); callback != nil {
		config.BeforeToolCallbacks = append(config.BeforeToolCallbacks, callback)
	}
	if callback := registeredPlugin.AfterToolCallback(); callback != nil {
		config.AfterToolCallbacks = append(config.AfterToolCallbacks, callback)
	}
	if callback := registeredPlugin.OnToolErrorCallback(); callback != nil {
		config.OnToolErrorCallbacks = append(config.OnToolErrorCallbacks, callback)
	}
}

func confirmationToolsets(values []tool.Toolset) []tool.Toolset {
	result := make([]tool.Toolset, 0, len(values))
	for _, current := range values {
		scoped := current.(scopedToolset)
		scoped.confirmation = true
		result = append(result, scoped)
	}
	return result
}

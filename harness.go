package plasmid

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/codingtools"
	"github.com/plasmid-dev/plasmid/compaction"
	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/contextresolver"
	"github.com/plasmid-dev/plasmid/extensions"
	"github.com/plasmid-dev/plasmid/foreign"
	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/lsp"
	plasmidmcp "github.com/plasmid-dev/plasmid/mcp"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/sessionstore"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/skills"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

const (
	rootAgentName = "plasmid"
	closeTimeout  = 10 * time.Second
)

// Harness is the in-process native Google ADK coding-agent runtime.
type Harness struct {
	configuration      config.Config
	logger             *slog.Logger
	warnings           *warning.SliceSink
	registry           *registry
	runner             *runner.Runner
	sessions           *sessionstore.Store
	contexts           *contextresolver.Resolver
	extensions         *extensions.Store
	mcpManager         *plasmidmcp.Manager
	lspManager         *lsp.Manager
	lspEnforcer        *lsp.Enforcer
	agentName          string
	unsubscribeContext func()
	unsubscribeSkills  func()

	rootContext context.Context
	cancelRoot  context.CancelFunc

	mu                 sync.Mutex
	closed             bool
	active             map[string]context.CancelFunc
	activeRuns         sync.WaitGroup
	plugins            []Plugin
	adkPlugins         []*plugin.Plugin
	closeDone          chan struct{}
	closeErr           error
	closeWaitTimeout   time.Duration
	initializingPlugin string
}

// Format prevents resolved credentials and owned transport state from
// appearing in diagnostic formatting.
func (*Harness) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "plasmid.Harness{redacted}")
}

// LogValue prevents structured logging from reflecting Harness internals.
func (*Harness) LogValue() slog.Value { return slog.StringValue("plasmid.Harness{redacted}") }

// New constructs a complete native ADK Harness transactionally.
func New(ctx context.Context, supplied ...Option) (*Harness, error) {
	if ctx == nil {
		return nil, codedError(CodeInvalidArgument, "construct harness", ErrInvalidArgument, errors.New("context is nil"))
	}
	var opts options
	for index, apply := range supplied {
		if apply == nil {
			return nil, codedError(CodeInvalidArgument, "construct harness", ErrInvalidArgument, fmt.Errorf("option %d is nil", index))
		}
		if err := apply(&opts); err != nil {
			return nil, codedError(CodeInvalidArgument, "apply option", ErrInvalidArgument, err)
		}
	}
	if nilInterface(opts.model) {
		return nil, codedError(CodeInvalidArgument, "construct harness", ErrInvalidArgument, errors.New("model is required"))
	}
	loaded, err := config.Load(ctx, opts.config)
	if err != nil {
		return nil, codedError(CodeConstructionFailed, "load configuration", ErrConstructionFailed, err)
	}
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

	rootContext, cancelRoot := context.WithCancel(context.Background())
	harness := &Harness{
		configuration:    loaded.Config,
		logger:           logger,
		warnings:         collector,
		registry:         &registry{},
		agentName:        rootAgentName,
		rootContext:      rootContext,
		cancelRoot:       cancelRoot,
		active:           make(map[string]context.CancelFunc),
		closeWaitTimeout: closeTimeout,
	}
	fail := func(op string, cause error) (*Harness, error) {
		cancelRoot()
		if harness.registry != nil {
			harness.adkPlugins = harness.registry.ownedADKPlugins()
		}
		cleanupErr := harness.closeResources()
		code := CodeOf(cause)
		if code == "" {
			code = CodeConstructionFailed
		}
		return nil, codedError(code, op, ErrConstructionFailed, errors.Join(cause, cleanupErr))
	}

	root, err := workspace.NewRoot(loaded.Config.WorkingDir)
	if err != nil {
		return fail("construct workspace", err)
	}
	store, err := sessionstore.OpenWith(sessionstore.Options{
		Dir: loaded.Config.SessionDir, Logger: logger, WarningSink: warnings,
	})
	if err != nil {
		return fail("open session store", err)
	}
	harness.sessions = store

	queue := workspace.NewMutationQueue()
	ledger := workspace.NewLedger()
	touches := workspace.NewTouchBus()
	policy := outputlimit.Defaults()
	policy.MaxBytes = loaded.Config.Tools.CallOutputBytes
	budget := outputlimit.NewBudget(loaded.Config.Tools.SessionOutputBytes)
	if loaded.Config.LSP.Mode != config.LSPOff {
		registryEntries := make([]lsp.Server, 0, len(loaded.Config.LSP.Servers))
		for _, server := range loaded.Config.LSP.Servers {
			registryEntries = append(registryEntries, lsp.Server{
				ID: server.ID, Command: server.Command, Args: append([]string(nil), server.Args...),
				Extensions: append([]string(nil), server.Extensions...), RootMarkers: append([]string(nil), server.RootMarkers...),
				Disabled: server.Disabled,
			})
		}
		lspRegistry := lsp.MergeRegistry(registryEntries, warnings)
		manager, managerErr := lsp.NewManager(rootContext, lspRegistry, lsp.ManagerOptions{
			Warnings: warnings, InitializeTimeout: loaded.Config.LSP.InitializeTimeout,
			RequestTimeout: loaded.Config.LSP.RequestTimeout, FailureLimit: loaded.Config.LSP.FailureThreshold,
			DiagnosticsPerFile: loaded.Config.LSP.MaxDiagnosticsPerFile,
		})
		if managerErr != nil {
			return fail("construct LSP manager", managerErr)
		}
		harness.lspManager = manager
		enforcer, enforcerErr := lsp.NewEnforcer(lsp.EnforcerOptions{
			WorkspaceDir: loaded.Config.WorkingDir, Touches: touches, Registry: lspRegistry, Manager: manager,
			SettleTimeout: loaded.Config.LSP.SettleTimeout, Output: policy, Warnings: warnings,
			Maximum: loaded.Config.LSP.MaxDiagnosticsPerFile,
		})
		if enforcerErr != nil {
			return fail("construct LSP enforcement", enforcerErr)
		}
		harness.lspEnforcer = enforcer
	}
	shell, shellErr := shellexec.New(shellexec.Config{
		Root: root, DefaultTimeout: loaded.Config.Tools.BashTimeout,
		MaxTimeout: loaded.Config.Tools.BashMaxTimeout, OutputLimit: policy,
	})
	if shellErr != nil && !errors.Is(shellErr, shellexec.ErrNoShell) {
		return fail("construct shell executor", shellErr)
	}
	builtins, err := codingtools.New(codingtools.Config{
		Root: root, Queue: queue, Ledger: ledger, Touch: touches, Shell: shell,
		Output: policy, Budget: budget,
		Logger: logger, WarningSink: warnings, DefaultBashTimeout: loaded.Config.Tools.BashTimeout,
		MaxTouchEvents: loaded.Config.Context.TouchesPerToolCall,
	})
	if err != nil {
		return fail("construct coding tools", err)
	}
	homeDir, _ := os.UserHomeDir()
	hosts := &contextresolver.HostSelection{}
	if loaded.Config.Foreign.Enabled {
		hosts.Claude = loaded.Config.Foreign.Claude
		hosts.Codex = loaded.Config.Foreign.Codex
		hosts.Copilot = loaded.Config.Foreign.Copilot
	}
	contexts, err := contextresolver.New(contextresolver.Options{
		Root: root, HomeDir: homeDir, ImportRoots: loaded.Config.Context.ImportRoots,
		TrustedRoots: loaded.Config.Foreign.TrustedRoots, MaxFileBytes: loaded.Config.Context.MaxFileBytes,
		MaxBytes: loaded.Config.Context.MaxBytes, MaxImportDepth: loaded.Config.Context.MaxImportDepth,
		MaxImportDepthSet: true,
		PromptCommands:    loaded.Config.Syntax.PromptCommands, CommandTimeout: loaded.Config.Syntax.CommandTimeout,
		DocumentTimeout: loaded.Config.Syntax.DocumentTimeout, CommandOutputBytes: loaded.Config.Syntax.CommandOutputBytes,
		DocumentOutputBytes: loaded.Config.Syntax.DocumentOutputBytes, Executor: shell, WarningSink: warnings,
		Hosts: hosts,
	})
	if err != nil {
		return fail("construct context resolver", err)
	}
	harness.contexts = contexts
	harness.unsubscribeContext = touches.Subscribe(contexts)
	if err := harness.registry.addTools(builtins.Tools()...); err != nil {
		return fail("register coding tools", err)
	}
	if err := harness.registry.addTools(opts.tools...); err != nil {
		return fail("register host tools", err)
	}
	if err := validatePlugins(opts.plugins); err != nil {
		return fail("validate compiled plugins", err)
	}
	compiledRecords := make([]extensions.CompiledPlugin, 0, len(opts.plugins))
	for _, compiled := range opts.plugins {
		name, _ := compiledPluginName(compiled)
		compiledRecords = append(compiledRecords, extensions.CompiledPlugin{
			Name:       name,
			Provenance: []extensions.Provenance{{Host: "plasmid", Scope: "compiled", PluginID: name, Enabled: true, Trusted: true, Classification: "compiled"}},
		})
	}
	harness.plugins = make([]Plugin, 0, len(opts.plugins))
	for _, compiled := range opts.plugins {
		harness.plugins = append(harness.plugins, compiled)
		if err := initializePlugin(compiled, harness); err != nil {
			name, _ := compiledPluginName(compiled)
			return fail("initialize compiled plugin "+name, err)
		}
	}
	fragments, pluginWarnings := harness.registry.extensionMetadata()
	for _, notice := range pluginWarnings {
		warnings.Warn(notice)
	}
	instructionRecords := make([]extensions.Instruction, 0, len(fragments))
	for _, fragment := range fragments {
		instructionRecords = append(instructionRecords, extensions.Instruction{
			Name:       fragment.value.Name,
			Provenance: []extensions.Provenance{{Host: "plasmid", Scope: "compiled", PluginID: fragment.plugin, Enabled: true, Trusted: true, Classification: "compiled"}},
		})
	}
	projectTrusted := pathWithinAny(loaded.Config.WorkingDir, loaded.Config.Foreign.TrustedRoots)
	extensionStore, err := extensions.NewStore(extensions.Options{
		WorkingDir: loaded.Config.WorkingDir, HomeDir: homeDir, SkillRoots: loaded.Config.Skills.Roots,
		Foreign: foreign.Options{HomeDir: homeDir, WorkingDir: loaded.Config.WorkingDir, RepositoryRoot: loaded.Config.WorkingDir, ProjectTrusted: projectTrusted, MaxFileBytes: int64(loaded.Config.Context.MaxFileBytes)},
		Claude:  hosts.Claude, Codex: hosts.Codex, Copilot: hosts.Copilot, MCP: loaded.Config.MCP,
		Instructions: instructionRecords, CompiledPlugins: compiledRecords, MaxResourceBytes: int64(loaded.Config.Context.MaxFileBytes), WarningSink: warnings,
	})
	if err != nil {
		return fail("construct extension catalog", err)
	}
	harness.extensions = extensionStore
	harness.unsubscribeSkills = touches.Subscribe(extensionStore)
	mcpManager, err := plasmidmcp.New(plasmidmcp.Options{
		Catalogs: extensionStore, WorkingDir: loaded.Config.WorkingDir, Warnings: warnings,
		Output: policy, Budget: budget,
	})
	if err != nil {
		return fail("construct MCP manager", err)
	}
	harness.mcpManager = mcpManager
	skillToolset, err := skills.New(skills.Config{
		Catalogs: extensionStore, Contexts: contexts, ProjectDir: loaded.Config.WorkingDir,
		Warnings: warnings, Output: policy, Budget: budget,
	})
	if err != nil {
		return fail("construct skill toolset", err)
	}
	if err := harness.registry.addReservedToolNames("list_skills", "load_skill", "load_skill_resource"); err != nil {
		return fail("reserve skill tool names", err)
	}
	if err := harness.registry.addBuiltinToolsets(skillToolset, mcpManager); err != nil {
		return fail("register extension toolsets", err)
	}
	if err := ctx.Err(); err != nil {
		return fail("construct harness", err)
	}
	if err := harness.registry.addNativeADKPlugins(opts.adkPlugins...); err != nil {
		return fail("register native ADK plugins", err)
	}
	registered, err := harness.registry.seal()
	if err != nil {
		return fail("seal registry", err)
	}
	harness.adkPlugins = append(append([]*plugin.Plugin(nil), registered.compiledADKPlugins...), registered.nativeADKPlugins...)

	scopedToolsets := []tool.Toolset{scopedToolset{name: "plasmid", tools: registered.tools, contexts: contexts}}
	for _, source := range registered.toolsets {
		scopedToolsets = append(scopedToolsets, scopedToolset{name: source.Name(), source: source, contexts: contexts})
	}
	agentConfig := llmagent.Config{
		Name: rootAgentName, Model: opts.model, Mode: llmagent.ModeChat,
		Toolsets: scopedToolsets,
		InstructionProvider: instructionProvider{
			contexts:  contexts,
			enforcer:  harness.lspEnforcer,
			fragments: registered.promptFragments,
		}.Provide,
	}
	if enforcer := harness.lspEnforcer; enforcer != nil {
		agentConfig.AfterToolCallbacks = append(agentConfig.AfterToolCallbacks, lspAfterToolCallback(enforcer, warnings))
	}
	compactor := compaction.New(compaction.Config{
		Policy: loaded.Config.Compaction, Store: store, Budget: budget, WarningSink: warnings,
	})
	agentConfig.BeforeModelCallbacks = append(agentConfig.BeforeModelCallbacks, compactor.BeforeModel)
	agentConfig.AfterModelCallbacks = append(agentConfig.AfterModelCallbacks, compactor.AfterModel)
	agentConfig.BeforeToolCallbacks = append(agentConfig.BeforeToolCallbacks, func(ctx agent.Context, current tool.Tool, args map[string]any) (map[string]any, error) {
		return nil, toolPolicyError(ctx, contexts, current.Name(), args)
	})
	orderedCallbacks := append(append([]*plugin.Plugin(nil), registered.compiledADKPlugins...), registered.nativeADKPlugins...)
	guardedCallbacks := make([]*plugin.Plugin, 0, len(orderedCallbacks))
	for _, registeredPlugin := range orderedCallbacks {
		guarded, guardErr := guardPluginCallbacks(registeredPlugin, warnings)
		if guardErr != nil {
			return fail("guard plugin callbacks", guardErr)
		}
		guardedCallbacks = append(guardedCallbacks, guarded)
	}
	for _, registeredPlugin := range guardedCallbacks {
		if callback := registeredPlugin.BeforeAgentCallback(); callback != nil {
			agentConfig.BeforeAgentCallbacks = append(agentConfig.BeforeAgentCallbacks, callback)
		}
		if callback := registeredPlugin.AfterAgentCallback(); callback != nil {
			agentConfig.AfterAgentCallbacks = append(agentConfig.AfterAgentCallbacks, callback)
		}
		if callback := registeredPlugin.BeforeModelCallback(); callback != nil {
			agentConfig.BeforeModelCallbacks = append(agentConfig.BeforeModelCallbacks, callback)
		}
		if callback := registeredPlugin.AfterModelCallback(); callback != nil {
			agentConfig.AfterModelCallbacks = append(agentConfig.AfterModelCallbacks, callback)
		}
		if callback := registeredPlugin.OnModelErrorCallback(); callback != nil {
			agentConfig.OnModelErrorCallbacks = append(agentConfig.OnModelErrorCallbacks, callback)
		}
		if callback := registeredPlugin.BeforeToolCallback(); callback != nil {
			agentConfig.BeforeToolCallbacks = append(agentConfig.BeforeToolCallbacks, callback)
		}
		if callback := registeredPlugin.AfterToolCallback(); callback != nil {
			agentConfig.AfterToolCallbacks = append(agentConfig.AfterToolCallbacks, callback)
		}
		if callback := registeredPlugin.OnToolErrorCallback(); callback != nil {
			agentConfig.OnToolErrorCallbacks = append(agentConfig.OnToolErrorCallbacks, callback)
		}
	}
	if loaded.Config.Tools.Confirmation {
		agentConfig.Toolsets = nil
		for _, current := range scopedToolsets {
			scoped := current.(scopedToolset)
			scoped.confirmation = true
			agentConfig.Toolsets = append(agentConfig.Toolsets, scoped)
		}
	}
	rootAgent, err := llmagent.New(agentConfig)
	if err != nil {
		return fail("construct native agent", err)
	}
	runnerPlugins, err := runnerCallbackPlugins(guardedCallbacks)
	if err != nil {
		return fail("project compiled plugin callbacks", err)
	}
	nativeRunner, err := runner.New(runner.Config{
		AppName: loaded.Config.AppName, Agent: rootAgent, SessionService: store,
		AutoCreateSession: false,
		PluginConfig:      runner.PluginConfig{Plugins: runnerPlugins, CloseTimeout: closeTimeout},
	})
	if err != nil {
		return fail("construct native runner", err)
	}
	harness.runner = nativeRunner
	if err := ctx.Err(); err != nil {
		return fail("construct harness", err)
	}
	return harness, nil
}

// NewSession creates a durable session with a store-generated canonical ID.
func (h *Harness) NewSession(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", codedError(CodeInvalidArgument, "create session", ErrInvalidArgument, errors.New("context is nil"))
	}
	operationContext, release, err := h.beginOperation(ctx, "create session")
	if err != nil {
		return "", err
	}
	defer release()
	ctx = operationContext
	response, err := h.sessions.Create(ctx, &session.CreateRequest{AppName: h.configuration.AppName, UserID: h.configuration.UserID})
	if err != nil {
		return "", h.sessionError("create session", err)
	}
	sessionID := response.Session.ID()
	if err := h.contexts.StartSession(ctx, sessionID); err != nil {
		deleteErr := h.sessions.Delete(context.Background(), &session.DeleteRequest{AppName: h.configuration.AppName, UserID: h.configuration.UserID, SessionID: sessionID})
		return "", codedError(CodeRuntimeFailed, "start session context", ErrRuntimeFailed, errors.Join(err, deleteErr))
	}
	if err := h.extensions.StartSessionWithInstructions(ctx, sessionID, extensionInstructionRecords(h.contexts.InstructionRecords(sessionID))); err != nil {
		h.contexts.DropSession(sessionID)
		deleteErr := h.sessions.Delete(context.Background(), &session.DeleteRequest{AppName: h.configuration.AppName, UserID: h.configuration.UserID, SessionID: sessionID})
		return "", codedError(CodeRuntimeFailed, "start session extensions", ErrRuntimeFailed, errors.Join(err, deleteErr))
	}
	return sessionID, nil
}

// ResumeSession verifies that a durable session already exists.
func (h *Harness) ResumeSession(ctx context.Context, sessionID string) error {
	if ctx == nil || sessionID == "" {
		return codedError(CodeInvalidArgument, "resume session", ErrInvalidArgument, errors.New("context and session id are required"))
	}
	operationContext, release, err := h.beginOperation(ctx, "resume session")
	if err != nil {
		return err
	}
	defer release()
	ctx = operationContext
	sessionContext, releaseSession, err := h.beginSessionOperation(ctx, sessionID, "resume session")
	if err != nil {
		return err
	}
	defer releaseSession()
	ctx = sessionContext
	_, err = h.sessions.Get(ctx, &session.GetRequest{
		AppName: h.configuration.AppName, UserID: h.configuration.UserID, SessionID: sessionID,
	})
	if err = h.sessionError("resume session", err); err != nil {
		return err
	}
	if err := h.mcpManager.DropSession(ctx, sessionID); err != nil {
		return codedError(CodeRuntimeFailed, "resume session MCP", ErrRuntimeFailed, err)
	}
	h.contexts.DropSession(sessionID)
	h.extensions.DropSession(sessionID)
	if err := h.contexts.StartSession(ctx, sessionID); err != nil {
		return codedError(CodeRuntimeFailed, "resume session context", ErrRuntimeFailed, err)
	}
	if err := h.extensions.StartSessionWithInstructions(ctx, sessionID, extensionInstructionRecords(h.contexts.InstructionRecords(sessionID))); err != nil {
		h.contexts.DropSession(sessionID)
		return codedError(CodeRuntimeFailed, "resume session extensions", ErrRuntimeFailed, err)
	}
	return nil
}

// Run executes one native ADK turn and yields native session events.
func (h *Harness) Run(ctx context.Context, sessionID, prompt string) iter.Seq2[*session.Event, error] {
	return h.run(ctx, sessionID, prompt, nil)
}

func (h *Harness) run(ctx context.Context, sessionID, prompt string, policy *syntax.ToolPolicy) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		if ctx == nil || sessionID == "" {
			yield(nil, codedError(CodeInvalidArgument, "run", ErrInvalidArgument, errors.New("context and session id are required")))
			return
		}
		runContext, release, err := h.beginRun(ctx, sessionID)
		if err != nil {
			yield(nil, err)
			return
		}
		defer release()
		h.runAcquired(runContext, sessionID, prompt, policy, yield)
	}
}

func (h *Harness) runAcquired(ctx context.Context, sessionID, prompt string, policy *syntax.ToolPolicy, yield func(*session.Event, error) bool) {
	defer h.contexts.ReleaseRun(sessionID)
	if policy != nil {
		if err := h.contexts.SetRunPolicy(sessionID, *policy); err != nil {
			yield(nil, codedError(CodeRuntimeFailed, "set run policy", ErrRuntimeFailed, err))
			return
		}
	}
	message := genai.NewContentFromText(prompt, genai.RoleUser)
	for event, runErr := range h.runner.Run(ctx, h.configuration.UserID, sessionID, message, agent.RunConfig{}) {
		if runErr != nil {
			runErr = h.sessionError("run", runErr)
		}
		if !yield(event, runErr) || runErr != nil {
			return
		}
	}
}

// ListTemplates returns the stable template snapshot for one session.
func (h *Harness) ListTemplates(ctx context.Context, sessionID string) ([]extensions.Template, error) {
	if ctx == nil || sessionID == "" {
		return nil, codedError(CodeInvalidArgument, "list templates", ErrInvalidArgument, errors.New("context and session id are required"))
	}
	operationContext, release, err := h.beginSessionOperation(ctx, sessionID, "list templates")
	if err != nil {
		return nil, err
	}
	defer release()
	catalog, err := h.extensionCatalog(operationContext, sessionID)
	if err != nil {
		return nil, err
	}
	return catalog.Templates(), nil
}

// GetTemplate resolves and expands one user-invocable template without running it.
func (h *Harness) GetTemplate(ctx context.Context, sessionID, name, arguments string) (string, error) {
	if ctx == nil || sessionID == "" {
		return "", codedError(CodeInvalidArgument, "get template", ErrInvalidArgument, errors.New("context and session id are required"))
	}
	operationContext, release, err := h.beginSessionOperation(ctx, sessionID, "get template")
	if err != nil {
		return "", err
	}
	defer release()
	loaded, err := h.loadTemplate(operationContext, sessionID, name, arguments)
	if err != nil {
		return "", err
	}
	return loaded.prompt, nil
}

// RunTemplate expands a template and executes it through the normal run path.
func (h *Harness) RunTemplate(ctx context.Context, sessionID, name, arguments string) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		if ctx == nil || sessionID == "" {
			yield(nil, codedError(CodeInvalidArgument, "run template", ErrInvalidArgument, errors.New("context and session id are required")))
			return
		}
		runContext, release, err := h.beginRun(ctx, sessionID)
		if err != nil {
			yield(nil, err)
			return
		}
		defer release()
		loaded, err := h.loadTemplate(runContext, sessionID, name, arguments)
		if err != nil {
			yield(nil, err)
			return
		}
		h.runAcquired(runContext, sessionID, loaded.prompt, &loaded.policy, yield)
	}
}

// AskTemplate returns the last final root-agent text for one template run.
func (h *Harness) AskTemplate(ctx context.Context, sessionID, name, arguments string) (string, error) {
	answer, found := "", false
	for event, err := range h.RunTemplate(ctx, sessionID, name, arguments) {
		if err != nil {
			return "", err
		}
		if event == nil || event.Author != h.agentName || !event.IsFinalResponse() || event.Content == nil {
			continue
		}
		answer, found = finalText(event.Content), true
	}
	if !found {
		return "", codedError(CodeNoFinalResponse, "ask template", ErrNoFinalResponse, nil)
	}
	return answer, nil
}

type expandedTemplate struct {
	prompt string
	policy syntax.ToolPolicy
}

func (h *Harness) loadTemplate(ctx context.Context, sessionID, name, arguments string) (expandedTemplate, error) {
	catalog, err := h.extensionCatalog(ctx, sessionID)
	if err != nil {
		return expandedTemplate{}, err
	}
	loaded, err := catalog.LoadTemplate(ctx, name, false)
	if err != nil {
		return expandedTemplate{}, codedError(CodeInvalidArgument, "load template", ErrInvalidArgument, err)
	}
	for _, notice := range loaded.Warnings {
		h.warnings.Warn(notice)
	}
	prompt, err := h.contexts.Expand(ctx, contextresolver.Expansion{
		Source: loaded.Body, Path: loaded.SelectedProvenance.SourcePath, Trust: contextresolver.ExtensionTrust(loaded.SelectedProvenance),
		Arguments: arguments, Declared: loaded.Arguments, SessionID: sessionID,
		SkillDir: loaded.Root, ProjectDir: h.configuration.WorkingDir,
		PluginRoot: loaded.PluginRoot, PluginData: loaded.PluginData, Effort: "normal",
	})
	if err != nil {
		return expandedTemplate{}, codedError(CodeInvalidArgument, "expand template", ErrInvalidArgument, err)
	}
	policy := contextresolver.ExtensionPolicy(loaded.AllowedTools, loaded.DeniedTools, loaded.Restricted)
	return expandedTemplate{prompt: prompt, policy: policy}, nil
}

func (h *Harness) extensionCatalog(ctx context.Context, sessionID string) (extensions.Catalog, error) {
	if ctx == nil || sessionID == "" {
		return extensions.Catalog{}, codedError(CodeInvalidArgument, "extension catalog", ErrInvalidArgument, errors.New("context and session id are required"))
	}
	if err := h.requireOpen("extension catalog"); err != nil {
		return extensions.Catalog{}, err
	}
	if _, err := h.sessions.Get(ctx, &session.GetRequest{AppName: h.configuration.AppName, UserID: h.configuration.UserID, SessionID: sessionID}); err != nil {
		return extensions.Catalog{}, h.sessionError("extension catalog", err)
	}
	if err := h.extensions.StartSession(ctx, sessionID); err != nil {
		return extensions.Catalog{}, codedError(CodeRuntimeFailed, "extension catalog", ErrRuntimeFailed, err)
	}
	catalog, ok := h.extensions.Snapshot(sessionID)
	if !ok {
		return extensions.Catalog{}, codedError(CodeRuntimeFailed, "extension catalog", ErrRuntimeFailed, errors.New("snapshot is unavailable"))
	}
	return catalog, nil
}

// Ask runs one turn and returns text from the last final root-agent response.
func (h *Harness) Ask(ctx context.Context, sessionID, prompt string) (string, error) {
	answer := ""
	found := false
	for event, err := range h.Run(ctx, sessionID, prompt) {
		if err != nil {
			return "", err
		}
		if event == nil || event.Author != h.agentName || !event.IsFinalResponse() || event.Content == nil {
			continue
		}
		answer = finalText(event.Content)
		found = true
	}
	if !found {
		return "", codedError(CodeNoFinalResponse, "ask", ErrNoFinalResponse, nil)
	}
	return answer, nil
}

// Config returns a defensive copy of the resolved configuration.
func (h *Harness) Config() config.Config {
	if h == nil {
		return config.Config{}
	}
	return h.configuration.Clone()
}

// WorkingDir returns the resolved workspace root.
func (h *Harness) WorkingDir() string {
	if h == nil {
		return ""
	}
	return h.configuration.WorkingDir
}

// SessionDir returns the resolved durable session directory.
func (h *Harness) SessionDir() string {
	if h == nil {
		return ""
	}
	return h.configuration.SessionDir
}

// Logger returns the configured logger or the default discard logger.
func (h *Harness) Logger() *slog.Logger {
	if h == nil {
		return nil
	}
	return h.logger
}

// Warnings returns a defensive snapshot of construction and runtime warnings.
func (h *Harness) Warnings() []warning.Warning {
	if h == nil || h.warnings == nil {
		return nil
	}
	return h.warnings.Warnings()
}

// Close cancels active runs and closes owned resources exactly once.
func (h *Harness) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	done := h.closeDone
	if done == nil {
		h.closed = true
		h.closeDone = make(chan struct{})
		done = h.closeDone
		cancels := make([]context.CancelFunc, 0, len(h.active))
		for _, cancel := range h.active {
			cancels = append(cancels, cancel)
		}
		h.mu.Unlock()

		h.cancelRoot()
		for _, cancel := range cancels {
			cancel()
		}
		go h.finishClose(done)
	} else {
		h.mu.Unlock()
	}
	<-done
	h.mu.Lock()
	err := h.closeErr
	h.mu.Unlock()
	return err
}

func (h *Harness) finishClose(done chan struct{}) {
	waited := make(chan struct{})
	go func() {
		h.activeRuns.Wait()
		close(waited)
	}()
	timeout := h.closeWaitTimeout
	if timeout <= 0 {
		timeout = closeTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var waitErr error
	select {
	case <-waited:
	case <-timer.C:
		waitErr = errors.Join(ErrCloseTimeout, fmt.Errorf("active runs did not stop within %s", timeout))
	}
	result := errors.Join(waitErr, h.closeResources())
	if result != nil {
		result = codedError(CodeCloseFailed, "close harness", ErrCloseFailed, result)
	}
	h.mu.Lock()
	h.closeErr = result
	close(done)
	h.mu.Unlock()
}

func (h *Harness) beginRun(ctx context.Context, sessionID string) (context.Context, func(), error) {
	return h.beginSessionOperation(ctx, sessionID, "run")
}

func (h *Harness) beginSessionOperation(ctx context.Context, sessionID, operation string) (context.Context, func(), error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, nil, codedError(CodeClosed, operation, ErrClosed, nil)
	}
	if _, exists := h.active[sessionID]; exists {
		h.mu.Unlock()
		return nil, nil, codedError(CodeSessionBusy, operation, ErrSessionBusy, fmt.Errorf("session %q already has an active operation", sessionID))
	}
	runContext, cancel := context.WithCancel(ctx)
	stopRoot := context.AfterFunc(h.rootContext, cancel)
	h.active[sessionID] = cancel
	h.activeRuns.Add(1)
	h.mu.Unlock()
	var once sync.Once
	return runContext, func() {
		once.Do(func() {
			stopRoot()
			cancel()
			h.mu.Lock()
			delete(h.active, sessionID)
			h.mu.Unlock()
			h.activeRuns.Done()
		})
	}, nil
}

func (h *Harness) beginOperation(ctx context.Context, operation string) (context.Context, func(), error) {
	if h == nil {
		return nil, nil, codedError(CodeClosed, operation, ErrClosed, nil)
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, nil, codedError(CodeClosed, operation, ErrClosed, nil)
	}
	operationContext, cancel := context.WithCancel(ctx)
	stopRoot := context.AfterFunc(h.rootContext, cancel)
	h.activeRuns.Add(1)
	h.mu.Unlock()
	var once sync.Once
	return operationContext, func() {
		once.Do(func() {
			stopRoot()
			cancel()
			h.activeRuns.Done()
		})
	}, nil
}

func (h *Harness) requireOpen(op string) error {
	if h == nil {
		return codedError(CodeClosed, op, ErrClosed, nil)
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return codedError(CodeClosed, op, ErrClosed, nil)
	}
	return nil
}

func (h *Harness) sessionError(op string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, sessionstore.ErrSessionNotFound):
		return codedError(CodeUnknownSession, op, ErrUnknownSession, err)
	case errors.Is(err, sessionstore.ErrClosed):
		return codedError(CodeClosed, op, ErrClosed, err)
	default:
		return codedError(CodeRuntimeFailed, op, ErrRuntimeFailed, err)
	}
}

func (h *Harness) closeResources() error {
	var failures []error
	closedNative := make(map[*plugin.Plugin]struct{}, len(h.adkPlugins))
	for index := len(h.adkPlugins) - 1; index >= 0; index-- {
		if h.adkPlugins[index] == nil {
			continue
		}
		if _, duplicate := closedNative[h.adkPlugins[index]]; duplicate {
			continue
		}
		closedNative[h.adkPlugins[index]] = struct{}{}
		if err := closeNativePlugin(h.adkPlugins[index]); err != nil {
			failures = append(failures, fmt.Errorf("close ADK plugin %q: %w", h.adkPlugins[index].Name(), err))
		}
	}
	h.adkPlugins = nil
	for index := len(h.plugins) - 1; index >= 0; index-- {
		name, _ := compiledPluginName(h.plugins[index])
		if err := closeCompiledPlugin(h.plugins[index]); err != nil {
			failures = append(failures, fmt.Errorf("close compiled plugin %q: %w", name, err))
		}
	}
	h.plugins = nil
	if h.unsubscribeContext != nil {
		h.unsubscribeContext()
		h.unsubscribeContext = nil
	}
	if h.unsubscribeSkills != nil {
		h.unsubscribeSkills()
		h.unsubscribeSkills = nil
	}
	if h.mcpManager != nil {
		if err := h.mcpManager.Close(); err != nil {
			failures = append(failures, fmt.Errorf("close MCP manager: %w", err))
		}
	}
	if h.extensions != nil {
		h.extensions.Close()
	}
	if h.contexts != nil {
		h.contexts.Close()
	}
	if h.lspEnforcer != nil {
		if err := h.lspEnforcer.Close(); err != nil {
			failures = append(failures, fmt.Errorf("close LSP enforcement: %w", err))
		}
	}
	if h.lspManager != nil {
		if err := h.lspManager.Close(); err != nil {
			failures = append(failures, fmt.Errorf("close LSP manager: %w", err))
		}
	}
	if h.sessions != nil {
		if err := h.sessions.Close(); err != nil {
			failures = append(failures, fmt.Errorf("close session store: %w", err))
		}
	}
	return errors.Join(failures...)
}

func validatePlugins(values []Plugin) error {
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		if nilInterface(value) {
			return codedError(CodeInvalidArgument, "validate compiled plugins", ErrInvalidArgument, fmt.Errorf("compiled plugin %d is nil", index))
		}
		name, err := compiledPluginName(value)
		if err != nil {
			return codedError(CodeInvalidArgument, "validate compiled plugins", ErrInvalidArgument, err)
		}
		if name == "" {
			return codedError(CodeInvalidArgument, "validate compiled plugins", ErrInvalidArgument, fmt.Errorf("compiled plugin %d has an empty name", index))
		}
		if _, duplicate := seen[name]; duplicate {
			return codedError(CodeDuplicate, "validate compiled plugins", ErrDuplicate, fmt.Errorf("duplicate compiled plugin name %q", name))
		}
		seen[name] = struct{}{}
	}
	return nil
}

func initializePlugin(value Plugin, harness *Harness) (err error) {
	name, nameErr := compiledPluginName(value)
	if nameErr != nil {
		return nameErr
	}
	defer func() {
		harness.setInitializingPlugin("")
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("plugin %q Init panicked: %v", name, recovered)
		}
	}()
	harness.setInitializingPlugin(name)
	return value.Init(harness)
}

func (h *Harness) setInitializingPlugin(name string) {
	h.mu.Lock()
	h.initializingPlugin = name
	h.mu.Unlock()
}

func (h *Harness) registrationPlugin() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.initializingPlugin
}

func closeCompiledPlugin(value Plugin) (err error) {
	name, nameErr := compiledPluginName(value)
	if nameErr != nil {
		name = "<unknown>"
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("plugin %q Close panicked: %v", name, recovered)
		}
	}()
	return value.Close()
}

func compiledPluginName(value Plugin) (name string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			name = ""
			err = fmt.Errorf("compiled plugin Name panicked: %v", recovered)
		}
	}()
	return value.Name(), nil
}

func closeNativePlugin(value *plugin.Plugin) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("ADK plugin %q Close panicked: %v", value.Name(), recovered)
		}
	}()
	return value.Close()
}

func runnerCallbackPlugins(ordered []*plugin.Plugin) ([]*plugin.Plugin, error) {
	result := make([]*plugin.Plugin, 0, len(ordered))
	for _, value := range ordered {
		projection, err := plugin.New(plugin.Config{
			Name:                  value.Name(),
			OnUserMessageCallback: value.OnUserMessageCallback(),
			OnEventCallback:       value.OnEventCallback(),
			BeforeRunCallback:     value.BeforeRunCallback(),
			AfterRunCallback:      value.AfterRunCallback(),
		})
		if err != nil {
			return nil, err
		}
		result = append(result, projection)
	}
	return result, nil
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

func finalText(content *genai.Content) string {
	result := ""
	for _, part := range content.Parts {
		if part != nil && !part.Thought {
			result += part.Text
		}
	}
	return result
}

func pathWithinAny(path string, roots []string) bool {
	for _, root := range roots {
		if workspace.ContainsCanonical(root, path) {
			return true
		}
	}
	return false
}

func extensionInstructionRecords(values []contextresolver.InstructionRecord) []extensions.Instruction {
	result := make([]extensions.Instruction, len(values))
	for index, value := range values {
		provenance := make([]extensions.Provenance, len(value.Provenance))
		for sourceIndex, source := range value.Provenance {
			provenance[sourceIndex] = extensions.Provenance{
				Host: source.Host, Scope: source.Scope, SourcePath: source.SourcePath,
				Enabled: source.Enabled, Trusted: source.Trusted, Classification: source.Classification,
			}
		}
		result[index] = extensions.Instruction{Name: value.Name, Provenance: provenance}
	}
	return result
}

type instructionProvider struct {
	contexts  *contextresolver.Resolver
	enforcer  *lsp.Enforcer
	fragments []registeredPromptFragment
}

func (p instructionProvider) Provide(ctx agent.ReadonlyContext) (string, error) {
	instruction, err := p.contexts.Instructions(ctx, ctx.SessionID(), ctx.InvocationID())
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, 2+len(p.fragments))
	if instruction != "" {
		parts = append(parts, instruction)
	}
	if p.enforcer != nil {
		if status := p.enforcer.Status(); status != "" {
			parts = append(parts, status)
		}
	}
	for _, fragment := range p.fragments {
		parts = append(parts, fragment.value.Content)
	}
	return strings.Join(parts, "\n\n"), nil
}

type scopedToolset struct {
	name         string
	tools        []tool.Tool
	source       tool.Toolset
	processor    toolsetRequestProcessor
	contexts     *contextresolver.Resolver
	confirmation bool
}

type toolsetRequestProcessor interface {
	ProcessRequest(agent.Context, *model.LLMRequest) error
}

func (s scopedToolset) ProcessRequest(ctx agent.Context, request *model.LLMRequest) error {
	processor := s.processor
	if processor == nil && s.source != nil {
		processor, _ = s.source.(toolsetRequestProcessor)
	}
	if processor != nil {
		if err := processor.ProcessRequest(ctx, request); err != nil {
			return err
		}
	}
	for name := range request.Tools {
		if !s.contexts.Visible(ctx.SessionID(), ctx.InvocationID(), name) {
			delete(request.Tools, name)
			continue
		}
		if current, ok := request.Tools[name].(tool.Tool); ok {
			guarded, err := guardToolExecution(current, s.contexts, s.confirmation)
			if err != nil {
				return err
			}
			request.Tools[name] = guarded
		}
	}
	return nil
}

func (s scopedToolset) Name() string { return s.name }
func (s scopedToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	if _, err := s.contexts.Instructions(ctx, ctx.SessionID(), ctx.InvocationID()); err != nil {
		return nil, err
	}
	values := append([]tool.Tool(nil), s.tools...)
	if s.source != nil {
		resolved, err := s.source.Tools(ctx)
		if err != nil {
			return nil, err
		}
		values = append([]tool.Tool(nil), resolved...)
	}
	visible := values[:0]
	for _, value := range values {
		if value != nil && s.contexts.Visible(ctx.SessionID(), ctx.InvocationID(), value.Name()) {
			guarded, err := guardToolExecution(value, s.contexts, s.confirmation)
			if err != nil {
				return nil, err
			}
			visible = append(visible, guarded)
		}
	}
	return visible, nil
}

type multiWarningSink []warning.Sink

func (s multiWarningSink) Warn(value warning.Warning) {
	for _, sink := range s {
		sink.Warn(value)
	}
}

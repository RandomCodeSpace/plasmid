package plasmid

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/contextresolver"
	"github.com/plasmid-dev/plasmid/extensions"
	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/lsp"
	plasmidmcp "github.com/plasmid-dev/plasmid/mcp"
	"github.com/plasmid-dev/plasmid/sessionstore"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

const (
	rootAgentName            = "plasmid"
	closeTimeout             = 10 * time.Second
	opConstructHarness       = "construct harness"
	opCreateSession          = "create session"
	opResumeSession          = "resume session"
	opExtensionCatalog       = "extension catalog"
	opValidateCompiledPlugin = "validate compiled plugins"
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

	rootDone   <-chan struct{}
	cancelRoot context.CancelFunc

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
	opts, loaded, err := loadHarnessOptions(ctx, supplied)
	if err != nil {
		return nil, err
	}
	construction, rootContext := newHarnessConstruction(ctx, opts, loaded)
	if err := construction.build(ctx, rootContext); err != nil {
		return nil, err
	}
	return construction.harness, nil
}

// NewSession creates a durable session with a store-generated canonical ID.
func (h *Harness) NewSession(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", codedError(CodeInvalidArgument, opCreateSession, ErrInvalidArgument, errors.New("context is nil"))
	}
	operationContext, release, err := h.beginOperation(ctx, opCreateSession)
	if err != nil {
		return "", err
	}
	defer release()
	ctx = operationContext
	response, err := h.sessions.Create(ctx, &session.CreateRequest{AppName: h.configuration.AppName, UserID: h.configuration.UserID})
	if err != nil {
		return "", h.sessionError(opCreateSession, err)
	}
	sessionID := response.Session.ID()
	if err := h.contexts.StartSession(ctx, sessionID); err != nil {
		deleteErr := h.sessions.Delete(context.WithoutCancel(ctx), &session.DeleteRequest{AppName: h.configuration.AppName, UserID: h.configuration.UserID, SessionID: sessionID})
		return "", codedError(CodeRuntimeFailed, "start session context", ErrRuntimeFailed, errors.Join(err, deleteErr))
	}
	if err := h.extensions.StartSessionWithInstructions(ctx, sessionID, extensionInstructionRecords(h.contexts.InstructionRecords(sessionID))); err != nil {
		h.contexts.DropSession(sessionID)
		deleteErr := h.sessions.Delete(context.WithoutCancel(ctx), &session.DeleteRequest{AppName: h.configuration.AppName, UserID: h.configuration.UserID, SessionID: sessionID})
		return "", codedError(CodeRuntimeFailed, "start session extensions", ErrRuntimeFailed, errors.Join(err, deleteErr))
	}
	return sessionID, nil
}

// ResumeSession verifies that a durable session already exists.
func (h *Harness) ResumeSession(ctx context.Context, sessionID string) error {
	if ctx == nil || sessionID == "" {
		return codedError(CodeInvalidArgument, opResumeSession, ErrInvalidArgument, errors.New("context and session id are required"))
	}
	operationContext, release, err := h.beginOperation(ctx, opResumeSession)
	if err != nil {
		return err
	}
	defer release()
	ctx = operationContext
	sessionContext, releaseSession, err := h.beginSessionOperation(ctx, sessionID, opResumeSession)
	if err != nil {
		return err
	}
	defer releaseSession()
	ctx = sessionContext
	_, err = h.sessions.Get(ctx, &session.GetRequest{
		AppName: h.configuration.AppName, UserID: h.configuration.UserID, SessionID: sessionID,
	})
	if err = h.sessionError(opResumeSession, err); err != nil {
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
	if _, err := h.sessions.Get(ctx, &session.GetRequest{AppName: h.configuration.AppName, UserID: h.configuration.UserID, SessionID: sessionID}); err != nil {
		return extensions.Catalog{}, h.sessionError(opExtensionCatalog, err)
	}
	if err := h.extensions.StartSession(ctx, sessionID); err != nil {
		return extensions.Catalog{}, codedError(CodeRuntimeFailed, opExtensionCatalog, ErrRuntimeFailed, err)
	}
	catalog, ok := h.extensions.Snapshot(sessionID)
	if !ok {
		return extensions.Catalog{}, codedError(CodeRuntimeFailed, opExtensionCatalog, ErrRuntimeFailed, errors.New("snapshot is unavailable"))
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
	runContext, cancel, stopRoot := linkedOperationContext(ctx, h.rootDone)
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
	operationContext, cancel, stopRoot := linkedOperationContext(ctx, h.rootDone)
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

func linkedOperationContext(ctx context.Context, rootDone <-chan struct{}) (context.Context, context.CancelFunc, func()) {
	linked, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		select {
		case <-rootDone:
			cancel()
		case <-finished:
		}
	}()
	var once sync.Once
	stop := func() { once.Do(func() { close(finished) }) }
	return linked, cancel, stop
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
	return errors.Join(h.closePluginResources(), h.closeSubscriptions(), h.closeRuntimeResources())
}

func (h *Harness) closePluginResources() error {
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
		failures = append(failures, closeNamedResource(
			fmt.Sprintf("ADK plugin %q", h.adkPlugins[index].Name()),
			func() error { return closeNativePlugin(h.adkPlugins[index]) },
		))
	}
	h.adkPlugins = nil
	for index := len(h.plugins) - 1; index >= 0; index-- {
		name, _ := compiledPluginName(h.plugins[index])
		failures = append(failures, closeNamedResource(
			fmt.Sprintf("compiled plugin %q", name),
			func() error { return closeCompiledPlugin(h.plugins[index]) },
		))
	}
	h.plugins = nil
	return errors.Join(failures...)
}

func (h *Harness) closeSubscriptions() error {
	if h.unsubscribeContext != nil {
		h.unsubscribeContext()
		h.unsubscribeContext = nil
	}
	if h.unsubscribeSkills != nil {
		h.unsubscribeSkills()
		h.unsubscribeSkills = nil
	}
	return nil
}

func (h *Harness) closeRuntimeResources() error {
	var closeMCP, closeLSPEnforcer, closeLSPManager, closeSessions func() error
	if h.mcpManager != nil {
		closeMCP = h.mcpManager.Close
	}
	mcpErr := closeNamedResource("MCP manager", closeMCP)
	if h.extensions != nil {
		h.extensions.Close()
	}
	if h.contexts != nil {
		h.contexts.Close()
	}
	if h.lspEnforcer != nil {
		closeLSPEnforcer = h.lspEnforcer.Close
	}
	if h.lspManager != nil {
		closeLSPManager = h.lspManager.Close
	}
	if h.sessions != nil {
		closeSessions = h.sessions.Close
	}
	return errors.Join(
		mcpErr,
		closeNamedResource("LSP enforcement", closeLSPEnforcer),
		closeNamedResource("LSP manager", closeLSPManager),
		closeNamedResource("session store", closeSessions),
	)
}

func closeNamedResource(name string, closeResource func() error) error {
	if closeResource == nil {
		return nil
	}
	if err := closeResource(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	return nil
}

func validatePlugins(values []Plugin) error {
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		if nilInterface(value) {
			return codedError(CodeInvalidArgument, opValidateCompiledPlugin, ErrInvalidArgument, fmt.Errorf("compiled plugin %d is nil", index))
		}
		name, err := compiledPluginName(value)
		if err != nil {
			return codedError(CodeInvalidArgument, opValidateCompiledPlugin, ErrInvalidArgument, err)
		}
		if name == "" {
			return codedError(CodeInvalidArgument, opValidateCompiledPlugin, ErrInvalidArgument, fmt.Errorf("compiled plugin %d has an empty name", index))
		}
		if _, duplicate := seen[name]; duplicate {
			return codedError(CodeDuplicate, opValidateCompiledPlugin, ErrDuplicate, fmt.Errorf("duplicate compiled plugin name %q", name))
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

type multiWarningSink []warning.Warner

func (s multiWarningSink) Warn(value warning.Warning) {
	for _, sink := range s {
		sink.Warn(value)
	}
}

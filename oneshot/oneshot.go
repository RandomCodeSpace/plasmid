// Package oneshot runs one bounded-lifetime Google ADK agent invocation.
//
// Each Run owns an ephemeral in-memory session and native ADK runner. It uses
// only the model, literal instruction, prompt, and tools supplied in Request.
// The package performs no discovery, persistence, or filesystem I/O. Supplied
// tools retain their own authority and responsibility for side effects.
package oneshot

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/platform"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

const (
	appName   = "plasmid-oneshot"
	agentName = "oneshot"
	userID    = "oneshot"
)

// ToolExecutionPolicy controls how calls from one model response execute.
type ToolExecutionPolicy uint8

const (
	// ToolExecutionSequential runs calls one at a time in response order.
	ToolExecutionSequential ToolExecutionPolicy = iota
	// ToolExecutionParallel allows calls from one response to overlap.
	ToolExecutionParallel
)

// Request contains the complete authority and input for one invocation. Every
// maximum must be positive. The zero-value ToolExecution policy is sequential.
type Request struct {
	Model                   model.LLM
	Instruction             string
	Prompt                  string
	Tools                   []tool.Tool
	MaxOutputTokens         int32
	MaxReturnedTextBytes    int
	MaxModelCalls           int
	MaxToolCallsPerResponse int
	ToolExecution           ToolExecutionPolicy
}

// Result contains final or partial root-agent text, completed tool results, and
// execution metadata. A non-nil error may carry partial results; a cleanup-only
// failure can accompany a complete execution result. Empty final text is valid
// when a successful final response contains no non-thought text parts.
type Result struct {
	Text        string
	ToolResults []ToolResult
	Metadata    Metadata
}

// ToolResult is a completed native tool response. Results retain model response
// order even when Request enables parallel tool execution.
type ToolResult struct {
	ID       string
	Name     string
	Response map[string]any
}

type executionControls struct {
	maxOutputTokens         int32
	maxReturnedTextBytes    int
	maxModelCalls           int
	maxToolCallsPerResponse int
	toolExecution           ToolExecutionPolicy
}

// Metadata reports work completed during one invocation.
type Metadata struct {
	ModelCalls int
	ToolCalls  int
	Usage      Usage
}

// Usage aggregates token counts reported by every model response.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

type errorUnwrapper interface {
	Unwrap() error
}

type joinedErrorUnwrapper interface {
	Unwrap() []error
}

// Run executes one synchronous, non-streaming native ADK turn. On a non-nil
// error, the returned Result may contain partial Text and completed ToolResults.
func Run(ctx context.Context, request Request) (Result, error) {
	return runWithSessionService(ctx, request, session.InMemoryService())
}

func runWithSessionService(ctx context.Context, request Request, sessions session.Service) (result Result, err error) {
	if ctx == nil {
		return Result{}, codedError(CodeInvalidArgument, "run", ErrInvalidArgument, errors.New("context is nil"))
	}
	if nilInterface(request.Model) {
		return Result{}, codedError(CodeInvalidArgument, "run", ErrInvalidArgument, errors.New("model is required"))
	}
	if sessions == nil {
		return Result{}, codedError(CodeInvalidArgument, "run", ErrInvalidArgument, errors.New("session service is required"))
	}
	controls := controlsFromRequest(request)
	if validationErr := validateControls(controls); validationErr != nil {
		return Result{}, validationErr
	}

	statistics := &runStatistics{}
	failures := &failureRecorder{}
	responses := &responseRecorder{}
	identities := newIdentityStripper(len(request.Tools) != 0)
	protectedModel, protectErr := protectModel(request.Model, statistics, failures, responses, controls)
	if protectErr != nil {
		return Result{}, protectErr
	}
	protectedTools, protectErr := protectTools(request.Tools, statistics, failures, identities)
	if protectErr != nil {
		return Result{}, protectErr
	}

	agentValue, constructionErr := llmagent.New(llmagent.Config{
		Name:  agentName,
		Model: protectedModel,
		Mode:  llmagent.ModeChat,
		InstructionProvider: func(agent.ReadonlyContext) (string, error) {
			return request.Instruction, nil
		},
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			func(_ agent.Context, modelRequest *model.LLMRequest) (*model.LLMResponse, error) {
				identities.beforeModel(modelRequest)
				return nil, nil
			},
		},
		Tools: protectedTools,
	})
	if failure := failures.failure(); failure != nil {
		return Result{}, failure
	}
	if constructionErr != nil {
		return Result{}, codedError(CodeExecutionFailed, "construct agent", ErrExecutionFailed, constructionErr)
	}

	runnerValue, constructionErr := runner.New(runner.Config{
		AppName:           appName,
		Agent:             agentValue,
		SessionService:    sessions,
		AutoCreateSession: false,
	})
	if failure := failures.failure(); failure != nil {
		return Result{}, failure
	}
	if constructionErr != nil {
		return Result{}, codedError(CodeExecutionFailed, "construct runner", ErrExecutionFailed, constructionErr)
	}

	created, createErr := sessions.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID})
	if createErr != nil {
		return Result{}, executionError(ctx, "create session", createErr)
	}
	sessionID := created.Session.ID()
	defer func() {
		cleanupErr := sessions.Delete(context.WithoutCancel(ctx), &session.DeleteRequest{
			AppName: appName, UserID: userID, SessionID: sessionID,
		})
		if cleanupErr == nil {
			return
		}
		cleanupErr = codedError(CodeCleanupFailed, "delete session", ErrCleanupFailed, cleanupErr)
		if err == nil {
			err = cleanupErr
			return
		}
		err = errors.Join(err, cleanupErr)
	}()

	message := genai.NewContentFromText(request.Prompt, genai.RoleUser)
	foundFinal := false
	runContext := platform.WithTaskRunner(ctx, taskRunner(controls.toolExecution))
	for event, runErr := range runnerValue.Run(runContext, userID, sessionID, message, agent.RunConfig{StreamingMode: agent.StreamingModeNone}) {
		if runErr != nil {
			result.Metadata = statistics.metadata()
			result.Text = responses.textValue()
			if failure := failures.failure(); failure != nil {
				return result, failure
			}
			return result, executionError(ctx, "run", runErr)
		}
		if event != nil && event.Author == agentName {
			result.ToolResults = appendToolResults(result.ToolResults, event.Content)
		}
		if event == nil || event.Author != agentName || !event.IsFinalResponse() || event.Content == nil {
			continue
		}
		result.Text = finalText(event.Content)
		foundFinal = true
	}
	result.Metadata = statistics.metadata()
	if failure := failures.failure(); failure != nil {
		result.Text = responses.textValue()
		return result, failure
	}
	if !foundFinal {
		result.Text = responses.textValue()
		return result, codedError(CodeNoFinalResponse, "run", ErrNoFinalResponse, nil)
	}
	return result, nil
}

func controlsFromRequest(request Request) executionControls {
	return executionControls{
		maxOutputTokens:         request.MaxOutputTokens,
		maxReturnedTextBytes:    request.MaxReturnedTextBytes,
		maxModelCalls:           request.MaxModelCalls,
		maxToolCallsPerResponse: request.MaxToolCallsPerResponse,
		toolExecution:           request.ToolExecution,
	}
}

func validateControls(controls executionControls) error {
	tests := []struct {
		name  string
		valid bool
	}{
		{name: "max output tokens", valid: controls.maxOutputTokens > 0},
		{name: "max returned text bytes", valid: controls.maxReturnedTextBytes > 0},
		{name: "max model calls", valid: controls.maxModelCalls > 0},
		{name: "max tool calls per response", valid: controls.maxToolCallsPerResponse > 0},
		{name: "tool execution", valid: controls.toolExecution == ToolExecutionSequential || controls.toolExecution == ToolExecutionParallel},
	}
	for _, test := range tests {
		if !test.valid {
			return codedError(CodeInvalidArgument, "validate "+test.name, ErrInvalidArgument, nil)
		}
	}
	return nil
}

func taskRunner(policy ToolExecutionPolicy) platform.TaskRunner {
	if policy == ToolExecutionParallel {
		return parallelTaskRunner
	}
	return sequentialTaskRunner
}

func sequentialTaskRunner(ctx context.Context, tasks []func(context.Context)) {
	for _, task := range tasks {
		task(ctx)
	}
}

func parallelTaskRunner(ctx context.Context, tasks []func(context.Context)) {
	var wait sync.WaitGroup
	wait.Add(len(tasks))
	for _, task := range tasks {
		go func() {
			defer wait.Done()
			task(ctx)
		}()
	}
	wait.Wait()
}

func appendToolResults(result []ToolResult, content *genai.Content) []ToolResult {
	if content == nil {
		return result
	}
	for _, part := range content.Parts {
		if part == nil || part.FunctionResponse == nil {
			continue
		}
		response := part.FunctionResponse
		result = append(result, ToolResult{ID: response.ID, Name: response.Name, Response: maps.Clone(response.Response)})
	}
	return result
}

func executionError(ctx context.Context, op string, cause error) error {
	classified, matchesCanceled, matchesDeadline := inspectExecutionCause(cause)
	if classified != nil {
		return classified
	}
	if !matchesCanceled {
		matchesCanceled = errors.Is(cause, context.Canceled)
	}
	if !matchesDeadline {
		matchesDeadline = errors.Is(cause, context.DeadlineExceeded)
	}
	activeCause := ctx.Err()
	if (activeCause == context.Canceled && matchesCanceled) ||
		(activeCause == context.DeadlineExceeded && matchesDeadline) {
		return codedError(CodeCanceled, op, ErrCanceled, activeCause)
	}
	return codedError(CodeExecutionFailed, op, ErrExecutionFailed, nil)
}

func inspectExecutionCause(cause error) (classified *internalError, matchesCanceled, matchesDeadline bool) {
	pending := []error{cause}
	for visits := 0; len(pending) != 0 && visits < 100; visits++ {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			continue
		}
		if internal, ok := current.(*internalError); ok {
			return internal, matchesCanceled, matchesDeadline
		}
		if boundary, ok := current.(*callerBoundaryError); ok {
			matchesCanceled = matchesCanceled || boundary.matchesCanceled
			matchesDeadline = matchesDeadline || boundary.matchesDeadline
			continue
		}
		switch wrapped := current.(type) {
		case joinedErrorUnwrapper:
			pending = append(pending, wrapped.Unwrap()...)
		case errorUnwrapper:
			pending = append(pending, wrapped.Unwrap())
		}
	}
	return nil, matchesCanceled, matchesDeadline
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

type identityStripper struct {
	mu       sync.Mutex
	hasTools bool
	seen     map[*model.LLMRequest]struct{}
}

func newIdentityStripper(hasTools bool) *identityStripper {
	return &identityStripper{hasTools: hasTools, seen: make(map[*model.LLMRequest]struct{})}
}

func (s *identityStripper) beforeTool(request *model.LLMRequest) {
	s.mu.Lock()
	if _, exists := s.seen[request]; exists {
		s.mu.Unlock()
		return
	}
	s.seen[request] = struct{}{}
	s.mu.Unlock()
	removeInjectedAgentIdentity(request)
}

func (s *identityStripper) beforeModel(request *model.LLMRequest) {
	s.mu.Lock()
	delete(s.seen, request)
	hasTools := s.hasTools
	s.mu.Unlock()
	if !hasTools {
		removeInjectedAgentIdentity(request)
	}
}

func removeInjectedAgentIdentity(request *model.LLMRequest) {
	if request == nil || request.Config == nil || request.Config.SystemInstruction == nil {
		return
	}
	identity := fmt.Sprintf("You are an agent. Your internal name is %q.", agentName)
	parts := request.Config.SystemInstruction.Parts
	if len(parts) == 0 || parts[len(parts)-1] == nil {
		return
	}
	last := parts[len(parts)-1]
	if last.Text == identity {
		request.Config.SystemInstruction.Parts = parts[:len(parts)-1]
		return
	}
	last.Text = strings.TrimSuffix(last.Text, "\n\n"+identity)
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	switch reflected := reflect.ValueOf(value); reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

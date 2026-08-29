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
	"reflect"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
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

// Request contains the complete authority and input for one invocation.
type Request struct {
	Model       model.LLM
	Instruction string
	Prompt      string
	Tools       []tool.Tool
}

// Result contains the last final root-agent text and execution metadata.
// Empty final text is valid when the model emitted a final response with no
// non-thought text parts.
type Result struct {
	Text     string
	Metadata Metadata
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

// Run executes one synchronous, non-streaming native ADK turn.
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

	statistics := &runStatistics{}
	failures := &failureRecorder{}
	protectedModel, protectErr := protectModel(request.Model, statistics, failures)
	if protectErr != nil {
		return Result{}, protectErr
	}
	protectedTools, protectErr := protectTools(request.Tools, statistics, failures)
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
				if modelRequest.Config == nil {
					modelRequest.Config = &genai.GenerateContentConfig{}
				}
				modelRequest.Config.SystemInstruction = genai.NewContentFromText(request.Instruction, genai.RoleUser)
				return nil, nil
			},
		},
		Tools: protectedTools,
	})
	if constructionErr != nil {
		return Result{}, codedError(CodeExecutionFailed, "construct agent", ErrExecutionFailed, constructionErr)
	}

	runnerValue, constructionErr := runner.New(runner.Config{
		AppName:           appName,
		Agent:             agentValue,
		SessionService:    sessions,
		AutoCreateSession: false,
	})
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
	for event, runErr := range runnerValue.Run(ctx, userID, sessionID, message, agent.RunConfig{StreamingMode: agent.StreamingModeNone}) {
		if runErr != nil {
			result.Metadata = statistics.metadata()
			return result, executionError(ctx, "run", runErr)
		}
		if event == nil || event.Author != agentName || !event.IsFinalResponse() || event.Content == nil {
			continue
		}
		result.Text = finalText(event.Content)
		foundFinal = true
	}
	result.Metadata = statistics.metadata()
	if failure := failures.failure(); failure != nil {
		return result, failure
	}
	if !foundFinal {
		return result, codedError(CodeNoFinalResponse, "run", ErrNoFinalResponse, nil)
	}
	return result, nil
}

func executionError(ctx context.Context, op string, cause error) error {
	var typed *Error
	if errors.As(cause, &typed) {
		return cause
	}
	if contextCause := ctx.Err(); contextCause != nil && errors.Is(cause, contextCause) {
		return codedError(CodeCanceled, op, ErrCanceled, contextCause)
	}
	return codedError(CodeExecutionFailed, op, ErrExecutionFailed, cause)
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

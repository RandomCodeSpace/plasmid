package oneshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
)

const oneshotContractFixtureRunner = "oneshot/contract"

type oneshotContractFixtureInput struct {
	Scenario string `json:"scenario"`
}

func init() {
	fixture.RegisterRunner("oneshot", oneshotContractFixtureRunner, "contract")
}

func TestOneshotContractFixtures(t *testing.T) {
	fixture.WalkKinds(t, "oneshot", oneshotContractFixtureRunner, []string{"contract"}, func(t *testing.T, testCase fixture.Case) {
		var input oneshotContractFixtureInput
		testCase.Decode(t, "input.json", &input)
		var projection any
		switch input.Scenario {
		case "lifecycle-cancellation":
			projection = runLifecycleCancellationFixture(t)
		case "panic-scope":
			projection = runPanicScopeFixture(t)
		case "parallel-policy":
			projection = runParallelPolicyFixture(t)
		case "caller-side-effect":
			projection = runCallerSideEffectFixture(t)
		default:
			t.Fatalf("unknown one-shot contract fixture scenario %q", input.Scenario)
		}
		testCase.CompareJSON(t, "expected.json", projection, fixture.Paths{}, fixture.GoldenReadOnly)
	})
}

func runLifecycleCancellationFixture(t *testing.T) any {
	t.Helper()
	service := newTracingSessionService()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, canceledErr := runWithSessionService(ctx, boundedRequest(Request{
		Model: cancellationModel{}, Prompt: "cancel",
	}), service)
	createdAfterCancel, deletedAfterCancel, deleteContextsAfterCancel := snapshotFixtureLifecycle(service)
	nonResumable := false
	if len(createdAfterCancel) == 1 {
		_, getErr := service.Service.Get(t.Context(), &session.GetRequest{
			AppName: appName, UserID: userID, SessionID: createdAfterCancel[0],
		})
		nonResumable = getErr != nil
	}

	recovery, recoveryErr := runWithSessionService(t.Context(), boundedRequest(Request{
		Model: finalModel("recovered"), Prompt: "recover",
	}), service)
	created, deleted, deleteContexts := snapshotFixtureLifecycle(service)
	return map[string]any{
		"cancel_cleanup": len(createdAfterCancel) == 1 && len(deletedAfterCancel) == 1 && createdAfterCancel[0] == deletedAfterCancel[0],
		"cancel_code":    CodeOf(canceledErr), "cancel_matches": errors.Is(canceledErr, ErrCanceled),
		"cancel_cleanup_context_live": len(deleteContextsAfterCancel) == 1 && deleteContextsAfterCancel[0] == nil,
		"independent_sessions":        len(created) == 2 && created[0] != created[1], "non_resumable": nonResumable,
		"recovery_cleanup": len(created) == 2 && len(deleted) == 2 && created[1] == deleted[1] && len(deleteContexts) == 2 && deleteContexts[1] == nil,
		"recovery_code":    CodeOf(recoveryErr), "recovery_text": recovery.Text,
	}
}

func snapshotFixtureLifecycle(service *tracingSessionService) ([]string, []string, []error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return append([]string(nil), service.createdIDs...), append([]string(nil), service.deletedIDs...), append([]error(nil), service.deleteCtx...)
}

func runPanicScopeFixture(t *testing.T) any {
	t.Helper()
	_, modelErr := Run(t.Context(), boundedRequest(Request{Model: lazyPanicModel{}, Prompt: "panic"}))
	_, toolErr := Run(t.Context(), boundedRequest(Request{
		Model: toolThenFinalModel("explode", "ignored"), Prompt: "panic",
		Tools: []tool.Tool{&testFunctionTool{name: "explode", panicRun: true}},
	}))

	statistics := &runStatistics{}
	failures := &failureRecorder{}
	request := boundedRequest(Request{Model: finalModel("done")})
	guarded, err := protectModel(request.Model, statistics, failures, &responseRecorder{}, controlsFromRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	consumerPanic := recoverFixturePanic(func() {
		guarded.GenerateContent(t.Context(), &model.LLMRequest{}, false)(func(*model.LLMResponse, error) bool {
			panic("consumer defect")
		})
	})
	internalPanic := recoverFixturePanic(func() {
		guarded.GenerateContent(t.Context(), nil, false)(func(*model.LLMResponse, error) bool { return true })
	})
	return map[string]any{
		"consumer_panic_propagated": consumerPanic == "consumer defect",
		"internal_panic_propagated": internalPanic != nil,
		"model_code":                CodeOf(modelErr), "model_matches": errors.Is(modelErr, ErrModelPanic),
		"model_redacted":          !strings.Contains(modelErr.Error(), "TOPSECRET"),
		"spurious_caller_failure": failures.failure() != nil,
		"tool_code":               CodeOf(toolErr), "tool_matches": errors.Is(toolErr, ErrToolPanic),
		"tool_redacted": !strings.Contains(toolErr.Error(), "TOPSECRET"),
	}
}

func recoverFixturePanic(run func()) (value any) {
	defer func() { value = recover() }()
	run()
	return nil
}

func runParallelPolicyFixture(t *testing.T) any {
	t.Helper()
	started := make(chan string, 2)
	completed := make(chan string, 2)
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	var firstOnce sync.Once
	var secondOnce sync.Once
	releaseFirstTool := func() { firstOnce.Do(func() { close(releaseFirst) }) }
	releaseSecondTool := func() { secondOnce.Do(func() { close(releaseSecond) }) }
	t.Cleanup(releaseFirstTool)
	t.Cleanup(releaseSecondTool)
	makeTool := func(name string) tool.Tool {
		return &testFunctionTool{name: name, run: func(agent.Context, any) (map[string]any, error) {
			started <- name
			if name == "first" {
				<-releaseFirst
			} else {
				<-releaseSecond
			}
			completed <- name
			return map[string]any{"name": name}, nil
		}}
	}
	type outcome struct {
		result Result
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := Run(t.Context(), boundedRequest(Request{
			Model: batchThenFinalModel("done", "first", "second"), Prompt: "parallel",
			Tools: []tool.Tool{makeTool("first"), makeTool("second")}, ToolExecution: ToolExecutionParallel,
		}))
		finished <- outcome{result: result, err: err}
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("parallel fixture tools did not overlap")
		}
	}
	releaseSecondTool()
	var firstCompleted string
	select {
	case firstCompleted = <-completed:
	case <-time.After(time.Second):
		t.Fatal("parallel fixture tool did not complete")
	}
	releaseFirstTool()
	var run outcome
	select {
	case run = <-finished:
	case <-time.After(time.Second):
		t.Fatal("parallel fixture run did not finish")
	}
	return map[string]any{
		"code": CodeOf(run.err), "first_completed": firstCompleted, "overlap": true,
		"result_order": toolResultNames(run.result.ToolResults), "text": run.result.Text,
	}
}

func runCallerSideEffectFixture(t *testing.T) any {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "caller-owned")
	writer := &testFunctionTool{name: "write_marker", run: func(agent.Context, any) (map[string]any, error) {
		if err := os.WriteFile(marker, []byte("caller-owned"), 0o600); err != nil {
			return nil, err
		}
		return map[string]any{"written": true}, nil
	}}
	result, err := Run(t.Context(), boundedRequest(Request{
		Model: toolThenFinalModel("write_marker", "done"), Prompt: "write", Tools: []tool.Tool{writer},
	}))
	written, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return map[string]any{
		"code": CodeOf(err), "side_effect": string(written), "text": result.Text,
		"tool_calls": result.Metadata.ToolCalls, "tool_results": toolResultNames(result.ToolResults),
	}
}

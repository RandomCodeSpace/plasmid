package plasmid

import (
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/functiontool"
)

func TestToolGuardPreservesDeferredResponseAcrossConfirmation(t *testing.T) {
	base, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name: "deferred", Description: "deferred response",
		InputSchema: &jsonschema.Schema{Type: "object"}, OutputSchema: &jsonschema.Schema{Type: "object"},
	}, func(agent.Context, map[string]any) (map[string]any, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	deferred := deferredFunctionTool{nativeFunctionTool: base.(nativeFunctionTool)}
	for _, confirmation := range []bool{false, true} {
		name := map[bool]string{false: "plain", true: "confirmation"}[confirmation]
		t.Run(name, func(t *testing.T) {
			guarded, err := guardToolExecution(deferred, nil, confirmation)
			if err != nil {
				t.Fatal(err)
			}
			deferrer, ok := guarded.(responseDeferrer)
			if !ok || !deferrer.DefersResponse() {
				t.Fatalf("guarded deferrer = %#v, %t", guarded, ok)
			}
		})
	}
}

type deferredFunctionTool struct{ nativeFunctionTool }

func (deferredFunctionTool) DefersResponse() bool { return true }

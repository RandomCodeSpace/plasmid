package skills

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/contextresolver"
	"github.com/plasmid-dev/plasmid/extensions"
	"github.com/plasmid-dev/plasmid/foreign"
	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

func TestLoadSkillNarrowsExistingScopeAtomically(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "skills", "review")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: review\ndescription: Review\narguments: [focus]\nallowed-tools: [read]\n---\nReview ${focus} in $PROJECT_DIR.\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	catalogs, err := extensions.NewStore(extensions.Options{WorkingDir: root, SkillRoots: []string{filepath.Join(root, "skills")}})
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err := workspace.NewRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	contexts, err := contextresolver.New(contextresolver.Options{Root: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := contexts.IntersectPolicy("missing", "missing", syntax.NewToolPolicy(nil, nil)); err == nil {
		t.Fatal("missing scope intersection succeeded")
	}
	set, err := New(Config{Catalogs: catalogs, Contexts: contexts, ProjectDir: root})
	if err != nil || len(set.tools) != 3 {
		t.Fatalf("toolset = %#v, err = %v", set, err)
	}
	if set.config.Output != outputlimit.Defaults() || set.config.Budget == nil {
		t.Fatalf("output defaults = %#v, budget = %#v", set.config.Output, set.config.Budget)
	}
}

func TestNativeSkillDeclarationsMarshalClosedObjectSchemas(t *testing.T) {
	root := t.TempDir()
	set, _ := newTestToolset(t, root, filepath.Join(root, "skills"), outputlimit.Defaults(), outputlimit.NewBudget(10_000), "schema-session")
	for _, current := range set.tools {
		declaration, ok := current.(interface {
			Declaration() *genai.FunctionDeclaration
		})
		if !ok {
			t.Fatalf("tool %q has no native function declaration", current.Name())
		}
		function := declaration.Declaration()
		encoded, err := json.Marshal(function)
		if err != nil {
			t.Fatalf("marshal %s declaration: %v", current.Name(), err)
		}
		parameters, err := json.Marshal(function.ParametersJsonSchema)
		if err != nil {
			t.Fatalf("marshal %s parameters: %v", current.Name(), err)
		}
		if !bytes.Contains(parameters, []byte(`"additionalProperties":false`)) {
			t.Fatalf("%s parameters permit undeclared properties: %s", current.Name(), encoded)
		}
		response, err := json.Marshal(function.ResponseJsonSchema)
		if err != nil {
			t.Fatalf("marshal %s response: %v", current.Name(), err)
		}
		if bytes.Contains(response, []byte(`"additionalProperties":false`)) {
			t.Fatalf("%s response rejects dynamic result fields: %s", current.Name(), response)
		}
	}
}

func TestToolsetFormattingRedactsCatalogActivationSecrets(t *testing.T) {
	root := t.TempDir()
	catalogs, err := extensions.NewStore(extensions.Options{
		WorkingDir: root,
		MCP: config.MCP{Servers: []config.MCPServer{{
			ID: "secret", Transport: config.MCPHTTP,
			URL:     "https://example.invalid/mcp?token=TOPSECRET",
			Headers: map[string]string{"Authorization": "Bearer TOPSECRET"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalogs.Close)
	workspaceRoot, err := workspace.NewRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	contexts, err := contextresolver.New(contextresolver.Options{Root: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(contexts.Close)
	set, err := New(Config{Catalogs: catalogs, Contexts: contexts, ProjectDir: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{fmt.Sprintf("%v", set), fmt.Sprintf("%+v", set), fmt.Sprintf("%#v", set)} {
		if strings.Contains(value, "TOPSECRET") || strings.Contains(value, "Bearer") {
			t.Fatalf("formatted toolset leaked activation secret: %s", value)
		}
	}
	var output bytes.Buffer
	slog.New(slog.NewJSONHandler(&output, nil)).Info("toolset", "value", set)
	if value := output.String(); strings.Contains(value, "TOPSECRET") || strings.Contains(value, "Bearer") {
		t.Fatalf("logged toolset leaked activation secret: %s", value)
	}
}

func TestListSkillsBoundsCompleteSerializedResult(t *testing.T) {
	root := t.TempDir()
	skillRoot := filepath.Join(root, "skills")
	for index := range 80 {
		directory := filepath.Join(skillRoot, fmt.Sprintf("skill-%03d", index))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		source := fmt.Sprintf("---\nname: skill-%03d\ndescription: %s\n---\nBody.\n", index, strings.Repeat("description", 20))
		if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy := outputlimit.Policy{MaxBytes: 600, MaxLines: 100, MaxLineBytes: 600, HeadFraction: 0.6}
	budget := outputlimit.NewBudget(10_000)
	set, ctx := newTestToolset(t, root, skillRoot, policy, budget, "list-session")
	result, err := set.list(ctx, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	encoded := marshalResult(t, result)
	if len(encoded) > policy.MaxBytes {
		t.Fatalf("list_skills output = %d bytes, limit %d", len(encoded), policy.MaxBytes)
	}
	if result["truncated"] != true {
		t.Fatalf("list_skills result = %#v, want deterministic truncation", result)
	}
	if used, _ := budget.Report("list-session"); used != len(encoded) {
		t.Fatalf("consumed bytes = %d, serialized output = %d", used, len(encoded))
	}
}

func TestLoadSkillAndResourceShareCumulativeSessionBudget(t *testing.T) {
	root := t.TempDir()
	skillRoot := filepath.Join(root, "skills")
	directory := filepath.Join(skillRoot, "large")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: large\ndescription: Large result\n---\n" + strings.Repeat("body-content-", 300)
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "resource.txt"), []byte(strings.Repeat("resource-content-", 300)), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := outputlimit.Policy{MaxBytes: 400, MaxLines: 100, MaxLineBytes: 400, HeadFraction: 0.6}
	budget := outputlimit.NewBudget(700)
	set, ctx := newTestToolset(t, root, skillRoot, policy, budget, "shared-session")
	if err := set.config.Contexts.StartSession(ctx, ctx.SessionID()); err != nil {
		t.Fatal(err)
	}
	if _, err := set.config.Contexts.Instructions(ctx, ctx.SessionID(), ctx.InvocationID()); err != nil {
		t.Fatal(err)
	}
	loaded, err := set.load(ctx, loadArgs{Name: "large"})
	if err != nil {
		t.Fatal(err)
	}
	loadedJSON := marshalResult(t, loaded)
	usedAfterLoad, limit := budget.Report(ctx.SessionID())
	if usedAfterLoad != len(loadedJSON) || usedAfterLoad > policy.MaxBytes {
		t.Fatalf("load_skill budget = %d/%d, output bytes = %d", usedAfterLoad, limit, len(loadedJSON))
	}
	resource, err := set.loadResource(ctx, resourceArgs{Name: "large", Path: "resource.txt"})
	if err != nil {
		t.Fatal(err)
	}
	resourceJSON := marshalResult(t, resource)
	usedAfterResource, _ := budget.Report(ctx.SessionID())
	if usedAfterResource != len(loadedJSON)+len(resourceJSON) {
		t.Fatalf("shared budget = %d, serialized calls = %d + %d", usedAfterResource, len(loadedJSON), len(resourceJSON))
	}
	if len(resourceJSON) > limit-usedAfterLoad {
		t.Fatalf("load_skill_resource output = %d bytes, remaining budget = %d", len(resourceJSON), limit-usedAfterLoad)
	}
	if loaded["truncated"] != true || resource["truncated"] != true {
		t.Fatalf("bounded load results = %#v / %#v", loaded, resource)
	}
}

func newTestToolset(t *testing.T, root, skillRoot string, policy outputlimit.Policy, budget *outputlimit.Budget, sessionID string) (*Toolset, skillAgentContext) {
	t.Helper()
	catalogs, err := extensions.NewStore(extensions.Options{
		WorkingDir: root, SkillRoots: []string{skillRoot}, Foreign: foreign.Options{ProjectTrusted: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err := workspace.NewRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	contexts, err := contextresolver.New(contextresolver.Options{Root: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	set, err := New(Config{Catalogs: catalogs, Contexts: contexts, ProjectDir: root, Output: policy, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalogs.Close)
	t.Cleanup(contexts.Close)
	return set, skillAgentContext{sessionID: sessionID, invocationID: "invocation"}
}

func marshalResult(t *testing.T, result map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type skillAgentContext struct {
	agent.Context
	sessionID    string
	invocationID string
}

func (skillAgentContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (skillAgentContext) Done() <-chan struct{}       { return nil }
func (skillAgentContext) Err() error                  { return nil }
func (skillAgentContext) Value(any) any               { return nil }
func (c skillAgentContext) SessionID() string         { return c.sessionID }
func (c skillAgentContext) InvocationID() string      { return c.invocationID }

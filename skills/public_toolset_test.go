package skills_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/plasmid-dev/plasmid/contextresolver"
	"github.com/plasmid-dev/plasmid/extensions"
	"github.com/plasmid-dev/plasmid/foreign"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/skills"
	"github.com/plasmid-dev/plasmid/workspace"
)

type runnableTool interface {
	Run(agent.Context, any) (map[string]any, error)
}

func TestReturnedNativeSkillToolsListLoadAndReadResources(t *testing.T) {
	root := t.TempDir()
	skillRoot := filepath.Join(root, "skills")
	skillDir := filepath.Join(skillRoot, "review")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: review\ndescription: Review code\narguments: [focus]\nallowed-tools: [read]\n---\nReview ${focus} in ${PROJECT_DIR}.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "guide.txt"), []byte("resource body"), 0o600); err != nil {
		t.Fatal(err)
	}

	set, catalogs, contexts := newPublicToolset(t, root, skillRoot, outputlimit.Defaults())
	ctx := nativeContext{sessionID: "session", invocationID: "invocation"}
	if set.Name() != "skills" {
		t.Fatalf("toolset name = %q", set.Name())
	}
	if err := contexts.StartSession(ctx, ctx.SessionID()); err != nil {
		t.Fatal(err)
	}
	if _, err := contexts.Instructions(ctx, ctx.SessionID(), ctx.InvocationID()); err != nil {
		t.Fatal(err)
	}

	tools, err := set.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(tools))
	}
	tools[0] = nil
	again, err := set.Tools(ctx)
	if err != nil || again[0] == nil {
		t.Fatalf("returned tool slice aliases internal state: %#v, %v", again, err)
	}
	byName := make(map[string]runnableTool, len(again))
	for _, value := range again {
		runner, ok := value.(runnableTool)
		if !ok {
			t.Fatalf("tool %q has no native Run contract", value.Name())
		}
		byName[value.Name()] = runner
	}

	listed, err := byName["list_skills"].Run(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := listed["skills"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("listed skills = %#v", listed)
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["name"] != "review" {
		t.Fatalf("listed skill = %#v", items[0])
	}
	loaded, err := byName["load_skill"].Run(ctx, map[string]any{"name": "review", "arguments": "focus=security"})
	if err != nil {
		t.Fatal(err)
	}
	if loaded["name"] != "review" || loaded["content"] != "Review security in "+root+".\n" {
		t.Fatalf("loaded skill = %#v", loaded)
	}
	resource, err := byName["load_skill_resource"].Run(ctx, map[string]any{"name": "review", "path": "guide.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if resource["content"] != "resource body" {
		t.Fatalf("resource = %#v", resource)
	}
	if _, err := byName["load_skill"].Run(ctx, map[string]any{"name": "missing"}); !errors.Is(err, extensions.ErrNotFound) {
		t.Fatalf("missing skill error = %v", err)
	}
	if _, err := byName["load_skill_resource"].Run(ctx, map[string]any{"name": "review", "path": "../escape"}); !errors.Is(err, extensions.ErrResource) {
		t.Fatalf("escaping resource error = %v", err)
	}
	if _, err := byName["load_skill"].Run(ctx, map[string]any{"name": "review", "arguments": `"unterminated`}); err == nil {
		t.Fatal("malformed skill arguments were accepted")
	}
	withoutScope := nativeContext{sessionID: "without-scope", invocationID: "invocation"}
	withoutScopeTools, err := set.Tools(withoutScope)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range withoutScopeTools {
		if value.Name() != "load_skill" {
			continue
		}
		if _, err := value.(runnableTool).Run(withoutScope, map[string]any{"name": "review", "arguments": "focus=correctness"}); err == nil {
			t.Fatal("skill load without an invocation scope succeeded")
		}
	}

	request := &model.LLMRequest{Tools: map[string]any{"stale": again[0]}}
	if err := set.ProcessRequest(ctx, request); err != nil {
		t.Fatal(err)
	}
	if len(request.Tools) != 4 || request.Tools["stale"] == nil {
		t.Fatalf("processed tools = %#v", request.Tools)
	}
	if err := os.RemoveAll(skillRoot); err != nil {
		t.Fatal(err)
	}
	emptyRequest := &model.LLMRequest{Tools: make(map[string]any)}
	for _, value := range again {
		emptyRequest.Tools[value.Name()] = value
	}
	emptyContext := nativeContext{sessionID: "after-removal", invocationID: "invocation"}
	if err := set.ProcessRequest(emptyContext, emptyRequest); err != nil {
		t.Fatal(err)
	}
	if len(emptyRequest.Tools) != 0 {
		t.Fatalf("stale skill tools survived refresh: %#v", emptyRequest.Tools)
	}
	catalogs.DropSession(ctx.SessionID())
}

func TestToolsetValidationAndEmptyCatalogBehavior(t *testing.T) {
	root := t.TempDir()
	workspaceRoot, err := workspace.NewRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	contexts, err := contextresolver.New(contextresolver.Options{Root: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(contexts.Close)
	catalogs, err := extensions.NewStore(extensions.Options{WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalogs.Close)

	invalid := []skills.Config{
		{},
		{Contexts: contexts, ProjectDir: root},
		{Catalogs: catalogs, ProjectDir: root},
		{Catalogs: catalogs, Contexts: contexts},
		{Catalogs: catalogs, Contexts: contexts, ProjectDir: root, Output: outputlimit.Policy{MaxBytes: -1}},
	}
	for _, value := range invalid {
		if _, err := skills.New(value); err == nil {
			t.Fatalf("invalid config accepted: %#v", value)
		}
	}
	set, err := skills.New(skills.Config{Catalogs: catalogs, Contexts: contexts, ProjectDir: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx := nativeContext{sessionID: "empty", invocationID: "invocation"}
	tools, err := set.Tools(ctx)
	if err != nil || len(tools) != 0 {
		t.Fatalf("empty tools = %#v, err = %v", tools, err)
	}
	request := &model.LLMRequest{Tools: make(map[string]any)}
	if err := set.ProcessRequest(ctx, request); err != nil || len(request.Tools) != 0 {
		t.Fatalf("empty processed tools = %#v, err = %v", request.Tools, err)
	}
	catalogs.Close()
	if _, err := set.Tools(ctx); !errors.Is(err, extensions.ErrClosed) {
		t.Fatalf("closed catalog error = %v", err)
	}
}

func newPublicToolset(t *testing.T, root, skillRoot string, policy outputlimit.Policy) (*skills.Toolset, *extensions.Store, *contextresolver.Resolver) {
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
	contexts, err := contextresolver.New(contextresolver.Options{Root: workspaceRoot, DocumentOutputBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	set, err := skills.New(skills.Config{Catalogs: catalogs, Contexts: contexts, ProjectDir: root, Output: policy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalogs.Close)
	t.Cleanup(contexts.Close)
	return set, catalogs, contexts
}

type nativeContext struct {
	agent.Context
	sessionID    string
	invocationID string
}

func (nativeContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (nativeContext) Done() <-chan struct{}       { return nil }
func (nativeContext) Err() error                  { return nil }
func (nativeContext) Value(any) any               { return nil }
func (c nativeContext) SessionID() string         { return c.sessionID }
func (c nativeContext) InvocationID() string      { return c.invocationID }
func (c nativeContext) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return nil
}

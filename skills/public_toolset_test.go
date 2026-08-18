package skills_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/plasmid-dev/plasmid/contextresolver"
	"github.com/plasmid-dev/plasmid/extensions"
	"github.com/plasmid-dev/plasmid/foreign"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/skills"
	"github.com/plasmid-dev/plasmid/workspace"
)

type runner interface {
	Run(agent.Context, any) (map[string]any, error)
}

type publicSkillFixture struct {
	root        string
	skillRoot   string
	set         *skills.Toolset
	catalogs    *extensions.Store
	ctx         *nativeContext
	byName      map[string]runner
	nativeTools []tool.Tool
}

func TestReturnedNativeSkillToolsListLoadAndReadResources(t *testing.T) {
	fixture := newPublicSkillFixture(t)
	assertPublicSkillHappyPath(t, fixture)
	assertPublicSkillFailures(t, fixture)
	assertPublicSkillRequiresInvocationScope(t, fixture)
	assertPublicSkillRequestRefresh(t, fixture)
	assertClosedCatalogFailures(t, fixture)
}

func newPublicSkillFixture(t *testing.T) publicSkillFixture {
	t.Helper()
	root := t.TempDir()
	skillRoot := filepath.Join(root, "skills")
	skillDir := filepath.Join(skillRoot, "review")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: review\ndescription: Review code\narguments: [focus]\nallowed-tools: [read]\nfuture-field: true\n---\nReview ${focus} in ${PROJECT_DIR}.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "guide.txt"), []byte("resource body"), 0o600); err != nil {
		t.Fatal(err)
	}

	set, catalogs, contexts := newPublicToolset(t, root, skillRoot, outputlimit.Defaults())
	ctx := &nativeContext{StrictContextMock: agent.NewStrictContextMock(t.Context()), sessionID: "session", invocationID: "invocation"}
	if set.Name() != "skills" {
		t.Fatalf("toolset name = %q", set.Name())
	}
	if err := contexts.StartSession(ctx, ctx.SessionID()); err != nil {
		t.Fatal(err)
	}
	if _, err := contexts.Instructions(ctx, ctx.SessionID(), ctx.InvocationID()); err != nil {
		t.Fatal(err)
	}
	byName, nativeTools := publicRunnableTools(t, set, ctx)
	return publicSkillFixture{
		root: root, skillRoot: skillRoot, set: set, catalogs: catalogs,
		ctx: ctx, byName: byName, nativeTools: nativeTools,
	}
}

func publicRunnableTools(t *testing.T, set *skills.Toolset, ctx *nativeContext) (map[string]runner, []tool.Tool) {
	t.Helper()
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
	byName := make(map[string]runner, len(again))
	for _, value := range again {
		runnable, ok := value.(runner)
		if !ok {
			t.Fatalf("tool %q has no native Run contract", value.Name())
		}
		byName[value.Name()] = runnable
	}
	return byName, again
}

func assertPublicSkillHappyPath(t *testing.T, fixture publicSkillFixture) {
	t.Helper()
	listed, err := fixture.byName["list_skills"].Run(fixture.ctx, map[string]any{})
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
	loaded, err := fixture.byName["load_skill"].Run(fixture.ctx, map[string]any{"name": "review", "arguments": "focus=security"})
	if err != nil {
		t.Fatal(err)
	}
	if loaded["name"] != "review" || loaded["content"] != "Review security in "+fixture.root+".\n" {
		t.Fatalf("loaded skill = %#v", loaded)
	}
	if notices, ok := loaded["warnings"].([]any); !ok || len(notices) == 0 {
		t.Fatalf("loaded skill warnings = %#v", loaded["warnings"])
	}
	resource, err := fixture.byName["load_skill_resource"].Run(fixture.ctx, map[string]any{"name": "review", "path": "guide.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if resource["content"] != "resource body" {
		t.Fatalf("resource = %#v", resource)
	}
}

func assertPublicSkillFailures(t *testing.T, fixture publicSkillFixture) {
	t.Helper()
	if _, err := fixture.byName["load_skill"].Run(fixture.ctx, map[string]any{"name": "missing"}); !errors.Is(err, extensions.ErrNotFound) {
		t.Fatalf("missing skill error = %v", err)
	}
	if _, err := fixture.byName["load_skill_resource"].Run(fixture.ctx, map[string]any{"name": "review", "path": "../escape"}); !errors.Is(err, extensions.ErrResource) {
		t.Fatalf("escaping resource error = %v", err)
	}
	if _, err := fixture.byName["load_skill"].Run(fixture.ctx, map[string]any{"name": "review", "arguments": `"unterminated`}); err == nil {
		t.Fatal("malformed skill arguments were accepted")
	}
}

func assertPublicSkillRequiresInvocationScope(t *testing.T, fixture publicSkillFixture) {
	t.Helper()
	withoutScope := &nativeContext{StrictContextMock: agent.NewStrictContextMock(t.Context()), sessionID: "without-scope", invocationID: "invocation"}
	withoutScopeTools, err := fixture.set.Tools(withoutScope)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range withoutScopeTools {
		if value.Name() != "load_skill" {
			continue
		}
		if _, err := value.(runner).Run(withoutScope, map[string]any{"name": "review", "arguments": "focus=correctness"}); err == nil {
			t.Fatal("skill load without an invocation scope succeeded")
		}
	}
}

func assertPublicSkillRequestRefresh(t *testing.T, fixture publicSkillFixture) {
	t.Helper()
	request := &model.LLMRequest{Tools: map[string]any{"stale": fixture.nativeTools[0]}}
	if err := fixture.set.ProcessRequest(fixture.ctx, request); err != nil {
		t.Fatal(err)
	}
	if len(request.Tools) != 4 || request.Tools["stale"] == nil {
		t.Fatalf("processed tools = %#v", request.Tools)
	}
	if err := os.RemoveAll(fixture.skillRoot); err != nil {
		t.Fatal(err)
	}
	emptyRequest := &model.LLMRequest{Tools: make(map[string]any)}
	for _, value := range fixture.nativeTools {
		emptyRequest.Tools[value.Name()] = value
	}
	emptyContext := &nativeContext{StrictContextMock: agent.NewStrictContextMock(t.Context()), sessionID: "after-removal", invocationID: "invocation"}
	if err := fixture.set.ProcessRequest(emptyContext, emptyRequest); err != nil {
		t.Fatal(err)
	}
	if len(emptyRequest.Tools) != 0 {
		t.Fatalf("stale skill tools survived refresh: %#v", emptyRequest.Tools)
	}
}

func assertClosedCatalogFailures(t *testing.T, fixture publicSkillFixture) {
	t.Helper()
	fixture.catalogs.DropSession(fixture.ctx.SessionID())
	fixture.catalogs.Close()
	if err := fixture.set.ProcessRequest(fixture.ctx, &model.LLMRequest{Tools: make(map[string]any)}); !errors.Is(err, extensions.ErrClosed) {
		t.Fatalf("process request with closed catalog error = %v", err)
	}
	for name, arguments := range map[string]map[string]any{
		"list_skills":         {},
		"load_skill":          {"name": "review"},
		"load_skill_resource": {"name": "review", "path": "guide.txt"},
	} {
		if _, err := fixture.byName[name].Run(fixture.ctx, arguments); !errors.Is(err, extensions.ErrClosed) {
			t.Fatalf("%s with closed catalog error = %v", name, err)
		}
	}
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
	ctx := &nativeContext{StrictContextMock: agent.NewStrictContextMock(t.Context()), sessionID: "empty", invocationID: "invocation"}
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
	agent.StrictContextMock
	sessionID    string
	invocationID string
}

func (c *nativeContext) SessionID() string    { return c.sessionID }
func (c *nativeContext) InvocationID() string { return c.invocationID }
func (*nativeContext) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return nil
}

package extensions_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/extensions"
	"github.com/plasmid-dev/plasmid/foreign"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

type cancelCatalogReadContext struct {
	limit int

	mu     sync.Mutex
	checks int
	done   chan struct{}
	err    error
}

func newCancelCatalogReadContext(limit int) *cancelCatalogReadContext {
	return &cancelCatalogReadContext{limit: limit, done: make(chan struct{})}
}

func (*cancelCatalogReadContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelCatalogReadContext) Done() <-chan struct{}     { return c.done }
func (*cancelCatalogReadContext) Value(any) any               { return nil }

func (c *cancelCatalogReadContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.checks++
	if c.checks < c.limit {
		return nil
	}
	c.err = context.Canceled
	close(c.done)
	return c.err
}

func TestCatalogLazyReadsHonorCancellationAndMissingSnapshotRoots(t *testing.T) {
	skillDir, store, catalog := newLazyReadCatalog(t)
	assertLazyReadCancellation(t, catalog)
	if content, err := catalog.LoadSkillResource(t.Context(), "large", "guide.txt", true); err != nil || content != "guide" {
		t.Fatalf("default-budget resource = %q, err = %v", content, err)
	}
	assertScopedResourceActivation(t, store, catalog)
	if err := os.RemoveAll(skillDir); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.LoadSkill(t.Context(), "large", true); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed snapshot root error = %v", err)
	}
}

func newLazyReadCatalog(t *testing.T) (string, *extensions.Store, extensions.Catalog) {
	t.Helper()
	root := t.TempDir()
	skillRoot := filepath.Join(root, "skills")
	skillDir := filepath.Join(skillRoot, "large")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: large\ndescription: Large skill\n---\n" + strings.Repeat("body line\n", 10_000)
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "guide.txt"), []byte("guide"), 0o600); err != nil {
		t.Fatal(err)
	}
	scopedDir := filepath.Join(skillRoot, "scoped")
	if err := os.MkdirAll(scopedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	scopedSource := "---\nname: scoped\ndescription: Scoped skill\nglobs: [src/**]\n---\nScoped body.\n"
	if err := os.WriteFile(filepath.Join(scopedDir, "SKILL.md"), []byte(scopedSource), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopedDir, "guide.txt"), []byte("scoped guide"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := extensions.NewStore(extensions.Options{
		WorkingDir: root, SkillRoots: []string{skillRoot}, Foreign: foreign.Options{ProjectTrusted: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	return skillDir, store, catalogFromStore(t, store, "session")
}

func assertLazyReadCancellation(t *testing.T, catalog extensions.Catalog) {
	t.Helper()
	succeeded := false
	for limit := 1; limit <= 64; limit++ {
		ctx := newCancelCatalogReadContext(limit)
		loaded, loadErr := catalog.LoadSkill(ctx, "large", true)
		if loadErr == nil {
			if loaded.Body == "" {
				t.Fatal("successful lazy read returned an empty body")
			}
			succeeded = true
			break
		}
		if !errors.Is(loadErr, context.Canceled) {
			t.Fatalf("lazy-read cancellation boundary %d error = %v", limit, loadErr)
		}
	}
	if !succeeded {
		t.Fatal("lazy read did not complete within the cancellation-boundary limit")
	}
}

func assertScopedResourceActivation(t *testing.T, store *extensions.Store, catalog extensions.Catalog) {
	t.Helper()
	if _, err := catalog.LoadSkillResource(t.Context(), "scoped", "guide.txt", true); !errors.Is(err, extensions.ErrInactive) {
		t.Fatalf("inactive scoped resource error = %v", err)
	}
	store.ObserveTouch(t.Context(), workspace.Touch{SessionID: "missing", Path: "src/main.go"})
	store.ObserveTouch(t.Context(), workspace.Touch{SessionID: "session", Path: "src/main.go"})
	store.ObserveTouch(t.Context(), workspace.Touch{SessionID: "session", Path: "src/second.go"})
	if content, err := catalogFromStore(t, store, "session").LoadSkillResource(t.Context(), "scoped", "guide.txt", true); err != nil || content != "scoped guide" {
		t.Fatalf("activated scoped resource = %q, err = %v", content, err)
	}
}

func catalogFromStore(t *testing.T, store *extensions.Store, sessionID string) extensions.Catalog {
	t.Helper()
	catalog, ok := store.Snapshot(sessionID)
	if !ok {
		t.Fatalf("snapshot %q is unavailable", sessionID)
	}
	return catalog
}

type defensiveCatalogFixture struct {
	root     string
	skillDir string
	source   string
	store    *extensions.Store
	catalog  extensions.Catalog
}

func TestStorePublishesDefensiveCatalogAndLazyResources(t *testing.T) {
	fixture := newDefensiveCatalogFixture(t)
	assertDefensiveCatalogMetadata(t, fixture.catalog)
	assertDefensiveMCP(t, fixture.catalog)
	assertLazySkillResources(t, fixture)
	fixture.store.DropSession("session")
	if _, ok := fixture.store.Snapshot("session"); ok {
		t.Fatal("dropped session retained its snapshot")
	}
}

func newDefensiveCatalogFixture(t *testing.T) defensiveCatalogFixture {
	t.Helper()
	root := t.TempDir()
	skillRoot := filepath.Join(root, "skills")
	skillDir := filepath.Join(skillRoot, "review")
	if err := os.MkdirAll(filepath.Join(skillDir, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: review\ndescription: Review code\nlicense: MIT\ncompatibility: Go\nmetadata:\n  owner: team\nallowed-tools: [read]\n---\nReview body.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "guide.txt"), []byte("guide"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "binary.bin"), []byte{0xff, 0xfe}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "large.txt"), make([]byte, 300), 0o600); err != nil {
		t.Fatal(err)
	}

	instructions := []extensions.Instruction{{Name: "host", Provenance: []extensions.Provenance{{Host: "plasmid", Scope: "user"}}}}
	plugins := []extensions.CompiledPlugin{{Name: "compiled", Provenance: []extensions.Provenance{{Host: "plasmid", Scope: "admin"}}}}
	store, err := extensions.NewStore(extensions.Options{
		WorkingDir: root, SkillRoots: []string{skillRoot}, Foreign: foreign.Options{ProjectTrusted: true},
		Instructions: instructions, CompiledPlugins: plugins, MaxResourceBytes: 256,
		MCP: config.MCP{Servers: []config.MCPServer{{
			ID: "local", Transport: config.MCPStdio, Command: "server", Args: []string{"serve"}, Env: map[string]string{"TOKEN": "secret"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	instructions[0].Name = "mutated"
	plugins[0].Name = "mutated"
	if _, ok := store.Snapshot("missing"); ok {
		t.Fatal("missing session had a snapshot")
	}
	if err := store.StartSessionWithInstructions(t.Context(), "session", []extensions.Instruction{{Name: "session"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatalf("duplicate start: %v", err)
	}
	return defensiveCatalogFixture{
		root: root, skillDir: skillDir, source: source, store: store,
		catalog: catalogFromStore(t, store, "session"),
	}
}

func assertDefensiveCatalogMetadata(t *testing.T, catalog extensions.Catalog) {
	t.Helper()
	all := catalog.AllSkills()
	visible := catalog.Skills()
	if len(all) != 1 || len(visible) != 1 || all[0].Name != "review" {
		t.Fatalf("skills = all %#v, visible %#v", all, visible)
	}
	all[0].QualifiedNames[0] = "changed"
	all[0].Metadata[0].Value = "changed"
	all[0].Provenance[0].Host = "changed"
	if reflect.DeepEqual(all[0], catalog.AllSkills()[0]) {
		t.Fatal("skill metadata aliases catalog state")
	}
	if got := catalog.Instructions(); len(got) != 2 || got[0].Name != "host" || got[1].Name != "session" {
		t.Fatalf("instructions = %#v", got)
	} else {
		got[0].Provenance[0].Host = "changed"
		if catalog.Instructions()[0].Provenance[0].Host == "changed" {
			t.Fatal("instruction provenance aliases catalog state")
		}
	}
	if got := catalog.CompiledPlugins(); len(got) != 1 || got[0].Name != "compiled" {
		t.Fatalf("compiled plugins = %#v", got)
	} else {
		got[0].Provenance[0].Host = "changed"
		if catalog.CompiledPlugins()[0].Provenance[0].Host == "changed" {
			t.Fatal("compiled plugin provenance aliases catalog state")
		}
	}
}

func assertDefensiveMCP(t *testing.T, catalog extensions.Catalog) {
	t.Helper()
	if names := catalog.AllowedMCPNames(); !reflect.DeepEqual(names, []string{"plasmid:configured:local"}) {
		t.Fatalf("allowed MCP names = %#v", names)
	}
	servers := catalog.MCPServers()
	if len(servers) != 1 || !servers[0].Allowed {
		t.Fatalf("MCP servers = %#v", servers)
	}
	servers[0].QualifiedNames[0] = "changed"
	if catalog.MCPServers()[0].QualifiedNames[0] == "changed" {
		t.Fatal("MCP metadata aliases catalog state")
	}
	resolved, err := catalog.ResolveMCP("local")
	if err != nil || resolved.ID != "local" || resolved.Env["TOKEN"] != "secret" {
		t.Fatalf("resolved MCP = %#v, err = %v", resolved, err)
	}
	resolved.Env["TOKEN"] = "changed"
	again, err := catalog.ResolveMCP("plasmid:configured:local")
	if err != nil || again.Env["TOKEN"] != "secret" {
		t.Fatalf("qualified MCP = %#v, err = %v", again, err)
	}
}

func assertLazySkillResources(t *testing.T, fixture defensiveCatalogFixture) {
	t.Helper()
	assertValidLazySkillResources(t, fixture)
	assertInvalidLazySkillResources(t, fixture)
	assertLazySkillReadFailures(t, fixture)
}

func assertValidLazySkillResources(t *testing.T, fixture defensiveCatalogFixture) {
	t.Helper()
	loaded, err := fixture.catalog.LoadSkill(t.Context(), "review", true)
	if err != nil || loaded.Body != "Review body.\n" || loaded.SelectedName != "plasmid:configured:review" {
		t.Fatalf("loaded skill = %#v, err = %v", loaded, err)
	}
	if content, err := fixture.catalog.LoadSkillResource(t.Context(), "review", "guide.txt", true); err != nil || content != "guide" {
		t.Fatalf("resource = %q, err = %v", content, err)
	}
}

func assertInvalidLazySkillResources(t *testing.T, fixture defensiveCatalogFixture) {
	t.Helper()
	for _, resource := range []string{"", ".", "../escape", filepath.Join(fixture.root, "absolute"), "docs"} {
		if _, err := fixture.catalog.LoadSkillResource(t.Context(), "review", resource, true); !errors.Is(err, extensions.ErrResource) {
			t.Fatalf("resource %q error = %v", resource, err)
		}
	}
	if _, err := fixture.catalog.LoadSkillResource(t.Context(), "review", "binary.bin", true); !errors.Is(err, extensions.ErrResource) {
		t.Fatalf("binary resource error = %v", err)
	}
	if _, err := fixture.catalog.LoadSkillResource(t.Context(), "review", "large.txt", true); !errors.Is(err, extensions.ErrResource) {
		t.Fatalf("large resource error = %v", err)
	}
	if _, err := fixture.catalog.LoadSkillResource(t.Context(), "review", "missing.txt", true); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing resource error = %v", err)
	}
}

func assertLazySkillReadFailures(t *testing.T, fixture defensiveCatalogFixture) {
	t.Helper()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := fixture.catalog.LoadSkill(canceled, "review", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled skill error = %v", err)
	}
	if _, err := fixture.catalog.LoadSkillResource(canceled, "review", "guide.txt", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resource error = %v", err)
	}
	if _, err := fixture.catalog.LoadSkill(nil, "review", true); err == nil || !strings.Contains(err.Error(), "nil context") {
		t.Fatalf("nil skill context error = %v", err)
	}
	if _, err := fixture.catalog.LoadSkillResource(nil, "review", "guide.txt", true); err == nil || !strings.Contains(err.Error(), "nil context") {
		t.Fatalf("nil resource context error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixture.skillDir, "SKILL.md"), []byte(fixture.source+"changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.catalog.LoadSkill(t.Context(), "review", true); !errors.Is(err, extensions.ErrChanged) {
		t.Fatalf("changed skill error = %v", err)
	}
}

func TestCatalogLoadsTemplatesAndRejectsAmbiguousMCPActivation(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(filepath.Join(codexHome, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "prompts", "review.md"), []byte("Template body.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	duplicate := config.MCPServer{ID: "duplicate", Transport: config.MCPStdio, Command: "server"}
	store, err := extensions.NewStore(extensions.Options{
		WorkingDir: root, HomeDir: home, Codex: true,
		Foreign: foreign.Options{HomeDir: home, CodexHome: codexHome, WorkingDir: root, RepositoryRoot: root},
		MCP:     config.MCP{Servers: []config.MCPServer{duplicate, duplicate}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	catalog, ok := store.Snapshot("session")
	if !ok || len(catalog.Templates()) != 1 {
		t.Fatalf("templates = %#v", catalog.Templates())
	}
	loaded, err := catalog.LoadTemplate(t.Context(), "review", false)
	if err != nil || loaded.Body != "Template body.\n" {
		t.Fatalf("loaded template = %#v, err = %v", loaded, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := catalog.LoadTemplate(canceled, "review", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled template error = %v", err)
	}
	if _, err := catalog.LoadTemplate(nil, "review", false); err == nil || !strings.Contains(err.Error(), "nil context") {
		t.Fatalf("nil template context error = %v", err)
	}
	if _, err := catalog.ResolveMCP("duplicate"); !errors.Is(err, extensions.ErrAmbiguous) {
		t.Fatalf("ambiguous MCP error = %v", err)
	}
}

func TestCatalogMergesIdenticalTemplatesAndRequiresQualificationForConflicts(t *testing.T) {
	catalog, codexPrompts := newTemplateConflictCatalog(t)
	assertTemplateConflictResolution(t, catalog)
	if err := os.WriteFile(filepath.Join(codexPrompts, "conflict.md"), []byte("Changed Codex template.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.LoadTemplate(t.Context(), "codex:user:conflict", false); !errors.Is(err, extensions.ErrChanged) {
		t.Fatalf("changed template error = %v", err)
	}
	if len(catalog.Warnings()) == 0 {
		t.Fatal("ambiguous template emitted no warning")
	}
}

func newTemplateConflictCatalog(t *testing.T) (extensions.Catalog, string) {
	t.Helper()
	root := t.TempDir()
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	claudeCommands := filepath.Join(home, ".claude", "commands")
	codexPrompts := filepath.Join(codexHome, "prompts")
	for _, directory := range []string{claudeCommands, codexPrompts} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(claudeCommands, "shared.md"):   "Shared template.\n",
		filepath.Join(codexPrompts, "shared.md"):     "Shared template.\n",
		filepath.Join(claudeCommands, "conflict.md"): "Claude template.\n",
		filepath.Join(codexPrompts, "conflict.md"):   "Codex template.\n",
		filepath.Join(claudeCommands, "manual.md"):   "---\ndisable-model-invocation: true\n---\nManual template.\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := extensions.NewStore(extensions.Options{
		WorkingDir: root, HomeDir: home, Claude: true, Codex: true,
		Foreign: foreign.Options{
			HomeDir: home, CodexHome: codexHome, WorkingDir: root, RepositoryRoot: root,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	return catalogFromStore(t, store, "session"), codexPrompts
}

func assertTemplateConflictResolution(t *testing.T, catalog extensions.Catalog) {
	t.Helper()
	templates := catalog.Templates()
	if len(templates) != 4 {
		t.Fatalf("templates = %#v", templates)
	}
	var shared extensions.Template
	for _, template := range templates {
		if template.Name == "shared" {
			shared = template
		}
	}
	if len(shared.Provenance) != 2 || len(shared.QualifiedNames) != 2 {
		t.Fatalf("merged shared template = %#v", shared)
	}
	if _, err := catalog.LoadTemplate(t.Context(), "conflict", false); !errors.Is(err, extensions.ErrAmbiguous) {
		t.Fatalf("unqualified conflicting template error = %v", err)
	}
	for _, qualified := range []string{"claude:user:conflict", "codex:user:conflict"} {
		loaded, err := catalog.LoadTemplate(t.Context(), qualified, false)
		if err != nil || loaded.Body == "" {
			t.Fatalf("qualified template %q = %#v, err = %v", qualified, loaded, err)
		}
	}
	if _, err := catalog.LoadTemplate(t.Context(), "manual", true); !errors.Is(err, extensions.ErrUntrusted) {
		t.Fatalf("model load of manual template error = %v", err)
	}
}

func TestStoreAndZeroCatalogRejectInvalidLifecycleRequests(t *testing.T) {
	if _, err := extensions.NewStore(extensions.Options{}); err == nil {
		t.Fatal("store without working directory was accepted")
	}
	store, err := extensions.NewStore(extensions.Options{WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartSession(nil, "session"); err == nil {
		t.Fatal("nil start context was accepted")
	}
	if err := store.StartSession(t.Context(), ""); err == nil {
		t.Fatal("empty session id was accepted")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.StartSession(canceled, "canceled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start error = %v", err)
	}
	store.ObserveTouch(t.Context(), workspace.Touch{})
	store.DropSession("missing")
	store.Close()
	store.Close()
	if err := store.StartSession(t.Context(), "closed"); !errors.Is(err, extensions.ErrClosed) {
		t.Fatalf("closed start error = %v", err)
	}
	var nilStore *extensions.Store
	nilStore.Close()
	nilStore.DropSession("session")
	nilStore.ObserveTouch(t.Context(), workspace.Touch{SessionID: "session", Path: "file.go"})

	var catalog extensions.Catalog
	if len(catalog.Skills()) != 0 || len(catalog.AllSkills()) != 0 || len(catalog.Templates()) != 0 || len(catalog.MCPServers()) != 0 || len(catalog.AllowedMCPNames()) != 0 || len(catalog.Instructions()) != 0 || len(catalog.CompiledPlugins()) != 0 || len(catalog.Warnings()) != 0 {
		t.Fatal("zero catalog exposed entries")
	}
	if _, err := catalog.ResolveMCP("missing"); !errors.Is(err, extensions.ErrNotFound) {
		t.Fatalf("zero MCP error = %v", err)
	}
	if _, err := catalog.LoadSkill(t.Context(), "missing", true); !errors.Is(err, extensions.ErrNotFound) {
		t.Fatalf("zero skill error = %v", err)
	}
	if _, err := catalog.LoadTemplate(t.Context(), "missing", true); !errors.Is(err, extensions.ErrNotFound) {
		t.Fatalf("zero template error = %v", err)
	}
	if _, err := catalog.LoadSkillResource(t.Context(), "missing", "resource", true); !errors.Is(err, extensions.ErrNotFound) {
		t.Fatalf("zero resource error = %v", err)
	}
}

func TestConfiguredDiscoverySkipsInvalidEntriesAndEnforcesExposure(t *testing.T) {
	root := t.TempDir()
	skillRoot := filepath.Join(root, "skills")
	writeInvalidConfiguredSkills(t, skillRoot)
	sink := &warning.SliceSink{}
	store, err := extensions.NewStore(extensions.Options{
		WorkingDir: root, SkillRoots: []string{filepath.Join(root, "missing"), skillRoot},
		Foreign: foreign.Options{ProjectTrusted: true}, MaxResourceBytes: 256, WarningSink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	catalog := catalogFromStore(t, store, "session")
	if len(catalog.Skills()) != 0 || len(catalog.AllSkills()) != 1 || catalog.AllSkills()[0].Name != "user-only" {
		t.Fatalf("catalog skills = visible %#v, all %#v", catalog.Skills(), catalog.AllSkills())
	}
	if _, err := catalog.LoadSkill(t.Context(), "user-only", true); !errors.Is(err, extensions.ErrUntrusted) {
		t.Fatalf("model load error = %v", err)
	}
	if loaded, err := catalog.LoadSkill(t.Context(), "user-only", false); err != nil || loaded.Body != "Body.\n" {
		t.Fatalf("user load = %#v, err = %v", loaded, err)
	}
	if _, err := catalog.LoadSkillResource(t.Context(), "user-only", "SKILL.md", true); !errors.Is(err, extensions.ErrUntrusted) {
		t.Fatalf("model resource error = %v", err)
	}
	if len(sink.Warnings()) == 0 {
		t.Fatal("invalid configured entries emitted no warning")
	}
}

func writeInvalidConfiguredSkills(t *testing.T, skillRoot string) {
	t.Helper()
	for _, name := range []string{"no-document", "incomplete", "too-large", "user-only"} {
		if err := os.MkdirAll(filepath.Join(skillRoot, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(skillRoot, "ordinary-file"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(skillRoot, "user-only"), filepath.Join(skillRoot, "symlink-directory")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillRoot, "incomplete", "SKILL.md"), []byte("---\nname: incomplete\n---\nBody.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillRoot, "too-large", "SKILL.md"), make([]byte, 257), 0o600); err != nil {
		t.Fatal(err)
	}
	userOnly := "---\nname: user-only\ndescription: User only\ndisable-model-invocation: true\n---\nBody.\n"
	if err := os.WriteFile(filepath.Join(skillRoot, "user-only", "SKILL.md"), []byte(userOnly), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredDiscoveryWarnsForNonDirectoryRootAndExhaustedLaterRoot(t *testing.T) {
	root := t.TempDir()
	nonDirectory := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(nonDirectory, []byte("ordinary file"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for skillRoot, name := range map[string]string{first: "alpha", second: "beta"} {
		directory := filepath.Join(skillRoot, name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		source := "---\nname: " + name + "\ndescription: test\n---\nBody.\n"
		if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sink := &warning.SliceSink{}
	store, err := extensions.NewStore(extensions.Options{
		WorkingDir: root, SkillRoots: []string{nonDirectory, first, second}, MaxEntries: 1,
		Foreign: foreign.Options{ProjectTrusted: true}, WarningSink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	catalog, ok := store.Snapshot("session")
	if !ok || len(catalog.AllSkills()) != 1 || catalog.AllSkills()[0].Name != "alpha" {
		t.Fatalf("catalog skills = %#v", catalog.AllSkills())
	}
	seen := make(map[string]bool)
	for _, item := range sink.Warnings() {
		seen[item.Code] = true
	}
	for _, code := range []string{warning.WarnForeignIndexUnreadable, warning.WarnForeignScanTruncated} {
		if !seen[code] {
			t.Fatalf("warning %q missing from %#v", code, sink.Warnings())
		}
	}
}

func TestForeignSkillProjectsMetadataAndToolPatterns(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	skillDir := filepath.Join(root, ".agents", "skills", "rich")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: rich\ndescription: Rich skill\nmetadata:\n  owner: team\nallowed-tools: [read]\n---\nBody.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := extensions.NewStore(extensions.Options{
		WorkingDir: root, HomeDir: home, Codex: true,
		Foreign: foreign.Options{
			HomeDir: home, WorkingDir: root, RepositoryRoot: root, ProjectTrusted: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	catalog := catalogFromStore(t, store, "session")
	all := catalog.AllSkills()
	if len(all) != 1 || len(all[0].Metadata) != 1 || all[0].Metadata[0].Name != "owner" {
		t.Fatalf("foreign skill metadata = %#v", all)
	}
	loaded, err := catalog.LoadSkill(t.Context(), "rich", true)
	if err != nil || len(loaded.AllowedTools) != 1 || loaded.AllowedTools[0].Tool != "read" {
		t.Fatalf("loaded foreign skill = %#v, err = %v", loaded, err)
	}
}

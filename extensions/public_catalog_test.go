package extensions_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/extensions"
	"github.com/plasmid-dev/plasmid/foreign"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

func TestStorePublishesDefensiveCatalogAndLazyResources(t *testing.T) {
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
	catalog, ok := store.Snapshot("session")
	if !ok {
		t.Fatal("session snapshot is unavailable")
	}

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

	loaded, err := catalog.LoadSkill(t.Context(), "review", true)
	if err != nil || loaded.Body != "Review body.\n" || loaded.SelectedName != "plasmid:configured:review" {
		t.Fatalf("loaded skill = %#v, err = %v", loaded, err)
	}
	if content, err := catalog.LoadSkillResource(t.Context(), "review", "guide.txt", true); err != nil || content != "guide" {
		t.Fatalf("resource = %q, err = %v", content, err)
	}
	for _, resource := range []string{"", ".", "../escape", filepath.Join(root, "absolute"), "docs"} {
		if _, err := catalog.LoadSkillResource(t.Context(), "review", resource, true); !errors.Is(err, extensions.ErrResource) {
			t.Fatalf("resource %q error = %v", resource, err)
		}
	}
	if _, err := catalog.LoadSkillResource(t.Context(), "review", "binary.bin", true); !errors.Is(err, extensions.ErrResource) {
		t.Fatalf("binary resource error = %v", err)
	}
	if _, err := catalog.LoadSkillResource(t.Context(), "review", "large.txt", true); !errors.Is(err, extensions.ErrResource) {
		t.Fatalf("large resource error = %v", err)
	}
	if _, err := catalog.LoadSkillResource(t.Context(), "review", "missing.txt", true); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing resource error = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := catalog.LoadSkill(canceled, "review", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled skill error = %v", err)
	}
	if _, err := catalog.LoadSkillResource(canceled, "review", "guide.txt", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resource error = %v", err)
	}
	if _, err := catalog.LoadSkill(nil, "review", true); err == nil || !strings.Contains(err.Error(), "nil context") {
		t.Fatalf("nil skill context error = %v", err)
	}
	if _, err := catalog.LoadSkillResource(nil, "review", "guide.txt", true); err == nil || !strings.Contains(err.Error(), "nil context") {
		t.Fatalf("nil resource context error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(source+"changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.LoadSkill(t.Context(), "review", true); !errors.Is(err, extensions.ErrChanged) {
		t.Fatalf("changed skill error = %v", err)
	}

	store.DropSession("session")
	if _, ok := store.Snapshot("session"); ok {
		t.Fatal("dropped session retained its snapshot")
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
	catalog, ok := store.Snapshot("session")
	if !ok {
		t.Fatal("snapshot unavailable")
	}
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

package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/foreign"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

func TestConfiguredCatalogExpandsIdentityAndConfinesLazyResources(t *testing.T) {
	root := t.TempDir()
	skillRoot := filepath.Join(root, "skills")
	directory := filepath.Join(skillRoot, "review")
	if err := os.MkdirAll(filepath.Join(directory, "resources"), 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: review\ndescription: Review code\narguments: [focus]\nallowed-tools: [read]\n---\nReview $ARGUMENTS ${focus} in $PROJECT_DIR.\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "resources", "guide.md"), []byte("guide\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(Options{WorkingDir: root, SkillRoots: []string{skillRoot}, Foreign: foreign.Options{ProjectTrusted: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	catalog, ok := store.Snapshot("session")
	if !ok {
		t.Fatal("missing session snapshot")
	}
	listed := catalog.Skills()
	if len(listed) != 1 || listed[0].QualifiedNames[0] != "plasmid:configured:review" {
		t.Fatalf("skills = %#v", listed)
	}
	loaded, err := catalog.LoadSkill(t.Context(), "review", true)
	if err != nil || loaded.Body != "Review $ARGUMENTS ${focus} in $PROJECT_DIR.\n" || !loaded.Restricted || len(loaded.Arguments) != 1 {
		t.Fatalf("loaded = %#v, err = %v", loaded, err)
	}
	resource, err := catalog.LoadSkillResource(t.Context(), "review", "resources/guide.md", true)
	if err != nil || resource != "guide\n" {
		t.Fatalf("resource = %q, err = %v", resource, err)
	}
	if _, err := catalog.LoadSkillResource(t.Context(), "review", "../outside", true); !errors.Is(err, ErrResource) {
		t.Fatalf("escape error = %v", err)
	}
}

func TestIdenticalCrossHostSkillDeduplicatesButResourcesRequireQualification(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	configured := filepath.Join(root, "configured")
	project := filepath.Join(root, ".agents", "skills")
	writeSkill(t, configured, "shared", "same")
	writeSkill(t, project, "shared", "same")
	if err := os.WriteFile(filepath.Join(configured, "shared", "guide.md"), []byte("configured"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "shared", "guide.md"), []byte("codex"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(Options{
		WorkingDir: root, HomeDir: home, SkillRoots: []string{configured}, Codex: true,
		Foreign: foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root, ProjectTrusted: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	catalog, _ := store.Snapshot("session")
	if skills := catalog.AllSkills(); len(skills) != 1 || len(skills[0].Provenance) != 2 {
		t.Fatalf("skills = %#v", skills)
	}
	if _, err := catalog.LoadSkillResource(t.Context(), "shared", "guide.md", true); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("unqualified resource error = %v", err)
	}
	configuredResource, err := catalog.LoadSkillResource(t.Context(), "plasmid:configured:shared", "guide.md", true)
	if err != nil || configuredResource != "configured" {
		t.Fatalf("configured resource = %q, err = %v", configuredResource, err)
	}
	codexResource, err := catalog.LoadSkillResource(t.Context(), "codex:project:shared", "guide.md", true)
	if err != nil || codexResource != "codex" {
		t.Fatalf("codex resource = %q, err = %v", codexResource, err)
	}
}

func TestLazySkillRejectsChangedBodyAndSymlinkResource(t *testing.T) {
	root := t.TempDir()
	skillRoot := filepath.Join(root, "skills")
	writeSkill(t, skillRoot, "review", "original")
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(skillRoot, "review", "escape.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	store, err := NewStore(Options{WorkingDir: root, SkillRoots: []string{skillRoot}, Foreign: foreign.Options{ProjectTrusted: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	catalog, _ := store.Snapshot("session")
	if _, err := catalog.LoadSkillResource(t.Context(), "review", "escape.txt", true); err == nil {
		t.Fatal("symlink resource escaped skill root")
	}
	writeSkill(t, skillRoot, "review", "changed")
	if _, err := catalog.LoadSkill(t.Context(), "review", true); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed source error = %v", err)
	}
}

func TestLazyResourceRejectsSnapshotRootReplacement(t *testing.T) {
	root := t.TempDir()
	skillRoot := filepath.Join(root, "skills")
	writeSkill(t, skillRoot, "review", "body")
	store, err := NewStore(Options{WorkingDir: root, SkillRoots: []string{skillRoot}, Foreign: foreign.Options{ProjectTrusted: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	catalog, _ := store.Snapshot("session")
	original := filepath.Join(skillRoot, "review")
	parked := filepath.Join(skillRoot, "parked")
	if err := os.Rename(original, parked); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, original); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := catalog.LoadSkillResource(t.Context(), "review", "secret.txt", true); !errors.Is(err, ErrChanged) {
		t.Fatalf("root replacement error = %v", err)
	}
}

func TestForeignMCPRequiresExactTrustedConsentAndCatalogStaysSecretFree(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"secret":{"command":"server","env":{"TOKEN":"TOPSECRET"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		allow []string
		want  bool
	}{
		{name: "no consent"},
		{name: "bare name is not canonical", allow: []string{"secret"}},
		{name: "wildcard is inert", allow: []string{"*"}},
		{name: "exact qualified name", allow: []string{"claude:project:secret"}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewStore(Options{
				WorkingDir: root, HomeDir: home, Claude: true,
				Foreign: foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root, ProjectTrusted: true},
				MCP:     config.MCP{AllowForeign: test.allow},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.StartSession(t.Context(), "session"); err != nil {
				t.Fatal(err)
			}
			catalog, _ := store.Snapshot("session")
			_, resolveErr := catalog.ResolveMCP("claude:project:secret")
			if test.want && resolveErr != nil {
				t.Fatal(resolveErr)
			}
			if !test.want && !errors.Is(resolveErr, ErrUntrusted) {
				t.Fatalf("ResolveMCP error = %v", resolveErr)
			}
			encoded, err := json.Marshal(catalog.MCPServers())
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) == "" || contains(string(encoded), "TOPSECRET") {
				t.Fatalf("catalog leaked activation secret: %s", encoded)
			}
			if formatted := fmt.Sprintf("%#v", catalog); contains(formatted, "TOPSECRET") {
				t.Fatalf("formatted catalog leaked activation secret: %s", formatted)
			}
		})
	}
}

func TestStoreFormattingRedactsActivationSecrets(t *testing.T) {
	store, err := NewStore(Options{
		WorkingDir: t.TempDir(),
		MCP: config.MCP{Servers: []config.MCPServer{{
			ID: "secret", Transport: config.MCPHTTP,
			URL:     "https://example.invalid/mcp?token=TOPSECRET",
			Headers: map[string]string{"Authorization": "Bearer TOPSECRET"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, value := range []string{
		fmt.Sprintf("%v", store),
		fmt.Sprintf("%+v", store),
		fmt.Sprintf("%#v", store),
	} {
		if strings.Contains(value, "TOPSECRET") || strings.Contains(value, "Bearer") {
			t.Fatalf("formatted store leaked activation secret: %s", value)
		}
	}
	var output bytes.Buffer
	slog.New(slog.NewJSONHandler(&output, nil)).Info("store", "value", store)
	if value := output.String(); strings.Contains(value, "TOPSECRET") || strings.Contains(value, "Bearer") {
		t.Fatalf("logged store leaked activation secret: %s", value)
	}
}

func TestConfiguredDiscoveryEntryBudgetTruncatesDeterministically(t *testing.T) {
	root := t.TempDir()
	skillRoot := filepath.Join(root, "skills")
	writeSkill(t, skillRoot, "alpha", "first")
	writeSkill(t, skillRoot, "beta", "second")
	store, err := NewStore(Options{WorkingDir: root, SkillRoots: []string{skillRoot}, MaxEntries: 1, Foreign: foreign.Options{ProjectTrusted: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	catalog, _ := store.Snapshot("session")
	if skills := catalog.AllSkills(); len(skills) != 1 || skills[0].Name != "alpha" {
		t.Fatalf("skills = %#v", skills)
	}
	if notices := catalog.Warnings(); len(notices) != 1 || notices[0].Code != warning.WarnForeignScanTruncated {
		t.Fatalf("warnings = %#v", notices)
	}
}

func TestSkillGlobsActivateThroughSessionTouches(t *testing.T) {
	root := t.TempDir()
	skillRoot := filepath.Join(root, "skills")
	directory := filepath.Join(skillRoot, "source-review")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: source-review\ndescription: Review source\nglobs: [src/**]\n---\nReview source.\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(Options{WorkingDir: root, SkillRoots: []string{skillRoot}, Foreign: foreign.Options{ProjectTrusted: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	catalog, _ := store.Snapshot("session")
	if skills := catalog.Skills(); len(skills) != 0 {
		t.Fatalf("path-scoped skills before touch = %#v", skills)
	}
	if _, err := catalog.LoadSkill(t.Context(), "source-review", true); !errors.Is(err, ErrInactive) {
		t.Fatalf("inactive load error = %v", err)
	}
	store.ObserveTouch(t.Context(), workspace.Touch{SessionID: "session", Path: "docs/readme.md", Kind: workspace.TouchRead})
	catalog, _ = store.Snapshot("session")
	if skills := catalog.Skills(); len(skills) != 0 {
		t.Fatalf("nonmatching touch activated skills = %#v", skills)
	}
	store.ObserveTouch(t.Context(), workspace.Touch{SessionID: "session", Path: "src/main.go", Kind: workspace.TouchRead})
	catalog, _ = store.Snapshot("session")
	if skills := catalog.Skills(); len(skills) != 1 || skills[0].Name != "source-review" {
		t.Fatalf("matching touch skills = %#v", skills)
	}
}

func TestSessionCatalogIncludesDiscoveredInstructionRecords(t *testing.T) {
	store, err := NewStore(Options{WorkingDir: t.TempDir(), Instructions: []Instruction{{
		Name: "compiled-rules", Provenance: []Provenance{{Host: "plasmid", Scope: "compiled", Enabled: true, Trusted: true, Classification: "compiled"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	discovered := []Instruction{{
		Name: "AGENTS.md", Provenance: []Provenance{{Host: "codex", Scope: "project", SourcePath: "AGENTS.md", Enabled: true, Classification: "documented"}},
	}}
	if err := store.StartSessionWithInstructions(t.Context(), "session", discovered); err != nil {
		t.Fatal(err)
	}
	catalog, _ := store.Snapshot("session")
	records := catalog.Instructions()
	if len(records) != 2 || records[0].Name != "compiled-rules" || records[1].Name != "AGENTS.md" {
		t.Fatalf("instruction records = %#v", records)
	}
	discovered[0].Provenance[0].SourcePath = "mutated"
	if got := catalog.Instructions()[1].Provenance[0].SourcePath; got != "AGENTS.md" {
		t.Fatalf("catalog instruction provenance mutated: %q", got)
	}
}

func TestCatalogKeepsConflictingNamesAmbiguousAndSnapshotsStable(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	writeSkill(t, first, "review", "first")
	writeSkill(t, second, "review", "second")
	store, err := NewStore(Options{WorkingDir: root, SkillRoots: []string{first, second}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartSession(t.Context(), "one"); err != nil {
		t.Fatal(err)
	}
	one, _ := store.Snapshot("one")
	if notices := one.Warnings(); len(notices) != 1 || notices[0].Code != warning.WarnForeignAmbiguousName {
		t.Fatalf("ambiguity warnings = %#v", notices)
	}
	if _, err := one.LoadSkill(t.Context(), "review", false); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("unqualified error = %v", err)
	}
	if err := os.RemoveAll(filepath.Join(second, "review")); err != nil {
		t.Fatal(err)
	}
	if _, err := one.LoadSkill(t.Context(), "review", false); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("existing snapshot changed: %v", err)
	}
	if err := store.StartSession(t.Context(), "two"); err != nil {
		t.Fatal(err)
	}
	two, _ := store.Snapshot("two")
	if _, err := two.LoadSkill(t.Context(), "review", false); err != nil {
		t.Fatalf("refreshed snapshot: %v", err)
	}
}

func TestQualifiedSkillResourceRejectsMultipleConfiguredRoots(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	writeSkill(t, first, "review", "same")
	writeSkill(t, second, "review", "same")
	if err := os.WriteFile(filepath.Join(first, "review", "guide.md"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "review", "guide.md"), []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(Options{WorkingDir: root, SkillRoots: []string{first, second}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	catalog, _ := store.Snapshot("session")
	if _, err := catalog.LoadSkillResource(t.Context(), "plasmid:configured:review", "guide.md", false); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("qualified resource error = %v", err)
	}
}

func TestStoreCloseCancelsAndWaitsForDiscovery(t *testing.T) {
	store, err := NewStore(Options{WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	cancelled := make(chan struct{})
	store.discover = func(ctx context.Context, _ Options) (Catalog, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return Catalog{}, ctx.Err()
	}
	startErr := make(chan error, 1)
	go func() { startErr <- store.StartSession(context.Background(), "session") }()
	<-started
	closed := make(chan struct{})
	go func() {
		store.Close()
		close(closed)
	}()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel discovery")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not wait for discovery completion")
	}
	if err := <-startErr; !errors.Is(err, ErrClosed) {
		t.Fatalf("StartSession error = %v", err)
	}
}

func writeSkill(t *testing.T, root, name, body string) {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: " + name + "\ndescription: test\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

func contains(value, fragment string) bool {
	return len(fragment) > 0 && len(value) >= len(fragment) && strings.Contains(value, fragment)
}

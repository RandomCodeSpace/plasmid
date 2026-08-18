package contextresolver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

func TestSessionViewDiscoversImportsAndActivatesNestedInstructions(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "AGENTS.md", "root agent\n")
	writeFile(t, rootDir, "CLAUDE.md", "@shared.md\nroot claude\n")
	writeFile(t, rootDir, "shared.md", "shared import\n")
	writeFile(t, rootDir, "pkg/AGENTS.md", "nested package\n")
	writeFile(t, rootDir, ".github/instructions/go.instructions.md", "---\napplyTo: \"**/*.go\"\n---\ngo scoped\n")

	resolver := newTestResolver(t, rootDir, Options{})
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	initial, err := resolver.Instructions(t.Context(), "session", "first")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"root agent", "shared import", "root claude"} {
		if !strings.Contains(initial, value) {
			t.Fatalf("initial instructions %q lack %q", initial, value)
		}
	}
	if strings.Contains(initial, "nested package") || strings.Contains(initial, "go scoped") {
		t.Fatalf("nested instructions activated before touch: %q", initial)
	}

	resolver.ObserveTouch(t.Context(), workspace.Touch{SessionID: "session", Path: "pkg/main.go", Kind: workspace.TouchRead})
	resolver.ReleaseRun("session")
	after, err := resolver.Instructions(t.Context(), "session", "second")
	if err != nil {
		t.Fatal(err)
	}
	positions := []int{
		strings.Index(after, "root agent"), strings.Index(after, "root claude"),
		strings.Index(after, "go scoped"), strings.Index(after, "nested package"),
	}
	for index, position := range positions {
		if position < 0 || index > 0 && position <= positions[index-1] {
			t.Fatalf("instructions not least-to-most specific: %q, positions %v", after, positions)
		}
	}
}

func TestInstructionRecordsRetainHostProvenanceAcrossContentDedup(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "AGENTS.md", "shared\n")
	writeFile(t, rootDir, "CLAUDE.md", "shared\n")
	writeFile(t, rootDir, ".github/copilot-instructions.md", "copilot\n")
	resolver := newTestResolver(t, rootDir, Options{})
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	records := resolver.InstructionRecords("session")
	if len(records) != 2 {
		t.Fatalf("instruction records = %#v", records)
	}
	hosts := make(map[string]bool)
	for _, record := range records {
		for _, source := range record.Provenance {
			hosts[source.Host] = true
			if source.Scope != "project" || !source.Enabled || source.Trusted || source.Classification != "documented" || source.SourcePath == "" {
				t.Fatalf("instruction provenance = %#v", source)
			}
		}
	}
	for _, host := range []string{"claude", "codex", "copilot"} {
		if !hosts[host] {
			t.Fatalf("instruction hosts = %#v", hosts)
		}
	}
}

func TestInstructionRecordsRetainSameRealPathAliases(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "AGENTS.md", "shared\n")
	if err := os.Symlink(filepath.Join(rootDir, "AGENTS.md"), filepath.Join(rootDir, "CLAUDE.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	resolver := newTestResolver(t, rootDir, Options{})
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	records := resolver.InstructionRecords("session")
	if len(records) != 1 || len(records[0].Provenance) != 2 {
		t.Fatalf("instruction records = %#v", records)
	}
	hosts := map[string]bool{}
	for _, source := range records[0].Provenance {
		hosts[source.Host] = true
	}
	if !hosts["claude"] || !hosts["codex"] {
		t.Fatalf("same-path provenance hosts = %#v", hosts)
	}
}

func TestExtensionExpansionRejectsTotalSubstitutionAmplification(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	resolver := newTestResolver(t, rootDir, Options{DocumentOutputBytes: 64})
	_, err := resolver.Expand(t.Context(), Expansion{
		Source: strings.Repeat("$ARGUMENTS", 16), Arguments: strings.Repeat("x", 32), Path: "skill.md",
	})
	if !errors.Is(err, syntax.ErrSubstitutionLimit) {
		t.Fatalf("Expand error = %v", err)
	}
}

func TestUserInstructionsLoadOutsideTheWorkspaceBeforeRepositoryRoot(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()
	homeDir := filepath.Join(baseDir, "home")
	rootDir := filepath.Join(baseDir, "workspace")
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, homeDir, ".codex/AGENTS.md", "user instruction\n")
	writeFile(t, rootDir, "AGENTS.md", "repository instruction\n")
	resolver := newTestResolver(t, rootDir, Options{HomeDir: homeDir})
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(assembled, "user instruction") ||
		strings.Index(assembled, "user instruction") >= strings.Index(assembled, "repository instruction") {
		t.Fatalf("assembled order = %q", assembled)
	}
}

func TestHostSelectionLimitsInstructionDiscovery(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "AGENTS.md", "codex instruction\n")
	writeFile(t, rootDir, "CLAUDE.md", "claude instruction\n")
	writeFile(t, rootDir, ".github/copilot-instructions.md", "copilot instruction\n")
	resolver := newTestResolver(t, rootDir, Options{Hosts: &HostSelection{Codex: true}})
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(assembled, "codex instruction") || strings.Contains(assembled, "claude instruction") || strings.Contains(assembled, "copilot instruction") {
		t.Fatalf("assembled = %q", assembled)
	}
}

func TestDiscoveryDeduplicatesContentAndContainsImportCycles(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "CLAUDE.md", "@first.md\n")
	writeFile(t, rootDir, "first.md", "@second.md\nfirst\n")
	writeFile(t, rootDir, "second.md", "@first.md\nsecond\n")
	writeFile(t, rootDir, "AGENTS.md", "identical\n")
	writeFile(t, rootDir, "pkg/AGENTS.md", "identical\n")
	sink := &warning.SliceSink{}
	resolver := newTestResolver(t, rootDir, Options{WarningSink: sink})
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(assembled, "identical") != 1 {
		t.Fatalf("dedup failed: %q", assembled)
	}
	if !hasWarning(sink.Warnings(), warning.WarnContextImportCycle) || !hasWarning(sink.Warnings(), warning.WarnContextDedupDropped) {
		t.Fatalf("warnings = %#v", sink.Warnings())
	}
}

func TestContentDedupKeepsDistinctFrontmatterPolicies(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "AGENT.md", "---\nallowed-tools: [Read, Write]\n---\nsame body\n")
	writeFile(t, rootDir, "AGENTS.md", "---\ndisallowed-tools: Write\n---\nsame body\n")
	resolver := newTestResolver(t, rootDir, Options{})
	if _, err := resolver.Instructions(t.Context(), "session", "invocation"); err != nil {
		t.Fatal(err)
	}
	if !resolver.Allows("session", "invocation", "read", map[string]any{"path": "x"}) ||
		resolver.Allows("session", "invocation", "write", map[string]any{"path": "x"}) {
		t.Fatal("dedup discarded a distinct frontmatter policy")
	}
}

func TestDroppedPathScopedSourceDoesNotShadowValidNestedContent(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, ".github/instructions/unscoped.instructions.md", "same content\n")
	writeFile(t, rootDir, "pkg/AGENTS.md", "same content\n")
	resolver := newTestResolver(t, rootDir, Options{})
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	resolver.ObserveTouch(t.Context(), workspace.Touch{SessionID: "session", Path: "pkg/file.go"})
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	if assembled != "same content" {
		t.Fatalf("assembled = %q, want nested content", assembled)
	}
}

func TestUnconditionalClaudeProjectRuleLoadsAtSessionStart(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, ".claude/rules/unconditional.md", "always active\n")
	resolver := newTestResolver(t, rootDir, Options{})
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	if assembled != "always active" {
		t.Fatalf("assembled = %q", assembled)
	}
}

func TestMalformedClaudeProjectRulePathScopeFailsClosed(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, ".claude/rules/malformed.md", "---\npaths: ['{bad}']\n---\nmust not become global\n")
	sink := &warning.SliceSink{}
	resolver := newTestResolver(t, rootDir, Options{WarningSink: sink})
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(assembled, "must not become global") || !hasWarning(sink.Warnings(), warning.WarnContextGlobUnsupported) {
		t.Fatalf("assembled = %q, warnings = %#v", assembled, sink.Warnings())
	}
}

func TestImportedToolPolicyRestrictsParentDocument(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "CLAUDE.md", "@restricted.md\nroot\n")
	writeFile(t, rootDir, "restricted.md", "---\nallowed-tools: Read\n---\nrestricted\n")
	resolver := newTestResolver(t, rootDir, Options{})
	if _, err := resolver.Instructions(t.Context(), "session", "invocation"); err != nil {
		t.Fatal(err)
	}
	if !resolver.Allows("session", "invocation", "read", map[string]any{"path": "x"}) ||
		resolver.Allows("session", "invocation", "write", map[string]any{"path": "x"}) {
		t.Fatal("imported restrictive policy was discarded")
	}
}

func TestClaudeImportsStayWithinApprovedRoots(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()
	rootDir := filepath.Join(baseDir, "workspace")
	if err := os.Mkdir(rootDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, baseDir, "secret.md", "outside secret\n")
	writeFile(t, rootDir, "CLAUDE.md", "@../secret.md\nroot\n")
	sink := &warning.SliceSink{}
	resolver := newTestResolver(t, rootDir, Options{WarningSink: sink})
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(assembled, "outside secret") || !strings.Contains(assembled, "root") {
		t.Fatalf("assembled = %q", assembled)
	}
	if !hasWarning(sink.Warnings(), warning.WarnContextImportEscape) {
		t.Fatalf("warnings = %#v", sink.Warnings())
	}
}

func TestRootedInstructionReadRejectsEscapeSymlink(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.md")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootDir, "escape.md")); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	if _, _, err := readBoundedAt(t.Context(), rootDir, "escape.md", 1024); err == nil {
		t.Fatal("rooted instruction read followed a symlink outside its approved root")
	}
}

func TestConfiguredZeroImportDepthDisablesImports(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "CLAUDE.md", "@imported.md\nroot\n")
	writeFile(t, rootDir, "imported.md", "must not load\n")
	sink := &warning.SliceSink{}
	resolver := newTestResolver(t, rootDir, Options{
		MaxImportDepth: 0, MaxImportDepthSet: true, WarningSink: sink,
	})
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(assembled, "must not load") || !strings.Contains(assembled, "root") {
		t.Fatalf("assembled = %q", assembled)
	}
	if !hasWarning(sink.Warnings(), warning.WarnContextImportDepth) {
		t.Fatalf("warnings = %#v", sink.Warnings())
	}
}

func TestImportedCommandsUseImportedSourceTrust(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()
	rootDir := filepath.Join(baseDir, "workspace")
	importDir := filepath.Join(baseDir, "imports")
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rootMarker := filepath.Join(rootDir, "root-ran")
	localMarker := filepath.Join(rootDir, "local-ran")
	externalMarker := filepath.Join(rootDir, "external-must-not-run")
	writeFile(t, rootDir, "CLAUDE.md", "!`printf x > "+shellQuote(rootMarker)+"`\n@local.md\n@../imports/external.md\nroot\n")
	writeFile(t, rootDir, "local.md", "!`printf x > "+shellQuote(localMarker)+"`\nlocal\n")
	writeFile(t, importDir, "external.md", "!`printf x > "+shellQuote(externalMarker)+"`\nexternal\n")
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := shellexec.New(shellexec.Config{
		Root: root, DefaultTimeout: time.Second, MaxTimeout: time.Second,
		OutputLimit: outputlimit.Policy{MaxBytes: 1024, MaxLines: 100, MaxLineBytes: 1024},
	})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	sink := &warning.SliceSink{}
	resolver := newTestResolver(t, rootDir, Options{
		ImportRoots: []string{importDir}, TrustedRoots: []string{rootDir},
		PromptCommands: config.PromptCommandsTrusted, CommandTimeout: time.Second,
		DocumentTimeout: time.Second, CommandOutputBytes: 64, DocumentOutputBytes: 64,
		Executor: executor, WarningSink: sink,
	})
	if _, err := resolver.Instructions(t.Context(), "session", "invocation"); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{rootMarker, localMarker} {
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("trusted command %q did not execute: %v", marker, err)
		}
	}
	if _, err := os.Stat(externalMarker); !os.IsNotExist(err) {
		t.Fatalf("untrusted imported command executed: %v", err)
	}
	if !hasWarning(sink.Warnings(), warning.WarnSyntaxExecDisabled) {
		t.Fatalf("warnings = %#v", sink.Warnings())
	}
}

func TestAssemblyMapsHarnessVariablesWithoutReadingEnvironment(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "AGENTS.md", "${SESSION_ID}|${PROJECT_DIR}|${EFFORT}|${HOME}\n")
	sink := &warning.SliceSink{}
	resolver := newTestResolver(t, rootDir, Options{WarningSink: sink})
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	want := "session|" + rootDir + "|normal|${HOME}"
	if assembled != want {
		t.Fatalf("assembled = %q, want %q", assembled, want)
	}
	if !hasWarning(sink.Warnings(), warning.WarnSyntaxUnresolvedVariable) {
		t.Fatalf("warnings = %#v", sink.Warnings())
	}
}

func TestAssemblyBudgetEvictsLeastSpecificContent(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "AGENTS.md", "least-specific-root\n")
	writeFile(t, rootDir, "pkg/AGENTS.md", "most-specific-nested\n")
	sink := &warning.SliceSink{}
	resolver := newTestResolver(t, rootDir, Options{MaxBytes: 24, WarningSink: sink})
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	resolver.ObserveTouch(t.Context(), workspace.Touch{SessionID: "session", Path: "pkg/file.go"})
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(assembled, "least-specific") || !strings.Contains(assembled, "most-specific") || !hasWarning(sink.Warnings(), warning.WarnContextBudgetDropped) {
		t.Fatalf("assembled = %q, warnings = %#v", assembled, sink.Warnings())
	}
}

func TestAssemblySkipsEmptyDocumentsWithoutDuplicatingLaterContent(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "AGENT.md", "")
	writeFile(t, rootDir, "AGENTS.md", "retained\n")
	resolver := newTestResolver(t, rootDir, Options{})
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	if assembled != "retained" {
		t.Fatalf("assembled = %q, want retained", assembled)
	}
}

func TestPromptCommandTrustMatrixAndBounds(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := shellexec.New(shellexec.Config{
		Root: root, DefaultTimeout: time.Second, MaxTimeout: time.Second,
		OutputLimit: outputlimit.Policy{MaxBytes: 1024, MaxLines: 100, MaxLineBytes: 1024},
	})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	for _, test := range promptCommandTrustCases() {
		t.Run(test.name, func(t *testing.T) {
			testPromptCommandTrust(t, rootDir, executor, test)
		})
	}
}

type promptCommandTrustCase struct {
	name  string
	mode  config.PromptCommandMode
	trust TrustLevel
}

func promptCommandTrustCases() []promptCommandTrustCase {
	var result []promptCommandTrustCase
	for _, mode := range []config.PromptCommandMode{config.PromptCommandsOff, config.PromptCommandsTrusted, config.PromptCommandsOn} {
		for _, trust := range []TrustLevel{TrustUser, TrustRepository, TrustUntrusted} {
			result = append(result, promptCommandTrustCase{name: string(mode) + "/" + trust.String(), mode: mode, trust: trust})
		}
	}
	return result
}

func testPromptCommandTrust(t *testing.T, rootDir string, executor *shellexec.Executor, test promptCommandTrustCase) {
	t.Helper()
	marker := filepath.Join(rootDir, strings.ReplaceAll(test.name, "/", "-"))
	source := "!`printf ran; printf x > " + shellQuote(marker) + "`"
	sink := &warning.SliceSink{}
	got := expandCommands(t.Context(), source, "fixture.md", test.trust, commandOptions{
		Mode: test.mode, CommandTimeout: time.Second, DocumentTimeout: time.Second,
		CommandOutputBytes: 64, DocumentOutputBytes: 64,
	}, executor, sink)
	wantRun := test.mode == config.PromptCommandsOn || test.mode == config.PromptCommandsTrusted && test.trust != TrustUntrusted
	_, statErr := os.Stat(marker)
	if wantRun != (statErr == nil) {
		t.Fatalf("execution = %v, want %v, output %q, warnings %#v", statErr == nil, wantRun, got, sink.Warnings())
	}
	if wantRun && got != "ran" {
		t.Fatalf("expanded = %q", got)
	}
	if !wantRun && !hasWarning(sink.Warnings(), warning.WarnSyntaxExecDisabled) {
		t.Fatalf("warnings = %#v", sink.Warnings())
	}
}

func TestPromptCommandExpansionIsStableAcrossLazyActivation(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	marker := filepath.Join(rootDir, "executions")
	writeFile(t, rootDir, "AGENTS.md", "!`printf x >> "+shellQuote(marker)+"`\nroot\n")
	writeFile(t, rootDir, "pkg/AGENTS.md", "nested\n")
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := shellexec.New(shellexec.Config{
		Root: root, DefaultTimeout: time.Second, MaxTimeout: time.Second,
		OutputLimit: outputlimit.Policy{MaxBytes: 1024, MaxLines: 100, MaxLineBytes: 1024},
	})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	resolver := newTestResolver(t, rootDir, Options{
		TrustedRoots: []string{rootDir}, PromptCommands: config.PromptCommandsTrusted,
		CommandTimeout: time.Second, DocumentTimeout: time.Second,
		CommandOutputBytes: 64, DocumentOutputBytes: 64, Executor: executor,
	})
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Instructions(t.Context(), "session", "invocation"); err != nil {
		t.Fatal(err)
	}
	resolver.ObserveTouch(t.Context(), workspace.Touch{SessionID: "session", Path: "pkg/file.go"})
	if _, err := resolver.Instructions(t.Context(), "session", "invocation"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "x" {
		t.Fatalf("command executions = %q, want one", got)
	}
	resolver.ReleaseRun("session")
	if _, err := resolver.Instructions(t.Context(), "session", "next-invocation"); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "xx" {
		t.Fatalf("command executions after next invocation = %q, want two", got)
	}
}

func TestPromptCommandDocumentBudgetSpansImportSplitFragments(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	marker := filepath.Join(rootDir, "second-command-must-not-run")
	writeFile(t, rootDir, "CLAUDE.md", "!`printf 1234`\n@import.md\n!`printf 5678; printf x > "+shellQuote(marker)+"`\n")
	writeFile(t, rootDir, "import.md", "imported\n")
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := shellexec.New(shellexec.Config{
		Root: root, DefaultTimeout: time.Second, MaxTimeout: time.Second,
		OutputLimit: outputlimit.Policy{MaxBytes: 1024, MaxLines: 100, MaxLineBytes: 1024},
	})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	sink := &warning.SliceSink{}
	resolver := newTestResolver(t, rootDir, Options{
		TrustedRoots: []string{rootDir}, PromptCommands: config.PromptCommandsTrusted,
		CommandTimeout: time.Second, DocumentTimeout: time.Second,
		CommandOutputBytes: 64, DocumentOutputBytes: 4, Executor: executor, WarningSink: sink,
	})
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("second command executed after document budget exhaustion: %v", err)
	}
	if strings.Contains(assembled, "5678") || !hasWarning(sink.Warnings(), warning.WarnSyntaxExecBudgetExhausted) {
		t.Fatalf("assembled = %q, warnings = %#v", assembled, sink.Warnings())
	}
}

func TestTrustIsComputedFromEachRepositorySourcePath(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()
	rootDir := filepath.Join(baseDir, "workspace")
	if err := os.Mkdir(rootDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ancestorMarker := filepath.Join(rootDir, "ancestor-must-not-run")
	rootMarker := filepath.Join(rootDir, "root-must-run")
	writeFile(t, baseDir, "AGENTS.md", "!`printf x > "+shellQuote(ancestorMarker)+"`\nancestor\n")
	writeFile(t, rootDir, "AGENTS.md", "!`printf x > "+shellQuote(rootMarker)+"`\nroot\n")
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := shellexec.New(shellexec.Config{
		Root: root, DefaultTimeout: time.Second, MaxTimeout: time.Second,
		OutputLimit: outputlimit.Policy{MaxBytes: 1024, MaxLines: 100, MaxLineBytes: 1024},
	})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	resolver := newTestResolver(t, rootDir, Options{
		TrustedRoots:   []string{rootDir},
		PromptCommands: config.PromptCommandsTrusted, CommandTimeout: time.Second,
		DocumentTimeout: time.Second, CommandOutputBytes: 64, DocumentOutputBytes: 64,
		Executor: executor,
	})
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Instructions(t.Context(), "session", "invocation"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ancestorMarker); !os.IsNotExist(err) {
		t.Fatalf("untrusted ancestor command executed: %v", err)
	}
	if _, err := os.Stat(rootMarker); err != nil {
		t.Fatalf("trusted root command did not execute: %v", err)
	}
}

func TestDiscoveryEntryBudgetStopsWorkspaceWalk(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	for _, name := range []string{"a/AGENTS.md", "b/AGENTS.md", "c/AGENTS.md"} {
		writeFile(t, rootDir, name, name+"\n")
	}
	sink := &warning.SliceSink{}
	resolver := newTestResolver(t, rootDir, Options{MaxDiscoveryEntries: 3, WarningSink: sink})
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	if !hasWarning(sink.Warnings(), warning.WarnContextDiscoveryTruncated) {
		t.Fatalf("warnings = %#v", sink.Warnings())
	}
}

func TestDiscoveryEntryBudgetAlsoBoundsImports(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "CLAUDE.md", "@one.md\n@two.md\nroot\n")
	writeFile(t, rootDir, "one.md", "one\n")
	writeFile(t, rootDir, "two.md", "two\n")
	sink := &warning.SliceSink{}
	resolver := newTestResolver(t, rootDir, Options{MaxDiscoveryEntries: 4, WarningSink: sink})
	assembled, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(assembled, "two") || !hasWarning(sink.Warnings(), warning.WarnContextDiscoveryTruncated) {
		t.Fatalf("assembled = %q, warnings = %#v", assembled, sink.Warnings())
	}
}

func TestResolverDefaultsToObservableWarnings(t *testing.T) {
	t.Parallel()
	resolver := newTestResolver(t, t.TempDir(), Options{})
	if _, ok := resolver.options.WarningSink.(warning.SlogSink); !ok {
		t.Fatalf("default warning sink = %T, want warning.SlogSink", resolver.options.WarningSink)
	}
}

func TestToolScopeReleasesAfterAbort(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	writeFile(t, rootDir, "AGENTS.md", "---\nallowed-tools: Read\n---\nroot\n")
	resolver := newTestResolver(t, rootDir, Options{})
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Instructions(t.Context(), "session", "first"); err != nil {
		t.Fatal(err)
	}
	if !resolver.Allows("session", "first", "read", map[string]any{"path": "x"}) || resolver.Allows("session", "first", "write", map[string]any{"path": "x"}) {
		t.Fatal("active tool policy was not enforced")
	}
	resolver.ReleaseRun("session")
	if resolver.ActiveScopes() != 0 || resolver.Allows("session", "first", "write", map[string]any{"path": "x"}) {
		t.Fatal("released scope widened authority")
	}
}

func TestStartSessionHonorsCancellation(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	resolver := newTestResolver(t, rootDir, Options{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := resolver.StartSession(ctx, "session"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func newTestResolver(t *testing.T, rootDir string, overrides Options) *Resolver {
	t.Helper()
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	overrides.Root = root
	if overrides.MaxFileBytes == 0 {
		overrides.MaxFileBytes = 16 << 10
	}
	if overrides.MaxBytes == 0 {
		overrides.MaxBytes = 256 << 10
	}
	if overrides.MaxImportDepth == 0 && !overrides.MaxImportDepthSet {
		overrides.MaxImportDepth = 4
	}
	resolver, err := New(overrides)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)
	return resolver
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hasWarning(values []warning.Warning, code string) bool {
	for _, value := range values {
		if value.Code == code {
			return true
		}
	}
	return false
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

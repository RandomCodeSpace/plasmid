package contextresolver_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/contextresolver"
	"github.com/plasmid-dev/plasmid/extensions"
	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

type cancelAtBoundaryContext struct {
	limit int

	mu     sync.Mutex
	checks int
	done   chan struct{}
	err    error
}

func newCancelAtBoundaryContext(limit int) *cancelAtBoundaryContext {
	return &cancelAtBoundaryContext{limit: limit, done: make(chan struct{})}
}

func (*cancelAtBoundaryContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAtBoundaryContext) Done() <-chan struct{}     { return c.done }
func (*cancelAtBoundaryContext) Value(any) any               { return nil }

func (c *cancelAtBoundaryContext) Err() error {
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

func TestResolverStopsAtEveryDiscoveryCancellationBoundary(t *testing.T) {
	rootDir := t.TempDir()
	for path, content := range map[string]string{
		"CLAUDE.md":             "@included.md\n" + strings.Repeat("project context\n", 3000),
		"included.md":           "imported context\n",
		"nested/AGENTS.md":      "nested context\n",
		".claude/rules/rule.md": "rule context\n",
	} {
		fullPath := filepath.Join(rootDir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}

	succeeded := false
	for limit := 1; limit <= 512; limit++ {
		resolver, newErr := contextresolver.New(contextresolver.Options{
			Root: root, MaxFileBytes: 128 << 10, MaxBytes: 256 << 10, MaxImportDepth: 4,
		})
		if newErr != nil {
			t.Fatal(newErr)
		}
		ctx := newCancelAtBoundaryContext(limit)
		err := resolver.StartSession(ctx, "session")
		resolver.Close()
		if err == nil {
			succeeded = true
			break
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation boundary %d error = %v", limit, err)
		}
	}
	if !succeeded {
		t.Fatal("discovery did not complete within the cancellation-boundary limit")
	}
}

func TestResolverReportsPublicDiscoveryEdgeCases(t *testing.T) {
	rootDir := t.TempDir()
	homeDir := t.TempDir()
	writePublicDiscoveryEdgeCaseFiles(t, rootDir, homeDir)
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	sink := &warning.SliceSink{}
	resolver, err := contextresolver.New(contextresolver.Options{
		Root: root, HomeDir: homeDir, MaxFileBytes: 1024, WarningSink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	text, err := resolver.Instructions(t.Context(), "session", "turn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "shared user context") || !strings.Contains(text, "codex context") {
		t.Fatalf("assembled context = %q", text)
	}
	assertContextWarningCodes(t, sink.Warnings())
}

func writePublicDiscoveryEdgeCaseFiles(t *testing.T, rootDir, homeDir string) {
	t.Helper()
	files := map[string]string{
		"AGENT.md":          "@ignored.md\ncodex context\n",
		"CLAUDE.md":         "@missing.md\n@same-one.md\n@same-two.md\n@same-one.md\n@large.md\n",
		"same-one.md":       "identical imported context\n",
		"same-two.md":       "identical imported context\n",
		"large.md":          strings.Repeat("large imported context\n", 100),
		"ignored.md":        "ignored import\n",
		".git/AGENTS.md":    "git metadata context\n",
		"nested/README.md":  "ordinary file\n",
		"nested/ignored.md": "ordinary nested file\n",
	}
	for path, content := range files {
		fullPath := filepath.Join(rootDir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dangling := filepath.Join(rootDir, "AGENTS.md")
	if err := os.Symlink(filepath.Join(rootDir, "missing-agents.md"), dangling); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	homeClaudeDir := filepath.Join(homeDir, ".claude")
	if err := os.MkdirAll(homeClaudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeClaudeDir, "CLAUDE.md"), []byte("@shared.md\nuser context\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeClaudeDir, "shared.md"), []byte("shared user context\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertContextWarningCodes(t *testing.T, warnings []warning.Warning) {
	t.Helper()
	wantCodes := []string{
		warning.WarnContextReadError,
		warning.WarnContextImportNotClaude,
		warning.WarnContextImportMissing,
		warning.WarnContextDedupDropped,
		warning.WarnContextFileTruncated,
	}
	seen := make(map[string]bool)
	for _, item := range warnings {
		seen[item.Code] = true
	}
	for _, code := range wantCodes {
		if !seen[code] {
			t.Fatalf("warning %q missing from %#v", code, warnings)
		}
	}
}

func TestInstructionsStopAtEveryAssemblyCancellationBoundary(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "CLAUDE.md"), []byte("first\n@part.md\nlast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "part.md"), []byte("imported\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}

	succeeded := false
	for limit := 1; limit <= 128; limit++ {
		if assemblyCompletesAtBoundary(t, root, limit) {
			succeeded = true
			break
		}
	}
	if !succeeded {
		t.Fatal("assembly did not complete within the cancellation-boundary limit")
	}
}

func assemblyCompletesAtBoundary(t *testing.T, root *workspace.Root, limit int) bool {
	t.Helper()
	resolver, err := contextresolver.New(contextresolver.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	ctx := newCancelAtBoundaryContext(limit)
	invocationID := fmt.Sprintf("turn-%d", limit)
	text, err := resolver.Instructions(ctx, "session", invocationID)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("assembly cancellation boundary %d error = %v", limit, err)
		}
		return false
	}
	cached, err := resolver.Instructions(t.Context(), "session", invocationID)
	if err != nil || cached != text {
		t.Fatalf("cached instructions = %q, err = %v", cached, err)
	}
	return true
}

func TestNilResolverRejectsRunPolicy(t *testing.T) {
	var resolver *contextresolver.Resolver
	if err := resolver.SetRunPolicy("session", syntax.NewToolPolicy(nil, nil)); err == nil {
		t.Fatal("nil resolver accepted a run policy")
	}
}

func TestResolverPublicSessionLifecycle(t *testing.T) {
	if _, err := contextresolver.New(contextresolver.Options{}); err == nil {
		t.Fatal("resolver without a workspace root was accepted")
	}
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contextresolver.New(contextresolver.Options{Root: root, MaxImportDepth: -1}); err == nil {
		t.Fatal("negative import depth was accepted")
	}

	resolver, err := contextresolver.New(contextresolver.Options{
		Root: root, PromptCommands: config.PromptCommandsOff, DocumentOutputBytes: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertResolverRejectsInvalidSessionRequests(t, resolver)
	denyRead := assertResolverScopeLifecycle(t, resolver)
	assertResolverExpansionAndClose(t, resolver, root, denyRead)
}

func assertResolverRejectsInvalidSessionRequests(t *testing.T, resolver *contextresolver.Resolver) {
	t.Helper()
	if resolver.Closed() {
		t.Fatal("new resolver is closed")
	}
	if err := resolver.StartSession(t.Context(), ""); err == nil {
		t.Fatal("empty session id was accepted")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := resolver.StartSession(canceled, "canceled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start error = %v", err)
	}
	if err := resolver.SetRunPolicy("missing", syntax.NewToolPolicy(nil, nil)); err == nil {
		t.Fatal("policy was attached to a missing session")
	}
	if _, err := resolver.Instructions(t.Context(), "", "invocation"); err == nil {
		t.Fatal("instructions accepted an empty session id")
	}
	if _, err := resolver.Instructions(t.Context(), "session", ""); err == nil {
		t.Fatal("instructions accepted an empty invocation id")
	}
}

func assertResolverScopeLifecycle(t *testing.T, resolver *contextresolver.Resolver) syntax.ToolPolicy {
	t.Helper()
	assertResolverSessionSnapshot(t, resolver)
	denyRead := assertResolverPolicies(t, resolver)
	assertResolverRelease(t, resolver, denyRead)
	return denyRead
}

func assertResolverSessionSnapshot(t *testing.T, resolver *contextresolver.Resolver) {
	t.Helper()
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	if err := resolver.StartSession(t.Context(), "session"); err != nil {
		t.Fatalf("duplicate session start: %v", err)
	}
	if records := resolver.InstructionRecords("session"); len(records) != 0 {
		t.Fatalf("instruction records = %#v", records)
	}
	if records := resolver.InstructionRecords("missing"); records != nil {
		t.Fatalf("missing instruction records = %#v", records)
	}
	text, err := resolver.Instructions(t.Context(), "session", "invocation")
	if err != nil || text != "" {
		t.Fatalf("instructions = %q, err = %v", text, err)
	}
	if resolver.ActiveScopes() != 1 || !resolver.Visible("session", "invocation", "read") || !resolver.Allows("session", "invocation", "read", nil) {
		t.Fatalf("unrestricted scope was not installed")
	}
}

func assertResolverPolicies(t *testing.T, resolver *contextresolver.Resolver) syntax.ToolPolicy {
	t.Helper()
	denyRead := syntax.NewToolPolicy(nil, []syntax.ToolPattern{{Tool: "read"}})
	if err := resolver.IntersectPolicy("session", "invocation", denyRead); err != nil {
		t.Fatal(err)
	}
	if resolver.Visible("session", "invocation", "read") || resolver.Allows("session", "invocation", "read", nil) {
		t.Fatal("intersected read denial was not enforced")
	}
	if err := resolver.SetRunPolicy("session", denyRead); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Instructions(t.Context(), "session", "next"); err != nil {
		t.Fatal(err)
	}
	if resolver.Visible("session", "next", "read") {
		t.Fatal("run policy did not narrow the next scope")
	}
	return denyRead
}

func assertResolverRelease(t *testing.T, resolver *contextresolver.Resolver, denyRead syntax.ToolPolicy) {
	t.Helper()
	resolver.ReleaseRun("session")
	if resolver.ActiveScopes() != 0 || !resolver.Visible("session", "missing", "read") {
		t.Fatal("release did not clear invocation scopes")
	}
	resolver.DropSession("session")
	if err := resolver.SetRunPolicy("session", denyRead); err == nil {
		t.Fatal("dropped session retained its view")
	}
	resolver.DropSession("missing")
}

func assertResolverExpansionAndClose(t *testing.T, resolver *contextresolver.Resolver, root *workspace.Root, denyRead syntax.ToolPolicy) {
	t.Helper()
	expanded, err := resolver.Expand(t.Context(), contextresolver.Expansion{
		Source: "args=$ARGUMENTS project=${PROJECT_DIR} session=${SESSION_ID}", Path: "skill.md",
		Arguments: "alpha beta", SessionID: "session",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(expanded, "args=alpha beta") || !strings.Contains(expanded, "project="+filepath.ToSlash(root.Dir())) {
		t.Fatalf("expanded content = %q", expanded)
	}

	resolver.Close()
	resolver.Close()
	if !resolver.Closed() {
		t.Fatal("closed resolver reports open")
	}
	if err := resolver.StartSession(t.Context(), "after-close"); err == nil {
		t.Fatal("closed resolver started a session")
	}
	if _, err := resolver.Instructions(t.Context(), "after-close", "invocation"); err == nil {
		t.Fatal("closed resolver assembled instructions")
	}
	if _, err := resolver.Expand(t.Context(), contextresolver.Expansion{Source: "body"}); err == nil {
		t.Fatal("closed resolver expanded content")
	}
	if err := resolver.IntersectPolicy("session", "invocation", denyRead); err == nil {
		t.Fatal("closed resolver intersected a policy")
	}
	if records := resolver.InstructionRecords("session"); records != nil {
		t.Fatalf("closed instruction records = %#v", records)
	}

	var nilResolver *contextresolver.Resolver
	nilResolver.DropSession("session")
	nilResolver.Close()
	if !nilResolver.Closed() {
		t.Fatal("nil resolver reports open")
	}
	resolver.ObserveTouch(t.Context(), workspace.Touch{SessionID: "missing", Path: "file.go"})
}

func TestExtensionTrustAndPolicyProjection(t *testing.T) {
	trustCases := []struct {
		provenance extensions.Provenance
		want       contextresolver.TrustLevel
	}{
		{provenance: extensions.Provenance{Scope: "user"}, want: contextresolver.TrustUser},
		{provenance: extensions.Provenance{Scope: "admin"}, want: contextresolver.TrustUser},
		{provenance: extensions.Provenance{Scope: "project", Trusted: true}, want: contextresolver.TrustRepository},
		{provenance: extensions.Provenance{Scope: "project"}, want: contextresolver.TrustUntrusted},
	}
	for _, test := range trustCases {
		if got := contextresolver.ExtensionTrust(test.provenance); got != test.want {
			t.Fatalf("trust for %#v = %v, want %v", test.provenance, got, test.want)
		}
	}
	if contextresolver.TrustUser.String() != "user" || contextresolver.TrustRepository.String() != "repository" || contextresolver.TrustUntrusted.String() != "untrusted" {
		t.Fatal("trust labels changed")
	}

	allowed := []extensions.ToolPattern{{Tool: "read"}}
	denied := []extensions.ToolPattern{{Tool: "bash", Argument: "rm *"}}
	unrestricted := contextresolver.ExtensionPolicy(nil, denied, false)
	if !unrestricted.Allows("read", "file") || unrestricted.Allows("bash", "rm file") || !unrestricted.Allows("write", "file") {
		t.Fatal("unrestricted extension policy projection is wrong")
	}
	restricted := contextresolver.ExtensionPolicy(allowed, denied, true)
	if !restricted.Allows("read", "file") || restricted.Allows("write", "file") {
		t.Fatal("restricted extension policy projection is wrong")
	}
}

func TestExtensionExpansionReportsPublicBoundaryFailures(t *testing.T) {
	root, err := workspace.NewRoot(t.TempDir())
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
	resolver, err := contextresolver.New(contextresolver.Options{
		Root: root, PromptCommands: config.PromptCommandsOn, Executor: executor, WarningSink: sink,
		CommandTimeout: 20 * time.Millisecond, DocumentTimeout: time.Second,
		CommandOutputBytes: 8, DocumentOutputBytes: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)
	if _, err := resolver.Expand(t.Context(), contextresolver.Expansion{Source: "$ARGUMENTS", Arguments: `"unterminated`}); err == nil {
		t.Fatal("malformed arguments were accepted")
	}

	small, err := contextresolver.New(contextresolver.Options{Root: root, DocumentOutputBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(small.Close)
	if _, err := small.Expand(t.Context(), contextresolver.Expansion{Source: "$ARGUMENTS", Arguments: "12345"}); err == nil {
		t.Fatal("substitution amplification exceeded the public document limit")
	}

	for _, source := range []string{
		"!`printf 1234567890`",
		"!`exit 7`",
		"!`sleep 1`",
	} {
		if _, err := resolver.Expand(t.Context(), contextresolver.Expansion{Source: source, Path: "skill.md", Trust: contextresolver.TrustUser}); err != nil {
			t.Fatalf("expand %q: %v", source, err)
		}
	}
	codes := make(map[string]bool)
	for _, notice := range sink.Warnings() {
		codes[notice.Code] = true
	}
	if !codes[warning.WarnSyntaxExecBudgetExhausted] || !codes[warning.WarnSyntaxExecFailed] || !codes[warning.WarnSyntaxExecTimeout] {
		t.Fatalf("command warnings = %#v", sink.Warnings())
	}

	overflow, err := contextresolver.New(contextresolver.Options{
		Root: root, PromptCommands: config.PromptCommandsOn, Executor: executor,
		CommandTimeout: time.Second, DocumentTimeout: time.Second,
		CommandOutputBytes: 64, DocumentOutputBytes: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(overflow.Close)
	if _, err := overflow.Expand(t.Context(), contextresolver.Expansion{
		Source: "1234567890!`printf 12345678901234567890`", Path: "skill.md", Trust: contextresolver.TrustUser,
	}); err == nil {
		t.Fatal("command expansion exceeded the total document limit")
	}
}

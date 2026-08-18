package contextresolver_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

	resolver.ReleaseRun("session")
	if resolver.ActiveScopes() != 0 || !resolver.Visible("session", "missing", "read") {
		t.Fatal("release did not clear invocation scopes")
	}
	resolver.DropSession("session")
	if err := resolver.SetRunPolicy("session", denyRead); err == nil {
		t.Fatal("dropped session retained its view")
	}
	resolver.DropSession("missing")

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

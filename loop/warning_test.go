package loop

import (
	"fmt"
	"sync"
	"testing"
)

func TestWarningSinks(t *testing.T) {
	t.Parallel()
	DiscardSink{}.Warn(Warning{Code: "discarded"})

	var sink SliceSink
	for index := range 3 {
		sink.Warn(Warning{Code: fmt.Sprintf("context.%d", index)})
	}
	first := sink.Warnings()
	first[0].Code = "mutated"
	second := sink.Warnings()
	if len(second) != 3 || second[0].Code != "context.0" || second[2].Code != "context.2" {
		t.Fatalf("warnings = %#v", second)
	}
}

func TestSliceSinkConcurrent(t *testing.T) {
	t.Parallel()
	var sink SliceSink
	var group sync.WaitGroup
	for index := range 100 {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			sink.Warn(Warning{Code: fmt.Sprintf("context.%d", index)})
		}(index)
	}
	group.Wait()
	if got := len(sink.Warnings()); got != 100 {
		t.Fatalf("warnings = %d", got)
	}
}

func TestWarningRegistryIsNamespaced(t *testing.T) {
	t.Parallel()
	codes := map[string]string{
		"context":    WarnContextReadError,
		"foreign":    WarnForeignIndexMissing,
		"syntax":     WarnSyntaxUnknownField,
		"lsp":        WarnLSPUnavailable,
		"config":     WarnConfigUnknownKey,
		"session":    WarnSessionRecordUnknown,
		"compaction": WarnCompactionBudgetExhausted,
		"walk":       WarnWalkInvalidIgnorePattern,
	}
	for namespace, code := range codes {
		if len(code) <= len(namespace) || code[:len(namespace)+1] != namespace+"." {
			t.Errorf("%s code %q is not namespaced", namespace, code)
		}
	}
}

func TestSessionWarningRegistryIncludesPersistenceCodes(t *testing.T) {
	got := []string{
		WarnSessionLogTornTail,
		WarnSessionLogCorruptMiddle,
		WarnSessionRecordUnsupportedVersion,
		WarnSessionRecordUnknown,
	}
	want := []string{
		"session.log.torn_tail",
		"session.log.corrupt_middle",
		"session.record.unsupported_version",
		"session.record.unknown",
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("session warning code %d = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestSyntaxWarningRegistryIncludesParserCodes(t *testing.T) {
	t.Parallel()
	got := []string{WarnSyntaxDuplicateField, WarnSyntaxInvalidFrontmatter}
	want := []string{"syntax.duplicate-field", "syntax.invalid-frontmatter"}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("syntax warning code %d = %q, want %q", index, got[index], want[index])
		}
	}
}

package warning

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
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

func TestSlogSinkWritesStructuredFields(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	value := Warning{
		Code:    WarnWalkInvalidIgnorePattern,
		Source:  "walk",
		Path:    "rules/.gitignore",
		Line:    7,
		Message: "unterminated character class",
	}
	SlogSink{Logger: slog.New(slog.NewJSONHandler(&output, nil))}.Warn(value)

	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"code":    value.Code,
		"source":  value.Source,
		"path":    value.Path,
		"line":    float64(value.Line),
		"message": value.Message,
	}
	for key, expected := range want {
		if !reflect.DeepEqual(got[key], expected) {
			t.Errorf("slog field %q = %#v, want %#v", key, got[key], expected)
		}
	}
	if got["level"] != "WARN" || got["msg"] != "plasmid warning" {
		t.Errorf("slog envelope = %#v", got)
	}
}

func TestSlogSinkUsesDefaultLogger(t *testing.T) {
	var output bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(prior) })

	SlogSink{}.Warn(Warning{Code: "test.default", Source: "test"})
	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["code"] != "test.default" || got["source"] != "test" {
		t.Fatalf("slog fields = %#v", got)
	}
}

func TestRegistry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"WarnContextReadError", WarnContextReadError, "context.read.error"},
		{"WarnContextFileTruncated", WarnContextFileTruncated, "context.file.truncated"},
		{"WarnContextDedupDropped", WarnContextDedupDropped, "context.dedup.dropped"},
		{"WarnContextBudgetDropped", WarnContextBudgetDropped, "context.budget.dropped"},
		{"WarnContextImportMissing", WarnContextImportMissing, "context.import.missing"},
		{"WarnContextImportCycle", WarnContextImportCycle, "context.import.cycle"},
		{"WarnContextImportDepth", WarnContextImportDepth, "context.import.depth"},
		{"WarnContextImportEscape", WarnContextImportEscape, "context.import.escape"},
		{"WarnContextImportNotClaude", WarnContextImportNotClaude, "context.import.notclaude"},
		{"WarnContextFrontmatterUnsupported", WarnContextFrontmatterUnsupported, "context.frontmatter.unsupported"},
		{"WarnContextGlobInvalid", WarnContextGlobInvalid, "context.glob.invalid"},
		{"WarnContextGlobUnsupported", WarnContextGlobUnsupported, "context.glob.unsupported"},
		{"WarnContextTouchOverflow", WarnContextTouchOverflow, "context.touch.overflow"},
		{"WarnCodingtoolsBashOmitted", WarnCodingtoolsBashOmitted, "codingtools.bash.omitted"},
		{"WarnForeignIndexMissing", WarnForeignIndexMissing, "foreign.index.missing"},
		{"WarnForeignIndexUnreadable", WarnForeignIndexUnreadable, "foreign.index.unreadable"},
		{"WarnForeignIndexUnsupportedVersion", WarnForeignIndexUnsupportedVersion, "foreign.index.unsupported_version"},
		{"WarnForeignEntryShapeUnknown", WarnForeignEntryShapeUnknown, "foreign.entry.shape_unknown"},
		{"WarnForeignInstallPathMissing", WarnForeignInstallPathMissing, "foreign.install_path.missing"},
		{"WarnForeignInstallPathRelative", WarnForeignInstallPathRelative, "foreign.install_path.relative"},
		{"WarnForeignManifestMissing", WarnForeignManifestMissing, "foreign.manifest.missing"},
		{"WarnForeignManifestInvalid", WarnForeignManifestInvalid, "foreign.manifest.invalid"},
		{"WarnForeignPathEscape", WarnForeignPathEscape, "foreign.path.escape"},
		{"WarnForeignSkillMissingMarkdown", WarnForeignSkillMissingMarkdown, "foreign.skill.missing_markdown"},
		{"WarnForeignDuplicateSkill", WarnForeignDuplicateSkill, "foreign.duplicate.skill"},
		{"WarnForeignDuplicateTemplate", WarnForeignDuplicateTemplate, "foreign.duplicate.template"},
		{"WarnForeignDuplicateMCPServer", WarnForeignDuplicateMCPServer, "foreign.duplicate.mcp_server"},
		{"WarnForeignMCPShapeUnknown", WarnForeignMCPShapeUnknown, "foreign.mcp.shape_unknown"},
		{"WarnForeignMCPInert", WarnForeignMCPInert, "foreign.mcp.inert"},
		{"WarnForeignTOMLUnsupported", WarnForeignTOMLUnsupported, "foreign.toml.unsupported"},
		{"WarnForeignScanPanic", WarnForeignScanPanic, "foreign.scan.panic"},
		{"WarnForeignScanTruncated", WarnForeignScanTruncated, "foreign.scan.truncated"},
		{"WarnForeignEcosystemDisabled", WarnForeignEcosystemDisabled, "foreign.ecosystem.disabled"},
		{"WarnSyntaxUnsupportedField", WarnSyntaxUnsupportedField, "syntax.unsupported-field"},
		{"WarnSyntaxUnknownField", WarnSyntaxUnknownField, "syntax.unknown-field"},
		{"WarnSyntaxInvalidField", WarnSyntaxInvalidField, "syntax.invalid-field"},
		{"WarnSyntaxDuplicateField", WarnSyntaxDuplicateField, "syntax.duplicate-field"},
		{"WarnSyntaxInvalidFrontmatter", WarnSyntaxInvalidFrontmatter, "syntax.invalid-frontmatter"},
		{"WarnSyntaxDuplicateArgument", WarnSyntaxDuplicateArgument, "syntax.duplicate-argument"},
		{"WarnSyntaxShellUnsupported", WarnSyntaxShellUnsupported, "syntax.shell-unsupported"},
		{"WarnSyntaxDocumentNotInvocable", WarnSyntaxDocumentNotInvocable, "syntax.document-not-invocable"},
		{"WarnSyntaxInvalidGlob", WarnSyntaxInvalidGlob, "syntax.invalid-glob"},
		{"WarnSyntaxUnresolvedVariable", WarnSyntaxUnresolvedVariable, "syntax.unresolved-variable"},
		{"WarnSyntaxMissingArgument", WarnSyntaxMissingArgument, "syntax.missing-argument"},
		{"WarnSyntaxExecDisabled", WarnSyntaxExecDisabled, "syntax.exec-disabled"},
		{"WarnSyntaxExecBudgetExhausted", WarnSyntaxExecBudgetExhausted, "syntax.exec-budget-exhausted"},
		{"WarnSyntaxExecFailed", WarnSyntaxExecFailed, "syntax.exec-failed"},
		{"WarnSyntaxExecTimeout", WarnSyntaxExecTimeout, "syntax.exec-timeout"},
		{"WarnSyntaxInvalidToolPattern", WarnSyntaxInvalidToolPattern, "syntax.invalid-tool-pattern"},
		{"WarnLSPConfigUnknownKey", WarnLSPConfigUnknownKey, "lsp.config.unknown_key"},
		{"WarnLSPConfigInvalidServer", WarnLSPConfigInvalidServer, "lsp.config.invalid_server"},
		{"WarnLSPConfigDuplicateServer", WarnLSPConfigDuplicateServer, "lsp.config.duplicate_server"},
		{"WarnLSPUnavailable", WarnLSPUnavailable, "lsp.server.unavailable"},
		{"WarnLSPStartFailed", WarnLSPStartFailed, "lsp.server.start_failed"},
		{"WarnLSPInitializeFailed", WarnLSPInitializeFailed, "lsp.server.initialize_failed"},
		{"WarnLSPRequestFailed", WarnLSPRequestFailed, "lsp.request.failed"},
		{"WarnLSPDiagnosticsUnsettled", WarnLSPDiagnosticsUnsettled, "lsp.diagnostics.unsettled"},
		{"WarnConfigUnknownKey", WarnConfigUnknownKey, "config.unknown_key"},
		{"WarnConfigPathMissing", WarnConfigPathMissing, "config.path_missing"},
		{"WarnConfigBadEnum", WarnConfigBadEnum, "config.bad_enum"},
		{"WarnConfigMCPIncomplete", WarnConfigMCPIncomplete, "config.mcp_incomplete"},
		{"WarnConfigAllowlistUnmatched", WarnConfigAllowlistUnmatched, "config.allowlist_unmatched"},
		{"WarnConfigVersionDefaulted", WarnConfigVersionDefaulted, "config.version_defaulted"},
		{"WarnSessionIDEmpty", WarnSessionIDEmpty, "session.id.empty"},
		{"WarnSessionIDTooLong", WarnSessionIDTooLong, "session.id.too_long"},
		{"WarnSessionLogCorrupt", WarnSessionLogCorrupt, "session.log.corrupt"},
		{"WarnSessionLogTornTail", WarnSessionLogTornTail, "session.log.torn_tail"},
		{"WarnSessionLogCorruptMiddle", WarnSessionLogCorruptMiddle, "session.log.corrupt_middle"},
		{"WarnSessionRecordUnsupportedVersion", WarnSessionRecordUnsupportedVersion, "session.record.unsupported_version"},
		{"WarnSessionRecordUnknown", WarnSessionRecordUnknown, "session.record.unknown"},
		{"WarnSessionSnapshotRefresh", WarnSessionSnapshotRefresh, "session.snapshot.refresh"},
		{"WarnSessionDurabilityRetry", WarnSessionDurabilityRetry, "session.durability.retry"},
		{"WarnSessionLegacyStateLoss", WarnSessionLegacyStateLoss, "session.state.legacy_incomplete"},
		{"WarnSessionOrderDuplicate", WarnSessionOrderDuplicate, "session.order.duplicate"},
		{"WarnCompactionBudgetExhausted", WarnCompactionBudgetExhausted, "compaction.budget.exhausted"},
		{"WarnCompactionSidecarLoad", WarnCompactionSidecarLoad, "compaction.sidecar.load"},
		{"WarnCompactionSidecarSave", WarnCompactionSidecarSave, "compaction.sidecar.save"},
		{"WarnADKEventMalformed", WarnADKEventMalformed, "adk.event.malformed"},
		{"WarnADKEventUnknownPart", WarnADKEventUnknownPart, "adk.event.unknown_part"},
		{"WarnWalkInvalidIgnorePattern", WarnWalkInvalidIgnorePattern, "walk.invalid_ignore_pattern"},
		{"WarnWalkUnreadableIgnore", WarnWalkUnreadableIgnore, "walk.unreadable_ignore"},
	}

	seen := make(map[string]string, len(tests))
	for _, test := range tests {
		if test.got != test.want {
			t.Errorf("%s = %q, want %q", test.name, test.got, test.want)
		}
		if previous, exists := seen[test.got]; exists {
			t.Errorf("%s and %s both use %q", previous, test.name, test.got)
		}
		seen[test.got] = test.name
	}
}

func TestWarningJSONShapeAndRendering(t *testing.T) {
	t.Parallel()
	value := Warning{
		Code:    WarnSyntaxUnknownField,
		Source:  "syntax",
		Path:    "skills/example/SKILL.md",
		Line:    7,
		Message: "field model is ignored",
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"code":    "syntax.unknown-field",
		"source":  "syntax",
		"path":    "skills/example/SKILL.md",
		"line":    float64(7),
		"message": "field model is ignored",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON = %#v, want %#v", got, want)
	}
	if got := value.String(); got != "skills/example/SKILL.md:7: syntax.unknown-field: field model is ignored" {
		t.Fatalf("String() = %q", got)
	}
}

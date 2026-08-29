package outputlimit

import (
	"math"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
)

func init() {
	fixture.RegisterRunner("outputlimit", "outputlimit/all", "apply", "logical_lines", "markers")
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

func TestDefaults(t *testing.T) {
	want := Policy{MaxBytes: 30000, MaxLines: 2000, MaxLineBytes: 2000, HeadFraction: 0.6}
	if got := Defaults(); got != want {
		t.Fatalf("Defaults() = %#v, want %#v", got, want)
	}
}

func TestMarker(t *testing.T) {
	for _, reason := range []string{ReasonBudget, ReasonBytes, ReasonFile, ReasonLineLength, ReasonLines} {
		got := Marker(reason, 4, 10, 2, 5)
		want := "[plasmid:truncated reason=" + reason + " bytes=4/10 lines=2/5]"
		if got != want || strings.TrimSpace(got) != got || strings.ContainsAny(got, "\r\n") || !isASCII(got) {
			t.Fatalf("Marker(%q) = %q", reason, got)
		}
	}
	if got := Marker("invalid", 1, 2, 3, 4); got != "" {
		t.Fatalf("invalid marker = %q", got)
	}
}

func TestPolicyApply(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		policy Policy
		output string
		report Report
	}{
		{
			name:   "empty",
			policy: Policy{MaxBytes: 1, MaxLines: 1, MaxLineBytes: 1},
			report: Report{},
		},
		{
			name:   "zero value",
			input:  "a\nb",
			output: "a\nb",
			report: Report{OriginalBytes: 3, OriginalLines: 2, KeptBytes: 3, KeptLines: 2},
		},
		{
			name:   "exact byte and line ceilings",
			input:  "a\nb",
			policy: Policy{MaxBytes: 3, MaxLines: 2, MaxLineBytes: 1},
			output: "a\nb",
			report: Report{OriginalBytes: 3, OriginalLines: 2, KeptBytes: 3, KeptLines: 2},
		},
		{
			name:   "line length",
			input:  "abcdef\n",
			policy: Policy{MaxLineBytes: 4, HeadFraction: 0.5},
			output: "ab[plasmid:truncated reason=line_length bytes=4/6 lines=1/1]ef\n",
			report: Report{Truncated: true, Reason: ReasonLineLength, OriginalBytes: 7, OriginalLines: 1, KeptBytes: 5, KeptLines: 1},
		},
		{
			name:   "line ceiling",
			input:  "a\nb\nc",
			policy: Policy{MaxLines: 2, HeadFraction: 1},
			output: "a\nb\n[plasmid:truncated reason=lines bytes=4/5 lines=2/3]",
			report: Report{Truncated: true, Reason: ReasonLines, OriginalBytes: 5, OriginalLines: 3, KeptBytes: 4, KeptLines: 2},
		},
		{
			name:   "byte fragments",
			input:  "abcdef",
			policy: Policy{MaxBytes: 4, HeadFraction: 0.5},
			output: "ab\n[plasmid:truncated reason=bytes bytes=4/6 lines=1/1]\nef",
			report: Report{Truncated: true, Reason: ReasonBytes, OriginalBytes: 6, OriginalLines: 1, KeptBytes: 4, KeptLines: 1},
		},
		{
			name:   "combined ceilings retain boundary fragments",
			input:  "abcd\nefgh\nijkl",
			policy: Policy{MaxBytes: 7, MaxLines: 2, HeadFraction: 0.5},
			output: "abc\n[plasmid:truncated reason=bytes bytes=7/14 lines=2/3]\nijkl",
			report: Report{Truncated: true, Reason: ReasonBytes, OriginalBytes: 14, OriginalLines: 3, KeptBytes: 7, KeptLines: 2},
		},
		{
			name:   "combined ceilings redistribute stranded bytes",
			input:  "abc\nuvwxyz",
			policy: Policy{MaxBytes: 5, MaxLines: 1, HeadFraction: 0.5},
			output: "[plasmid:truncated reason=bytes bytes=5/10 lines=1/2]\nvwxyz",
			report: Report{Truncated: true, Reason: ReasonBytes, OriginalBytes: 10, OriginalLines: 2, KeptBytes: 5, KeptLines: 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, report := test.policy.Apply(test.input)
			if got != test.output || report != test.report {
				t.Fatalf("Apply() = %q, %#v; want %q, %#v", got, report, test.output, test.report)
			}
		})
	}
}

func TestLineLengthElisionPrecedesAggregateSelection(t *testing.T) {
	policy := Policy{MaxBytes: 6, MaxLines: 2, MaxLineBytes: 4, HeadFraction: 0.5}
	got, report := policy.Apply("abcdef\nuvwxyz")
	if report != (Report{Truncated: true, Reason: ReasonBytes, OriginalBytes: 13, OriginalLines: 2, KeptBytes: 6, KeptLines: 2}) {
		t.Fatal(report)
	}
	if strings.Count(got, "reason=line_length") != 2 || strings.Count(got, "reason=bytes") != 1 {
		t.Fatalf("markers lost after aggregate selection: %q", got)
	}
}

func TestReasonPrecedenceAndSafeBoundaries(t *testing.T) {
	input := "a🙂b\r\n終わり\r\nlast"
	got, report := (Policy{MaxBytes: 9, MaxLines: 1, MaxLineBytes: 6, HeadFraction: 0.5}).Apply(input)
	if report.Reason != ReasonBytes {
		t.Fatalf("reason = %q", report.Reason)
	}
	if !utf8.ValidString(got) || strings.Contains(got, "\r[plasmid:") || strings.Contains(got, "[plasmid:\n") {
		t.Fatalf("unsafe result %q", got)
	}
}

func TestLogicalLineAccounting(t *testing.T) {
	for _, test := range []struct {
		input string
		lines int
	}{
		{"", 0},
		{"\n", 1},
		{"\r\n", 1},
		{"a\n", 1},
		{"a\n\n", 2},
		{"a\nb", 2},
	} {
		_, report := (Policy{}).Apply(test.input)
		if report.OriginalLines != test.lines || report.KeptLines != test.lines {
			t.Fatalf("Apply(%q) lines = %d/%d", test.input, report.KeptLines, report.OriginalLines)
		}
	}
}

func TestApplyLinesMatchesApply(t *testing.T) {
	policy := Policy{MaxBytes: 5, MaxLines: 2, MaxLineBytes: 3, HeadFraction: 0.6}
	for _, lines := range [][]string{nil, {"a"}, {"a", "b"}, {"abcdef", "xyz"}} {
		input := strings.Join(lines, "\n")
		want, wantReport := policy.Apply(input)
		got, gotReport := policy.ApplyLines(lines, 99)
		if got != want || gotReport != wantReport {
			t.Fatalf("ApplyLines(%q) = %q, %#v; want %q, %#v", lines, got, gotReport, want, wantReport)
		}
	}
}

func TestHeadFractionNormalization(t *testing.T) {
	for _, fraction := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0, 1, 2} {
		got, report := (Policy{MaxBytes: 4, HeadFraction: fraction}).Apply("abcdef")
		if !report.Truncated || !utf8.ValidString(got) {
			t.Fatalf("fraction %v produced %q, %#v", fraction, got, report)
		}
	}
}

func TestLimitSplitDoesNotOverflow(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, test := range []struct {
		fraction float64
		head     int
		tail     int
	}{
		{fraction: 0, tail: maximum},
		{fraction: 1, head: maximum},
	} {
		head, tail := limitSplit(maximum, test.fraction)
		if head != test.head || tail != test.tail {
			t.Fatalf("limitSplit(max, %v) = %d, %d; want %d, %d", test.fraction, head, tail, test.head, test.tail)
		}
	}
}

func TestOutputlimitFixtureCoverage(t *testing.T) {
	fixture.AssertCoverage(t, "outputlimit")
}

func TestOutputlimitFixtures(t *testing.T) {
	fixture.Walk(t, "outputlimit", "outputlimit/all", func(t *testing.T, testCase fixture.Case) {
		var metadata struct {
			Area string `json:"area"`
			ID   string `json:"id"`
			Kind string `json:"kind"`
		}
		testCase.Decode(t, "case.json", &metadata)
		if metadata.Area != "outputlimit" || metadata.ID != testCase.ID {
			t.Fatalf("invalid fixture metadata in %s: %#v", testCase.ID, metadata)
		}
		switch metadata.Kind {
		case "apply":
			runApplyFixture(t, testCase)
		case "logical_lines":
			runLogicalLinesFixture(t, testCase)
		case "markers":
			runMarkersFixture(t, testCase)
		default:
			t.Fatalf("%s: unknown kind %q", testCase.ID, metadata.Kind)
		}
	})
}

func runApplyFixture(t *testing.T, testCase fixture.Case) {
	var input struct {
		Input  string `json:"input"`
		Policy Policy `json:"policy"`
	}
	var actual struct {
		Output string `json:"output"`
		Report Report `json:"report"`
	}
	testCase.Decode(t, "input.json", &input)
	actual.Output, actual.Report = input.Policy.Apply(input.Input)
	testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runLogicalLinesFixture(t *testing.T, testCase fixture.Case) {
	var input struct {
		Inputs []string `json:"inputs"`
	}
	var actual struct {
		OriginalLines []int `json:"original_lines"`
	}
	testCase.Decode(t, "input.json", &input)
	actual.OriginalLines = make([]int, len(input.Inputs))
	for index, value := range input.Inputs {
		_, report := (Policy{}).Apply(value)
		actual.OriginalLines[index] = report.OriginalLines
	}
	testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runMarkersFixture(t *testing.T, testCase fixture.Case) {
	var input struct {
		KeptBytes int      `json:"kept_bytes"`
		KeptLines int      `json:"kept_lines"`
		OrigBytes int      `json:"orig_bytes"`
		OrigLines int      `json:"orig_lines"`
		Reasons   []string `json:"reasons"`
	}
	var actual struct {
		Markers map[string]string `json:"markers"`
	}
	testCase.Decode(t, "input.json", &input)
	actual.Markers = make(map[string]string, len(input.Reasons))
	for _, reason := range input.Reasons {
		actual.Markers[reason] = Marker(reason, input.KeptBytes, input.OrigBytes, input.KeptLines, input.OrigLines)
	}
	testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
}

func isASCII(value string) bool {
	for _, char := range []byte(value) {
		if char > 0x7f {
			return false
		}
	}
	return true
}

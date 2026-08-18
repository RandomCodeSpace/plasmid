package textmatch

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type applyCase struct {
	name         string
	request      Request
	wantContent  string
	wantTier     Tier
	wantCount    int
	wantRanges   []Range
	wantEnding   string
	wantBOM      bool
	wantError    error
	wantAmbLines []int
}

func TestApply(t *testing.T) {
	tests := []applyCase{
		{
			name:        "exact at byte zero",
			request:     Request{Content: "old\nkeep\n", Old: "old", New: "new"},
			wantContent: "new\nkeep\n", wantTier: TierExact, wantCount: 1,
			wantRanges: []Range{{Start: 0, End: 3}}, wantEnding: "\n",
		},
		{
			name:        "exact at eof without trailing newline",
			request:     Request{Content: "keep\nold", Old: "old", New: "new"},
			wantContent: "keep\nnew", wantTier: TierExact, wantCount: 1,
			wantRanges: []Range{{Start: 5, End: 8}}, wantEnding: "\n",
		},
		{
			name:        "trailing whitespace",
			request:     Request{Content: "keep\nvalue  \n", Old: "value\t", New: "changed"},
			wantContent: "keep\nchanged\n", wantTier: TierTrailingWhitespace, wantCount: 1,
			wantRanges: []Range{{Start: 5, End: 12}}, wantEnding: "\n",
		},
		{
			name:        "uniform indentation",
			request:     Request{Content: "    one\n    two\n", Old: "one\ntwo", New: "three\nfour"},
			wantContent: "    three\n    four\n", wantTier: TierIndentation, wantCount: 1,
			wantRanges: []Range{{Start: 0, End: 15}}, wantEnding: "\n",
		},
		{
			name:        "exact tier precedence",
			request:     Request{Content: "one\ntwo\n    one\n    two\n", Old: "one\ntwo", New: "three\nfour"},
			wantContent: "three\nfour\n    one\n    two\n", wantTier: TierExact, wantCount: 1,
			wantRanges: []Range{{Start: 0, End: 7}}, wantEnding: "\n",
		},
		{
			name:         "ambiguous",
			request:      Request{Content: "old\nkeep\nold\n", Old: "old", New: "new"},
			wantError:    ErrAmbiguousMatch,
			wantAmbLines: []int{1, 3},
		},
		{
			name:        "replace all scans original",
			request:     Request{Content: "old old", Old: "old", New: "old-old", ReplaceAll: true},
			wantContent: "old-old old-old", wantTier: TierExact, wantCount: 2,
			wantRanges: []Range{{Start: 0, End: 3}, {Start: 4, End: 7}}, wantEnding: "\n",
		},
		{
			name:        "deletion preserves final newline",
			request:     Request{Content: "keep\nold\n", Old: "old\n", New: ""},
			wantContent: "keep\n", wantTier: TierExact, wantCount: 1,
			wantRanges: []Range{{Start: 5, End: 9}}, wantEnding: "\n",
		},
		{
			name:        "new text cannot add trailing newline",
			request:     Request{Content: "old", Old: "old", New: "new\n"},
			wantContent: "new", wantTier: TierExact, wantCount: 1,
			wantRanges: []Range{{Start: 0, End: 3}}, wantEnding: "\n",
		},
		{
			name:        "crlf and bom",
			request:     Request{Content: "\uFEFFone\r\ntwo\r\n", Old: "two", New: "three"},
			wantContent: "\uFEFFone\r\nthree\r\n", wantTier: TierExact, wantCount: 1,
			wantRanges: []Range{{Start: 4, End: 7}}, wantEnding: "\r\n", wantBOM: true,
		},
		{
			name:        "dominant crlf normalizes mixed endings",
			request:     Request{Content: "one\r\nold\r\nthree\n", Old: "old", New: "new"},
			wantContent: "one\r\nnew\r\nthree\r\n", wantTier: TierExact, wantCount: 1,
			wantRanges: []Range{{Start: 4, End: 7}}, wantEnding: "\r\n",
		},
		{
			name:        "line ending tie selects lf",
			request:     Request{Content: "one\r\nold\n", Old: "old", New: "new"},
			wantContent: "one\nnew\n", wantTier: TierExact, wantCount: 1,
			wantRanges: []Range{{Start: 4, End: 7}}, wantEnding: "\n",
		},
		{
			name:        "multiple trailing newlines remain",
			request:     Request{Content: "old\n\n", Old: "old", New: "new"},
			wantContent: "new\n\n", wantTier: TierExact, wantCount: 1,
			wantRanges: []Range{{Start: 0, End: 3}}, wantEnding: "\n",
		},
		{
			name:        "combining characters use byte ranges",
			request:     Request{Content: "a\u0301 old\n", Old: "old", New: "new"},
			wantContent: "a\u0301 new\n", wantTier: TierExact, wantCount: 1,
			wantRanges: []Range{{Start: 4, End: 7}}, wantEnding: "\n",
		},
		{name: "empty old", request: Request{Content: "x", New: "y"}, wantError: ErrEmptyOld},
		{name: "bom-only old", request: Request{Content: "x", Old: "\uFEFF", New: "y"}, wantError: ErrEmptyOld},
		{name: "no-op", request: Request{Content: "x", Old: "x", New: "x"}, wantError: ErrNoOpEdit},
		{name: "normalized no-op", request: Request{Content: "x\n", Old: "x\r\n", New: "x\n"}, wantError: ErrNoOpEdit},
		{name: "no match", request: Request{Content: "x", Old: "y", New: "z"}, wantError: ErrNoMatch},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertApplyCase(t, test)
		})
	}
}

func assertApplyCase(t *testing.T, test applyCase) {
	t.Helper()
	got, err := Apply(test.request)
	if test.wantError == nil {
		if err != nil {
			t.Fatal(err)
		}
		if got.Content != test.wantContent || got.Tier != test.wantTier || got.Replacements != test.wantCount || got.LineEnding != test.wantEnding || got.HadBOM != test.wantBOM || !reflect.DeepEqual(got.Ranges, test.wantRanges) {
			t.Fatalf("Apply() = %#v, want content %q tier %v count %d ranges %#v ending %q BOM %t", got, test.wantContent, test.wantTier, test.wantCount, test.wantRanges, test.wantEnding, test.wantBOM)
		}
		return
	}
	if !errors.Is(err, test.wantError) {
		t.Fatalf("error = %v, want errors.Is(_, %v)", err, test.wantError)
	}
	if test.wantAmbLines != nil {
		assertAmbiguityLines(t, err, test.wantAmbLines)
	}
}

func assertAmbiguityLines(t *testing.T, err error, want []int) {
	t.Helper()
	var ambiguity *AmbiguityError
	if !errors.As(err, &ambiguity) {
		t.Fatalf("error type = %T, want *AmbiguityError", err)
	}
	if ambiguity.Count != len(want) || !reflect.DeepEqual(ambiguity.Lines, want) {
		t.Fatalf("ambiguity = %#v, want lines %v", ambiguity, want)
	}
}

func TestAmbiguityErrorLimitsRenderedLines(t *testing.T) {
	err := (&AmbiguityError{Count: 7, Lines: []int{1, 2, 3, 4, 5, 6, 7}}).Error()
	if !strings.Contains(err, "7 locations") || !strings.Contains(err, "[1 2 3 4 5]") || strings.Contains(err, "6 7") || !strings.Contains(err, "replace_all") {
		t.Fatalf("unexpected error: %q", err)
	}
}

func TestTierString(t *testing.T) {
	for tier, want := range map[Tier]string{
		TierExact: "exact", TierTrailingWhitespace: "whitespace", TierIndentation: "indentation", Tier(99): "unknown",
	} {
		if got := tier.String(); got != want {
			t.Errorf("Tier(%d).String() = %q, want %q", tier, got, want)
		}
	}
}

func TestApplyIndentationReplaceAll(t *testing.T) {
	result, err := Apply(Request{
		Content: "  one\n  two\nkeep\n\tone\n\ttwo\n",
		Old:     "one\ntwo", New: "three\nfour", ReplaceAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = "  three\n  four\nkeep\n\tthree\n\tfour\n"
	if result.Content != want || result.Replacements != 2 || result.Tier != TierIndentation {
		t.Fatalf("Apply() = %#v, want content %q with two indentation matches", result, want)
	}
}

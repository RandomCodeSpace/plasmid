package textmatch_test

import (
	"errors"
	"testing"

	"github.com/plasmid-dev/plasmid/codingtools/internal/textmatch"
)

func TestApplyLineMatchingBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request textmatch.Request
		want    string
		wantErr error
	}{
		{
			name: "terminated pattern does not match unterminated content",
			request: textmatch.Request{
				Content: "value", Old: "value\n", New: "changed",
			},
			wantErr: textmatch.ErrNoMatch,
		},
		{
			name: "inconsistent indentation does not match",
			request: textmatch.Request{
				Content: "  one\n\ttwo\n", Old: "one\ntwo", New: "changed",
			},
			wantErr: textmatch.ErrNoMatch,
		},
		{
			name: "deletion preserves source newline",
			request: textmatch.Request{
				Content: "old\n", Old: "old\n", New: "",
			},
			want: "\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, err := textmatch.Apply(test.request)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("Apply() error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Content != test.want {
				t.Fatalf("Apply() content = %q, want %q", result.Content, test.want)
			}
		})
	}
}

func TestUnifiedDiffExactReportsUnterminatedEqualContext(t *testing.T) {
	t.Parallel()
	got := textmatch.UnifiedDiffExact("same\nold", "same\nnew", "file.txt", 1)
	const want = "--- a/file.txt\n+++ b/file.txt\n@@ -1,2 +1,2 @@\n same\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n"
	if got != want {
		t.Fatalf("UnifiedDiffExact() = %q, want %q", got, want)
	}
}

package textmatch

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestUnifiedDiff(t *testing.T) {
	tests := []struct {
		name       string
		oldText    string
		newText    string
		context    int
		want       string
		wantHunks  int
		wantAbsent string
	}{
		{
			name: "replacement", oldText: "one\ntwo\nthree\n", newText: "one\nchanged\nthree\n",
			want: "--- a/file.txt\n+++ b/file.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+changed\n three\n",
		},
		{
			name: "empty to content", newText: "one\n",
			want: "--- a/file.txt\n+++ b/file.txt\n@@ -0,0 +1,1 @@\n+one\n",
		},
		{
			name: "content to empty", oldText: "one\n",
			want: "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +0,0 @@\n-one\n",
		},
		{
			name: "old missing newline", oldText: "one", newText: "two\n",
			want: "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +1,1 @@\n-one\n\\ No newline at end of file\n+two\n",
		},
		{
			name: "new missing newline", oldText: "one\n", newText: "two",
			want: "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +1,1 @@\n-one\n+two\n\\ No newline at end of file\n",
		},
		{
			name: "both missing newline", oldText: "one", newText: "two",
			want: "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +1,1 @@\n-one\n\\ No newline at end of file\n+two\n\\ No newline at end of file\n",
		},
		{
			name: "newline only", oldText: "one", newText: "one\n",
			want: "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +1,1 @@\n-one\n\\ No newline at end of file\n+one\n",
		},
		{
			name:    "distant hunks",
			oldText: numberedLines("old", 12), newText: replaceNumberedLines(numberedLines("old", 12), map[int]string{1: "first", 12: "last"}),
			wantHunks: 2,
		},
		{
			name:    "negative context is explicit zero",
			oldText: "one\ntwo\nthree\n", newText: "one\nchanged\nthree\n", context: -1,
			want: "--- a/file.txt\n+++ b/file.txt\n@@ -2,1 +2,1 @@\n-two\n+changed\n",
		},
		{
			name: "crlf and bom normalized", oldText: "\uFEFFone\r\ntwo\r\n", newText: "one\ntwo\n",
			want: "",
		},
		{name: "empty unchanged", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := UnifiedDiff(test.oldText, test.newText, "file.txt", test.context)
			if test.wantHunks != 0 {
				if count := strings.Count(got, "@@"); count != test.wantHunks*2 {
					t.Fatalf("hunk delimiters = %d, want %d; diff:\n%s", count, test.wantHunks*2, got)
				}
			} else if got != test.want {
				t.Fatalf("diff mismatch\ngot:\n%q\nwant:\n%q", got, test.want)
			}
		})
	}
}

func TestUnifiedDiffExactPreservesBOMAndLineEndings(t *testing.T) {
	got := UnifiedDiffExact("\uFEFFone\r\n", "one\n", "file.txt", 3)
	want := "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +1,1 @@\n-\uFEFFone\r\n+one\n"
	if got != want {
		t.Fatalf("exact diff mismatch\ngot:  %q\nwant: %q", got, want)
	}
}

func TestUnifiedDiffDefaultContextAndGrouping(t *testing.T) {
	oldText := numberedLines("line", 14)
	newText := replaceNumberedLines(oldText, map[int]string{4: "change-a", 11: "change-b"})
	gotDefault := UnifiedDiff(oldText, newText, "file", 0)
	gotThree := UnifiedDiff(oldText, newText, "file", 3)
	if gotDefault != gotThree {
		t.Fatal("zero context did not select the default of three")
	}
	if strings.Count(gotDefault, "@@") != 2 {
		t.Fatalf("changes separated by six unchanged lines should merge; diff:\n%s", gotDefault)
	}
	newText = replaceNumberedLines(oldText, map[int]string{3: "change-a", 11: "change-b"})
	if got := UnifiedDiff(oldText, newText, "file", 3); strings.Count(got, "@@") != 4 {
		t.Fatalf("changes separated by seven unchanged lines should split; diff:\n%s", got)
	}
}

func TestUnifiedDiffRepeatedLinesIsDeterministic(t *testing.T) {
	oldText := strings.Repeat("same\n", 100) + "old\n" + strings.Repeat("same\n", 100)
	newText := strings.Repeat("same\n", 100) + "new\n" + strings.Repeat("same\n", 100)
	want := UnifiedDiff(oldText, newText, "repeat", 3)
	for i := 0; i < 20; i++ {
		if got := UnifiedDiff(oldText, newText, "repeat", 3); got != want {
			t.Fatalf("run %d was nondeterministic", i)
		}
	}
}

func TestUnifiedDiffLargeInputsFinishPromptly(t *testing.T) {
	tests := []struct {
		name    string
		oldText string
		newText string
	}{
		{
			name:    "equal",
			oldText: strings.Repeat("equal-line\n", 20000),
			newText: strings.Repeat("equal-line\n", 20000),
		},
		{
			name:    "large insertion",
			oldText: "prefix\nsuffix\n",
			newText: "prefix\n" + strings.Repeat("inserted\n", 10000) + "suffix\n",
		},
		{
			name:    "large deletion",
			oldText: "prefix\n" + strings.Repeat("deleted\n", 10000) + "suffix\n",
			newText: "prefix\nsuffix\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			done := make(chan string, 1)
			go func() { done <- UnifiedDiff(test.oldText, test.newText, "large", 3) }()
			select {
			case first := <-done:
				if second := UnifiedDiff(test.oldText, test.newText, "large", 3); second != first {
					t.Fatal("large-input diff was nondeterministic")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("large-input diff did not finish promptly")
			}
		})
	}
}

func TestUnifiedDiffEdgeInsertionsAndDeletions(t *testing.T) {
	tests := []struct {
		name string
		old  string
		new  string
		want string
	}{
		{
			name: "insert at eof", old: "one\n", new: "one\ntwo\n",
			want: "--- a/edge\n+++ b/edge\n@@ -1,1 +1,2 @@\n one\n+two\n",
		},
		{
			name: "delete at eof", old: "one\ntwo\n", new: "one\n",
			want: "--- a/edge\n+++ b/edge\n@@ -1,2 +1,1 @@\n one\n-two\n",
		},
		{
			name: "insert at line one", old: "two\n", new: "one\ntwo\n",
			want: "--- a/edge\n+++ b/edge\n@@ -1,1 +1,2 @@\n+one\n two\n",
		},
		{
			name: "delete at line one", old: "one\ntwo\n", new: "two\n",
			want: "--- a/edge\n+++ b/edge\n@@ -1,2 +1,1 @@\n-one\n two\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := UnifiedDiff(test.old, test.new, "edge", 0); got != test.want {
				t.Fatalf("diff mismatch\ngot:  %q\nwant: %q", got, test.want)
			}
		})
	}
}

func TestUnifiedDiffWorkCeilingFallback(t *testing.T) {
	oldLines := make([]string, 500)
	newLines := make([]string, 500)
	for i := range oldLines {
		oldLines[i] = fmt.Sprintf("old-%03d", i)
		newLines[i] = fmt.Sprintf("new-%03d", i)
	}
	oldText := strings.Join(oldLines, "\n") + "\n"
	newText := strings.Join(newLines, "\n") + "\n"

	done := make(chan string, 1)
	go func() { done <- UnifiedDiff(oldText, newText, "large", 3) }()
	var got string
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("diff exceeded the work ceiling timeout")
	}
	var want strings.Builder
	want.WriteString("--- a/large\n+++ b/large\n@@ -1,500 +1,500 @@\n")
	for _, line := range oldLines {
		fmt.Fprintf(&want, "-%s\n", line)
	}
	for _, line := range newLines {
		fmt.Fprintf(&want, "+%s\n", line)
	}
	if got != want.String() {
		t.Fatalf("whole-file fallback mismatch\ngot:\n%q\nwant:\n%q", got, want.String())
	}
	if again := UnifiedDiff(oldText, newText, "large", 3); again != got {
		t.Fatal("fallback output was nondeterministic")
	}
}

func TestMyersWorkCeilingIncludesOuterOperations(t *testing.T) {
	common := make([]diffLine, maxMyersWorkPoints+1)
	for i := range common {
		common[i] = diffLine{text: "same", terminated: true}
	}
	different := diffLine{text: "different", terminated: true}

	tests := []struct {
		name string
		old  []diffLine
		new  []diffLine
	}{
		{name: "common prefix", old: append(append([]diffLine{}, common...), different), new: append(append([]diffLine{}, common...), diffLine{text: "new", terminated: true})},
		{name: "common suffix", old: append([]diffLine{different}, common...), new: append([]diffLine{{text: "new", terminated: true}}, common...)},
		{name: "insert-only fast path", new: common},
		{name: "delete-only fast path", old: common},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, ok := myersDiff(test.old, test.new); ok {
				t.Fatal("edit script beyond the work ceiling did not request fallback")
			}
		})
	}
}

func TestMyersWorkCeilingIncludesMatchingSnakes(t *testing.T) {
	oldLines := make([]diffLine, maxMyersWorkPoints+1)
	newLines := make([]diffLine, maxMyersWorkPoints+1)
	for i := range maxMyersWorkPoints {
		oldLines[i] = diffLine{text: "same", terminated: true}
		newLines[i+1] = diffLine{text: "same", terminated: true}
	}
	oldLines[len(oldLines)-1] = diffLine{text: "old", terminated: true}
	newLines[0] = diffLine{text: "new", terminated: true}

	if _, ok := myersMiddle(oldLines, newLines); ok {
		t.Fatal("matching snake beyond the work ceiling did not request fallback")
	}
}

func TestMyersProducesMinimalEditScripts(t *testing.T) {
	sequences := allLineSequences([]string{"a", "b"}, 4)
	for _, oldLines := range sequences {
		for _, newLines := range sequences {
			ops, ok := myersDiff(oldLines, newLines)
			if !ok {
				t.Fatalf("small diff unexpectedly exceeded work ceiling: old=%v new=%v", oldLines, newLines)
			}
			var rebuiltOld, rebuiltNew []diffLine
			edits := 0
			for _, op := range ops {
				switch op.kind {
				case diffEqual:
					rebuiltOld = append(rebuiltOld, op.old)
					rebuiltNew = append(rebuiltNew, op.new)
				case diffDelete:
					rebuiltOld = append(rebuiltOld, op.old)
					edits++
				case diffInsert:
					rebuiltNew = append(rebuiltNew, op.new)
					edits++
				}
			}
			if !equalDiffLines(rebuiltOld, oldLines) || !equalDiffLines(rebuiltNew, newLines) {
				t.Fatalf("script does not rebuild inputs: old=%v new=%v ops=%v", oldLines, newLines, ops)
			}
			minimum := len(oldLines) + len(newLines) - 2*longestCommonSubsequence(oldLines, newLines)
			if edits != minimum {
				t.Fatalf("edit count = %d, want minimal %d: old=%v new=%v ops=%v", edits, minimum, oldLines, newLines, ops)
			}
		}
	}
}

func numberedLines(prefix string, count int) string {
	var b strings.Builder
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&b, "%s-%02d\n", prefix, i)
	}
	return b.String()
}

func replaceNumberedLines(text string, replacements map[int]string) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for line, replacement := range replacements {
		lines[line-1] = replacement
	}
	return strings.Join(lines, "\n") + "\n"
}

func allLineSequences(alphabet []string, maxLength int) [][]diffLine {
	sequences := [][]diffLine{nil}
	for length := 1; length <= maxLength; length++ {
		count := 1
		for range length {
			count *= len(alphabet)
		}
		for encoded := 0; encoded < count; encoded++ {
			value := encoded
			sequence := make([]diffLine, length)
			for i := range sequence {
				sequence[i] = diffLine{text: alphabet[value%len(alphabet)], terminated: true}
				value /= len(alphabet)
			}
			sequences = append(sequences, sequence)
		}
	}
	return sequences
}

func longestCommonSubsequence(a, b []diffLine) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for _, left := range a {
		for j, right := range b {
			if left == right {
				current[j+1] = previous[j] + 1
			} else {
				current[j+1] = max(current[j], previous[j+1])
			}
		}
		previous, current = current, previous
		clear(current)
	}
	return previous[len(b)]
}

package outputlimit

import (
	"errors"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestWriterMatchesAggregateBytePolicyAcrossChunks(t *testing.T) {
	tests := []struct {
		name     string
		policy   Policy
		input    string
		patterns [][]int
	}{
		{"empty", Policy{MaxBytes: 8}, "", [][]int{{1}}},
		{"exact", Policy{MaxBytes: 8}, "abc\ndef\n", [][]int{{8}, {1}, {3, 2}}},
		{"ascii", Policy{MaxBytes: 7, HeadFraction: 0.6}, "abcdefghijklmnop", [][]int{{16}, {1}, {2, 5}}},
		{"utf8", Policy{MaxBytes: 11, HeadFraction: 0.5}, "a🙂b終わりlast", [][]int{{1}, {2}, {7, 3}}},
		{"crlf", Policy{MaxBytes: 9, HeadFraction: 0.5}, "first\r\nsecond\r\nthird", [][]int{{1}, {6}, {8, 2}}},
		{"all head", Policy{MaxBytes: 5, HeadFraction: 1}, "abcd\r\nefgh", [][]int{{2}}},
		{"all tail", Policy{MaxBytes: 5, HeadFraction: 0}, "abcd\r\nefgh", [][]int{{3}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want, wantReport := (Policy{MaxBytes: test.policy.MaxBytes, HeadFraction: test.policy.HeadFraction}).Apply(test.input)
			for _, pattern := range test.patterns {
				writer, err := NewWriter(test.policy)
				if err != nil {
					t.Fatal(err)
				}
				writeChunks(t, writer, test.input, pattern)
				got, report := writer.String()
				if got != want || report != wantReport {
					t.Fatalf("pattern %v: String() = %q, %#v; want %q, %#v", pattern, got, report, want, wantReport)
				}
				if !utf8.ValidString(got) {
					t.Fatalf("String() returned invalid UTF-8: %q", got)
				}
			}
		})
	}
}

func TestWriterBroadDeterministicEquivalence(t *testing.T) {
	random := rand.New(rand.NewSource(7331))
	atoms := []string{"a", "b", "🙂", "終", "\n", "\r\n", "xyz", "0123456789"}
	for caseIndex := 0; caseIndex < 3000; caseIndex++ {
		var input strings.Builder
		for range random.Intn(80) {
			input.WriteString(atoms[random.Intn(len(atoms))])
		}
		policy := Policy{MaxBytes: 1 + random.Intn(48), HeadFraction: []float64{0, 0.25, 0.5, 0.6, 1}[random.Intn(5)]}
		writer, err := NewWriter(policy)
		if err != nil {
			t.Fatal(err)
		}
		value := input.String()
		position := 0
		for position < len(value) {
			size := 1 + random.Intn(9)
			if size > len(value)-position {
				size = len(value) - position
			}
			if _, err := writer.Write([]byte(value[position : position+size])); err != nil {
				t.Fatal(err)
			}
			position += size
		}
		got, report := writer.String()
		want, wantReport := policy.Apply(value)
		if got != want || report != wantReport {
			t.Fatalf("case %d policy %#v input %q: got %q, %#v; want %q, %#v", caseIndex, policy, value, got, report, want, wantReport)
		}
	}
}

func TestWriterRunawayStreamRetainsAtMostTwiceLimit(t *testing.T) {
	policy := Policy{MaxBytes: 64, HeadFraction: 0.5}
	writer, err := NewWriter(policy)
	if err != nil {
		t.Fatal(err)
	}
	chunk := []byte(strings.Repeat("abcdefghijklmnop\r\n", 1000))
	for range 100 {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if retained := cap(writer.head) + cap(writer.tail); retained > 2*policy.MaxBytes {
		t.Fatalf("writer retained capacity %d; limit is %d", retained, 2*policy.MaxBytes)
	}
	got, report := writer.String()
	if !utf8.ValidString(got) || !report.Truncated || report.OriginalBytes != 100*len(chunk) {
		t.Fatalf("invalid runaway result: %q, %#v", got, report)
	}
}

func TestWriterResetValidationAndOverflow(t *testing.T) {
	for _, policy := range []Policy{{MaxBytes: -1}} {
		if _, err := NewWriter(policy); !errors.Is(err, ErrInvalidLimit) {
			t.Fatalf("NewWriter(%#v) error = %v", policy, err)
		}
	}
	unlimited, err := NewWriter(Policy{})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Repeat("unlimited", 10_000)
	if _, err := unlimited.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	if got, report := unlimited.String(); got != input || report.Truncated || report.OriginalBytes != len(input) {
		t.Fatalf("unlimited writer = %d bytes, %#v", len(got), report)
	}
	writer, err := NewWriter(Policy{MaxBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("abcdef"))
	writer.Reset()
	if got, report := writer.String(); got != "" || report != (Report{}) {
		t.Fatalf("after Reset: %q, %#v", got, report)
	}
	writer.total = maxIntValue()
	if n, err := writer.Write([]byte("x")); n != 0 || !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("Write() = %d, %v", n, err)
	}
}

func TestWriterConcurrentAccess(t *testing.T) {
	writer, err := NewWriter(Policy{MaxBytes: 32, HeadFraction: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 200 {
				_, _ = writer.Write([]byte("x"))
				_, _ = writer.String()
			}
		}()
	}
	group.Wait()
	got, report := writer.String()
	want, wantReport := (Policy{MaxBytes: 32, HeadFraction: 0.5}).Apply(strings.Repeat("x", 1600))
	if got != want || report != wantReport {
		t.Fatalf("String() = %q, %#v; want %q, %#v", got, report, want, wantReport)
	}
}

func writeChunks(t *testing.T, writer *Writer, input string, pattern []int) {
	t.Helper()
	position, patternIndex := 0, 0
	for position < len(input) {
		size := pattern[patternIndex%len(pattern)]
		patternIndex++
		if size <= 0 {
			continue
		}
		if size > len(input)-position {
			size = len(input) - position
		}
		n, err := writer.Write([]byte(input[position : position+size]))
		if err != nil || n != size {
			t.Fatalf("Write() = %d, %v; want %d, nil", n, err, size)
		}
		position += size
	}
}

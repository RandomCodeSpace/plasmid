package sessionstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSegmentsAreCanonical(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		encoded string
	}{
		{name: "plain", input: "abc_-.Z9", encoded: "abc_-.Z9"},
		{name: "dot", input: ".", encoded: "%2E"},
		{name: "dot dot", input: "..", encoded: "%2E%2E"},
		{name: "separators", input: "/\\", encoded: "%2F%5C"},
		{name: "space colon percent", input: "a b:c%", encoded: "a%20b%3Ac%25"},
		{name: "unicode", input: "cafe\u0301", encoded: "cafe%CC%81"},
		{name: "nul", input: "a\x00b", encoded: "a%00b"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := encodeSegment(test.input)
			if err != nil || encoded != test.encoded {
				t.Fatalf("encodeSegment(%q) = %q, %v; want %q, nil", test.input, encoded, err, test.encoded)
			}
			decoded, err := decodeSegment(encoded)
			if err != nil || decoded != test.input {
				t.Fatalf("decodeSegment(%q) = %q, %v; want %q, nil", encoded, decoded, err, test.input)
			}
		})
	}
}

func TestDecodeSegmentRejectsNonCanonicalInput(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", ".", "..", "%2e", "%41", "%", "%G0", "a/b", "a\\b", "%2F" + string(make([]byte, 199))} {
		t.Run(input, func(t *testing.T) {
			_, err := decodeSegment(input)
			if !errors.Is(err, ErrInvalidID) {
				t.Fatalf("decodeSegment(%q) error = %v, want ErrInvalidID", input, err)
			}
		})
	}
	tooLong := string(make([]byte, maxSegmentLen+1))
	for i := range tooLong {
		tooLong = tooLong[:i] + "a" + tooLong[i+1:]
	}
	if _, err := encodeSegment(tooLong); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("encodeSegment(overlong) error = %v, want ErrInvalidID", err)
	}
}

func TestPathsUseEvaluatedRootAndConfinedLayout(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	paths, err := openPaths(link)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = paths.close() })
	if paths.dir != dir {
		t.Fatalf("root = %q, want evaluated %q", paths.dir, dir)
	}
	name, err := paths.sessionLog("app", "user", "../id")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("apps", "app", "users", "user", "sessions", "..%2Fid.jsonl"); name != want {
		t.Fatalf("sessionLog = %q, want %q", name, want)
	}
	if err := paths.ensureParent(name); err != nil {
		t.Fatal(err)
	}
	file, err := paths.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != fileMode {
		t.Fatalf("file mode = %o, want %o", info.Mode().Perm(), fileMode)
	}
	if _, err := paths.root.Open("../outside"); err == nil {
		t.Fatal("root allowed traversal outside storage")
	}
}

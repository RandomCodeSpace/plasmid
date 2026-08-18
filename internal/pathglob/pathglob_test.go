package pathglob

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestMatcher(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		patterns []string
		path     string
		want     bool
	}{
		{name: "star root", patterns: []string{"*.log"}, path: "a.log", want: true},
		{name: "star nested", patterns: []string{"*.log"}, path: "nested/a.log", want: true},
		{name: "star suffix", patterns: []string{"*.log"}, path: "a.log.bak"},
		{name: "anchored root", patterns: []string{"/build"}, path: "build", want: true},
		{name: "anchored nested", patterns: []string{"/build"}, path: "nested/build"},
		{name: "directory path", patterns: []string{"build/"}, path: "build/", want: true},
		{name: "directory child", patterns: []string{"build/"}, path: "nested/build/file", want: true},
		{name: "directory regular file", patterns: []string{"build/"}, path: "build"},
		{name: "double star root", patterns: []string{"**/vendor"}, path: "vendor", want: true},
		{name: "double star nested", patterns: []string{"**/vendor"}, path: "a/b/vendor", want: true},
		{name: "double star zero directories", patterns: []string{"foo/**/bar"}, path: "foo/bar", want: true},
		{name: "double star many directories", patterns: []string{"foo/**/bar"}, path: "foo/a/b/bar", want: true},
		{name: "question one", patterns: []string{"a?.txt"}, path: "ab.txt", want: true},
		{name: "question zero", patterns: []string{"a?.txt"}, path: "a.txt"},
		{name: "class", patterns: []string{"[abc].txt"}, path: "b.txt", want: true},
		{name: "class range", patterns: []string{"[a-c].txt"}, path: "b.txt", want: true},
		{name: "class miss", patterns: []string{"[abc].txt"}, path: "d.txt"},
		{name: "case sensitive", patterns: []string{"name.txt"}, path: "Name.txt"},
		{name: "unicode", patterns: []string{"café/*.txt"}, path: "café/文.txt", want: true},
		{name: "negation", patterns: []string{"*.log", "!keep.log"}, path: "keep.log"},
		{name: "negation leaves other match", patterns: []string{"*.log", "!keep.log"}, path: "drop.log", want: true},
		{name: "escaped bang", patterns: []string{`\!keep`}, path: "!keep", want: true},
		{name: "trailing backslash", patterns: []string{`foo\`}, path: `foo\`, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			matcher, compileErrors := Compile(test.patterns)
			if len(compileErrors) != 0 {
				t.Fatalf("Compile() errors = %v", compileErrors)
			}
			if got := matcher.Match(test.path); got != test.want {
				t.Fatalf("Match(%q) = %v, want %v", test.path, got, test.want)
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pattern string
		target  error
	}{
		{name: "empty", pattern: "", target: ErrEmptyPattern},
		{name: "brace", pattern: "{a,b}", target: ErrUnsupportedBrace},
		{name: "class", pattern: "[abc", target: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := CompileOne(test.pattern)
			if err == nil {
				t.Fatal("CompileOne() error = nil")
			}
			if test.target != nil && !errors.Is(err, test.target) {
				t.Fatalf("CompileOne() error = %v, want %v", err, test.target)
			}
		})
	}
}

func TestCharacterClassPatterns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
		wantErr string
	}{
		{name: "empty", pattern: "[]", wantErr: "empty character class"},
		{name: "unterminated", pattern: "[abc", wantErr: "unterminated character class"},
		{name: "separator", pattern: "[/]", wantErr: "path separator"},
		{name: "negated", pattern: "[!ab].txt", path: "c.txt", want: true},
		{name: "escaped closing bracket", pattern: `[\]].txt`, path: "].txt", want: true},
		{name: "escaped slash suffix", pattern: `name\/`, path: "name/", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			matcher, err := CompileOne(test.pattern)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("CompileOne(%q) error = %v, want %q", test.pattern, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := matcher.Match(test.path); got != test.want {
				t.Fatalf("Match(%q) = %v, want %v", test.path, got, test.want)
			}
		})
	}
}

func TestPatternsAndSplitList(t *testing.T) {
	t.Parallel()
	patterns := []string{"*.go", "!vendor/**"}
	matcher, compileErrors := Compile(patterns)
	if len(compileErrors) != 0 {
		t.Fatal(compileErrors)
	}
	if got := matcher.Patterns(); !reflect.DeepEqual(got, patterns) {
		t.Fatalf("Patterns() = %#v, want %#v", got, patterns)
	}
	patterns[0] = "changed"
	if got := matcher.Patterns()[0]; got != "*.go" {
		t.Fatalf("Patterns() was aliased: %q", got)
	}
	if got, want := SplitList(" *.go, , **/*.md "), []string{"*.go", "**/*.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SplitList() = %#v, want %#v", got, want)
	}
}

func TestMatchString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		{pattern: "secret/*", value: "secret/a/b", want: true},
		{pattern: "git ?iff", value: "git diff", want: true},
		{pattern: "file-?.txt", value: "file-文.txt", want: true},
		{pattern: `literal\*`, value: "literal*", want: true},
		{pattern: `literal\`, value: `literal\`, want: true},
		{pattern: "Read", value: "Write"},
	}
	for _, test := range tests {
		if got := MatchString(test.pattern, test.value); got != test.want {
			t.Errorf("MatchString(%q, %q) = %v, want %v", test.pattern, test.value, got, test.want)
		}
	}
}

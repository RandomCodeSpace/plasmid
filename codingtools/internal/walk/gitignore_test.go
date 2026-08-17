package walk

import (
	"strings"
	"testing"

	"github.com/plasmid-dev/plasmid/warning"
)

func TestGitignorePatterns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		lines    string
		path     string
		isDir    bool
		want     bool
		warnings int
	}{
		{name: "star root", lines: "*.log\n", path: "a.log", want: true},
		{name: "star nested", lines: "*.log\n", path: "nested/a.log", want: true},
		{name: "star suffix", lines: "*.log\n", path: "a.log.bak"},
		{name: "anchored root", lines: "/build\n", path: "build", isDir: true, want: true},
		{name: "anchored nested", lines: "/build\n", path: "nested/build", isDir: true},
		{name: "directory", lines: "build/\n", path: "build", isDir: true, want: true},
		{name: "directory child", lines: "build/\n", path: "build/file", want: true},
		{name: "directory regular file", lines: "build/\n", path: "build"},
		{name: "double star root", lines: "**/vendor\n", path: "vendor", isDir: true, want: true},
		{name: "double star nested", lines: "**/vendor\n", path: "a/vendor", isDir: true, want: true},
		{name: "double star zero", lines: "foo/**/bar\n", path: "foo/bar", want: true},
		{name: "double star deep", lines: "foo/**/bar\n", path: "foo/a/b/bar", want: true},
		{name: "question", lines: "a?.txt\n", path: "ab.txt", want: true},
		{name: "class", lines: "[abc].txt\n", path: "c.txt", want: true},
		{name: "negation", lines: "*.log\n!keep.log\n", path: "keep.log"},
		{name: "comment blank", lines: "\n  # comment\n*.tmp\n", path: "a.tmp", want: true},
		{name: "escaped hash", lines: "\\#file\n", path: "#file", want: true},
		{name: "escaped bang", lines: "\\!keep\n", path: "!keep", want: true},
		{name: "trailing slash escape", lines: "foo\\\n", path: `foo\`, want: true},
		{name: "malformed class", lines: "[abc\n*.log\n", path: "a.log", want: true, warnings: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var warnings warning.SliceSink
			rules := parseIgnore(strings.NewReader(test.lines), ".gitignore", ".", &warnings)
			if got := ignoredBy(rules, test.path, test.isDir); got != test.want {
				t.Fatalf("ignoredBy(%q) = %v, want %v", test.path, got, test.want)
			}
			if got := warnings.Warnings(); len(got) != test.warnings {
				t.Fatalf("warnings = %#v, want %d", got, test.warnings)
			}
		})
	}
}

func TestNestedRuleBase(t *testing.T) {
	t.Parallel()
	rootRules := parseIgnore(strings.NewReader("nested/*.tmp\nnested/drop/\n"), ".gitignore", ".", warning.DiscardSink{})
	nestedRules := parseIgnore(strings.NewReader("!keep.tmp\n!drop/\n"), "nested/.gitignore", "nested", warning.DiscardSink{})
	rules := append(rootRules, nestedRules...)
	tests := []struct {
		path string
		dir  bool
		want bool
	}{
		{path: "nested/drop.tmp", want: true},
		{path: "nested/keep.tmp"},
		{path: "other/keep.tmp"},
		{path: "nested/drop", dir: true},
	}
	for _, test := range tests {
		if got := ignoredBy(rules, test.path, test.dir); got != test.want {
			t.Errorf("ignoredBy(%q) = %v, want %v", test.path, got, test.want)
		}
	}
}

func TestGitignorePatternWithSlashIsRelativeToRuleBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		base string
		path string
		want bool
	}{
		{name: "root exact", base: ".", path: "docs/generated", want: true},
		{name: "root nested suffix", base: ".", path: "nested/docs/generated"},
		{name: "nested exact", base: "nested", path: "nested/docs/generated", want: true},
		{name: "nested deeper suffix", base: "nested", path: "nested/deeper/docs/generated"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rules := parseIgnore(strings.NewReader("docs/generated\n"), ".gitignore", test.base, warning.DiscardSink{})
			if got := ignoredBy(rules, test.path, true); got != test.want {
				t.Fatalf("ignoredBy(%q) = %v, want %v", test.path, got, test.want)
			}
		})
	}
}

package fixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/plasmid-dev/plasmid/warning"
)

func TestExpand(t *testing.T) {
	paths := Paths{Home: `/home/test`, WorkDir: `/home/test/work`, ConfigDir: `/home/test/.config`}
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "all values", input: `${HOME}|${WORKDIR}|${CONFIG_DIR}`, want: `/home/test|/home/test/work|/home/test/.config`},
		{name: "unknown preserved", input: `${TMPDIR}`, want: `${TMPDIR}`},
		{name: "unterminated preserved", input: `${HOME`, want: `${HOME`},
		{name: "missing value preserved", input: `${HOME}`, want: `${HOME}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			currentPaths := paths
			if test.name == "missing value preserved" {
				currentPaths.Home = ""
			}
			got := Expand([]byte(test.input), currentPaths)
			if string(got) != test.want {
				t.Fatalf("Expand() = %q, want %q", got, test.want)
			}
		})
	}
	got := Expand([]byte(`${HOME}`), Paths{Home: `${WORKDIR}`, WorkDir: "/work"})
	if string(got) != `${WORKDIR}` {
		t.Fatalf("Expand() recursively expanded to %q", got)
	}
	got = Expand([]byte(`file://${WORKDIR}/main.go`), Paths{WorkDir: `C:\repo`})
	if string(got) != `file:///C:/repo/main.go` {
		t.Fatalf("Expand() file URI = %q", got)
	}
	got = Expand([]byte(`file://${WORKDIR}/main%20file.go`), Paths{WorkDir: `C:\My Repo`})
	if string(got) != `file:///C:/My%20Repo/main%20file.go` {
		t.Fatalf("Expand() escaped file URI = %q", got)
	}
	got = Expand([]byte(`file://${WORKDIR}/main.go`), Paths{WorkDir: `\\Server\Share`})
	if string(got) != `file://server/Share/main.go` {
		t.Fatalf("Expand() UNC file URI = %q", got)
	}
}

func TestRequireNonEmptyArea(t *testing.T) {
	if err := requireNonEmptyArea("tools", 1); err != nil {
		t.Fatal(err)
	}
	if err := requireNonEmptyArea("tools", 0); err == nil || err.Error() != `fixture area "tools" has no cases` {
		t.Fatalf("requireNonEmptyArea() error = %v", err)
	}
}

func TestUnmatchedKinds(t *testing.T) {
	got := unmatchedKinds([]string{"bash", "find", "grep", "ls"}, map[string]int{"find": 2, "ls": 1})
	want := []string{"bash", "grep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unmatchedKinds() = %#v, want %#v", got, want)
	}
}

func TestStableWarningsExcludesMessage(t *testing.T) {
	got := StableWarnings([]warning.Warning{{
		Code: "syntax.unknown-field", Source: "syntax", Path: `skill\nested\SKILL.md`, Line: 7, Message: "mutable prose",
	}})
	want := []WarningFields{{Code: "syntax.unknown-field", Source: "syntax", Path: "skill/nested/SKILL.md", Line: 7}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StableWarnings() = %#v, want %#v", got, want)
	}
	if empty := StableWarnings(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("StableWarnings(nil) = %#v, want non-nil empty slice", empty)
	}
}

func TestValidateWarningPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: ""},
		{path: "nested/file.go"},
		{path: "/absolute/file.go", want: "root-relative"},
		{path: `C:/absolute/file.go`, want: "volume or drive"},
		{path: `C:\absolute\file.go`, want: "not slash-separated"},
		{path: "../escape.go", want: "normalized root-relative"},
		{path: "nested/../../escape.go", want: "normalized root-relative"},
		{path: "nested/./file.go", want: "normalized root-relative"},
		{path: "nested//file.go", want: "normalized root-relative"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			err := validateWarningPath(test.path)
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("validateWarningPath() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateWarningsJSON(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "empty", data: "[]\n"},
		{name: "exact fields", data: `[{"code":"walk.invalid_ignore_pattern","line":3,"path":".gitignore","source":"walk"}]`},
		{name: "null", data: `null`, want: "must be a JSON array"},
		{name: "prose message", data: `[{"code":"x","line":1,"message":"unstable","path":"a","source":"test"}]`, want: `unknown field "message"`},
		{name: "unknown field", data: `[{"code":"x","extra":true,"line":1,"path":"a","source":"test"}]`, want: `unknown field "extra"`},
		{name: "missing field", data: `[{"code":"x","line":1,"source":"test"}]`, want: "must contain exactly"},
		{name: "wrong type", data: `[{"code":"x","line":"1","path":"a","source":"test"}]`, want: "cannot unmarshal"},
		{name: "negative line", data: `[{"code":"x","line":-1,"path":"a","source":"test"}]`, want: "negative line"},
		{name: "backslash path", data: `[{"code":"x","line":1,"path":"a\\b","source":"test"}]`, want: "not slash-separated"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateWarningsJSON([]byte(test.data))
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("validateWarningsJSON() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompareJSONUpdateThenReadOnly(t *testing.T) {
	dir := t.TempDir()
	testCase := Case{Area: "tools", ID: "portable-path", Dir: dir}
	paths := Paths{Home: "/home/test", WorkDir: "/home/test/work", ConfigDir: "/home/test/.config"}
	actual := map[string]any{
		"config": "/home/test/.config/plasmid.json",
		"home":   "/home/test",
		"items":  []any{json.Number("1"), "/home/test/work/file.go"},
		"nested": "/tmp/home/test",
		"other":  "/home/tester",
	}
	if err := compareJSON(testCase, "expected.json", actual, paths, GoldenUpdate); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"config\": \"${CONFIG_DIR}/plasmid.json\",\n  \"home\": \"${HOME}\",\n  \"items\": [\n    1,\n    \"${WORKDIR}/file.go\"\n  ],\n  \"nested\": \"/tmp/home/test\",\n  \"other\": \"/home/tester\"\n}\n"
	if string(data) != want {
		t.Fatalf("updated golden = %s, want %s", data, want)
	}
	if err := compareJSON(testCase, "expected.json", actual, paths, GoldenReadOnly); err != nil {
		t.Fatal(err)
	}
	if err := compareJSON(testCase, "expected.json", map[string]any{"home": "/elsewhere"}, paths, GoldenReadOnly); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestCompareJSONRejectsAmbiguousPathValues(t *testing.T) {
	testCase := Case{Dir: t.TempDir()}
	err := compareJSON(testCase, "expected.json", map[string]any{}, Paths{Home: "/same", WorkDir: "/same"}, GoldenUpdate)
	if err == nil || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("compareJSON() error = %v", err)
	}
}

func TestCompareJSONRejectsInvalidMode(t *testing.T) {
	testCase := Case{Dir: t.TempDir()}
	err := compareJSON(testCase, "expected.json", map[string]any{}, Paths{}, GoldenMode(99))
	if err == nil || err.Error() != "invalid golden mode 99" {
		t.Fatalf("compareJSON() error = %v", err)
	}
}

func TestCompareJSONDoesNotExpandActualPlaceholderLiterals(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "expected.json"), "{\"value\":\"${HOME}\"}\n")
	err := compareJSON(Case{Dir: dir}, "expected.json", map[string]string{"value": "${HOME}"}, Paths{Home: "/home/test"}, GoldenReadOnly)
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("compareJSON() error = %v, want literal placeholder mismatch", err)
	}
	if err := compareJSON(Case{Dir: dir}, "expected.json", map[string]string{"value": "/home/test"}, Paths{Home: "/home/test"}, GoldenReadOnly); err != nil {
		t.Fatalf("runtime path comparison failed: %v", err)
	}
}

func TestCaseComparisonReceiptIsPerCase(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "expected.json"), "{}\n")
	readCase := Case{Area: "tools", ID: "read-case", Dir: dir, receipt: &comparisonReceipt{}}
	writeCase := Case{Area: "tools", ID: "write-case", Dir: dir, receipt: &comparisonReceipt{}}

	readCopy := readCase
	readCopy.CompareJSON(t, "expected.json", map[string]any{}, Paths{}, GoldenReadOnly)
	if err := readCase.comparisonError(); err != nil {
		t.Fatalf("compared case receipt: %v", err)
	}
	if err := writeCase.comparisonError(); err == nil || !strings.Contains(err.Error(), "tools/write-case executed without CompareJSON") {
		t.Fatalf("missing comparison receipt error = %v", err)
	}
}

func TestCaseComparisonReceiptRequiresExpectedGolden(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "warnings.json"), "[]\n")
	testCase := Case{Area: "tools", ID: "warnings-only", Dir: dir, receipt: &comparisonReceipt{}}
	testCase.CompareJSON(t, "warnings.json", []any{}, Paths{}, GoldenReadOnly)
	if err := testCase.comparisonError(); err == nil || !strings.Contains(err.Error(), "expected.json") {
		t.Fatalf("warnings-only comparison receipt error = %v", err)
	}
}

func TestVerifyRunnerReceipts(t *testing.T) {
	fixturesRoot := t.TempDir()
	for _, kind := range []string{"read", "write"} {
		id := kind + "-case"
		dir := filepath.Join(fixturesRoot, "tools", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dir, "case.json"), fmt.Sprintf(`{"area":"tools","id":%q,"kind":%q}`, id, kind))
	}
	t.Run("runner never executed", func(t *testing.T) {
		problems := verifyRunnerReceipts(fixturesRoot, []runnerSnapshot{{
			key: runnerKey{area: "tools", name: "tools/all"}, kinds: []string{"read"},
		}}, true)
		if !errorsContain(problems, `did not execute fixture case tools/read-case of kind "read"`) {
			t.Fatalf("verification errors = %v", problems)
		}
	})
	t.Run("every registered kind executes", func(t *testing.T) {
		readReceipt := &comparisonReceipt{}
		readReceipt.expectedCompared.Store(true)
		problems := verifyRunnerReceipts(fixturesRoot, []runnerSnapshot{{
			key:   runnerKey{area: "tools", name: "tools/all"},
			kinds: []string{"read", "write"},
			executions: []caseExecution{
				{id: "read-case", kind: "read", receipt: readReceipt},
			},
		}}, true)
		if !errorsContain(problems, `did not execute fixture case tools/write-case of kind "write"`) {
			t.Fatalf("verification errors = %v", problems)
		}
	})
	t.Run("dormant runner exempt", func(t *testing.T) {
		problems := verifyRunnerReceipts(filepath.Join(t.TempDir(), "missing"), []runnerSnapshot{{
			key: runnerKey{area: "tools", name: "tools/dormant"},
		}}, true)
		if len(problems) != 0 {
			t.Fatalf("verification errors = %v", problems)
		}
	})
}

func TestVerifyRunnerReceiptsAllowsTargetedRuns(t *testing.T) {
	fixturesRoot := t.TempDir()
	caseDir := filepath.Join(fixturesRoot, "tools", "read-case")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(caseDir, "case.json"), `{"area":"tools","id":"read-case","kind":"read"}`)
	snapshot := runnerSnapshot{key: runnerKey{area: "tools", name: "tools/read"}, kinds: []string{"read"}}
	if problems := verifyRunnerReceipts(fixturesRoot, []runnerSnapshot{snapshot}, false); len(problems) != 0 {
		t.Fatalf("targeted-run verification errors = %v", problems)
	}
	snapshot.executions = []caseExecution{{id: "read-case", kind: "read", receipt: &comparisonReceipt{}}}
	problems := verifyRunnerReceipts(fixturesRoot, []runnerSnapshot{snapshot}, false)
	if !errorsContain(problems, "executed without CompareJSON for expected.json") {
		t.Fatalf("targeted-run verification errors = %v", problems)
	}
}

func TestRequireAllRunnerReceipts(t *testing.T) {
	tests := []struct {
		listFilter string
		runFilter  string
		want       bool
	}{
		{runFilter: "", want: true},
		{runFilter: ".", want: true},
		{runFilter: ".*", want: true},
		{runFilter: "^.*$", want: true},
		{runFilter: "TestReadFixtures"},
		{runFilter: "TestRead/valid"},
		{runFilter: "^$"},
		{listFilter: "."},
		{runFilter: ".*", listFilter: "."},
	}
	for _, test := range tests {
		name := "run=" + test.runFilter + "/list=" + test.listFilter
		t.Run(name, func(t *testing.T) {
			if got := requireAllRunnerReceipts(test.runFilter, test.listFilter); got != test.want {
				t.Fatalf("requireAllRunnerReceipts(%q, %q) = %v, want %v", test.runFilter, test.listFilter, got, test.want)
			}
		})
	}
}

func TestCollapseJSONPathsNormalizesOnlyPathSpans(t *testing.T) {
	paths := Paths{WorkDir: `C:\repo`}
	value := map[string]any{
		"mixed":       `regex \d+ at C:\repo\pkg\main.go, keep \w+`,
		"nested":      `/tmp/C:/repo/pkg/main.go`,
		"prefix":      `C:\repository\main.go`,
		"punctuation": `(<C:\repo>);`,
		"uri":         `open <file:///C:/repo/pkg/main.go>; keep \s`,
	}
	got, err := collapseJSONPaths(value, paths)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"mixed":       `regex \d+ at ${WORKDIR}/pkg/main.go, keep \w+`,
		"nested":      `/tmp/C:/repo/pkg/main.go`,
		"prefix":      `C:\repository\main.go`,
		"punctuation": `(<${WORKDIR}>);`,
		"uri":         `open <file://${WORKDIR}/pkg/main.go>; keep \s`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collapseJSONPaths() = %#v, want %#v", got, want)
	}
}

func TestCollapseJSONPathsParsesFileURIs(t *testing.T) {
	paths := Paths{WorkDir: `C:\My Repo`, ConfigDir: `\\Server\Share`}
	value := map[string]any{
		"local":     `open <file:///C:/My%20Repo/pkg/a%20b.go?mode=read#part%201>`,
		"localhost": `file://localhost/C:/My%20Repo/main.go`,
		"other":     `file://other/Share/main.go`,
		"unc":       `file://SERVER/Share/pkg/a%20b.go`,
	}
	got, err := collapseJSONPaths(value, paths)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"local":     `open <file://${WORKDIR}/pkg/a%20b.go?mode=read#part%201>`,
		"localhost": `file://${WORKDIR}/main.go`,
		"other":     `file://other/Share/main.go`,
		"unc":       `file://${CONFIG_DIR}/pkg/a%20b.go`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collapseJSONPaths() = %#v, want %#v", got, want)
	}
}

func TestCollapseJSONPathsCanonicalizesRoots(t *testing.T) {
	tests := []struct {
		name     string
		paths    Paths
		value    string
		want     string
		expanded string
	}{
		{
			name:     "trailing POSIX separator",
			paths:    Paths{WorkDir: "/workspace/"},
			value:    "/workspace/pkg/main.go file:///workspace/pkg/main.go",
			want:     "${WORKDIR}/pkg/main.go file://${WORKDIR}/pkg/main.go",
			expanded: "/workspace/pkg/main.go file:///workspace/pkg/main.go",
		},
		{
			name:     "POSIX root",
			paths:    Paths{Home: "/"},
			value:    "/etc/config file:///etc/config",
			want:     "${HOME}/etc/config file://${HOME}/etc/config",
			expanded: "/etc/config file:///etc/config",
		},
		{
			name:     "Windows trailing separator",
			paths:    Paths{WorkDir: `C:\repo\`},
			value:    `C:\repo\pkg\main.go file:///C:/repo/pkg/main.go`,
			want:     `${WORKDIR}/pkg/main.go file://${WORKDIR}/pkg/main.go`,
			expanded: "C:/repo/pkg/main.go file:///C:/repo/pkg/main.go",
		},
		{
			name:     "Windows drive root",
			paths:    Paths{WorkDir: "C:/"},
			value:    "C:/pkg/main.go file:///C:/pkg/main.go",
			want:     "${WORKDIR}/pkg/main.go file://${WORKDIR}/pkg/main.go",
			expanded: "C:/pkg/main.go file:///C:/pkg/main.go",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := collapseJSONPaths(test.value, test.paths)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("collapseJSONPaths() = %q, want %q", got, test.want)
			}
			if expanded := string(Expand([]byte(test.want), test.paths)); expanded != test.expanded {
				t.Fatalf("Expand() = %q, want %q", expanded, test.expanded)
			}
		})
	}
}

func TestCompareJSONUpdateRejectsSymlinkedParent(t *testing.T) {
	caseDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(caseDir, "escape")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	testCase := Case{Dir: caseDir}
	err := compareJSON(testCase, "escape/expected.json", map[string]any{"ok": true}, Paths{}, GoldenUpdate)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("compareJSON() error = %v, want symlink rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "expected.json")); !os.IsNotExist(statErr) {
		t.Fatalf("outside golden stat error = %v, want not exist", statErr)
	}
}

func TestReadRejectsSymlinkedParent(t *testing.T) {
	caseDir := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "input.json"), "{}\n")
	symlinkOrSkip(t, outside, filepath.Join(caseDir, "escape"))
	_, err := (Case{Dir: caseDir}).read("escape/input.json")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("read() error = %v, want symlink rejection", err)
	}
}

func TestDecodeRejectsSymlinkedFile(t *testing.T) {
	caseDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	writeTestFile(t, outside, "{}\n")
	symlinkOrSkip(t, outside, filepath.Join(caseDir, "input.json"))
	var destination map[string]any
	err := (Case{Dir: caseDir}).decode("input.json", &destination, nil)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("decode() error = %v, want symlink rejection", err)
	}
}

func TestCompareJSONReadOnlyRejectsSymlinkedParent(t *testing.T) {
	caseDir := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "expected.json"), "{}\n")
	symlinkOrSkip(t, outside, filepath.Join(caseDir, "escape"))
	err := compareJSON(Case{Dir: caseDir}, "escape/expected.json", map[string]any{}, Paths{}, GoldenReadOnly)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("compareJSON() error = %v, want symlink rejection", err)
	}
}

func TestDecodePaths(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "input.json"), "{\"path\":\"${WORKDIR}/file.go\"}\n")
	testCase := Case{Dir: dir}
	var got map[string]string
	testCase.DecodePaths(t, "input.json", &got, Paths{WorkDir: "/work"})
	want := map[string]string{"path": "/work/file.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DecodePaths() = %#v, want %#v", got, want)
	}
}

func TestValidateArea(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
		input    string
		warnings string
		want     []string
	}{
		{
			name:     "valid JSON input",
			metadata: "{\"area\":\"tools\",\"id\":\"read-basic\",\"kind\":\"read\"}\n",
			input:    "file",
		},
		{
			name:     "valid directory input",
			metadata: "{\"area\":\"tools\",\"id\":\"read-basic\",\"kind\":\"read\"}\n",
			input:    "directory",
		},
		{
			name:     "metadata mismatch and missing input",
			metadata: "{\"area\":\"other\",\"id\":\"wrong\",\"kind\":\"\"}\n",
			want:     []string{"exactly one", "metadata must name"},
		},
		{
			name:     "both input forms",
			metadata: "{\"area\":\"tools\",\"id\":\"read-basic\",\"kind\":\"read\"}\n",
			input:    "both",
			want:     []string{"exactly one"},
		},
		{
			name:     "unsorted JSON",
			metadata: "{\"id\":\"read-basic\",\"area\":\"tools\",\"kind\":\"read\"}\n",
			input:    "file",
			want:     []string{"out of order"},
		},
		{
			name:     "metadata type error",
			metadata: "{\"area\":123,\"id\":\"read-basic\",\"kind\":\"read\"}\n",
			input:    "file",
			want:     []string{"decode case.json", "cannot unmarshal"},
		},
		{
			name:     "warning prose rejected",
			metadata: "{\"area\":\"tools\",\"id\":\"read-basic\",\"kind\":\"read\"}\n",
			input:    "file",
			warnings: `[{"code":"x","line":1,"message":"prose","path":"a","source":"test"}]`,
			want:     []string{"decode warnings.json", `unknown field "message"`},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "read-basic")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(dir, "case.json"), test.metadata)
			writeTestFile(t, filepath.Join(dir, "expected.json"), "{}\n")
			if test.warnings != "" {
				writeTestFile(t, filepath.Join(dir, "warnings.json"), test.warnings)
			}
			switch test.input {
			case "file":
				writeTestFile(t, filepath.Join(dir, "input.json"), "{}\n")
			case "directory":
				if err := os.Mkdir(filepath.Join(dir, "input"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "both":
				writeTestFile(t, filepath.Join(dir, "input.json"), "{}\n")
				if err := os.Mkdir(filepath.Join(dir, "input"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			problems := validateArea(root, "tools")
			for _, fragment := range test.want {
				if !errorsContain(problems, fragment) {
					t.Fatalf("validateArea() errors = %v, want fragment %q", problems, fragment)
				}
			}
			if len(test.want) == 0 && len(problems) != 0 {
				t.Fatalf("validateArea() errors = %v, want none", problems)
			}
		})
	}
}

func TestCasePathRejectsEscape(t *testing.T) {
	for _, name := range []string{"", "../expected.json", "/tmp/expected.json"} {
		if _, err := casePath("/fixture", name); err == nil {
			t.Fatalf("casePath(%q) succeeded", name)
		}
	}
}

func TestValidateSortedJSON(t *testing.T) {
	tests := []struct {
		name string
		json string
		ok   bool
	}{
		{name: "sorted nested", json: `{"a":{"a":1,"b":2},"b":[{"a":1,"b":2}]}`, ok: true},
		{name: "unsorted", json: `{"b":1,"a":2}`},
		{name: "duplicate", json: `{"a":1,"a":2}`},
		{name: "trailing", json: `{} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSortedJSON([]byte(test.json))
			if (err == nil) != test.ok {
				t.Fatalf("validateSortedJSON() error = %v, ok = %v", err, test.ok)
			}
		})
	}
}

func errorsContain(errs []error, fragment string) bool {
	for _, err := range errs {
		if strings.Contains(err.Error(), fragment) {
			return true
		}
	}
	return false
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlinkOrSkip(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
}

package fixture

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

const fixtureImportPath = "github.com/plasmid-dev/plasmid/internal/fixture"

type sourceRunner struct {
	area       string
	file       string
	kinds      []string
	name       string
	packageKey string
}

type sourceAreaOwner struct {
	file string
}

type parsedSourceFile struct {
	constrained    bool
	fixtureAliases map[string]bool
	osAliases      map[string]bool
	packageKey     string
	path           string
	syntax         *ast.File
	testingAliases map[string]bool
}

func TestRepositoryFixtureKindCoverage(t *testing.T) {
	for _, err := range validateRepositoryKindCoverage(repositoryRoot(t)) {
		t.Error(err)
	}
}

func TestRepositoryFixtureKindCoverageRejectsOrphan(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "testdata", "fixtures", "tools", "orphan-case")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(caseDir, "case.json"), `{"area":"tools","id":"orphan-case","kind":"orphan"}`)
	problems := validateRepositoryKindCoverage(root)
	if !errorsContain(problems, `fixture kind "tools/orphan" has no named runner`) {
		t.Fatalf("coverage errors = %v", problems)
	}
}

func TestRepositoryFixtureKindCoverageFindsSeparatePackageRunner(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "testdata", "fixtures", "tools", "read-case")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(caseDir, "case.json"), `{"area":"tools","id":"read-case","kind":"read"}`)
	packageDir := filepath.Join(root, "consumer")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(packageDir, "fixture_test.go"), `package consumer
import (
    "os"
    "testing"
    "github.com/plasmid-dev/plasmid/internal/fixture"
)
func init() { fixture.RegisterRunner("tools", "consumer/read", "read") }
func TestMain(m *testing.M) { os.Exit(fixture.Run(m)) }
func TestRead(t *testing.T) { runRead(t) }
func TestFixtureCoverage(t *testing.T) { fixture.AssertCoverage(t, "tools") }
func runRead(t *testing.T) {
    fixture.WalkKinds(t, "tools", "consumer/read", []string{"read"}, func(t *testing.T, testCase fixture.Case) {
        compareRead(t, testCase)
    })
}
func compareRead(t *testing.T, testCase fixture.Case) {
    testCase.CompareJSON(t, "expected.json", map[string]any{}, fixture.Paths{}, fixture.GoldenReadOnly)
}
`)
	if problems := validateRepositoryKindCoverage(root); len(problems) != 0 {
		t.Fatalf("coverage errors = %v", problems)
	}
}

func TestRepositoryFixtureKindCoverageRequiresOneAreaOwner(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		root := writeCoverageSource(t, "fixture_test.go", `package consumer
import (
    "os"
    "testing"
    "github.com/plasmid-dev/plasmid/internal/fixture"
)
func init() { fixture.RegisterRunner("tools", "consumer/read", "read") }
func TestMain(m *testing.M) { os.Exit(fixture.Run(m)) }
`)
		problems := validateRepositoryKindCoverage(root)
		if !errorsContain(problems, `fixture area "tools" has no AssertCoverage owner`) {
			t.Fatalf("coverage errors = %v", problems)
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		root := writeCoverageRepository(t, "func TestMain(m *testing.M) { os.Exit(fixture.Run(m)) }\n")
		writeTestFile(t, filepath.Join(root, "consumer", "duplicate_test.go"), `package consumer
import (
    "testing"
    "github.com/plasmid-dev/plasmid/internal/fixture"
)
func TestDuplicateCoverage(t *testing.T) { fixture.AssertCoverage(t, "tools") }
`)
		problems := validateRepositoryKindCoverage(root)
		if !errorsContain(problems, `fixture area "tools" has multiple AssertCoverage owners`) {
			t.Fatalf("coverage errors = %v", problems)
		}
	})
	for _, test := range []struct {
		declaration string
		name        string
	}{
		{
			name:        "dead helper",
			declaration: `func fixtureCoverage(t *testing.T) { fixture.AssertCoverage(t, "tools") }`,
		},
		{
			name: "closure",
			declaration: `func TestFixtureCoverage(t *testing.T) {
    run := func() { fixture.AssertCoverage(t, "tools") }
    run()
}`,
		},
		{
			name:        "non-runnable test name",
			declaration: `func TestfixtureCoverage(t *testing.T) { fixture.AssertCoverage(t, "tools") }`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := writeCoverageSource(t, "fixture_test.go", `package consumer
import (
    "os"
    "testing"
    "github.com/plasmid-dev/plasmid/internal/fixture"
)
func init() { fixture.RegisterRunner("tools", "consumer/read", "read") }
func TestMain(m *testing.M) { os.Exit(fixture.Run(m)) }
`+test.declaration+"\n")
			problems := validateRepositoryKindCoverage(root)
			for _, want := range []string{
				`fixture AssertCoverage`,
				`must be a direct call in a runnable top-level test`,
				`fixture area "tools" has no AssertCoverage owner`,
			} {
				if !errorsContain(problems, want) {
					t.Fatalf("coverage errors = %v, want %q", problems, want)
				}
			}
		})
	}
}

func TestRepositoryFixtureKindCoverageRequiresExactTestMain(t *testing.T) {
	tests := []struct {
		name     string
		testMain string
		want     string
	}{
		{
			name: "missing",
			want: "lacks exact TestMain wrapper",
		},
		{
			name:     "malformed",
			testMain: "func TestMain(m *testing.M) { m.Run() }\n",
			want:     "must be exactly os.Exit(fixture.Run(m))",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeCoverageRepository(t, test.testMain)
			problems := validateRepositoryKindCoverage(root)
			if !errorsContain(problems, test.want) {
				t.Fatalf("coverage errors = %v", problems)
			}
		})
	}
}

func TestRepositoryFixtureKindCoverageRejectsNonportableOwnership(t *testing.T) {
	tests := []struct {
		filename string
		name     string
		prefix   string
		initBody string
		want     []string
	}{
		{
			filename: "fixture_test.go",
			name:     "go build tag with tab",
			prefix:   "//go:build\tlinux\n\n",
			initBody: `fixture.RegisterRunner("tools", "consumer/read", "read")`,
			want:     []string{"RegisterRunner in constrained test file", "TestMain in constrained test file"},
		},
		{
			filename: "fixture_test.go",
			name:     "legacy build tag without space",
			prefix:   "//+build linux\n\n",
			initBody: `fixture.RegisterRunner("tools", "consumer/read", "read")`,
			want:     []string{"RegisterRunner in constrained test file", "TestMain in constrained test file"},
		},
		{
			filename: "fixture_windows_test.go",
			name:     "GOOS suffix",
			initBody: `fixture.RegisterRunner("tools", "consumer/read", "read")`,
			want:     []string{"RegisterRunner in constrained test file", "TestMain in constrained test file"},
		},
		{
			filename: "fixture_test.go",
			name:     "after return",
			initBody: "return\nfixture.RegisterRunner(\"tools\", \"consumer/read\", \"read\")",
			want:     []string{"requires an init body containing only direct top-level RegisterRunner expression statements"},
		},
		{
			filename: "fixture_test.go",
			name:     "nested block",
			initBody: "{\nfixture.RegisterRunner(\"tools\", \"consumer/read\", \"read\")\n}",
			want:     []string{"requires an init body containing only direct top-level RegisterRunner expression statements"},
		},
		{
			filename: "fixture_test.go",
			name:     "labeled registration",
			initBody: "owner:\nfixture.RegisterRunner(\"tools\", \"consumer/read\", \"read\")",
			want:     []string{"requires an init body containing only direct top-level RegisterRunner expression statements"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := test.prefix + `package consumer
import (
    "os"
    "testing"
    "github.com/plasmid-dev/plasmid/internal/fixture"
)
func init() {
` + test.initBody + `
}
func TestMain(m *testing.M) { os.Exit(fixture.Run(m)) }
`
			root := writeCoverageSource(t, test.filename, source)
			problems := validateRepositoryKindCoverage(root)
			for _, want := range test.want {
				if !errorsContain(problems, want) {
					t.Fatalf("coverage errors = %v, want %q", problems, want)
				}
			}
		})
	}
}

func TestRepositoryFixtureKindCoverageSkipsGoIgnoredSources(t *testing.T) {
	root := writeCoverageRepository(t, "func TestMain(m *testing.M) { os.Exit(fixture.Run(m)) }\n")
	badSource := `package consumer
import "github.com/plasmid-dev/plasmid/internal/fixture"
func init() { fixture.RegisterRunner("tools", "phantom/read", "read") }
`
	for _, name := range []string{"_phantom_test.go", ".phantom_test.go"} {
		writeTestFile(t, filepath.Join(root, "consumer", name), badSource)
	}
	badDirectorySource := strings.Replace(badSource, "package consumer", "package phantom", 1)
	for _, name := range []string{"_phantom", ".phantom"} {
		directory := filepath.Join(root, "consumer", name)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(directory, "fixture_test.go"), badDirectorySource)
	}

	if problems := validateRepositoryKindCoverage(root); len(problems) != 0 {
		t.Fatalf("coverage errors = %v", problems)
	}
}

func writeCoverageRepository(t *testing.T, declarations string) string {
	source := `package consumer
import (
	"os"
    "testing"
    "github.com/plasmid-dev/plasmid/internal/fixture"
)
func init() { fixture.RegisterRunner("tools", "consumer/read", "read") }
func TestFixtureCoverage(t *testing.T) { fixture.AssertCoverage(t, "tools") }
` + declarations
	return writeCoverageSource(t, "fixture_test.go", source)
}

func writeCoverageSource(t *testing.T, filename, source string) string {
	t.Helper()
	root := t.TempDir()
	caseDir := filepath.Join(root, "testdata", "fixtures", "tools", "read-case")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(caseDir, "case.json"), `{"area":"tools","id":"read-case","kind":"read"}`)
	packageDir := filepath.Join(root, "consumer")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(packageDir, filename), source)
	return root
}

func TestDiscoverFixtureKindsRejectsMalformedEntries(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
		want  string
	}{
		{
			name: "file at fixture root",
			setup: func(t *testing.T, fixturesRoot string) {
				writeTestFile(t, filepath.Join(fixturesRoot, "stray.json"), "{}\n")
			},
			want: `fixture root contains non-area entry "stray.json"`,
		},
		{
			name: "malformed area name",
			setup: func(t *testing.T, fixturesRoot string) {
				if err := os.Mkdir(filepath.Join(fixturesRoot, "Bad_Area"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: `invalid fixture area "Bad_Area"`,
		},
		{
			name: "file in fixture area",
			setup: func(t *testing.T, fixturesRoot string) {
				area := filepath.Join(fixturesRoot, "tools")
				if err := os.Mkdir(area, 0o755); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(area, "stray.json"), "{}\n")
			},
			want: `fixture area "tools" contains non-case entry "stray.json"`,
		},
		{
			name: "malformed case name",
			setup: func(t *testing.T, fixturesRoot string) {
				if err := os.MkdirAll(filepath.Join(fixturesRoot, "tools", "Bad_Case"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: `invalid fixture case ID "Bad_Case"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			fixturesRoot := filepath.Join(root, "testdata", "fixtures")
			if err := os.MkdirAll(fixturesRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			test.setup(t, fixturesRoot)
			_, problems := discoverFixtureKinds(root)
			if !errorsContain(problems, test.want) {
				t.Fatalf("coverage errors = %v, want %q", problems, test.want)
			}
		})
	}
}

func TestDiscoverFixtureKindsRejectsSymlinks(t *testing.T) {
	tests := []struct {
		name string
		area string
		want string
	}{
		{name: "fixture root", want: `fixture root contains symlink entry "linked"`},
		{name: "fixture area", area: "tools", want: `fixture area "tools" contains symlink entry "linked"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			fixturesRoot := filepath.Join(root, "testdata", "fixtures")
			parent := fixturesRoot
			if test.area != "" {
				parent = filepath.Join(fixturesRoot, test.area)
			}
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			symlinkOrSkip(t, t.TempDir(), filepath.Join(parent, "linked"))
			_, problems := discoverFixtureKinds(root)
			if !errorsContain(problems, test.want) {
				t.Fatalf("coverage errors = %v, want %q", problems, test.want)
			}
		})
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate fixture coverage test")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

func validateRepositoryKindCoverage(root string) []error {
	caseKinds, problems := discoverFixtureKinds(root)
	runners, wrappers, owners, sourceProblems := discoverSourceRunners(root)
	problems = append(problems, sourceProblems...)
	registrations := make(map[runnerKey]sourceRunner, len(runners))
	coverage := make(map[string]map[string][]string)
	packages := make(map[string]string)
	for _, runner := range runners {
		key := runnerKey{area: runner.area, name: runner.name}
		if existing, duplicate := registrations[key]; duplicate {
			problems = append(problems, fmt.Errorf("duplicate fixture runner %q for area %q in %s and %s", runner.name, runner.area, existing.file, runner.file))
			continue
		}
		registrations[key] = runner
		packages[runner.packageKey] = runner.file
		for _, kind := range runner.kinds {
			if _, exists := caseKinds[runner.area][kind]; !exists {
				problems = append(problems, fmt.Errorf("fixture runner %q registers absent kind %q in area %q", runner.name, kind, runner.area))
				continue
			}
			if coverage[runner.area] == nil {
				coverage[runner.area] = make(map[string][]string)
			}
			coverage[runner.area][kind] = append(coverage[runner.area][kind], runner.name)
		}
	}
	for packageKey, file := range packages {
		if !wrappers[packageKey] {
			problems = append(problems, fmt.Errorf("fixture-owning test package in %s lacks exact TestMain wrapper os.Exit(fixture.Run(m))", filepath.Dir(file)))
		}
	}
	for area, kinds := range caseKinds {
		areaOwners := owners[area]
		switch len(areaOwners) {
		case 0:
			problems = append(problems, fmt.Errorf("fixture area %q has no AssertCoverage owner", area))
		case 1:
		default:
			files := make([]string, len(areaOwners))
			for index, owner := range areaOwners {
				files[index] = owner.file
			}
			sort.Strings(files)
			problems = append(problems, fmt.Errorf("fixture area %q has multiple AssertCoverage owners: %s", area, strings.Join(files, ", ")))
		}
		for kind := range kinds {
			if len(coverage[area][kind]) == 0 {
				problems = append(problems, fmt.Errorf("fixture kind %q has no named runner", area+"/"+kind))
			}
		}
	}
	for area, areaOwners := range owners {
		if _, exists := caseKinds[area]; exists {
			continue
		}
		for _, owner := range areaOwners {
			problems = append(problems, fmt.Errorf("fixture AssertCoverage owner in %s names absent area %q", owner.file, area))
		}
	}
	sort.Slice(problems, func(i, j int) bool { return problems[i].Error() < problems[j].Error() })
	return problems
}

func discoverFixtureKinds(root string) (map[string]map[string]struct{}, []error) {
	result := make(map[string]map[string]struct{})
	fixturesRoot := filepath.Join(root, "testdata", "fixtures")
	areas, err := os.ReadDir(fixturesRoot)
	if err != nil {
		return result, []error{fmt.Errorf("read fixture root: %w", err)}
	}
	var problems []error
	for _, areaEntry := range areas {
		if areaEntry.Type()&os.ModeSymlink != 0 {
			problems = append(problems, fmt.Errorf("fixture root contains symlink entry %q", areaEntry.Name()))
			continue
		}
		if !areaEntry.IsDir() {
			problems = append(problems, fmt.Errorf("fixture root contains non-area entry %q", areaEntry.Name()))
			continue
		}
		area := areaEntry.Name()
		if nameErr := validateName("area", area); nameErr != nil {
			problems = append(problems, nameErr)
			continue
		}
		cases, readErr := os.ReadDir(filepath.Join(fixturesRoot, area))
		if readErr != nil {
			problems = append(problems, fmt.Errorf("read fixture area %q: %w", area, readErr))
			continue
		}
		for _, caseEntry := range cases {
			if caseEntry.Type()&os.ModeSymlink != 0 {
				problems = append(problems, fmt.Errorf("fixture area %q contains symlink entry %q", area, caseEntry.Name()))
				continue
			}
			if !caseEntry.IsDir() {
				problems = append(problems, fmt.Errorf("fixture area %q contains non-case entry %q", area, caseEntry.Name()))
				continue
			}
			if nameErr := validateName("case ID", caseEntry.Name()); nameErr != nil {
				problems = append(problems, nameErr)
				continue
			}
			path := filepath.Join(fixturesRoot, area, caseEntry.Name(), "case.json")
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				problems = append(problems, fmt.Errorf("read %s: %w", path, readErr))
				continue
			}
			var metadata Metadata
			if decodeErr := json.Unmarshal(data, &metadata); decodeErr != nil {
				problems = append(problems, fmt.Errorf("decode %s: %w", path, decodeErr))
				continue
			}
			if metadata.Area != area || metadata.ID != caseEntry.Name() || metadata.Kind == "" {
				problems = append(problems, fmt.Errorf("fixture metadata mismatch in %s", path))
				continue
			}
			if result[area] == nil {
				result[area] = make(map[string]struct{})
			}
			result[area][metadata.Kind] = struct{}{}
		}
	}
	return result, problems
}

func discoverSourceRunners(root string) ([]sourceRunner, map[string]bool, map[string][]sourceAreaOwner, []error) {
	var runners []sourceRunner
	wrappers := make(map[string]bool)
	owners := make(map[string][]sourceAreaOwner)
	malformedTestMain := make(map[string]string)
	var problems []error
	packages := make(map[string][]*parsedSourceFile)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			problems = append(problems, walkErr)
			return nil
		}
		if path != root && (strings.HasPrefix(entry.Name(), ".") || strings.HasPrefix(entry.Name(), "_")) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == "vendor" || entry.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
		if parseErr != nil {
			problems = append(problems, fmt.Errorf("parse %s: %w", path, parseErr))
			return nil
		}
		parsed := &parsedSourceFile{
			constrained:    hasBuildConstraint(file) || hasPlatformTestSuffix(entry.Name()),
			fixtureAliases: importAliases(file, fixtureImportPath, "fixture"),
			osAliases:      importAliases(file, "os", "os"),
			path:           path,
			syntax:         file,
			testingAliases: importAliases(file, "testing", "testing"),
		}
		key := filepath.Dir(path) + "\x00" + file.Name.Name
		parsed.packageKey = key
		packages[key] = append(packages[key], parsed)
		return nil
	})
	if err != nil {
		problems = append(problems, err)
	}
	for packageKey, files := range packages {
		for _, file := range files {
			for _, declaration := range file.syntax.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Body == nil {
					continue
				}
				if function.Name.Name == "TestMain" {
					exact := isExactFixtureTestMain(function, file)
					if exact && file.constrained {
						problems = append(problems, fmt.Errorf("fixture TestMain in constrained test file %s is not portable", file.path))
					} else if !exact {
						malformedTestMain[packageKey] = file.path
					} else if wrappers[packageKey] {
						problems = append(problems, fmt.Errorf("duplicate fixture TestMain in package %s", filepath.Dir(file.path)))
					} else {
						wrappers[packageKey] = true
					}
				}
				validRegistrations := make(map[*ast.CallExpr]bool)
				if function.Name.Name == "init" {
					for _, call := range registrationOnlyInitCalls(function.Body, file.fixtureAliases) {
						validRegistrations[call] = true
					}
				}
				validCoverageCalls := make(map[*ast.CallExpr]bool)
				if parameter, runnable := runnableFixtureTest(function, file); runnable {
					for _, statement := range function.Body.List {
						expression, ok := statement.(*ast.ExprStmt)
						if !ok {
							continue
						}
						call, ok := expression.X.(*ast.CallExpr)
						if !ok || !isFixtureSelector(call.Fun, file.fixtureAliases, "AssertCoverage") || len(call.Args) != 2 {
							continue
						}
						argument, ok := call.Args[0].(*ast.Ident)
						if ok && argument.Name == parameter {
							validCoverageCalls[call] = true
						}
					}
				}
				ast.Inspect(function.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					if isFixtureSelector(call.Fun, file.fixtureAliases, "AssertCoverage") {
						if file.constrained {
							problems = append(problems, fmt.Errorf("fixture AssertCoverage in constrained test file %s is not portable", file.path))
							return true
						}
						if !validCoverageCalls[call] {
							problems = append(problems, fmt.Errorf("fixture AssertCoverage in %s must be a direct call in a runnable top-level test", file.path))
							return true
						}
						area, ok := "", false
						if len(call.Args) == 2 {
							area, ok = stringArgument(call.Args[1])
						}
						if !ok {
							problems = append(problems, fmt.Errorf("fixture AssertCoverage in %s must name one literal area", file.path))
							return true
						}
						owners[area] = append(owners[area], sourceAreaOwner{file: file.path})
						return true
					}
					if !isFixtureSelector(call.Fun, file.fixtureAliases, "RegisterRunner") {
						return true
					}
					if file.constrained {
						problems = append(problems, fmt.Errorf("fixture RegisterRunner in constrained test file %s is not portable", file.path))
						return true
					}
					if !validRegistrations[call] {
						problems = append(problems, fmt.Errorf("fixture RegisterRunner in %s requires an init body containing only direct top-level RegisterRunner expression statements", file.path))
						return true
					}
					values, valueErr := stringArguments(call.Args)
					if valueErr != nil || len(values) < 2 {
						problems = append(problems, fmt.Errorf("fixture RegisterRunner in %s must use literal area, runner, and kinds", file.path))
						return true
					}
					runners = append(runners, sourceRunner{
						area: values[0], name: values[1], kinds: values[2:], file: file.path, packageKey: file.packageKey,
					})
					return true
				})
			}
		}
	}
	for _, runner := range runners {
		if path := malformedTestMain[runner.packageKey]; path != "" {
			problems = append(problems, fmt.Errorf("fixture TestMain in %s must be exactly os.Exit(fixture.Run(m))", path))
			wrappers[runner.packageKey] = true
			delete(malformedTestMain, runner.packageKey)
		}
	}
	return runners, wrappers, owners, problems
}

func runnableFixtureTest(function *ast.FuncDecl, file *parsedSourceFile) (string, bool) {
	if function.Recv != nil || function.Type.Results != nil || function.Type.TypeParams != nil ||
		len(function.Type.Params.List) != 1 || !strings.HasPrefix(function.Name.Name, "Test") || len(function.Name.Name) == len("Test") {
		return "", false
	}
	first := []rune(strings.TrimPrefix(function.Name.Name, "Test"))[0]
	if unicode.IsLower(first) {
		return "", false
	}
	parameter := function.Type.Params.List[0]
	if len(parameter.Names) != 1 || !isImportedPointerType(parameter.Type, file.testingAliases, "T") {
		return "", false
	}
	return parameter.Names[0].Name, true
}

func isExactFixtureTestMain(function *ast.FuncDecl, file *parsedSourceFile) bool {
	if function.Recv != nil || function.Type.Results != nil || len(function.Type.Params.List) != 1 || len(function.Body.List) != 1 {
		return false
	}
	parameter := function.Type.Params.List[0]
	if len(parameter.Names) != 1 || !isImportedPointerType(parameter.Type, file.testingAliases, "M") {
		return false
	}
	expression, ok := function.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	exitCall, ok := expression.X.(*ast.CallExpr)
	if !ok || !isImportedSelector(exitCall.Fun, file.osAliases, "Exit") || len(exitCall.Args) != 1 {
		return false
	}
	runCall, ok := exitCall.Args[0].(*ast.CallExpr)
	if !ok || !isImportedSelector(runCall.Fun, file.fixtureAliases, "Run") || len(runCall.Args) != 1 {
		return false
	}
	argument, ok := runCall.Args[0].(*ast.Ident)
	return ok && argument.Name == parameter.Names[0].Name
}

func isImportedPointerType(expression ast.Expr, aliases map[string]bool, name string) bool {
	pointer, ok := expression.(*ast.StarExpr)
	if !ok {
		return false
	}
	return isImportedSelector(pointer.X, aliases, name)
}

func isImportedSelector(expression ast.Expr, aliases map[string]bool, name string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && aliases[identifier.Name]
}

func registrationOnlyInitCalls(body *ast.BlockStmt, aliases map[string]bool) []*ast.CallExpr {
	var calls []*ast.CallExpr
	for _, statement := range body.List {
		expression, ok := statement.(*ast.ExprStmt)
		if !ok {
			return nil
		}
		call, ok := expression.X.(*ast.CallExpr)
		if !ok || !isFixtureSelector(call.Fun, aliases, "RegisterRunner") {
			return nil
		}
		calls = append(calls, call)
	}
	return calls
}

func hasBuildConstraint(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			continue
		}
		for _, comment := range group.List {
			if constraint.IsGoBuild(comment.Text) || constraint.IsPlusBuild(comment.Text) {
				return true
			}
		}
	}
	return false
}

func hasPlatformTestSuffix(name string) bool {
	base := strings.TrimSuffix(name, "_test.go")
	parts := strings.Split(base, "_")
	if len(parts) < 2 {
		return false
	}
	platforms := map[string]bool{
		"386": true, "aix": true, "amd64": true, "android": true, "arm": true, "arm64": true,
		"darwin": true, "dragonfly": true, "freebsd": true, "hurd": true, "illumos": true, "ios": true,
		"js": true, "linux": true, "loong64": true, "mips": true, "mips64": true, "mips64le": true,
		"mipsle": true, "netbsd": true, "openbsd": true, "plan9": true, "ppc64": true, "ppc64le": true,
		"riscv64": true, "s390x": true, "solaris": true, "wasip1": true, "wasm": true, "windows": true,
	}
	return platforms[parts[len(parts)-1]]
}

func fixtureSelector(expression ast.Expr, aliases map[string]bool) (*ast.SelectorExpr, bool) {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return selector, ok && aliases[identifier.Name]
}

func isFixtureSelector(expression ast.Expr, aliases map[string]bool, name string) bool {
	selector, ok := fixtureSelector(expression, aliases)
	return ok && selector.Sel.Name == name
}

func importAliases(file *ast.File, importPath, defaultName string) map[string]bool {
	aliases := make(map[string]bool)
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		name := defaultName
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name != "." && name != "_" {
			aliases[name] = true
		}
	}
	return aliases
}

func stringArguments(arguments []ast.Expr) ([]string, error) {
	values := make([]string, len(arguments))
	for index, argument := range arguments {
		value, ok := stringArgument(argument)
		if !ok {
			return nil, fmt.Errorf("argument %d is not a string literal", index)
		}
		values[index] = value
	}
	return values, nil
}

func stringArgument(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

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
	coverage, packages, runnerProblems := indexSourceRunners(runners, caseKinds)
	problems = append(problems, runnerProblems...)
	problems = append(problems, validateFixturePackages(packages, wrappers)...)
	problems = append(problems, validateFixtureOwners(caseKinds, coverage, owners)...)
	problems = append(problems, validateOrphanFixtureOwners(caseKinds, owners)...)
	sort.Slice(problems, func(i, j int) bool { return problems[i].Error() < problems[j].Error() })
	return problems
}

func indexSourceRunners(runners []sourceRunner, caseKinds map[string]map[string]struct{}) (map[string]map[string][]string, map[string]string, []error) {
	registrations := make(map[runnerKey]sourceRunner, len(runners))
	coverage := make(map[string]map[string][]string)
	packages := make(map[string]string)
	var problems []error
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
	return coverage, packages, problems
}

func validateFixturePackages(packages map[string]string, wrappers map[string]bool) []error {
	var problems []error
	for packageKey, file := range packages {
		if !wrappers[packageKey] {
			problems = append(problems, fmt.Errorf("fixture-owning test package in %s lacks exact TestMain wrapper os.Exit(fixture.Run(m))", filepath.Dir(file)))
		}
	}
	return problems
}

func validateFixtureOwners(caseKinds map[string]map[string]struct{}, coverage map[string]map[string][]string, owners map[string][]sourceAreaOwner) []error {
	var problems []error
	for area, kinds := range caseKinds {
		if err := validateAreaOwner(area, owners[area]); err != nil {
			problems = append(problems, err)
		}
		for kind := range kinds {
			if len(coverage[area][kind]) == 0 {
				problems = append(problems, fmt.Errorf("fixture kind %q has no named runner", area+"/"+kind))
			}
		}
	}
	return problems
}

func validateAreaOwner(area string, owners []sourceAreaOwner) error {
	switch len(owners) {
	case 0:
		return fmt.Errorf("fixture area %q has no AssertCoverage owner", area)
	case 1:
		return nil
	default:
		files := make([]string, len(owners))
		for index, owner := range owners {
			files[index] = owner.file
		}
		sort.Strings(files)
		return fmt.Errorf("fixture area %q has multiple AssertCoverage owners: %s", area, strings.Join(files, ", "))
	}
}

func validateOrphanFixtureOwners(caseKinds map[string]map[string]struct{}, owners map[string][]sourceAreaOwner) []error {
	var problems []error
	for area, areaOwners := range owners {
		if _, exists := caseKinds[area]; exists {
			continue
		}
		for _, owner := range areaOwners {
			problems = append(problems, fmt.Errorf("fixture AssertCoverage owner in %s names absent area %q", owner.file, area))
		}
	}
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
		area, err := discoveredAreaName(areaEntry)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		kinds, areaProblems := discoverAreaKinds(fixturesRoot, area)
		problems = append(problems, areaProblems...)
		if len(kinds) != 0 {
			result[area] = kinds
		}
	}
	return result, problems
}

func discoveredAreaName(entry fs.DirEntry) (string, error) {
	if entry.Type()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("fixture root contains symlink entry %q", entry.Name())
	}
	if !entry.IsDir() {
		return "", fmt.Errorf("fixture root contains non-area entry %q", entry.Name())
	}
	if err := validateName("area", entry.Name()); err != nil {
		return "", err
	}
	return entry.Name(), nil
}

func discoverAreaKinds(fixturesRoot, area string) (map[string]struct{}, []error) {
	result := make(map[string]struct{})
	cases, err := os.ReadDir(filepath.Join(fixturesRoot, area))
	if err != nil {
		return result, []error{fmt.Errorf("read fixture area %q: %w", area, err)}
	}
	var problems []error
	for _, entry := range cases {
		kind, err := discoveredCaseKind(fixturesRoot, area, entry)
		if err != nil {
			problems = append(problems, err)
		} else {
			result[kind] = struct{}{}
		}
	}
	return result, problems
}

func discoveredCaseKind(fixturesRoot, area string, entry fs.DirEntry) (string, error) {
	if entry.Type()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("fixture area %q contains symlink entry %q", area, entry.Name())
	}
	if !entry.IsDir() {
		return "", fmt.Errorf("fixture area %q contains non-case entry %q", area, entry.Name())
	}
	if err := validateName("case ID", entry.Name()); err != nil {
		return "", err
	}
	path := filepath.Join(fixturesRoot, area, entry.Name(), caseMetadataName)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("decode %s: %w", path, err)
	}
	if metadata.Area != area || metadata.ID != entry.Name() || metadata.Kind == "" {
		return "", fmt.Errorf("fixture metadata mismatch in %s", path)
	}
	return metadata.Kind, nil
}

func discoverSourceRunners(root string) ([]sourceRunner, map[string]bool, map[string][]sourceAreaOwner, []error) {
	discovery := sourceDiscovery{
		wrappers: make(map[string]bool), owners: make(map[string][]sourceAreaOwner),
		malformedTestMain: make(map[string]string), packages: make(map[string][]*parsedSourceFile),
	}
	discovery.collectPackages(root)
	discovery.inspectPackages()
	discovery.recordMalformedTestMains()
	return discovery.runners, discovery.wrappers, discovery.owners, discovery.problems
}

type sourceDiscovery struct {
	runners           []sourceRunner
	wrappers          map[string]bool
	owners            map[string][]sourceAreaOwner
	malformedTestMain map[string]string
	problems          []error
	packages          map[string][]*parsedSourceFile
}

func (d *sourceDiscovery) collectPackages(root string) {
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		return d.visitSourcePath(root, path, entry, walkErr)
	}); err != nil {
		d.problems = append(d.problems, err)
	}
}

func (d *sourceDiscovery) visitSourcePath(root, path string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		d.problems = append(d.problems, walkErr)
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
	if strings.HasSuffix(entry.Name(), "_test.go") {
		d.parseSourceFile(path, entry.Name())
	}
	return nil
}

func (d *sourceDiscovery) parseSourceFile(path, name string) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
	if err != nil {
		d.problems = append(d.problems, fmt.Errorf("parse %s: %w", path, err))
		return
	}
	parsed := &parsedSourceFile{
		constrained:    hasBuildConstraint(file) || hasPlatformTestSuffix(name),
		fixtureAliases: importAliases(file, fixtureImportPath, "fixture"),
		osAliases:      importAliases(file, "os", "os"),
		path:           path,
		syntax:         file,
		testingAliases: importAliases(file, "testing", "testing"),
	}
	parsed.packageKey = filepath.Dir(path) + "\x00" + file.Name.Name
	d.packages[parsed.packageKey] = append(d.packages[parsed.packageKey], parsed)
}

func (d *sourceDiscovery) inspectPackages() {
	for packageKey, files := range d.packages {
		for _, file := range files {
			d.inspectSourceFile(packageKey, file)
		}
	}
}

func (d *sourceDiscovery) inspectSourceFile(packageKey string, file *parsedSourceFile) {
	for _, declaration := range file.syntax.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Body != nil {
			d.inspectSourceFunction(packageKey, file, function)
		}
	}
}

func (d *sourceDiscovery) inspectSourceFunction(packageKey string, file *parsedSourceFile, function *ast.FuncDecl) {
	if function.Name.Name == "TestMain" {
		d.recordTestMain(packageKey, file, function)
	}
	validRegistrations := directRegistrationCalls(function, file)
	validCoverage := directCoverageCalls(function, file)
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			d.inspectFixtureCall(file, call, validRegistrations, validCoverage)
		}
		return true
	})
}

func (d *sourceDiscovery) recordTestMain(packageKey string, file *parsedSourceFile, function *ast.FuncDecl) {
	exact := isExactFixtureTestMain(function, file)
	switch {
	case exact && file.constrained:
		d.problems = append(d.problems, fmt.Errorf("fixture TestMain in constrained test file %s is not portable", file.path))
	case !exact:
		d.malformedTestMain[packageKey] = file.path
	case d.wrappers[packageKey]:
		d.problems = append(d.problems, fmt.Errorf("duplicate fixture TestMain in package %s", filepath.Dir(file.path)))
	default:
		d.wrappers[packageKey] = true
	}
}

func directRegistrationCalls(function *ast.FuncDecl, file *parsedSourceFile) map[*ast.CallExpr]bool {
	valid := make(map[*ast.CallExpr]bool)
	if function.Name.Name == "init" {
		for _, call := range registrationOnlyInitCalls(function.Body, file.fixtureAliases) {
			valid[call] = true
		}
	}
	return valid
}

func directCoverageCalls(function *ast.FuncDecl, file *parsedSourceFile) map[*ast.CallExpr]bool {
	valid := make(map[*ast.CallExpr]bool)
	parameter, runnable := runnableFixtureTest(function, file)
	if !runnable {
		return valid
	}
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
			valid[call] = true
		}
	}
	return valid
}

func (d *sourceDiscovery) inspectFixtureCall(file *parsedSourceFile, call *ast.CallExpr, validRegistrations, validCoverage map[*ast.CallExpr]bool) {
	if isFixtureSelector(call.Fun, file.fixtureAliases, "AssertCoverage") {
		d.recordCoverageCall(file, call, validCoverage[call])
		return
	}
	if isFixtureSelector(call.Fun, file.fixtureAliases, "RegisterRunner") {
		d.recordRegistrationCall(file, call, validRegistrations[call])
	}
}

func (d *sourceDiscovery) recordCoverageCall(file *parsedSourceFile, call *ast.CallExpr, direct bool) {
	if file.constrained {
		d.problems = append(d.problems, fmt.Errorf("fixture AssertCoverage in constrained test file %s is not portable", file.path))
		return
	}
	if !direct {
		d.problems = append(d.problems, fmt.Errorf("fixture AssertCoverage in %s must be a direct call in a runnable top-level test", file.path))
		return
	}
	area, ok := literalCoverageArea(call)
	if !ok {
		d.problems = append(d.problems, fmt.Errorf("fixture AssertCoverage in %s must name one literal area", file.path))
		return
	}
	d.owners[area] = append(d.owners[area], sourceAreaOwner{file: file.path})
}

func literalCoverageArea(call *ast.CallExpr) (string, bool) {
	if len(call.Args) != 2 {
		return "", false
	}
	return stringArgument(call.Args[1])
}

func (d *sourceDiscovery) recordRegistrationCall(file *parsedSourceFile, call *ast.CallExpr, direct bool) {
	if file.constrained {
		d.problems = append(d.problems, fmt.Errorf("fixture RegisterRunner in constrained test file %s is not portable", file.path))
		return
	}
	if !direct {
		d.problems = append(d.problems, fmt.Errorf("fixture RegisterRunner in %s requires an init body containing only direct top-level RegisterRunner expression statements", file.path))
		return
	}
	values, err := stringArguments(call.Args)
	if err != nil || len(values) < 2 {
		d.problems = append(d.problems, fmt.Errorf("fixture RegisterRunner in %s must use literal area, runner, and kinds", file.path))
		return
	}
	d.runners = append(d.runners, sourceRunner{
		area: values[0], name: values[1], kinds: values[2:], file: file.path, packageKey: file.packageKey,
	})
}

func (d *sourceDiscovery) recordMalformedTestMains() {
	for _, runner := range d.runners {
		path := d.malformedTestMain[runner.packageKey]
		if path == "" {
			continue
		}
		d.problems = append(d.problems, fmt.Errorf("fixture TestMain in %s must be exactly os.Exit(fixture.Run(m))", path))
		d.wrappers[runner.packageKey] = true
		delete(d.malformedTestMain, runner.packageKey)
	}
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

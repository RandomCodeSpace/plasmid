package foreign

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDiscoveryHasNoExecutionOrNetworkImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		files = append(files, filepath.Clean(entry.Name()))
	}
	findings, err := inspectDiscoveryCapabilities(files)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		t.Error(finding)
	}
}

func TestDiscoveryDependencyClosureHasNoExecutionOrNetworkPackages(t *testing.T) {
	moduleCommand := exec.Command("go", "list", "-m", "-f", "{{.Path}}")
	moduleCommand.Env = os.Environ()
	moduleOutput, err := moduleCommand.Output()
	if err != nil {
		t.Fatal(err)
	}
	modulePath := strings.TrimSpace(string(moduleOutput))
	buildContexts := []struct{ goos, goarch string }{
		{goos: "linux", goarch: "amd64"},
		{goos: "linux", goarch: "arm64"},
		{goos: "windows", goarch: "amd64"},
		{goos: "windows", goarch: "arm64"},
		{goos: "darwin", goarch: "amd64"},
		{goos: "darwin", goarch: "arm64"},
	}
	for _, buildContext := range buildContexts {
		findings, err := auditDiscoveryClosure(".", modulePath, buildContext.goos, buildContext.goarch)
		if err != nil {
			t.Fatalf("%s/%s: %v", buildContext.goos, buildContext.goarch, err)
		}
		for _, finding := range findings {
			t.Errorf("%s/%s: %s", buildContext.goos, buildContext.goarch, finding)
		}
	}
}

func TestDiscoveryCapabilityInspectionRejectsImportAndSelectorBypasses(t *testing.T) {
	cases := map[string]string{
		"aliased start process": "package bypass\nimport operatingSystem \"os\"\nvar _ = operatingSystem.StartProcess\n",
		"execution import":      "package bypass\nimport \"os/exec\"\nvar _ = exec.Command\n",
		"plugin import":         "package bypass\nimport \"plugin\"\nvar _ = plugin.Open\n",
		"syscall import":        "package bypass\nimport \"syscall\"\nvar _ = syscall.Exec\n",
		"unsafe import":         "package bypass\nimport \"unsafe\"\nvar _ unsafe.Pointer\n",
		"network import":        "package bypass\nimport \"net/http\"\nvar _ = http.Get\n",
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bypass.go")
			if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			findings, err := inspectDiscoveryCapabilities([]string{path})
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) == 0 {
				t.Fatal("bypass was not rejected")
			}
		})
	}
}

func TestDiscoveryClosureRejectsForbiddenExternalDependencies(t *testing.T) {
	for _, importPath := range []string{"net/http", "os/exec", "plugin"} {
		if !forbiddenDiscoveryDependency(importPath) {
			t.Errorf("dependency %q was not rejected", importPath)
		}
	}
	for _, importPath := range []string{"encoding/json", "syscall", "unsafe"} {
		if forbiddenDiscoveryDependency(importPath) {
			t.Errorf("dependency %q was rejected", importPath)
		}
	}
}

func TestProductionGoFilesIncludeWindowsAndCgoVariants(t *testing.T) {
	directory := t.TempDir()
	files := map[string]string{
		"safe.go":           "package bypass\n",
		"helper_windows.go": "//go:build windows\n\npackage bypass\nimport operatingSystem \"os\"\nvar _ = operatingSystem.StartProcess\n",
		"helper_cgo.go":     "//go:build cgo\n\npackage bypass\n/*\n#include <stdlib.h>\n*/\nimport \"C\"\nfunc run() { C.system(nil) }\n",
		"ignored_test.go":   "package bypass\nimport \"net/http\"\nvar _ = http.Get\n",
	}
	for name, source := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	production, err := productionGoFiles(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(production) != 3 {
		t.Fatalf("production files = %v", production)
	}
	findings, err := inspectDiscoveryCapabilities(production)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %v", findings)
	}
}

func TestDiscoveryClosureRejectsThirdPartyProcessBypass(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                    "module example.com/root\n\ngo 1.26\n\nrequire corp/helper v0.0.0\nreplace corp/helper => ./helper\n",
		"foreign/foreign.go":        "package foreign\nimport _ \"corp/helper\"\n",
		"helper/go.mod":             "module corp/helper\n\ngo 1.26\n",
		"helper/process_windows.go": "//go:build windows\n\npackage helper\nimport operatingSystem \"os\"\nvar _ = operatingSystem.StartProcess\n",
		"helper/process.go":         "//go:build !windows\n\npackage helper\n",
	}
	for name, source := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	findings, err := auditDiscoveryClosure(filepath.Join(root, "foreign"), "example.com/root", "windows", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0], "third-party dependency") {
		t.Fatalf("findings = %v", findings)
	}
}

func TestDiscoveryClosureFollowsCgoOnlySameModuleHelpers(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                 "module example.com/root\n\ngo 1.26\n",
		"foreign/foreign.go":     "package foreign\n",
		"foreign/foreign_cgo.go": "//go:build cgo\n\npackage foreign\nimport _ \"example.com/root/helper\"\n",
		"helper/helper.go":       "//go:build !cgo\n\npackage helper\n",
		"helper/helper_cgo.go":   "//go:build cgo\n\npackage helper\n/*\n#include <stdlib.h>\n*/\nimport \"C\"\nimport (\n\t\"net/http\"\n\toperatingSystem \"os\"\n)\nvar _ = http.Get\nvar _ = operatingSystem.StartProcess\n",
	}
	for name, source := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	findings, err := auditDiscoveryClosure(filepath.Join(root, "foreign"), "example.com/root", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) < 3 {
		t.Fatalf("findings = %v", findings)
	}
}

func TestDiscoveryClosureRejectsCgoOnlyNestedModuleHelper(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                 "module example.com/root\n\ngo 1.26\n",
		"foreign/foreign.go":     "package foreign\n",
		"foreign/foreign_cgo.go": "//go:build cgo\n\npackage foreign\nimport _ \"example.com/root/helper\"\n",
		"helper/go.mod":          "module example.com/root/helper\n\ngo 1.26\n",
		"helper/helper_cgo.go":   "//go:build cgo\n\npackage helper\nimport operatingSystem \"os\"\nvar _ = operatingSystem.StartProcess\n",
	}
	for name, source := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	findings, err := auditDiscoveryClosure(filepath.Join(root, "foreign"), "example.com/root", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	want := `closure includes third-party dependency "example.com/root/helper"`
	if len(findings) != 1 || findings[0] != want {
		t.Fatalf("findings = %v, want %q", findings, want)
	}
}

func inspectDiscoveryCapabilities(files []string) ([]string, error) {
	findings := []string{}
	for _, name := range files {
		fileFindings, err := inspectDiscoveryFile(name)
		if err != nil {
			return nil, err
		}
		findings = append(findings, fileFindings...)
	}
	return findings, nil
}

func inspectDiscoveryFile(name string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
	if err != nil {
		return nil, err
	}
	aliases, dotOS, findings, err := discoveryImportCapabilities(name, file)
	if err != nil {
		return nil, err
	}
	ast.Inspect(file, func(node ast.Node) bool {
		if selectsStartProcess(node, aliases, dotOS) {
			findings = append(findings, fmt.Sprintf("%s selects forbidden discovery capability os.StartProcess", name))
		}
		return true
	})
	return findings, nil
}

func discoveryImportCapabilities(name string, file *ast.File) (map[string]bool, bool, []string, error) {
	aliases := make(map[string]bool)
	dotOS := false
	findings := []string{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, false, nil, err
		}
		if forbiddenDiscoveryImport(importPath) {
			findings = append(findings, fmt.Sprintf("%s imports forbidden discovery capability %q", name, importPath))
		}
		if importPath == "os" {
			alias := pathpkg.Base(importPath)
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			dotOS = dotOS || alias == "."
			if alias != "." && alias != "_" {
				aliases[alias] = true
			}
		}
	}
	return aliases, dotOS, findings, nil
}

func selectsStartProcess(node ast.Node, aliases map[string]bool, dotOS bool) bool {
	switch expression := node.(type) {
	case *ast.SelectorExpr:
		identifier, ok := expression.X.(*ast.Ident)
		return ok && aliases[identifier.Name] && expression.Sel.Name == "StartProcess"
	case *ast.Ident:
		return dotOS && expression.Name == "StartProcess"
	default:
		return false
	}
}

func forbiddenDiscoveryImport(importPath string) bool {
	return importPath == "C" || importPath == "os/exec" || importPath == "plugin" || importPath == "syscall" || importPath == "unsafe" || importPath == "net" || strings.HasPrefix(importPath, "net/")
}

func forbiddenDiscoveryDependency(importPath string) bool {
	return importPath == "os/exec" || importPath == "plugin" || importPath == "net" || strings.HasPrefix(importPath, "net/")
}

type discoveryListPackage struct {
	ImportPath string
	Dir        string
	Standard   bool
	Module     *struct{ Path string }
}

func auditDiscoveryClosure(directory, modulePath, goos, goarch string) ([]string, error) {
	moduleOutput, err := discoveryCommand(directory, goos, goarch, "list", "-m", "-f", "{{.Dir}}")
	if err != nil {
		return nil, err
	}
	moduleDirectory := strings.TrimSpace(string(moduleOutput))
	goRootOutput, err := discoveryCommand(directory, goos, goarch, "env", "GOROOT")
	if err != nil {
		return nil, err
	}
	goRoot := strings.TrimSpace(string(goRootOutput))
	output, err := discoveryCommand(directory, goos, goarch, "list", "-deps", "-json", ".")
	if err != nil {
		return nil, err
	}
	findings, err := auditListedDiscoveryPackages(output, modulePath)
	if err != nil {
		return nil, err
	}
	sourceFindings, err := auditLocalDiscoverySources(directory, moduleDirectory, modulePath, goRoot)
	if err != nil {
		return nil, err
	}
	return uniqueFindings(append(findings, sourceFindings...)), nil
}

func discoveryCommand(directory, goos, goarch string, arguments ...string) ([]byte, error) {
	command := exec.Command("go", arguments...)
	command.Dir = directory
	command.Env = discoveryBuildEnvironment(goos, goarch)
	return command.Output()
}

func auditListedDiscoveryPackages(output []byte, modulePath string) ([]string, error) {
	findings := []string{}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for decoder.More() {
		var item discoveryListPackage
		if err := decoder.Decode(&item); err != nil {
			return nil, err
		}
		itemFindings, err := auditListedDiscoveryPackage(item, modulePath)
		if err != nil {
			return nil, err
		}
		findings = append(findings, itemFindings...)
	}
	return findings, nil
}

func auditListedDiscoveryPackage(item discoveryListPackage, modulePath string) ([]string, error) {
	findings := []string{}
	if forbiddenDiscoveryDependency(item.ImportPath) {
		findings = append(findings, fmt.Sprintf("closure includes forbidden discovery dependency %q", item.ImportPath))
	}
	if item.Standard {
		return findings, nil
	}
	if item.Module == nil || item.Module.Path != modulePath {
		return append(findings, fmt.Sprintf("closure includes third-party dependency %q", item.ImportPath)), nil
	}
	files, err := productionGoFiles(item.Dir)
	if err != nil {
		return nil, err
	}
	capabilities, err := inspectDiscoveryCapabilities(files)
	for _, capability := range capabilities {
		findings = append(findings, fmt.Sprintf("%s: %s", item.ImportPath, capability))
	}
	return findings, err
}

func auditLocalDiscoverySources(startDirectory, moduleDirectory, modulePath, goRoot string) ([]string, error) {
	startDirectory, err := canonicalAuditDirectory(startDirectory)
	if err != nil {
		return nil, err
	}
	moduleDirectory, err = canonicalAuditDirectory(moduleDirectory)
	if err != nil {
		return nil, err
	}
	audit := localDiscoveryAudit{moduleDirectory: moduleDirectory, modulePath: modulePath, goRoot: goRoot, queue: []string{startDirectory}, visited: make(map[string]bool)}
	return audit.run()
}

func canonicalAuditDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

type localDiscoveryAudit struct {
	moduleDirectory string
	modulePath      string
	goRoot          string
	queue           []string
	visited         map[string]bool
	findings        []string
}

func (a *localDiscoveryAudit) run() ([]string, error) {
	for len(a.queue) > 0 {
		directory := filepath.Clean(a.queue[0])
		a.queue = a.queue[1:]
		if a.visited[directory] {
			continue
		}
		a.visited[directory] = true
		if err := a.auditDirectory(directory); err != nil {
			return nil, err
		}
	}
	return uniqueFindings(a.findings), nil
}

func (a *localDiscoveryAudit) auditDirectory(directory string) error {
	packagePath, err := a.packagePath(directory)
	if err != nil {
		return err
	}
	files, err := productionGoFiles(directory)
	if err != nil {
		return err
	}
	capabilities, err := inspectDiscoveryCapabilities(files)
	for _, capability := range capabilities {
		a.findings = append(a.findings, fmt.Sprintf("%s: %s", packagePath, capability))
	}
	if err != nil {
		return err
	}
	imports, err := discoverySourceImports(files)
	if err != nil {
		return err
	}
	for _, importPath := range imports {
		if err := a.auditImport(importPath); err != nil {
			return err
		}
	}
	return nil
}

func (a *localDiscoveryAudit) packagePath(directory string) (string, error) {
	relative, err := filepath.Rel(a.moduleDirectory, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("module-local discovery import escapes module root: %s", directory)
	}
	if relative == "." {
		return a.modulePath, nil
	}
	return a.modulePath + "/" + filepath.ToSlash(relative), nil
}

func (a *localDiscoveryAudit) auditImport(importPath string) error {
	if importPath == "C" {
		return nil
	}
	if importPath != a.modulePath && !strings.HasPrefix(importPath, a.modulePath+"/") {
		if !standardLibraryImport(a.goRoot, importPath) {
			a.findings = append(a.findings, fmt.Sprintf("closure includes third-party dependency %q", importPath))
		}
		return nil
	}
	return a.auditLocalImport(importPath)
}

func (a *localDiscoveryAudit) auditLocalImport(importPath string) error {
	relative := strings.TrimPrefix(strings.TrimPrefix(importPath, a.modulePath), "/")
	target := filepath.Join(a.moduleDirectory, filepath.FromSlash(relative))
	resolved, err := filepath.EvalSymlinks(target)
	if err == nil {
		if info, statErr := os.Stat(resolved); statErr != nil || !info.IsDir() {
			err = errors.New("not a directory")
		}
	}
	if err != nil {
		a.findings = append(a.findings, fmt.Sprintf("module-local discovery dependency %q is unavailable", importPath))
		return nil
	}
	nested, err := nestedModuleBoundary(a.moduleDirectory, resolved)
	if err != nil {
		return err
	}
	if nested {
		a.findings = append(a.findings, fmt.Sprintf("closure includes third-party dependency %q", importPath))
		return nil
	}
	a.queue = append(a.queue, resolved)
	return nil
}

func nestedModuleBoundary(moduleDirectory, target string) (bool, error) {
	moduleDirectory = filepath.Clean(moduleDirectory)
	for directory := filepath.Clean(target); directory != moduleDirectory; directory = filepath.Dir(directory) {
		relative, err := filepath.Rel(moduleDirectory, directory)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return false, fmt.Errorf("module-local discovery import escapes module root: %s", target)
		}
		info, err := os.Stat(filepath.Join(directory, "go.mod"))
		switch {
		case err == nil && !info.IsDir():
			return true, nil
		case err != nil && !os.IsNotExist(err):
			return false, err
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return false, fmt.Errorf("module root %s is not an ancestor of %s", moduleDirectory, target)
		}
	}
	return false, nil
}

func discoverySourceImports(files []string) ([]string, error) {
	imports := []string{}
	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return nil, err
			}
			imports = append(imports, importPath)
		}
	}
	return imports, nil
}

func standardLibraryImport(goRoot, importPath string) bool {
	standardRoot := filepath.Join(goRoot, "src")
	target := filepath.Join(standardRoot, filepath.FromSlash(importPath))
	relative, err := filepath.Rel(standardRoot, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	info, err := os.Stat(target)
	return err == nil && info.IsDir()
}

func uniqueFindings(findings []string) []string {
	result := make([]string, 0, len(findings))
	seen := make(map[string]bool)
	for _, finding := range findings {
		if !seen[finding] {
			result = append(result, finding)
			seen[finding] = true
		}
	}
	return result
}

func productionGoFiles(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	files := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		files = append(files, filepath.Join(directory, entry.Name()))
	}
	return files, nil
}

func discoveryBuildEnvironment(goos, goarch string) []string {
	environment := []string{}
	for _, variable := range os.Environ() {
		if strings.HasPrefix(variable, "GOOS=") || strings.HasPrefix(variable, "GOARCH=") || strings.HasPrefix(variable, "CGO_ENABLED=") || strings.HasPrefix(variable, "GOPACKAGESDRIVER=") {
			continue
		}
		environment = append(environment, variable)
	}
	return append(environment, "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0", "GOPACKAGESDRIVER=off")
}

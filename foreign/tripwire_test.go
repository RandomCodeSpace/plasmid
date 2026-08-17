package foreign

import (
	"encoding/json"
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
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			return nil, err
		}
		osAliases := make(map[string]bool)
		dotOS := false
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return nil, err
			}
			if forbiddenDiscoveryImport(importPath) {
				findings = append(findings, fmt.Sprintf("%s imports forbidden discovery capability %q", name, importPath))
			}
			if importPath != "os" {
				continue
			}
			alias := pathpkg.Base(importPath)
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			switch alias {
			case ".":
				dotOS = true
			case "_":
			default:
				osAliases[alias] = true
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch expression := node.(type) {
			case *ast.SelectorExpr:
				identifier, ok := expression.X.(*ast.Ident)
				if ok && osAliases[identifier.Name] && expression.Sel.Name == "StartProcess" {
					findings = append(findings, fmt.Sprintf("%s selects forbidden discovery capability os.StartProcess", name))
				}
			case *ast.Ident:
				if dotOS && expression.Name == "StartProcess" {
					findings = append(findings, fmt.Sprintf("%s selects forbidden discovery capability os.StartProcess", name))
				}
			}
			return true
		})
	}
	return findings, nil
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
	moduleCommand := exec.Command("go", "list", "-m", "-f", "{{.Dir}}")
	moduleCommand.Dir = directory
	moduleCommand.Env = discoveryBuildEnvironment(goos, goarch)
	moduleOutput, err := moduleCommand.Output()
	if err != nil {
		return nil, err
	}
	moduleDirectory := strings.TrimSpace(string(moduleOutput))
	goRootCommand := exec.Command("go", "env", "GOROOT")
	goRootCommand.Dir = directory
	goRootCommand.Env = discoveryBuildEnvironment(goos, goarch)
	goRootOutput, err := goRootCommand.Output()
	if err != nil {
		return nil, err
	}
	goRoot := strings.TrimSpace(string(goRootOutput))
	command := exec.Command("go", "list", "-deps", "-json", ".")
	command.Dir = directory
	command.Env = discoveryBuildEnvironment(goos, goarch)
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	findings := []string{}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for decoder.More() {
		var item discoveryListPackage
		if err := decoder.Decode(&item); err != nil {
			return nil, err
		}
		if forbiddenDiscoveryDependency(item.ImportPath) {
			findings = append(findings, fmt.Sprintf("closure includes forbidden discovery dependency %q", item.ImportPath))
		}
		if item.Standard {
			continue
		}
		if item.Module == nil || item.Module.Path != modulePath {
			findings = append(findings, fmt.Sprintf("closure includes third-party dependency %q", item.ImportPath))
			continue
		}
		files, err := productionGoFiles(item.Dir)
		if err != nil {
			return nil, err
		}
		capabilities, err := inspectDiscoveryCapabilities(files)
		if err != nil {
			return nil, err
		}
		for _, capability := range capabilities {
			findings = append(findings, fmt.Sprintf("%s: %s", item.ImportPath, capability))
		}
	}
	sourceFindings, err := auditLocalDiscoverySources(directory, moduleDirectory, modulePath, goRoot)
	if err != nil {
		return nil, err
	}
	return uniqueFindings(append(findings, sourceFindings...)), nil
}

func auditLocalDiscoverySources(startDirectory, moduleDirectory, modulePath, goRoot string) ([]string, error) {
	startDirectory, err := filepath.Abs(startDirectory)
	if err != nil {
		return nil, err
	}
	startDirectory, err = filepath.EvalSymlinks(startDirectory)
	if err != nil {
		return nil, err
	}
	moduleDirectory, err = filepath.Abs(moduleDirectory)
	if err != nil {
		return nil, err
	}
	moduleDirectory, err = filepath.EvalSymlinks(moduleDirectory)
	if err != nil {
		return nil, err
	}
	queue := []string{startDirectory}
	visited := make(map[string]bool)
	findings := []string{}
	for len(queue) > 0 {
		directory := filepath.Clean(queue[0])
		queue = queue[1:]
		if visited[directory] {
			continue
		}
		visited[directory] = true
		relative, err := filepath.Rel(moduleDirectory, directory)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("module-local discovery import escapes module root: %s", directory)
		}
		packagePath := modulePath
		if relative != "." {
			packagePath += "/" + filepath.ToSlash(relative)
		}
		files, err := productionGoFiles(directory)
		if err != nil {
			return nil, err
		}
		capabilities, err := inspectDiscoveryCapabilities(files)
		if err != nil {
			return nil, err
		}
		for _, capability := range capabilities {
			findings = append(findings, fmt.Sprintf("%s: %s", packagePath, capability))
		}
		imports, err := discoverySourceImports(files)
		if err != nil {
			return nil, err
		}
		for _, importPath := range imports {
			if importPath == "C" {
				continue
			}
			if importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/") {
				relativeImport := strings.TrimPrefix(importPath, modulePath)
				target := filepath.Join(moduleDirectory, filepath.FromSlash(strings.TrimPrefix(relativeImport, "/")))
				resolvedTarget, resolveErr := filepath.EvalSymlinks(target)
				if resolveErr != nil {
					findings = append(findings, fmt.Sprintf("module-local discovery dependency %q is unavailable", importPath))
					continue
				}
				if info, statErr := os.Stat(resolvedTarget); statErr != nil || !info.IsDir() {
					findings = append(findings, fmt.Sprintf("module-local discovery dependency %q is unavailable", importPath))
					continue
				}
				nested, boundaryErr := nestedModuleBoundary(moduleDirectory, resolvedTarget)
				if boundaryErr != nil {
					return nil, boundaryErr
				}
				if nested {
					findings = append(findings, fmt.Sprintf("closure includes third-party dependency %q", importPath))
					continue
				}
				queue = append(queue, resolvedTarget)
				continue
			}
			if !standardLibraryImport(goRoot, importPath) {
				findings = append(findings, fmt.Sprintf("closure includes third-party dependency %q", importPath))
			}
		}
	}
	return uniqueFindings(findings), nil
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

package architecture_test

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestReleaseModulePinsAndTestOnlyTools(t *testing.T) {
	root := repositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	moduleFile := string(data)
	wantDirect := []string{
		"github.com/google/jsonschema-go@v0.4.3",
		"github.com/modelcontextprotocol/go-sdk@v1.7.0",
		"github.com/sourcegraph/jsonrpc2@v0.2.2",
		"go.lsp.dev/protocol@v1.0.1",
		"golang.org/x/tools@v0.49.0",
		"google.golang.org/adk/v2@v2.2.0",
		"google.golang.org/genai@v1.66.0",
	}
	if got := directRequirements(t, moduleFile); !slices.Equal(got, wantDirect) {
		t.Fatalf("direct requirements = %v, want %v", got, wantDirect)
	}
	verifyModulePins(t, moduleFile, map[string]string{
		"github.com/modelcontextprotocol/go-sdk": "v1.7.0",
		"golang.org/x/mod":                       "v0.40.0",
		"golang.org/x/tools":                     "v0.49.0",
	})
	if countXToolsTestImports(t) == 0 {
		t.Fatal("golang.org/x/tools is pinned but no test imports it")
	}
}

func verifyModulePins(t *testing.T, moduleFile string, pins map[string]string) {
	t.Helper()
	for module, version := range pins {
		pattern := `(?m)^\s*` + regexp.QuoteMeta(module) + `\s+` + regexp.QuoteMeta(version) + `(?:\s+// indirect)?\s*$`
		if !regexp.MustCompile(pattern).MatchString(moduleFile) {
			t.Errorf("go.mod does not pin %s %s", module, version)
		}
	}
}

func countXToolsTestImports(t *testing.T) int {
	t.Helper()
	count := 0
	walkRepositoryGoFiles(t, func(path, _ string, _ *token.FileSet, file *ast.File) error {
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(importPath, "golang.org/x/tools/") {
				continue
			}
			if !strings.HasSuffix(path, "_test.go") {
				t.Errorf("production file %s imports test-only module %s", path, importPath)
				continue
			}
			count++
		}
		return nil
	})
	return count
}

func directRequirements(t *testing.T, moduleFile string) []string {
	t.Helper()
	var result []string
	inBlock := false
	scanner := bufio.NewScanner(strings.NewReader(moduleFile))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "require (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "require "))
		case !inBlock:
			continue
		}
		if strings.Contains(line, "// indirect") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed direct requirement line %q", line)
		}
		result = append(result, fields[0]+"@"+fields[1])
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(result)
	return result
}

func TestReleaseRootPublicAPISurface(t *testing.T) {
	want := []string{
		"const CodeCloseFailed", "const CodeClosed", "const CodeConstructionFailed", "const CodeDuplicate",
		"const CodeInvalidArgument", "const CodeNoFinalResponse", "const CodeRegistrationSealed", "const CodeRuntimeFailed",
		"const CodeSessionBusy", "const CodeUnknownSession", "const LSPAuto", "const LSPOff",
		"func CodeOf", "func New", "func WithADKPlugins", "func WithAppName", "func WithConfig",
		"func WithForeignResolution", "func WithLSP", "func WithLogger", "func WithModel", "func WithPlugins",
		"func WithSessionDir", "func WithToolConfirmation", "func WithTools", "func WithUserID", "func WithWorkingDir",
		"method Error.Error", "method Error.Unwrap", "method Harness.Ask", "method Harness.AskTemplate",
		"method Harness.Close", "method Harness.Config", "method Harness.Format", "method Harness.GetTemplate",
		"method Harness.ListTemplates", "method Harness.LogValue", "method Harness.Logger", "method Harness.NewSession",
		"method Harness.RegisterADKPlugins", "method Harness.RegisterPromptFragments", "method Harness.RegisterTools",
		"method Harness.RegisterToolsets", "method Harness.RegisterWarnings", "method Harness.ResumeSession",
		"method Harness.Run", "method Harness.RunTemplate", "method Harness.SessionDir", "method Harness.Warnings",
		"method Harness.WorkingDir",
		"type Error", "type ErrorCode", "type ForeignResolution", "type Harness", "type LSPMode", "type Option",
		"type Plugin", "type PromptFragment",
		"var ErrCloseFailed", "var ErrCloseTimeout", "var ErrClosed", "var ErrConstructionFailed", "var ErrDuplicate",
		"var ErrInvalidArgument", "var ErrNoFinalResponse", "var ErrRegistrationSealed", "var ErrRuntimeFailed",
		"var ErrSessionBusy", "var ErrUnknownSession",
	}
	got := rootPublicAPI(t, repositoryRoot(t))
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("root public API =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	fingerprint := typedRootPublicAPI(t, repositoryRoot(t))
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(fingerprint, "\n"))))
	const wantDigest = "8044c51f7bf25b9516dd765d2f359fc1ef2081c48205103175b14322f97bb776"
	if digest != wantDigest {
		t.Fatalf("typed root public API digest = %s, want %s; surface:\n%s", digest, wantDigest, strings.Join(fingerprint, "\n"))
	}
}

func typedRootPublicAPI(t *testing.T, root string) []string {
	t.Helper()
	rootPackage := findRootPackage(loadProductionPackages(t, root, callableBuildContexts[0], "."))
	if rootPackage == nil {
		t.Fatal("root package was not loaded")
	}
	qualifier := func(imported *types.Package) string {
		if imported == rootPackage {
			return ""
		}
		return imported.Path()
	}
	var result []string
	methods := make(map[string]struct{})
	scope := rootPackage.Scope()
	for _, name := range scope.Names() {
		result = appendPublicObject(result, methods, name, scope.Lookup(name), qualifier)
	}
	for method := range methods {
		result = append(result, method)
	}
	slices.Sort(result)
	return result
}

func findRootPackage(loaded []*packages.Package) *types.Package {
	for _, candidate := range loaded {
		if candidate.PkgPath == "github.com/plasmid-dev/plasmid" {
			return candidate.Types
		}
	}
	return nil
}

func appendPublicObject(result []string, methods map[string]struct{}, name string, object types.Object, qualifier types.Qualifier) []string {
	if !ast.IsExported(name) {
		return result
	}
	switch value := object.(type) {
	case *types.Const:
		return append(result, "const "+name+" "+types.TypeString(value.Type(), qualifier)+" = "+value.Val().ExactString())
	case *types.Func:
		return append(result, "func "+name+" "+types.TypeString(value.Type(), qualifier))
	case *types.TypeName:
		collectPublicMethods(methods, value, qualifier)
		return append(result, publicTypeFingerprint(value, qualifier))
	case *types.Var:
		return append(result, "var "+name+" "+types.TypeString(value.Type(), qualifier))
	default:
		return result
	}
}

func collectPublicMethods(result map[string]struct{}, value *types.TypeName, qualifier types.Qualifier) {
	resolved, ok := types.Unalias(value.Type()).(*types.Named)
	if !ok {
		return
	}
	for _, methodType := range []types.Type{resolved, types.NewPointer(resolved)} {
		set := types.NewMethodSet(methodType)
		for index := range set.Len() {
			method, _ := set.At(index).Obj().(*types.Func)
			if method == nil || !method.Exported() {
				continue
			}
			signature, _ := method.Type().(*types.Signature)
			receiver := types.TypeString(signature.Recv().Type(), qualifier)
			result["method "+receiver+"."+method.Name()+" "+types.TypeString(signature, qualifier)] = struct{}{}
		}
	}
}

func publicTypeFingerprint(value *types.TypeName, qualifier types.Qualifier) string {
	if value.IsAlias() {
		return "type " + value.Name() + " = " + types.TypeString(types.Unalias(value.Type()), qualifier)
	}
	resolved, ok := types.Unalias(value.Type()).(*types.Named)
	if !ok {
		return "type " + value.Name() + " " + types.TypeString(value.Type(), qualifier)
	}
	switch underlying := resolved.Underlying().(type) {
	case *types.Struct:
		var fields []string
		for index := range underlying.NumFields() {
			field := underlying.Field(index)
			if !field.Exported() {
				continue
			}
			entry := field.Name() + " " + types.TypeString(field.Type(), qualifier)
			if field.Embedded() {
				entry += " embedded"
			}
			if tag := underlying.Tag(index); tag != "" {
				entry += " tag=" + strconv.Quote(tag)
			}
			fields = append(fields, entry)
		}
		return "type " + value.Name() + " struct{" + strings.Join(fields, "; ") + "}"
	default:
		return "type " + value.Name() + " " + types.TypeString(underlying, qualifier)
	}
}

func rootPublicAPI(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var result []string
	for _, entry := range entries {
		if !isProductionGoEntry(entry) {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		result = appendPublicDeclarations(result, file.Decls)
	}
	slices.Sort(result)
	return result
}

func isProductionGoEntry(entry os.DirEntry) bool {
	return !entry.IsDir() && filepath.Ext(entry.Name()) == ".go" && !strings.HasSuffix(entry.Name(), "_test.go")
}

func appendPublicDeclarations(result []string, declarations []ast.Decl) []string {
	for _, declaration := range declarations {
		switch value := declaration.(type) {
		case *ast.GenDecl:
			result = appendPublicSpecifications(result, value)
		case *ast.FuncDecl:
			result = appendPublicFunction(result, value)
		}
	}
	return result
}

func appendPublicSpecifications(result []string, declaration *ast.GenDecl) []string {
	for _, specification := range declaration.Specs {
		switch typed := specification.(type) {
		case *ast.TypeSpec:
			if ast.IsExported(typed.Name.Name) {
				result = append(result, "type "+typed.Name.Name)
			}
		case *ast.ValueSpec:
			result = appendPublicValues(result, declaration.Tok, typed)
		}
	}
	return result
}

func appendPublicValues(result []string, kind token.Token, specification *ast.ValueSpec) []string {
	if kind != token.CONST && kind != token.VAR {
		return result
	}
	for _, name := range specification.Names {
		if ast.IsExported(name.Name) {
			result = append(result, kind.String()+" "+name.Name)
		}
	}
	return result
}

func appendPublicFunction(result []string, function *ast.FuncDecl) []string {
	if !ast.IsExported(function.Name.Name) {
		return result
	}
	if function.Recv == nil {
		return append(result, "func "+function.Name.Name)
	}
	receiver := exportedReceiverName(function.Recv.List[0].Type)
	if receiver == "" {
		return result
	}
	return append(result, "method "+receiver+"."+function.Name.Name)
}

func exportedReceiverName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		if ast.IsExported(value.Name) {
			return value.Name
		}
	case *ast.StarExpr:
		return exportedReceiverName(value.X)
	case *ast.IndexExpr:
		return exportedReceiverName(value.X)
	case *ast.IndexListExpr:
		return exportedReceiverName(value.X)
	}
	return ""
}

func TestReleaseWarningShapeHasOneOwner(t *testing.T) {
	var owners []string
	root := repositoryRoot(t)
	walkRepositoryGoFiles(t, func(path string, _ string, _ *token.FileSet, file *ast.File) error {
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, name := range warningTypeNames(file.Decls) {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			owners = append(owners, filepath.ToSlash(relative)+":"+name)
		}
		return nil
	})
	slices.Sort(owners)
	want := []string{"warning/warning.go:Warner", "warning/warning.go:Warning"}
	if !slices.Equal(owners, want) {
		t.Fatalf("warning shape owners = %v, want %v", owners, want)
	}
}

func warningTypeNames(declarations []ast.Decl) []string {
	var names []string
	for _, declaration := range declarations {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, specification := range general.Specs {
			typed, ok := specification.(*ast.TypeSpec)
			if ok && (typed.Name.Name == "Warning" || typed.Name.Name == "Warner") {
				names = append(names, typed.Name.Name)
			}
		}
	}
	return names
}

func TestReleaseWorkflowRunsFullPackageRace(t *testing.T) {
	if got := strings.Count(releaseWorkflow(t), "run: go test -race ./..."); got != 1 {
		t.Fatalf("full-package race steps = %d, want 1", got)
	}
}

func TestReleaseWorkflowRunsLockedSecurityTools(t *testing.T) {
	workflow := releaseWorkflow(t)
	if strings.Contains(workflow, "go install ") {
		t.Fatal("release workflow installs security tools outside their locked tools module")
	}
	for _, line := range []string{
		`go -C tools build -mod=readonly -o "${RUNNER_TEMP}/gitleaks" github.com/zricethezav/gitleaks/v8`,
		`"${RUNNER_TEMP}/gitleaks" dir --no-banner --redact=100 --exit-code 1 --max-target-megabytes 10 --timeout 120 .`,
		`go -C tools build -mod=readonly -o "${RUNNER_TEMP}/govulncheck" golang.org/x/vuln/cmd/govulncheck`,
		`"${RUNNER_TEMP}/govulncheck" ./...`,
	} {
		if got := strings.Count(workflow, line); got != 1 {
			t.Errorf("workflow occurrences of %q = %d, want 1", line, got)
		}
	}
	moduleFile, err := os.ReadFile(filepath.Join(repositoryRoot(t), "tools", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"github.com/zricethezav/gitleaks/v8@v8.30.1",
		"golang.org/x/vuln@v1.7.0",
	}
	if got := directRequirements(t, string(moduleFile)); !slices.Equal(got, want) {
		t.Fatalf("locked security tools = %v, want %v", got, want)
	}
	if !regexp.MustCompile(`(?m)^\s*github\.com/ulikunitz/xz\s+v0\.5\.15\s+// indirect\s*$`).Match(moduleFile) {
		t.Fatal("locked security tools do not retain the patched github.com/ulikunitz/xz v0.5.15")
	}
}

func releaseWorkflow(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

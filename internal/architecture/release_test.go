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
	for module, version := range map[string]string{
		"github.com/modelcontextprotocol/go-sdk": "v1.7.0",
		"golang.org/x/mod":                       "v0.40.0",
		"golang.org/x/tools":                     "v0.49.0",
	} {
		pattern := `(?m)^\s*` + regexp.QuoteMeta(module) + `\s+` + regexp.QuoteMeta(version) + `(?:\s+// indirect)?\s*$`
		if !regexp.MustCompile(pattern).MatchString(moduleFile) {
			t.Errorf("go.mod does not pin %s %s", module, version)
		}
	}

	testImports := 0
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
			testImports++
		}
		return nil
	})
	if testImports == 0 {
		t.Fatal("golang.org/x/tools is pinned but no test imports it")
	}
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
	loaded := loadProductionPackages(t, root, callableBuildContexts[0], ".")
	var rootPackage *types.Package
	for _, candidate := range loaded {
		if candidate.PkgPath == "github.com/plasmid-dev/plasmid" {
			rootPackage = candidate.Types
			break
		}
	}
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
		if !ast.IsExported(name) {
			continue
		}
		object := scope.Lookup(name)
		switch value := object.(type) {
		case *types.Const:
			result = append(result, "const "+name+" "+types.TypeString(value.Type(), qualifier)+" = "+value.Val().ExactString())
		case *types.Func:
			result = append(result, "func "+name+" "+types.TypeString(value.Type(), qualifier))
		case *types.TypeName:
			result = append(result, publicTypeFingerprint(value, qualifier))
			resolved, ok := types.Unalias(value.Type()).(*types.Named)
			if !ok {
				continue
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
					entry := "method " + receiver + "." + method.Name() + " " + types.TypeString(signature, qualifier)
					methods[entry] = struct{}{}
				}
			}
		case *types.Var:
			result = append(result, "var "+name+" "+types.TypeString(value.Type(), qualifier))
		}
	}
	for method := range methods {
		result = append(result, method)
	}
	slices.Sort(result)
	return result
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
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			switch value := declaration.(type) {
			case *ast.GenDecl:
				kind := value.Tok.String()
				for _, specification := range value.Specs {
					switch typed := specification.(type) {
					case *ast.TypeSpec:
						if ast.IsExported(typed.Name.Name) {
							result = append(result, "type "+typed.Name.Name)
						}
					case *ast.ValueSpec:
						if value.Tok != token.CONST && value.Tok != token.VAR {
							continue
						}
						for _, name := range typed.Names {
							if ast.IsExported(name.Name) {
								result = append(result, kind+" "+name.Name)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if !ast.IsExported(value.Name.Name) {
					continue
				}
				if value.Recv == nil {
					result = append(result, "func "+value.Name.Name)
					continue
				}
				if receiver := exportedReceiverName(value.Recv.List[0].Type); receiver != "" {
					result = append(result, "method "+receiver+"."+value.Name.Name)
				}
			}
		}
	}
	slices.Sort(result)
	return result
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
	walkRepositoryGoFiles(t, func(path string, _ string, _ *token.FileSet, file *ast.File) error {
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, specification := range general.Specs {
				typed, ok := specification.(*ast.TypeSpec)
				if ok && (typed.Name.Name == "Warning" || typed.Name.Name == "Warner") {
					relative, err := filepath.Rel(repositoryRoot(t), path)
					if err != nil {
						return err
					}
					owners = append(owners, filepath.ToSlash(relative)+":"+typed.Name.Name)
				}
			}
		}
		return nil
	})
	slices.Sort(owners)
	want := []string{"warning/warning.go:Warner", "warning/warning.go:Warning"}
	if !slices.Equal(owners, want) {
		t.Fatalf("warning shape owners = %v, want %v", owners, want)
	}
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
		"go -C tools run -mod=readonly github.com/zricethezav/gitleaks/v8 dir --no-banner --redact=100 --exit-code 1 --max-target-megabytes 10 --timeout 120 ..",
		"go -C tools run -mod=readonly golang.org/x/vuln/cmd/govulncheck -C .. ./...",
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

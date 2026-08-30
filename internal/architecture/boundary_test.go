package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

const (
	zeroImportDeletionOwner   = "E08 / issue #24: Build the native Harness checkpoint and delete loop bridges"
	legacyLoopImport          = "github.com/RandomCodeSpace/plasmid/loop"
	legacyADKLoopImport       = "github.com/RandomCodeSpace/plasmid/adkloop"
	oneShotToolRecoveryImport = "github.com/RandomCodeSpace/plasmid/internal/toolcallrecovery"
	anonymousFieldLabel       = "field"
)

type legacyImportMaximum struct {
	legacyPackage string
	importers     []string
}

// The native Harness checkpoint deleted both legacy packages. These zero
// maximums remain as a permanent no-reintroduction gate.
var legacyImportMaximums = []legacyImportMaximum{
	{
		legacyPackage: legacyLoopImport,
		importers:     []string{},
	},
	{
		legacyPackage: legacyADKLoopImport,
		importers:     []string{},
	},
}

func TestDeletedLoopBridgesStayAbsent(t *testing.T) {
	root := repositoryRoot(t)
	assertLegacyDirectoriesAbsent(t, root)
	actual := collectLegacyImporters(t)
	assertLegacyImportMaximums(t, actual)
}

func assertLegacyDirectoriesAbsent(t *testing.T, root string) {
	t.Helper()
	for _, directory := range []string{"loop", "adkloop"} {
		if _, err := os.Stat(filepath.Join(root, directory)); !os.IsNotExist(err) {
			t.Errorf("deleted legacy package %q exists or could not be checked: %v", directory, err)
		}
	}
}

func collectLegacyImporters(t *testing.T) map[string][]string {
	t.Helper()
	actual := make(map[string][]string, len(legacyImportMaximums))
	for _, maximum := range legacyImportMaximums {
		actual[maximum.legacyPackage] = nil
	}

	walkRepositoryGoFiles(t, func(_ string, relativeDirectory string, _ *token.FileSet, file *ast.File) error {
		if relativeDirectory == "loop" || relativeDirectory == "adkloop" {
			return nil
		}
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			if err != nil {
				return err
			}
			if _, tracked := actual[importPath]; tracked {
				actual[importPath] = append(actual[importPath], relativeDirectory)
			}
		}
		return nil
	})
	return actual
}

func assertLegacyImportMaximums(t *testing.T, actual map[string][]string) {
	t.Helper()
	for _, maximum := range legacyImportMaximums {
		got := actual[maximum.legacyPackage]
		slices.Sort(got)
		got = slices.Compact(got)
		if additions := unexpectedImporters(got, maximum.importers); len(additions) != 0 {
			t.Errorf("deleted legacy package %s was reintroduced through importers %v; deletion owner: %s", maximum.legacyPackage, additions, zeroImportDeletionOwner)
		}
	}
}

func unexpectedImporters(actual, maximum []string) []string {
	maximumSet := make(map[string]struct{}, len(maximum))
	for _, importer := range maximum {
		maximumSet[importer] = struct{}{}
	}
	var additions []string
	for _, importer := range actual {
		if _, approved := maximumSet[importer]; !approved {
			additions = append(additions, importer)
		}
	}
	return additions
}

// Each entry is an intentional native-framework seam. Any new package must be
// added with its architectural role instead of acquiring ADK by accident.
var nativeIntegrationPackages = map[string]string{
	".":            "root Harness owns native llmagent and runner construction",
	"callbacks":    "native ADK callback implementations",
	"codingtools":  "native ADK function tools",
	"compaction":   "native before-model and after-model callbacks",
	"mcp":          "lifecycle-owned native ADK MCP toolsets",
	"oneshot":      "ephemeral native ADK model, tool, runner, and session integration",
	"openai":       "typed OpenAI construction and native ADK model decoration",
	"plugins":      "compiled-plugin native tools and callbacks",
	"sessionstore": "native ADK session.Service implementation",
	"skills":       "native ADK skill toolset",
}

var oneShotProductionImports = map[string]struct{}{
	"context":                        {},
	"errors":                         {},
	"fmt":                            {},
	"iter":                           {},
	"maps":                           {},
	oneShotToolRecoveryImport:        {},
	"reflect":                        {},
	"strconv":                        {},
	"strings":                        {},
	"sync":                           {},
	"sync/atomic":                    {},
	"google.golang.org/adk/v2/agent": {},
	"google.golang.org/adk/v2/agent/llmagent": {},
	"google.golang.org/adk/v2/model":          {},
	"google.golang.org/adk/v2/platform":       {},
	"google.golang.org/adk/v2/runner":         {},
	"google.golang.org/adk/v2/session":        {},
	"google.golang.org/adk/v2/tool":           {},
	"google.golang.org/adk/v2/tool/toolutils": {},
	"google.golang.org/genai":                 {},
}

func TestNativeFrameworkImportsStayInExplicitIntegrationPackages(t *testing.T) {
	walkRepositoryGoFiles(t, func(path string, relativeDirectory string, _ *token.FileSet, file *ast.File) error {
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			if err != nil {
				return err
			}
			if isNativeFrameworkImport(importPath) && nativeIntegrationPackages[relativeDirectory] == "" {
				t.Errorf("%s imports native framework package %s outside the explicit integration inventory", path, importPath)
			}
		}
		return nil
	})
}

func TestOneShotProductionImportsStayAuthorityNeutral(t *testing.T) {
	observed := make(map[string]int, len(oneShotProductionImports))
	walkRepositoryGoFiles(t, func(path string, relativeDirectory string, _ *token.FileSet, file *ast.File) error {
		if relativeDirectory != "oneshot" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			if err != nil {
				return err
			}
			if _, approved := oneShotProductionImports[importPath]; !approved {
				t.Errorf("%s imports %q outside the one-shot authority inventory", filepath.Base(path), importPath)
				continue
			}
			observed[importPath]++
		}
		return nil
	})
	approved := make([]string, 0, len(oneShotProductionImports))
	for importPath := range oneShotProductionImports {
		approved = append(approved, importPath)
	}
	slices.Sort(approved)
	for _, importPath := range approved {
		if observed[importPath] == 0 {
			t.Errorf("approved one-shot production import %q is unused; remove stale authority", importPath)
		}
	}
}

func isNativeFrameworkImport(importPath string) bool {
	return strings.HasPrefix(importPath, "google.golang.org/adk/") ||
		strings.HasPrefix(importPath, "google.golang.org/genai") ||
		strings.HasPrefix(importPath, "github.com/google/jsonschema-go") ||
		strings.HasPrefix(importPath, "github.com/modelcontextprotocol/go-sdk")
}

type interfaceApproval struct {
	file        string
	owner       string
	fingerprint string
	count       int
	rationale   string
}

// This is the complete production interface inventory outside the legacy
// packages. A new runtime facade cannot evade review by changing its spelling
// or method set; adding any interface requires an explicit rationale here.
var approvedInterfaceDeclarations = []interfaceApproval{
	{
		file:        "oneshot/oneshot.go",
		owner:       "type errorUnwrapper",
		fingerprint: "interface{Unwrap() error}",
		count:       1,
		rationale:   "traverses single-cause errors for private one-shot provenance without recovering non-caller panics",
	},
	{
		file:        "oneshot/oneshot.go",
		owner:       "type joinedErrorUnwrapper",
		fingerprint: "interface{Unwrap() []error}",
		count:       1,
		rationale:   "traverses joined errors for private one-shot provenance without recovering non-caller panics",
	},
	{
		file:        "oneshot/boundaries.go",
		owner:       "type requestProcessor",
		fingerprint: "interface{ProcessRequest(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error}",
		count:       1,
		rationale:   "preserves caller tool request processing through the one-shot boundary wrapper",
	},
	{
		file:        "oneshot/boundaries.go",
		owner:       "type declarer",
		fingerprint: "interface{Declaration() *google.golang.org/genai.FunctionDeclaration}",
		count:       1,
		rationale:   "preserves native function declarations through the one-shot boundary wrapper",
	},
	{
		file:        "oneshot/boundaries.go",
		owner:       "type functionTool",
		fingerprint: "interface{Run(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error); google.golang.org/adk/v2/tool.Tool; declarer}",
		count:       1,
		rationale:   "panic-isolated native function-tool execution for one-shot runs",
	},
	{
		file:        "oneshot/boundaries.go",
		owner:       "type streamingTool",
		fingerprint: "interface{RunStream(google.golang.org/adk/v2/agent.Context, any) iter.Seq2[string, error]; google.golang.org/adk/v2/tool.Tool; declarer}",
		count:       1,
		rationale:   "panic-isolated native streaming-tool execution for one-shot runs",
	},
	{
		file:        "oneshot/boundaries.go",
		owner:       "type responseDeferrer",
		fingerprint: "interface{DefersResponse() bool}",
		count:       1,
		rationale:   "preserves native deferred-response semantics through the one-shot boundary wrapper",
	},
	{
		file:        "tool_guard.go",
		owner:       "type nativeFunctionTool",
		fingerprint: "interface{Declaration() *google.golang.org/genai.FunctionDeclaration; Run(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error); google.golang.org/adk/v2/tool.Tool}",
		count:       1,
		rationale:   "native ADK function-tool execution seam for post-callback policy enforcement",
	},
	{
		file:        "tool_guard.go",
		owner:       "type nativeStreamingTool",
		fingerprint: "interface{Declaration() *google.golang.org/genai.FunctionDeclaration; RunStream(google.golang.org/adk/v2/agent.Context, any) iter.Seq2[string, error]; google.golang.org/adk/v2/tool.Tool}",
		count:       1,
		rationale:   "native ADK streaming-tool execution seam for post-callback policy enforcement",
	},
	{
		file:        "tool_guard.go",
		owner:       "type responseDeferrer",
		fingerprint: "interface{DefersResponse() bool}",
		count:       1,
		rationale:   "preserves native ADK deferred-response semantics through the policy wrapper",
	},
	{
		file:        "internal/processtree/tree.go",
		owner:       "type Terminator",
		fingerprint: "interface{Terminate() error}",
		count:       1,
		rationale:   "shared cross-platform descendant-process lifecycle seam for LSP and MCP",
	},
	{
		file:        "plugin.go",
		owner:       "type Plugin",
		fingerprint: "interface{Close() error; Init(*Harness) error; Name() string}",
		count:       1,
		rationale:   "host-compiled extension lifecycle and sealed registration seam",
	},
	{
		file:        "harness.go",
		owner:       "type toolsetRequestProcessor",
		fingerprint: "interface{ProcessRequest(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error}",
		count:       1,
		rationale:   "native ADK per-model-request toolset processing extension point",
	},
	{
		file:        "internal/pathglob/pathglob.go",
		owner:       "type Matcher",
		fingerprint: "interface{Match(relPath string) bool; Patterns() []string}",
		count:       1,
		rationale:   "framework-free compiled path-matching leaf seam",
	},
	{
		file:        "internal/toolcallrecovery/recovery.go",
		owner:       "type RequestMarker",
		fingerprint: "interface{MarkToolCallRecovery(map[string]any)}",
		count:       1,
		rationale:   "private capability gate for one-shot malformed-argument recovery",
	},
	{
		file:        "sessionstore/log.go",
		owner:       "type warningLogger",
		fingerprint: "interface{warn(code string, path string, line int, message string)}",
		count:       1,
		rationale:   "narrow internal warning emission seam for session recovery",
	},
	{
		file:        "warning/warning.go",
		owner:       "type Warner",
		fingerprint: "interface{Warn(Warning)}",
		count:       1,
		rationale:   "shared framework-free structured warning destination",
	},
	{
		file:        "warning/warning.go",
		owner:       "type Sink",
		fingerprint: "interface{Warn(Warning)}",
		count:       1,
		rationale:   "source-compatible alias for the shared warning destination",
	},
	{
		file:        "workspace/touch.go",
		owner:       "type TouchObserver",
		fingerprint: "interface{ObserveTouch(context.Context, Touch)}",
		count:       1,
		rationale:   "framework-free workspace touch notification seam",
	},
}

func TestProductionInterfacesMatchExplicitArchitectureInventory(t *testing.T) {
	approved := make(map[string]interfaceApproval, len(approvedInterfaceDeclarations))
	for _, declaration := range approvedInterfaceDeclarations {
		key := interfaceInventoryKey(declaration.file, declaration.owner, declaration.fingerprint)
		if declaration.rationale == "" || declaration.count < 1 {
			t.Errorf("interface approval %s has no rationale", key)
		}
		if _, exists := approved[key]; exists {
			t.Errorf("duplicate interface approval %s", key)
		}
		approved[key] = declaration
	}

	actual, examples, platforms := typedInterfaceUnion(t, repositoryRoot(t), callableBuildContexts, "./...")
	for key, count := range actual {
		if _, exists := approved[key]; exists {
			continue
		}
		discovered := examples[key]
		t.Errorf("unapproved resolved interface %s at %s:%d for %v (count %d); add an architecture rationale or use the native concrete type", discovered.fingerprint, discovered.file, discovered.line, platforms[key], count)
	}

	for key, approval := range approved {
		if actual[key] != approval.count {
			t.Errorf("interface inventory %s has count %d, want %d", key, actual[key], approval.count)
		}
	}
}

type discoveredInterface struct {
	packagePath string
	file        string
	owner       string
	fingerprint string
	line        int
}

func interfaceInventoryKey(file, owner, fingerprint string) string {
	return file + ":" + owner + ":" + fingerprint
}

func typedInterfaceUnion(t *testing.T, directory string, buildContexts []buildContext, patterns ...string) (map[string]int, map[string]discoveredInterface, map[string][]string) {
	t.Helper()
	maximumCounts := make(map[string]int)
	examples := make(map[string]discoveredInterface)
	platforms := make(map[string][]string)
	for _, buildContext := range buildContexts {
		contextCounts := make(map[string]int)
		for _, discovered := range loadTypedInterfaces(t, directory, buildContext, patterns...) {
			if discovered.packagePath == legacyLoopImport || discovered.packagePath == legacyADKLoopImport {
				continue
			}
			key := interfaceInventoryKey(discovered.file, discovered.owner, discovered.fingerprint)
			contextCounts[key]++
			examples[key] = discovered
		}
		for key, count := range contextCounts {
			if count > maximumCounts[key] {
				maximumCounts[key] = count
			}
			platforms[key] = append(platforms[key], buildContext.String())
		}
	}
	return maximumCounts, examples, platforms
}

func loadTypedInterfaces(t *testing.T, directory string, buildContext buildContext, patterns ...string) []discoveredInterface {
	t.Helper()
	loaded := loadProductionPackages(t, directory, buildContext, patterns...)
	var declarations []discoveredInterface
	for _, loadedPackage := range loaded {
		if loadedPackage.Module == nil || !loadedPackage.Module.Main || loadedPackage.TypesInfo == nil {
			continue
		}
		for _, file := range loadedPackage.Syntax {
			declarations = append(declarations, collectNamedInterfaces(t, directory, loadedPackage, file)...)
		}
	}
	return declarations
}

func collectNamedInterfaces(t *testing.T, directory string, loadedPackage *packages.Package, file *ast.File) []discoveredInterface {
	t.Helper()
	var declarations []discoveredInterface
	ast.Inspect(file, func(node ast.Node) bool {
		declaration, ok := node.(*ast.TypeSpec)
		if !ok {
			return true
		}
		object, _ := loadedPackage.TypesInfo.Defs[declaration.Name].(*types.TypeName)
		if object == nil {
			return true
		}
		interfaceType, ok := types.Unalias(object.Type()).Underlying().(*types.Interface)
		if !ok {
			return true
		}
		discovered, ok := namedInterfaceDeclaration(t, directory, loadedPackage, declaration, interfaceType)
		if ok {
			declarations = append(declarations, discovered)
		}
		return ok
	})
	return declarations
}

func namedInterfaceDeclaration(t *testing.T, directory string, loadedPackage *packages.Package, declaration *ast.TypeSpec, interfaceType *types.Interface) (discoveredInterface, bool) {
	t.Helper()
	location := loadedPackage.Fset.Position(declaration.Pos())
	relativeFile, err := filepath.Rel(directory, location.Filename)
	if err != nil {
		t.Errorf("resolve interface path %s: %v", location.Filename, err)
		return discoveredInterface{}, false
	}
	return discoveredInterface{
		packagePath: loadedPackage.PkgPath,
		file:        filepath.ToSlash(relativeFile),
		owner:       "type " + declaration.Name.Name,
		fingerprint: types.TypeString(interfaceType.Complete(), packageQualifier(loadedPackage.Types)),
		line:        location.Line,
	}, true
}

func packageQualifier(owner *types.Package) types.Qualifier {
	return func(imported *types.Package) string {
		if imported == owner {
			return ""
		}
		return imported.Path()
	}
}

// Anonymous interfaces are separate from the TypeName inventory above. There
// are currently no approved production instances; any addition needs a file,
// resolved fingerprint, and architectural rationale here.
var approvedAnonymousInterfaces = []interfaceApproval{}

func TestAnonymousInterfacesMatchExplicitResolvedInventory(t *testing.T) {
	approved := make(map[string]interfaceApproval, len(approvedAnonymousInterfaces))
	for _, declaration := range approvedAnonymousInterfaces {
		key := interfaceInventoryKey(declaration.file, declaration.owner, declaration.fingerprint)
		if declaration.rationale == "" || declaration.count < 1 {
			t.Errorf("anonymous interface approval %s has no rationale", key)
		}
		if _, exists := approved[key]; exists {
			t.Errorf("duplicate anonymous interface approval %s", key)
		}
		approved[key] = declaration
	}

	actual, examples, platforms := typedAnonymousInterfaceUnion(t, repositoryRoot(t), callableBuildContexts, "./...")
	for key, count := range actual {
		if _, exists := approved[key]; exists {
			continue
		}
		discovered := examples[key]
		t.Errorf("unapproved anonymous interface %s %s at %s:%d for %v (count %d); add an architecture rationale", discovered.owner, discovered.fingerprint, discovered.file, discovered.line, platforms[key], count)
	}
	for key, approval := range approved {
		if actual[key] != approval.count {
			t.Errorf("anonymous interface inventory %s has count %d, want %d", key, actual[key], approval.count)
		}
	}
}

func typedAnonymousInterfaceUnion(t *testing.T, directory string, buildContexts []buildContext, patterns ...string) (map[string]int, map[string]discoveredInterface, map[string][]string) {
	t.Helper()
	maximumCounts := make(map[string]int)
	examples := make(map[string]discoveredInterface)
	platforms := make(map[string][]string)
	for _, buildContext := range buildContexts {
		contextCounts := make(map[string]int)
		for _, discovered := range loadTypedAnonymousInterfaces(t, directory, buildContext, patterns...) {
			if discovered.packagePath == legacyLoopImport || discovered.packagePath == legacyADKLoopImport {
				continue
			}
			key := interfaceInventoryKey(discovered.file, discovered.owner, discovered.fingerprint)
			contextCounts[key]++
			examples[key] = discovered
		}
		for key, count := range contextCounts {
			if count > maximumCounts[key] {
				maximumCounts[key] = count
			}
			platforms[key] = append(platforms[key], buildContext.String())
		}
	}
	return maximumCounts, examples, platforms
}

func loadTypedAnonymousInterfaces(t *testing.T, directory string, buildContext buildContext, patterns ...string) []discoveredInterface {
	t.Helper()
	loaded := loadProductionPackages(t, directory, buildContext, patterns...)
	var declarations []discoveredInterface
	for _, loadedPackage := range loaded {
		if loadedPackage.Module == nil || !loadedPackage.Module.Main || loadedPackage.TypesInfo == nil {
			continue
		}
		for _, file := range loadedPackage.Syntax {
			declarations = append(declarations, collectAnonymousInterfaces(t, directory, loadedPackage, file)...)
		}
	}
	return declarations
}

type anonymousInterfaceCollector struct {
	t             *testing.T
	directory     string
	loadedPackage *packages.Package
	parents       []ast.Node
	declarations  []discoveredInterface
}

func collectAnonymousInterfaces(t *testing.T, directory string, loadedPackage *packages.Package, file *ast.File) []discoveredInterface {
	t.Helper()
	collector := &anonymousInterfaceCollector{t: t, directory: directory, loadedPackage: loadedPackage}
	ast.Inspect(file, collector.inspect)
	return collector.declarations
}

func (collector *anonymousInterfaceCollector) inspect(node ast.Node) bool {
	if node == nil {
		collector.parents = collector.parents[:len(collector.parents)-1]
		return true
	}
	interfaceNode, ok := node.(*ast.InterfaceType)
	if ok && !isNamedInterfaceSyntax(interfaceNode, collector.parents) {
		collector.append(interfaceNode)
	}
	collector.parents = append(collector.parents, node)
	return true
}

func (collector *anonymousInterfaceCollector) append(interfaceNode *ast.InterfaceType) {
	resolved := collector.loadedPackage.TypesInfo.TypeOf(interfaceNode)
	if resolved == nil {
		return
	}
	resolvedInterface, ok := resolved.Underlying().(*types.Interface)
	if !ok {
		return
	}
	location := collector.loadedPackage.Fset.Position(interfaceNode.Pos())
	relativeFile, err := filepath.Rel(collector.directory, location.Filename)
	if err != nil {
		collector.t.Errorf("resolve anonymous interface path %s: %v", location.Filename, err)
		return
	}
	collector.declarations = append(collector.declarations, discoveredInterface{
		packagePath: collector.loadedPackage.PkgPath,
		file:        filepath.ToSlash(relativeFile),
		owner:       anonymousInterfaceOwner(collector.parents),
		fingerprint: types.TypeString(resolvedInterface.Complete(), packageQualifier(collector.loadedPackage.Types)),
		line:        location.Line,
	})
}

func isNamedInterfaceSyntax(interfaceNode *ast.InterfaceType, parents []ast.Node) bool {
	for index := len(parents) - 1; index >= 0; index-- {
		if declaration, ok := parents[index].(*ast.TypeSpec); ok {
			return declaration.Type == interfaceNode
		}
	}
	return false
}

func anonymousInterfaceOwner(parents []ast.Node) string {
	for index := len(parents) - 1; index >= 0; index-- {
		switch declaration := parents[index].(type) {
		case *ast.Field:
			return anonymousFieldOwner(declaration, parents, index)
		case *ast.ValueSpec:
			name := "anonymous"
			if len(declaration.Names) != 0 {
				name = declaration.Names[0].Name
			}
			return "variable " + name + " in " + enclosingCallableOwner(parents[:index])
		}
	}
	return "anonymous interface in " + enclosingCallableOwner(parents)
}

func anonymousFieldOwner(field *ast.Field, parents []ast.Node, index int) string {
	name := "anonymous"
	if len(field.Names) != 0 {
		name = field.Names[0].Name
	}
	return anonymousFieldKind(parents, index) + " " + name + " in " + enclosingCallableOwner(parents[:index])
}

func anonymousFieldKind(parents []ast.Node, index int) string {
	if index == 0 {
		return anonymousFieldLabel
	}
	fields, ok := parents[index-1].(*ast.FieldList)
	if !ok {
		return anonymousFieldLabel
	}
	for outer := index - 2; outer >= 0; outer-- {
		function, ok := parents[outer].(*ast.FuncType)
		if !ok {
			continue
		}
		if fields == function.Params {
			return "parameter"
		}
		if fields == function.Results {
			return "result"
		}
		return anonymousFieldLabel
	}
	return anonymousFieldLabel
}

type callableApproval struct {
	file        string
	kind        string
	owner       string
	fingerprint string
	count       int
	rationale   string
}

// This is the complete typed inventory of production callable surfaces outside
// loop and adkloop that accept context.Context or a type assignable to it. It
// deliberately includes error-only and no-result callables. E08 owns deleting
// the excluded legacy packages; this inventory survives that deletion.
var approvedContextCallables = []callableApproval{
	{file: "oneshot/oneshot.go", kind: "top-level function", owner: "func Run", fingerprint: "func(ctx context.Context, request Request) (Result, error)", count: 1, rationale: "public bounded one-shot execution entry point"},
	{file: "oneshot/probe.go", kind: "top-level function", owner: "func Probe", fingerprint: "func(ctx context.Context, request ProbeRequest) (Result, error)", count: 1, rationale: "public configurable one-request model capability probe"},
	{file: "oneshot/probe.go", kind: "top-level function", owner: "func ProbeToolCalling", fingerprint: "func(ctx context.Context, llm google.golang.org/adk/v2/model.LLM) (Result, error)", count: 1, rationale: "one-request direct native model capability probe"},
	{file: "oneshot/oneshot.go", kind: "top-level function", owner: "func runWithSessionService", fingerprint: "func(ctx context.Context, request Request, sessions google.golang.org/adk/v2/session.Service) (result Result, err error)", count: 1, rationale: "ephemeral native runner and session lifecycle owner"},
	{file: "oneshot/oneshot.go", kind: "top-level function", owner: "func sequentialTaskRunner", fingerprint: "func(ctx context.Context, tasks []func(context.Context))", count: 1, rationale: "deterministic response-order execution of native ADK tool tasks"},
	{file: "oneshot/oneshot.go", kind: "top-level function", owner: "func parallelTaskRunner", fingerprint: "func(ctx context.Context, tasks []func(context.Context))", count: 1, rationale: "explicit opt-in overlap of native ADK tool tasks"},
	{file: "oneshot/oneshot.go", kind: "top-level function", owner: "func nestedTaskRunner", fingerprint: "func(ctx context.Context, tasks []func(context.Context))", count: 1, rationale: "policy-independent concurrent containment of caller-internal ADK task fan-out"},
	{file: "oneshot/oneshot.go", kind: "top-level function", owner: "func runConcurrentTasks", fingerprint: "func(ctx context.Context, tasks []func(context.Context))", count: 1, rationale: "shared concurrent task execution with response-order panic propagation"},
	nestedCallableApproval("oneshot/oneshot.go", "func sequentialTaskRunner via parameter tasks -> slice element", "func(context.Context)", "native ADK task callback invoked sequentially in response order"),
	nestedCallableApproval("oneshot/oneshot.go", "func parallelTaskRunner via parameter tasks -> slice element", "func(context.Context)", "native ADK task callback invoked only under explicit parallel policy"),
	nestedCallableApproval("oneshot/oneshot.go", "func nestedTaskRunner via parameter tasks -> slice element", "func(context.Context)", "caller-internal ADK task callbacks invoked concurrently outside the outer tool policy"),
	nestedCallableApproval("oneshot/oneshot.go", "func runConcurrentTasks via parameter tasks -> slice element", "func(context.Context)", "concurrent task callbacks contained with deterministic panic propagation"),
	{file: "oneshot/oneshot.go", kind: "function literal", owner: "literal in func runWithSessionService", fingerprint: "func(google.golang.org/adk/v2/agent.ReadonlyContext) (string, error)", count: 1, rationale: "literal caller instruction provider for one-shot runs"},
	{file: "oneshot/oneshot.go", kind: "function literal", owner: "literal in func runWithSessionService", fingerprint: "func(_ google.golang.org/adk/v2/agent.Context, modelRequest *google.golang.org/adk/v2/model.LLMRequest) (*google.golang.org/adk/v2/model.LLMResponse, error)", count: 1, rationale: "removes the ADK agent identity without replacing caller or tool instructions"},
	{file: "oneshot/oneshot.go", kind: "top-level function", owner: "func executionError", fingerprint: "func(ctx context.Context, op string, cause error) error", count: 1, rationale: "maps cancellation and native runner failures to stable one-shot errors"},
	{file: "oneshot/boundaries.go", kind: "method", owner: "method *protectedModel.GenerateContent", fingerprint: "func(ctx context.Context, request *google.golang.org/adk/v2/model.LLMRequest, stream bool) iter.Seq2[*google.golang.org/adk/v2/model.LLMResponse, error]", count: 1, rationale: "panic-isolated caller model invocation and lazy iteration"},
	{file: "oneshot/boundaries.go", kind: "method", owner: "method *protectedModel.generate", fingerprint: "func(ctx context.Context, request *google.golang.org/adk/v2/model.LLMRequest, stream bool, yield func(*google.golang.org/adk/v2/model.LLMResponse, error) bool)", count: 1, rationale: "narrow caller-model panic boundary with downstream panic preservation"},
	{file: "oneshot/boundaries.go", kind: "top-level function", owner: "func contextWithoutTaskRunner", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context) google.golang.org/adk/v2/agent.Context", count: 1, rationale: "masks one-shot tool scheduling from caller request-processor internals while retaining native ADK context derivation"},
	{file: "oneshot/boundaries.go", kind: "method", owner: "method *toolDescriptor.processRequest", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest, packed google.golang.org/adk/v2/tool/toolutils.Tool) error", count: 1, rationale: "preserves native tool request processing through one-shot wrappers"},
	{file: "oneshot/boundaries.go", kind: "top-level function", owner: "func callToolProcessor", fingerprint: "func(processor requestProcessor, ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest) (err error, panicked bool)", count: 1, rationale: "records private panic provenance at the caller request-processor boundary"},
	{file: "oneshot/boundaries.go", kind: "method", owner: "method *protectedRequestTool.ProcessRequest", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest) error", count: 1, rationale: "native request-only tool wrapper for one-shot runs"},
	{file: "oneshot/boundaries.go", kind: "method", owner: "method *protectedFunctionTool.ProcessRequest", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest) error", count: 1, rationale: "native function-tool request wrapper for one-shot runs"},
	{file: "oneshot/boundaries.go", kind: "method", owner: "method *protectedFunctionTool.Run", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, arguments any) (result map[string]any, err error)", count: 1, rationale: "panic-isolated caller function-tool invocation"},
	{file: "oneshot/boundaries.go", kind: "top-level function", owner: "func callFunctionTool", fingerprint: "func(source functionTool, ctx google.golang.org/adk/v2/agent.Context, arguments any) (result map[string]any, err error, panicked bool)", count: 1, rationale: "records private panic provenance at the caller function-tool boundary"},
	{file: "oneshot/boundaries.go", kind: "method", owner: "method *protectedStreamingTool.ProcessRequest", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest) error", count: 1, rationale: "native streaming-tool request wrapper for one-shot runs"},
	{file: "oneshot/boundaries.go", kind: "method", owner: "method *protectedStreamingTool.RunStream", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, arguments any) iter.Seq2[string, error]", count: 1, rationale: "panic-isolated caller streaming-tool invocation and lazy iteration"},
	nestedCallableApproval("oneshot/boundaries.go", "type requestProcessor via method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "native request-processor contract retained by the one-shot wrapper"),
	nestedCallableApproval("oneshot/boundaries.go", "type functionTool via method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "native function-tool execution contract retained by the one-shot wrapper"),
	nestedCallableApproval("oneshot/boundaries.go", "type streamingTool via method RunStream", "func(google.golang.org/adk/v2/agent.Context, any) iter.Seq2[string, error]", "native streaming-tool execution contract retained by the one-shot wrapper"),
	nestedCallableApproval("oneshot/boundaries.go", "type toolDescriptor via field processor -> method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "stored caller request processor for the one-shot wrapper"),
	nestedCallableApproval("oneshot/boundaries.go", "variable descriptor in func protectTool via pointer element -> field processor -> method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "inspected caller request processor passed into the one-shot wrapper"),
	nestedCallableApproval("oneshot/boundaries.go", "func inspectTool via result descriptor -> pointer element -> field processor -> method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "tool inspection result retains the caller request processor"),
	nestedCallableApproval("oneshot/boundaries.go", "func protectedToolMetadata via parameter descriptor -> pointer element -> field processor -> method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "lazy metadata forwarding retains the descriptor caller request processor"),
	nestedCallableApproval("oneshot/boundaries.go", "variable processor in func inspectTool via method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "resolved caller request processor during tool inspection"),
	nestedCallableApproval("oneshot/boundaries.go", "func callToolProcessor via parameter processor -> method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "caller request processor input to the panic boundary"),
	nestedCallableApproval("oneshot/boundaries.go", "type protectedRequestTool via field toolDescriptor -> pointer element -> field processor -> method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "request-only wrapper retains the caller request processor"),
	nestedCallableApproval("oneshot/boundaries.go", "type protectedFunctionTool via field toolDescriptor -> pointer element -> field processor -> method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "function-tool wrapper retains the caller request processor"),
	nestedCallableApproval("oneshot/boundaries.go", "type protectedFunctionTool via field source -> method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "function-tool wrapper retains the caller execution method"),
	nestedCallableApproval("oneshot/boundaries.go", "func callFunctionTool via parameter source -> method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "caller function-tool input to the panic boundary"),
	nestedCallableApproval("oneshot/boundaries.go", "type protectedStreamingTool via field toolDescriptor -> pointer element -> field processor -> method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "streaming-tool wrapper retains the caller request processor"),
	nestedCallableApproval("oneshot/boundaries.go", "type protectedStreamingTool via field source -> method RunStream", "func(google.golang.org/adk/v2/agent.Context, any) iter.Seq2[string, error]", "streaming-tool wrapper retains the caller execution method"),
	{file: "oneshot/boundaries.go", kind: "nested callable", owner: "variable _ in file scope via method ProcessRequest", fingerprint: "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", count: 3, rationale: "compile-time native request-processor assertions for all one-shot wrappers"},
	nestedCallableApproval("oneshot/boundaries.go", "variable _ in file scope via method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "compile-time native function-tool wrapper assertion"),
	nestedCallableApproval("oneshot/boundaries.go", "variable _ in file scope via method RunStream", "func(google.golang.org/adk/v2/agent.Context, any) iter.Seq2[string, error]", "compile-time native streaming-tool wrapper assertion"),
	{file: "openai/openai.go", kind: "top-level function", owner: "func New", fingerprint: "func(ctx context.Context, cfg Config) (google.golang.org/adk/v2/model.LLM, error)", count: 1, rationale: "typed OpenAI construction returning the native ADK model contract"},
	{file: "openai/openai.go", kind: "top-level function", owner: "func validate", fingerprint: "func(ctx context.Context, cfg Config) (*net/url.URL, error)", count: 1, rationale: "shared zero-I/O validation before OpenAI protocol construction"},
	{file: "openai/openai.go", kind: "method", owner: "method *redactedModel.GenerateContent", fingerprint: "func(ctx context.Context, request *google.golang.org/adk/v2/model.LLMRequest, stream bool) iter.Seq2[*google.golang.org/adk/v2/model.LLMResponse, error]", count: 1, rationale: "native ADK model decoration that redacts provider failures"},
	{file: "openai/chat.go", kind: "method", owner: "method *chatModel.GenerateContent", fingerprint: "func(ctx context.Context, request *google.golang.org/adk/v2/model.LLMRequest, stream bool) iter.Seq2[*google.golang.org/adk/v2/model.LLMResponse, error]", count: 1, rationale: "native non-streaming ADK model implementation for Chat Completions"},
	{file: "tool_guard.go", kind: "method", owner: "method *guardedFunctionTool.ProcessRequest", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest) error", count: 1, rationale: "preserves native ADK request processing through the execution-policy wrapper"},
	{file: "tool_guard.go", kind: "method", owner: "method *guardedFunctionTool.Run", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, arguments any) (map[string]any, error)", count: 1, rationale: "authoritative post-callback tool-policy enforcement at execution"},
	{file: "tool_guard.go", kind: "method", owner: "method *guardedStreamingTool.ProcessRequest", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest) error", count: 1, rationale: "preserves native ADK streaming request processing through the execution-policy wrapper"},
	{file: "tool_guard.go", kind: "method", owner: "method *guardedStreamingTool.RunStream", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, arguments any) iter.Seq2[string, error]", count: 1, rationale: "authoritative post-callback streaming-tool policy enforcement at execution"},
	{file: "tool_guard.go", kind: "method", owner: "method singleToolset.Tools", fingerprint: "func(google.golang.org/adk/v2/agent.ReadonlyContext) ([]google.golang.org/adk/v2/tool.Tool, error)", count: 1, rationale: "single-value adapter into the native ADK confirmation wrapper"},
	{file: "tool_guard.go", kind: "top-level function", owner: "func processGuardedToolRequest", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest, source google.golang.org/adk/v2/tool.Tool, wrapper google.golang.org/adk/v2/tool/toolutils.Tool, contexts *github.com/RandomCodeSpace/plasmid/contextresolver.Resolver, confirmation bool) error", count: 1, rationale: "single owner for native request packing and processor-selected delegate decoration"},
	{file: "tool_guard.go", kind: "top-level function", owner: "func toolPolicyError", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, contexts *github.com/RandomCodeSpace/plasmid/contextresolver.Resolver, name string, args map[string]any) error", count: 1, rationale: "single owner for built-in and execution-time tool-policy checks"},
	nestedCallableApproval("tool_guard.go", "type nativeFunctionTool via method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "native ADK function-tool execution contract"),
	nestedCallableApproval("tool_guard.go", "type guardedFunctionTool via field source -> method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "retained native function-tool execution contract"),
	nestedCallableApproval("tool_guard.go", "type guardedFunctionTool via field confirmed -> method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "retained native confirmation execution contract"),
	nestedCallableApproval("tool_guard.go", "type nativeStreamingTool via method RunStream", "func(google.golang.org/adk/v2/agent.Context, any) iter.Seq2[string, error]", "native ADK streaming-tool execution contract"),
	nestedCallableApproval("tool_guard.go", "type guardedStreamingTool via field source -> method RunStream", "func(google.golang.org/adk/v2/agent.Context, any) iter.Seq2[string, error]", "retained native streaming-tool execution contract"),
	nestedCallableApproval("tool_guard.go", "variable processor in func processGuardedToolRequest via method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "retained native request processor for a guarded tool"),
	nestedCallableApproval("tool_guard.go", "func nativeConfirmationTool via parameter value -> method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "native ADK function-tool input to confirmation decoration"),
	nestedCallableApproval("tool_guard.go", "func nativeConfirmationTool via result 0 -> method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "native ADK confirmed function-tool output"),
	nestedCallableApproval("tool_guard.go", "func reguardFunctionTool via parameter current -> pointer element -> field source -> method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "policy wrapper retains the selected function-tool delegate during redecoration"),
	nestedCallableApproval("tool_guard.go", "func reguardFunctionTool via parameter current -> pointer element -> field confirmed -> method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "policy wrapper retains the native confirmation delegate during redecoration"),
	nestedCallableApproval("tool_guard.go", "func reguardStreamingTool via parameter current -> pointer element -> field source -> method RunStream", "func(google.golang.org/adk/v2/agent.Context, any) iter.Seq2[string, error]", "policy wrapper retains the streaming delegate during redecoration"),
	nestedCallableApproval("tool_guard.go", "func guardFunctionTool via parameter current -> method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "native function-tool input to policy decoration"),
	nestedCallableApproval("tool_guard.go", "func guardStreamingTool via parameter current -> method RunStream", "func(google.golang.org/adk/v2/agent.Context, any) iter.Seq2[string, error]", "native streaming-tool input to policy decoration"),
	nestedCallableApproval("tool_guard.go", "variable guarded in func guardFunctionTool via pointer element -> field source -> method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "new policy wrapper retains the selected function-tool delegate"),
	nestedCallableApproval("tool_guard.go", "variable guarded in func guardFunctionTool via pointer element -> field confirmed -> method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "new policy wrapper retains the native confirmation delegate"),
	nestedCallableApproval("tool_guard.go", "variable confirmed in func guardFunctionTool via method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "resolved native confirmation delegate"),
	nestedCallableApproval("tool_guard.go", "variable confirmed in func nativeConfirmationTool via method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "validated native confirmation delegate"),
	nestedCallableApproval("tool_guard.go", "variable executor in method *guardedFunctionTool.Run via method Run", "func(google.golang.org/adk/v2/agent.Context, any) (map[string]any, error)", "selected confirmed or direct native execution delegate"),
	{file: "harness_construction.go", kind: "top-level function", owner: "func loadHarnessOptions", fingerprint: "func(ctx context.Context, supplied []Option) (options, github.com/RandomCodeSpace/plasmid/config.Result, error)", count: 1, rationale: "cancellation-aware Harness option and configuration loading"},
	{file: "harness_construction.go", kind: "top-level function", owner: "func newHarnessConstruction", fingerprint: "func(ctx context.Context, opts options, loaded github.com/RandomCodeSpace/plasmid/config.Result) (*harnessConstruction, context.Context)", count: 1, rationale: "creates the Harness-owned root cancellation lifecycle without retaining the caller context"},
	{file: "harness_construction.go", kind: "method", owner: "method *harnessConstruction.build", fingerprint: "func(ctx context.Context, rootContext context.Context) error", count: 1, rationale: "transactional cancellation-aware Harness construction coordinator"},
	{file: "harness_construction.go", kind: "method", owner: "method *harnessConstruction.configureLSP", fingerprint: "func(rootContext context.Context) error", count: 1, rationale: "constructs lifecycle-owned LSP resources under the Harness root context"},
	{file: "harness_construction.go", kind: "method", owner: "method *harnessConstruction.configureAgentAndRunner", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "constructs native ADK agent and runner while honoring caller cancellation"},
	{file: "harness_construction.go", kind: "function literal", owner: "literal in method *harnessConstruction.agentConfig", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, current google.golang.org/adk/v2/tool.Tool, args map[string]any) (map[string]any, error)", count: 1, rationale: "native ADK argument-aware before-tool policy callback"},
	nestedCallableApproval("harness_construction.go", "variable scoped in func confirmationToolsets via field processor -> method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "confirmation mode retains the scoped source request processor"),
	{file: "mcp/manager.go", kind: "method", owner: "method *Manager.connect", fingerprint: "func(ctx context.Context, key connectionKey, qualified string, server github.com/RandomCodeSpace/plasmid/config.MCPServer) (*connection, error)", count: 1, rationale: "lifecycle-owned native MCP operation with cancellation and deterministic bounds"},
	{file: "plugin_callbacks.go", kind: "function literal", owner: "literal in func guardPluginCallbacks", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, current google.golang.org/adk/v2/tool.Tool, arguments map[string]any, output map[string]any, cause error) (result map[string]any, err error)", count: 1, rationale: "panic-isolated native ADK plugin callback wrapper"},
	{file: "extensions/store.go", kind: "nested callable", owner: "type Store via field discover", fingerprint: "func(context.Context, Options) (Catalog, error)", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "harness.go", kind: "method", owner: "method *Harness.run", fingerprint: "func(ctx context.Context, sessionID string, prompt string, policy *github.com/RandomCodeSpace/plasmid/internal/syntax.ToolPolicy) iter.Seq2[*google.golang.org/adk/v2/session.Event, error]", count: 1, rationale: "Harness-owned native ADK session and template operation"},
	{file: "mcp/command_transport.go", kind: "method", owner: "method *processConnection.Read", fingerprint: "func(ctx context.Context) (github.com/modelcontextprotocol/go-sdk/jsonrpc.Message, error)", count: 1, rationale: "lifecycle-owned native MCP operation with cancellation and deterministic bounds"},
	{file: "mcp/manager.go", kind: "top-level function", owner: "func linkedContext", fingerprint: "func(ctx context.Context, rootDone <-chan struct{}) (context.Context, context.CancelFunc)", count: 1, rationale: "links one MCP operation to lifecycle cancellation without retaining context in an owner struct"},
	{file: "mcp/manager.go", kind: "method", owner: "method *Manager.DropSession", fingerprint: "func(ctx context.Context, sessionID string) error", count: 1, rationale: "lifecycle-owned native MCP operation with cancellation and deterministic bounds"},
	{file: "mcp/manager.go", kind: "method", owner: "method *Manager.waitForSessionTeardowns", fingerprint: "func(ctx context.Context, connections []*connection, results <-chan error) error", count: 1, rationale: "bounded MCP session teardown wait separated from transport scheduling"},
	{file: "harness.go", kind: "method", owner: "method *Harness.extensionCatalog", fingerprint: "func(ctx context.Context, sessionID string) (github.com/RandomCodeSpace/plasmid/extensions.Catalog, error)", count: 1, rationale: "Harness-owned native ADK session and template operation"},
	{file: "extensions/store.go", kind: "nested callable", owner: "func NewStore via result 0 -> pointer element -> field discover", fingerprint: "func(context.Context, Options) (Catalog, error)", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "plugin_callbacks.go", kind: "function literal", owner: "literal in func guardPluginCallbacks", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, current google.golang.org/adk/v2/tool.Tool, arguments map[string]any, cause error) (result map[string]any, err error)", count: 1, rationale: "panic-isolated native ADK plugin callback wrapper"},
	{file: "plugin_callbacks.go", kind: "function literal", owner: "literal in func guardPluginCallbacks", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest) (result *google.golang.org/adk/v2/model.LLMResponse, err error)", count: 1, rationale: "panic-isolated native ADK plugin callback wrapper"},
	{file: "skills/toolset.go", kind: "method", owner: "method *Toolset.load", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, args loadArgs) (map[string]any, error)", count: 1, rationale: "native ADK skill toolset activation operation"},
	{file: "foreign/copilot.go", kind: "top-level function", owner: "func ScanCopilotWithActivations", fingerprint: "func(ctx context.Context, options Options, vault *github.com/RandomCodeSpace/plasmid/internal/foreignactivation.Vault) (HostCatalog, error)", count: 1, rationale: "bounded cancellation-aware foreign activation discovery"},
	{file: "plugin_callbacks.go", kind: "function literal", owner: "literal in func guardPluginCallbacks", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, response *google.golang.org/adk/v2/model.LLMResponse, cause error) (result *google.golang.org/adk/v2/model.LLMResponse, err error)", count: 1, rationale: "panic-isolated native ADK plugin callback wrapper"},
	{file: "skills/toolset.go", kind: "method", owner: "method *Toolset.Tools", fingerprint: "func(ctx google.golang.org/adk/v2/agent.ReadonlyContext) ([]google.golang.org/adk/v2/tool.Tool, error)", count: 1, rationale: "native ADK skill toolset activation operation"},
	{file: "extensions/catalog.go", kind: "top-level function", owner: "func readConfined", fingerprint: "func(ctx context.Context, rootPath string, relative string, maximum int64, expectedRoot os.FileInfo) (data []byte, err error)", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "contextresolver/expansion.go", kind: "method", owner: "method *Resolver.Expand", fingerprint: "func(ctx context.Context, value Expansion) (string, error)", count: 1, rationale: "bounded extension prompt expansion"},
	{file: "harness.go", kind: "method", owner: "method *Harness.beginSessionOperation", fingerprint: "func(ctx context.Context, sessionID string, operation string) (context.Context, func(), error)", count: 1, rationale: "Harness-owned native ADK session and template operation"},
	{file: "mcp/manager.go", kind: "method", owner: "method *connection.loadTools", fingerprint: "func(ctx context.Context) ([]google.golang.org/adk/v2/tool.Tool, error)", count: 1, rationale: "lifecycle-owned native MCP operation with cancellation and deterministic bounds"},
	{file: "mcp/manager.go", kind: "method", owner: "method *connection.discoverTools", fingerprint: "func(ctx context.Context) (map[string]projectedTool, error)", count: 1, rationale: "bounded cancellable MCP tool discovery separated from native projection"},
	{file: "plugin_callbacks.go", kind: "function literal", owner: "literal in func guardPluginCallbacks", fingerprint: "func(ctx google.golang.org/adk/v2/agent.InvocationContext)", count: 1, rationale: "panic-isolated native ADK plugin callback wrapper"},
	{file: "skills/toolset.go", kind: "method", owner: "method *Toolset.ProcessRequest", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest) error", count: 1, rationale: "native ADK skill toolset activation operation"},
	{file: "foreign/claude.go", kind: "top-level function", owner: "func ScanClaudeWithActivations", fingerprint: "func(ctx context.Context, options Options, vault *github.com/RandomCodeSpace/plasmid/internal/foreignactivation.Vault) (HostCatalog, error)", count: 1, rationale: "bounded cancellation-aware foreign activation discovery"},
	{file: "mcp/manager.go", kind: "method", owner: "method *Manager.connection", fingerprint: "func(ctx context.Context, sessionID string, name string, catalog github.com/RandomCodeSpace/plasmid/extensions.Catalog) (*connection, error)", count: 1, rationale: "lifecycle-owned native MCP operation with cancellation and deterministic bounds"},
	{file: "extensions/discovery.go", kind: "method", owner: "method *catalogBuilder.scanConfiguredRoot", fingerprint: "func(ctx context.Context, rootPath string, options Options) (err error)", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "extensions/store.go", kind: "method", owner: "method *Store.StartSession", fingerprint: "func(ctx context.Context, sessionID string) error", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "plugin_callbacks.go", kind: "function literal", owner: "literal in func guardPluginCallbacks", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest, cause error) (result *google.golang.org/adk/v2/model.LLMResponse, err error)", count: 1, rationale: "panic-isolated native ADK plugin callback wrapper"},
	{file: "mcp/manager.go", kind: "function literal", owner: "literal in method *connection.nativeTools", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, arguments map[string]any) (map[string]any, error)", count: 1, rationale: "lifecycle-owned native MCP operation with cancellation and deterministic bounds"},
	{file: "extensions/store.go", kind: "method", owner: "method *Store.startSession", fingerprint: "func(ctx context.Context, sessionID string, instructions []Instruction) error", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "plugin_callbacks.go", kind: "function literal", owner: "literal in func guardPluginCallbacks", fingerprint: "func(ctx google.golang.org/adk/v2/agent.InvocationContext, event *google.golang.org/adk/v2/session.Event) (result *google.golang.org/adk/v2/session.Event, err error)", count: 1, rationale: "panic-isolated native ADK plugin callback wrapper"},
	{file: "extensions/catalog.go", kind: "method", owner: "method Catalog.LoadSkillResource", fingerprint: "func(ctx context.Context, name string, resource string, model bool) (string, error)", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "foreign/codex.go", kind: "top-level function", owner: "func ScanCodexWithActivations", fingerprint: "func(ctx context.Context, options Options, vault *github.com/RandomCodeSpace/plasmid/internal/foreignactivation.Vault) (HostCatalog, error)", count: 1, rationale: "bounded cancellation-aware foreign activation discovery"},
	{file: "extensions/catalog.go", kind: "method", owner: "method Catalog.LoadSkill", fingerprint: "func(ctx context.Context, name string, model bool) (LoadedSkill, error)", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "mcp/command_transport.go", kind: "method", owner: "method *commandTransport.Connect", fingerprint: "func(context.Context) (github.com/modelcontextprotocol/go-sdk/mcp.Connection, error)", count: 1, rationale: "lifecycle-owned native MCP operation with cancellation and deterministic bounds"},
	{file: "extensions/catalog.go", kind: "method", owner: "method Catalog.LoadTemplate", fingerprint: "func(ctx context.Context, name string, model bool) (LoadedTemplate, error)", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "extensions/store.go", kind: "method", owner: "method *Store.StartSessionWithInstructions", fingerprint: "func(ctx context.Context, sessionID string, instructions []Instruction) error", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "plugin_callbacks.go", kind: "function literal", owner: "literal in func guardPluginCallbacks", fingerprint: "func(ctx google.golang.org/adk/v2/agent.InvocationContext) (result *google.golang.org/genai.Content, err error)", count: 1, rationale: "panic-isolated native ADK plugin callback wrapper"},
	{file: "harness.go", kind: "method", owner: "method *Harness.AskTemplate", fingerprint: "func(ctx context.Context, sessionID string, name string, arguments string) (string, error)", count: 1, rationale: "Harness-owned native ADK session and template operation"},
	{file: "skills/toolset.go", kind: "method", owner: "method *Toolset.loadResource", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, args resourceArgs) (map[string]any, error)", count: 1, rationale: "native ADK skill toolset activation operation"},
	{file: "plugin_callbacks.go", kind: "function literal", owner: "literal in func guardPluginCallbacks", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, current google.golang.org/adk/v2/tool.Tool, arguments map[string]any) (result map[string]any, err error)", count: 1, rationale: "panic-isolated native ADK plugin callback wrapper"},
	{file: "harness.go", kind: "method", owner: "method *Harness.loadTemplate", fingerprint: "func(ctx context.Context, sessionID string, name string, arguments string) (expandedTemplate, error)", count: 1, rationale: "Harness-owned native ADK session and template operation"},
	{file: "extensions/discovery.go", kind: "nested callable", owner: "variable hostScans in func discover via slice element -> field scan", fingerprint: "func(context.Context, github.com/RandomCodeSpace/plasmid/foreign.Options, *github.com/RandomCodeSpace/plasmid/internal/foreignactivation.Vault) (github.com/RandomCodeSpace/plasmid/foreign.HostCatalog, error)", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "harness.go", kind: "method", owner: "method *Harness.RunTemplate", fingerprint: "func(ctx context.Context, sessionID string, name string, arguments string) iter.Seq2[*google.golang.org/adk/v2/session.Event, error]", count: 1, rationale: "Harness-owned native ADK session and template operation"},
	{file: "harness.go", kind: "method", owner: "method *Harness.beginOperation", fingerprint: "func(ctx context.Context, operation string) (context.Context, func(), error)", count: 1, rationale: "Harness-owned native ADK session and template operation"},
	{file: "harness.go", kind: "top-level function", owner: "func linkedOperationContext", fingerprint: "func(ctx context.Context, rootDone <-chan struct{}) (context.Context, context.CancelFunc, func())", count: 1, rationale: "links one Harness operation to lifecycle cancellation without retaining context in the Harness"},
	{file: "mcp/manager.go", kind: "method", owner: "method *ownedTransport.Connect", fingerprint: "func(ctx context.Context) (github.com/modelcontextprotocol/go-sdk/mcp.Connection, error)", count: 1, rationale: "lifecycle-owned native MCP operation with cancellation and deterministic bounds"},
	{file: "extensions/discovery.go", kind: "top-level function", owner: "func discover", fingerprint: "func(ctx context.Context, options Options) (Catalog, error)", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "mcp/manager.go", kind: "method", owner: "method *connection.call", fingerprint: "func(ctx context.Context, sessionID string, name string, arguments map[string]any) (resultMap map[string]any, err error)", count: 1, rationale: "lifecycle-owned native MCP operation with cancellation and deterministic bounds"},
	{file: "skills/toolset.go", kind: "method", owner: "method *Toolset.catalog", fingerprint: "func(ctx context.Context) (github.com/RandomCodeSpace/plasmid/extensions.Catalog, error)", count: 1, rationale: "native ADK skill toolset activation operation"},
	{file: "harness.go", kind: "method", owner: "method *Harness.ListTemplates", fingerprint: "func(ctx context.Context, sessionID string) ([]github.com/RandomCodeSpace/plasmid/extensions.Template, error)", count: 1, rationale: "Harness-owned native ADK session and template operation"},
	{file: "extensions/store.go", kind: "method", owner: "method *Store.ObserveTouch", fingerprint: "func(_ context.Context, touch github.com/RandomCodeSpace/plasmid/workspace.Touch)", count: 1, rationale: "bounded cancellation-aware extension snapshot operation"},
	{file: "plugin_callbacks.go", kind: "function literal", owner: "literal in func guardPluginCallbacks", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context) (result *google.golang.org/genai.Content, err error)", count: 2, rationale: "panic-isolated native ADK plugin callback wrapper"},
	{file: "plugin_callbacks.go", kind: "function literal", owner: "literal in func guardPluginCallbacks", fingerprint: "func(ctx google.golang.org/adk/v2/agent.InvocationContext, content *google.golang.org/genai.Content) (result *google.golang.org/genai.Content, err error)", count: 1, rationale: "panic-isolated native ADK plugin callback wrapper"},
	{file: "mcp/command_transport.go", kind: "method", owner: "method *processConnection.Write", fingerprint: "func(ctx context.Context, message github.com/modelcontextprotocol/go-sdk/jsonrpc.Message) error", count: 1, rationale: "lifecycle-owned native MCP operation with cancellation and deterministic bounds"},
	{file: "mcp/manager.go", kind: "method", owner: "method *Manager.Tools", fingerprint: "func(ctx google.golang.org/adk/v2/agent.ReadonlyContext) ([]google.golang.org/adk/v2/tool.Tool, error)", count: 1, rationale: "lifecycle-owned native MCP operation with cancellation and deterministic bounds"},
	{file: "harness.go", kind: "method", owner: "method *Harness.runAcquired", fingerprint: "func(ctx context.Context, sessionID string, prompt string, policy *github.com/RandomCodeSpace/plasmid/internal/syntax.ToolPolicy, yield func(*google.golang.org/adk/v2/session.Event, error) bool)", count: 1, rationale: "Harness-owned native ADK session and template operation"},
	{file: "skills/toolset.go", kind: "method", owner: "method *Toolset.list", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, _ struct{}) (map[string]any, error)", count: 1, rationale: "native ADK skill toolset activation operation"},
	{file: "harness.go", kind: "method", owner: "method *Harness.GetTemplate", fingerprint: "func(ctx context.Context, sessionID string, name string, arguments string) (string, error)", count: 1, rationale: "Harness-owned native ADK session and template operation"},
	{file: "harness.go", kind: "top-level function", owner: "func New", fingerprint: "func(ctx context.Context, supplied ...Option) (*Harness, error)", count: 1, rationale: "transactional native Harness construction entry point"},
	{file: "harness.go", kind: "method", owner: "method *Harness.NewSession", fingerprint: "func(ctx context.Context) (string, error)", count: 1, rationale: "public durable session creation operation"},
	{file: "harness.go", kind: "method", owner: "method *Harness.ResumeSession", fingerprint: "func(ctx context.Context, sessionID string) error", count: 1, rationale: "public existing-session verification operation"},
	{file: "harness.go", kind: "method", owner: "method *Harness.Run", fingerprint: "func(ctx context.Context, sessionID string, prompt string) iter.Seq2[*google.golang.org/adk/v2/session.Event, error]", count: 1, rationale: "public native ADK event-stream operation"},
	{file: "harness.go", kind: "method", owner: "method *Harness.Ask", fingerprint: "func(ctx context.Context, sessionID string, prompt string) (string, error)", count: 1, rationale: "public final root-agent text convenience operation"},
	{file: "harness.go", kind: "method", owner: "method *Harness.beginRun", fingerprint: "func(ctx context.Context, sessionID string) (context.Context, func(), error)", count: 1, rationale: "private active-run cancellation and per-session exclusion owner"},
	{file: "harness.go", kind: "method", owner: "method scopedToolset.ProcessRequest", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest) error", count: 1, rationale: "native ADK per-request dynamic tool visibility and source processor delegation"},
	{file: "harness.go", kind: "method", owner: "method scopedToolset.Tools", fingerprint: "func(ctx google.golang.org/adk/v2/agent.ReadonlyContext) ([]google.golang.org/adk/v2/tool.Tool, error)", count: 1, rationale: "native ADK toolset projection through session-scoped syntax policy"},
	nestedCallableApproval("harness.go", "type toolsetRequestProcessor via method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "native ADK toolset request-processor contract"),
	nestedCallableApproval("harness.go", "type scopedToolset via field processor -> method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "retained source toolset request processor"),
	nestedCallableApproval("harness.go", "variable processor in method scopedToolset.ProcessRequest via method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "resolved source toolset request processor"),
	{file: "harness.go", kind: "method", owner: "method instructionProvider.Provide", fingerprint: "func(ctx google.golang.org/adk/v2/agent.ReadonlyContext) (string, error)", count: 1, rationale: "native ADK dynamic context and LSP instruction composition"},
	{file: "compaction/manager.go", kind: "method", owner: "method *Manager.BeforeModel", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, request *google.golang.org/adk/v2/model.LLMRequest) (*google.golang.org/adk/v2/model.LLMResponse, error)", count: 1, rationale: "native before-model request compaction callback"},
	{file: "compaction/manager.go", kind: "method", owner: "method *Manager.AfterModel", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, response *google.golang.org/adk/v2/model.LLMResponse, responseError error) (*google.golang.org/adk/v2/model.LLMResponse, error)", count: 1, rationale: "native after-model prompt-usage calibration callback"},
	{file: "compaction/manager.go", kind: "method", owner: "method *Manager.before", fingerprint: "func(ctx context.Context, current identity, request *google.golang.org/adk/v2/model.LLMRequest) policyResult", count: 1, rationale: "testable durable before-model callback implementation"},
	{file: "compaction/manager.go", kind: "method", owner: "method *Manager.after", fingerprint: "func(ctx context.Context, current identity, response *google.golang.org/adk/v2/model.LLMResponse, responseError error)", count: 1, rationale: "testable durable calibration implementation"},
	{file: "compaction/manager.go", kind: "method", owner: "method *Manager.load", fingerprint: "func(ctx context.Context, current identity, state *sessionState)", count: 1, rationale: "cancellation-aware compaction sidecar load"},
	{file: "compaction/manager.go", kind: "method", owner: "method *Manager.save", fingerprint: "func(ctx context.Context, current identity, state *sessionState)", count: 1, rationale: "cancellation-aware compaction sidecar save"},
	{file: "lsp_callback.go", kind: "top-level function", owner: "func decorateLSPAfterTool", fingerprint: "func(decorator lspDecorator, ctx google.golang.org/adk/v2/agent.Context, currentTool google.golang.org/adk/v2/tool.Tool, result map[string]any, toolErr error) (map[string]any, error)", count: 1, rationale: "bounded native after-tool diagnostic decoration separated from panic containment"},
	nestedCallableApproval("lsp_callback.go", "func decorateLSPAfterTool via parameter decorator -> method Await", "func(context.Context, string, string) (github.com/RandomCodeSpace/plasmid/lsp.Decoration, bool)", "retains the framework-free diagnostic wait contract"),
	{file: "config/load.go", kind: "top-level function", owner: "func Load", fingerprint: "func(ctx context.Context, options Options) (Result, error)", count: 1, rationale: "cancellation-aware versioned configuration entry point"},
	{file: "contextresolver/commands.go", kind: "top-level function", owner: "func expandCommands", fingerprint: "func(ctx context.Context, source string, path string, trust TrustLevel, options commandOptions, executor *github.com/RandomCodeSpace/plasmid/shellexec.Executor, sink github.com/RandomCodeSpace/plasmid/warning.Warner) string", count: 1, rationale: "trust-gated bounded prompt command expansion leaf"},
	{file: "contextresolver/commands.go", kind: "top-level function", owner: "func expandCommandsWithBudget", fingerprint: "func(ctx context.Context, input commandExpansion) string", count: 1, rationale: "trust-gated prompt command expansion sharing source-document time and output budgets across imports"},
	{file: "contextresolver/commands.go", kind: "method", owner: "method commandExpansion.run", fingerprint: "func(ctx context.Context, directive github.com/RandomCodeSpace/plasmid/internal/syntax.CommandDirective) string", count: 1, rationale: "explicit context propagation for bounded prompt command execution"},
	{file: "contextresolver/discovery.go", kind: "method", owner: "method *Resolver.discover", fingerprint: "func(ctx context.Context) ([]document, error)", count: 1, rationale: "bounded cancellation-aware instruction discovery"},
	{file: "contextresolver/discovery.go", kind: "method", owner: "method *Resolver.candidates", fingerprint: "func(ctx context.Context, state *discoveryState) ([]candidate, error)", count: 1, rationale: "cancellation-aware instruction candidate collection sharing the discovery budget"},
	{file: "contextresolver/discovery.go", kind: "method", owner: "method *Resolver.loadDocument", fingerprint: "func(ctx context.Context, source candidate, state *discoveryState) (document, bool)", count: 1, rationale: "bounded fail-soft instruction loading"},
	{file: "contextresolver/discovery.go", kind: "method", owner: "method *Resolver.expandImports", fingerprint: "func(ctx context.Context, sourcePath string, body string, host github.com/RandomCodeSpace/plasmid/internal/syntax.Host, trust TrustLevel, depth int, state *discoveryState) importExpansion", count: 1, rationale: "confined cancellation-aware Claude import expansion with policy provenance"},
	{file: "contextresolver/discovery.go", kind: "method", owner: "method *Resolver.loadImport", fingerprint: "func(ctx context.Context, parent string, requested string, trust TrustLevel, depth int, state *discoveryState) importExpansion", count: 1, rationale: "bounded confined instruction import loading with source policy and trust"},
	{file: "contextresolver/discovery.go", kind: "top-level function", owner: "func readBoundedAt", fingerprint: "func(ctx context.Context, rootPath string, relative string, maximum int) (data []byte, truncated bool, err error)", count: 1, rationale: "descriptor-confined nonblocking cancellation-aware file reader"},
	{file: "contextresolver/resolver.go", kind: "method", owner: "method *Resolver.StartSession", fingerprint: "func(ctx context.Context, sessionID string) error", count: 1, rationale: "session-scoped immutable instruction snapshot construction"},
	{file: "contextresolver/resolver.go", kind: "method", owner: "method *Resolver.startSession", fingerprint: "func(ctx context.Context, sessionID string) error", count: 1, rationale: "shared session snapshot construction beneath synchronized public operations"},
	{file: "contextresolver/resolver.go", kind: "method", owner: "method *Resolver.Instructions", fingerprint: "func(ctx context.Context, sessionID string, invocationID string) (string, error)", count: 1, rationale: "native instruction assembly entry point with turn scope recording"},
	{file: "contextresolver/resolver.go", kind: "method", owner: "method *Resolver.assemble", fingerprint: "func(ctx context.Context, sessionID string, view *sessionView) (string, github.com/RandomCodeSpace/plasmid/internal/syntax.ToolPolicy, error)", count: 1, rationale: "bounded session-view prompt assembly"},
	{file: "contextresolver/resolver.go", kind: "method", owner: "method *Resolver.ObserveTouch", fingerprint: "func(_ context.Context, touch github.com/RandomCodeSpace/plasmid/workspace.Touch)", count: 1, rationale: "framework-free lazy instruction activation observer"},
	{file: "foreign/claude.go", kind: "top-level function", owner: "func ScanClaude", fingerprint: "func(ctx context.Context, options Options) (HostCatalog, error)", count: 1, rationale: "bounded cancellation-aware Claude metadata discovery entry point"},
	{file: "foreign/codex.go", kind: "top-level function", owner: "func ScanCodex", fingerprint: "func(ctx context.Context, options Options) (HostCatalog, error)", count: 1, rationale: "bounded cancellation-aware Codex metadata discovery entry point"},
	{file: "foreign/copilot.go", kind: "top-level function", owner: "func ScanCopilot", fingerprint: "func(ctx context.Context, options Options) (HostCatalog, error)", count: 1, rationale: "bounded cancellation-aware Copilot metadata discovery entry point"},
	{file: "foreign/scan.go", kind: "top-level function", owner: "func Scan", fingerprint: "func(ctx context.Context, options Options) (Catalog, error)", count: 1, rationale: "bounded cancellation-aware combined foreign discovery entry point"},
	{file: "foreign/scan.go", kind: "top-level function", owner: "func newScanner", fingerprint: "func(ctx context.Context, host Host, options Options) (*scanner, error)", count: 1, rationale: "shared cancellation-aware foreign scanner construction"},
	{file: "foreign/claude.go", kind: "function literal", owner: "literal in func ScanClaudeWithActivations", fingerprint: "func(scanCtx context.Context) error", count: 6, rationale: "explicit cancellation propagation across ordered Claude discovery steps"},
	nestedCallableApproval("foreign/claude.go", "variable steps in func ScanClaudeWithActivations via slice element", "func(context.Context) error", "ordered Claude discovery step contract"),
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.scanClaudePlugins", fingerprint: "func(ctx context.Context, catalog *HostCatalog) error", count: 1, rationale: "cancellation-aware Claude plugin discovery"},
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.loadClaudePluginIndex", fingerprint: "func(ctx context.Context, path string) (claudePluginIndex, bool)", count: 1, rationale: "cancellation-aware Claude plugin index read"},
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.scanClaudePluginIndex", fingerprint: "func(ctx context.Context, catalog *HostCatalog, path string, confinedPlugins *github.com/RandomCodeSpace/plasmid/workspace.Root, index claudePluginIndex) error", count: 1, rationale: "cancellation-aware Claude plugin index traversal"},
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.scanClaudePluginInstall", fingerprint: "func(ctx context.Context, catalog *HostCatalog, indexPath string, confinedPlugins *github.com/RandomCodeSpace/plasmid/workspace.Root, identifier string, entry claudeInstall, enabled bool) error", count: 1, rationale: "cancellation-aware Claude plugin installation discovery"},
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.scanClaudePluginComponents", fingerprint: "func(ctx context.Context, catalog *HostCatalog, root string, manifest pluginManifest, hasManifest bool, origin discoverySource) error", count: 1, rationale: "cancellation-aware Claude plugin component discovery"},
	{file: "foreign/claude.go", kind: "function literal", owner: "literal in method *scanner.scanClaudePluginComponents", fingerprint: "func(scanCtx context.Context) error", count: 2, rationale: "explicit cancellation propagation across Claude plugin component steps"},
	nestedCallableApproval("foreign/claude.go", "variable steps in method *scanner.scanClaudePluginComponents via slice element", "func(context.Context) error", "ordered Claude plugin component step contract"),
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.scanClaudePluginMCP", fingerprint: "func(ctx context.Context, catalog *HostCatalog, root string, manifest pluginManifest, hasManifest bool, origin discoverySource) error", count: 1, rationale: "cancellation-aware Claude plugin MCP discovery"},
	{file: "foreign/claude.go", kind: "function literal", owner: "literal in method *scanner.scanClaudePluginMCP", fingerprint: "func(scanCtx context.Context) error", count: 1, rationale: "explicit cancellation propagation across Claude plugin MCP steps"},
	nestedCallableApproval("foreign/claude.go", "variable steps in method *scanner.scanClaudePluginMCP via slice element", "func(context.Context) error", "ordered Claude plugin MCP step contract"),
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.claudeEnabledPlugins", fingerprint: "func(ctx context.Context) map[string]bool", count: 1, rationale: "cancellation-aware Claude settings discovery"},
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.scanClaudeMCP", fingerprint: "func(ctx context.Context, catalog *HostCatalog) error", count: 1, rationale: "cancellation-aware Claude MCP discovery"},
	{file: "foreign/claude.go", kind: "function literal", owner: "literal in method *scanner.scanClaudeMCP", fingerprint: "func(scanCtx context.Context) error", count: 3, rationale: "explicit cancellation propagation across ordered Claude MCP steps"},
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.loadClaudeMCPRoot", fingerprint: "func(ctx context.Context, path string) (map[string]encoding/json.RawMessage, error)", count: 1, rationale: "cancellation-aware Claude MCP configuration read"},
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.scanClaudeLocalMCP", fingerprint: "func(ctx context.Context, catalog *HostCatalog, path string, root map[string]encoding/json.RawMessage) error", count: 1, rationale: "cancellation-aware Claude local MCP traversal"},
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.scanClaudeLocalProject", fingerprint: "func(ctx context.Context, catalog *HostCatalog, path string, projects map[string]encoding/json.RawMessage, directory string) error", count: 1, rationale: "cancellation-aware Claude project MCP extraction"},
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.scanClaudeProjectMCP", fingerprint: "func(ctx context.Context, catalog *HostCatalog) error", count: 1, rationale: "cancellation-aware trusted-project MCP discovery"},
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.scanClaudeUserMCP", fingerprint: "func(ctx context.Context, catalog *HostCatalog, path string, root map[string]encoding/json.RawMessage) error", count: 1, rationale: "cancellation-aware Claude user MCP extraction"},
	{file: "foreign/claude.go", kind: "method", owner: "method *scanner.scanClaudeMCPFile", fingerprint: "func(ctx context.Context, catalog *HostCatalog, path string, origin discoverySource) error", count: 1, rationale: "cancellation-aware Claude MCP declaration read"},
	{file: "foreign/codex.go", kind: "function literal", owner: "literal in func ScanCodexWithActivations", fingerprint: "func(scanCtx context.Context) error", count: 7, rationale: "explicit cancellation propagation across ordered Codex discovery steps"},
	nestedCallableApproval("foreign/codex.go", "variable steps in func ScanCodexWithActivations via slice element", "func(context.Context) error", "ordered Codex discovery step contract"),
	{file: "foreign/codex.go", kind: "method", owner: "method *scanner.warnIfUntrustedFile", fingerprint: "func(ctx context.Context, path string)", count: 1, rationale: "cancellation-aware untrusted project metadata check"},
	{file: "foreign/codex.go", kind: "method", owner: "method *scanner.scanCodexConfig", fingerprint: "func(ctx context.Context, catalog *HostCatalog, path string, scope Scope, pluginEnabled map[string]bool) error", count: 1, rationale: "cancellation-aware Codex configuration discovery"},
	{file: "foreign/codex.go", kind: "method", owner: "method *scanner.scanCodexMarketplace", fingerprint: "func(ctx context.Context, catalog *HostCatalog, path string, root string, scope Scope, classification Classification, pluginEnabled map[string]bool) error", count: 1, rationale: "cancellation-aware Codex marketplace traversal"},
	{file: "foreign/codex.go", kind: "method", owner: "method *scanner.scanCodexPlugin", fingerprint: "func(ctx context.Context, catalog *HostCatalog, input codexPluginInput) error", count: 1, rationale: "cancellation-aware Codex plugin discovery"},
	{file: "foreign/codex.go", kind: "method", owner: "method *scanner.scanCodexPluginMCP", fingerprint: "func(ctx context.Context, catalog *HostCatalog, path string, origin discoverySource) error", count: 1, rationale: "cancellation-aware Codex plugin MCP discovery"},
	{file: "foreign/copilot.go", kind: "function literal", owner: "literal in func ScanCopilotWithActivations", fingerprint: "func(scanCtx context.Context) error", count: 7, rationale: "explicit cancellation propagation across ordered Copilot discovery steps"},
	nestedCallableApproval("foreign/copilot.go", "variable steps in func ScanCopilotWithActivations via slice element", "func(context.Context) error", "ordered Copilot discovery step contract"),
	{file: "foreign/copilot.go", kind: "method", owner: "method *scanner.scanCopilotPreview", fingerprint: "func(ctx context.Context, catalog *HostCatalog) error", count: 1, rationale: "cancellation-aware Copilot preview discovery"},
	{file: "foreign/copilot.go", kind: "method", owner: "method *scanner.scanCopilotPreviewRoot", fingerprint: "func(ctx context.Context, catalog *HostCatalog, root string) error", count: 1, rationale: "cancellation-aware Copilot preview root read"},
	{file: "foreign/copilot.go", kind: "method", owner: "method *scanner.scanCopilotMCP", fingerprint: "func(ctx context.Context, catalog *HostCatalog) error", count: 1, rationale: "cancellation-aware Copilot MCP discovery"},
	{file: "foreign/copilot.go", kind: "method", owner: "method *scanner.scanCopilotPlugins", fingerprint: "func(ctx context.Context, catalog *HostCatalog) error", count: 1, rationale: "cancellation-aware Copilot plugin discovery"},
	{file: "foreign/copilot.go", kind: "method", owner: "method *scanner.scanCopilotPluginGroup", fingerprint: "func(ctx context.Context, catalog *HostCatalog, root string) error", count: 1, rationale: "cancellation-aware Copilot plugin group traversal"},
	{file: "foreign/copilot.go", kind: "method", owner: "method *scanner.scanCopilotPlugin", fingerprint: "func(ctx context.Context, catalog *HostCatalog, root string) error", count: 1, rationale: "cancellation-aware Copilot plugin discovery"},
	{file: "foreign/copilot.go", kind: "method", owner: "method *scanner.scanCopilotMCPFile", fingerprint: "func(ctx context.Context, catalog *HostCatalog, path string, origin discoverySource, options copilotMCPOptions) error", count: 1, rationale: "cancellation-aware Copilot MCP declaration read"},
	{file: "foreign/plugin.go", kind: "method", owner: "method *scanner.loadPluginManifest", fingerprint: "func(ctx context.Context, root string, candidates []string, required bool) (pluginManifest, bool)", count: 1, rationale: "cancellation-aware foreign plugin manifest read"},
	{file: "foreign/records.go", kind: "method", owner: "method *scanner.scanTemplateRoot", fingerprint: "func(ctx context.Context, catalog *HostCatalog, root string, suffix string, origin discoverySource) error", count: 1, rationale: "cancellation-aware foreign template traversal"},
	{file: "foreign/records.go", kind: "method", owner: "method *scanner.scanTemplateEntry", fingerprint: "func(ctx context.Context, catalog *HostCatalog, path string, name string, origin discoverySource) error", count: 1, rationale: "cancellation-aware foreign template read"},
	{file: "foreign/records.go", kind: "method", owner: "method *scanner.addMCPMap", fingerprint: "func(ctx context.Context, catalog *HostCatalog, servers map[string]encoding/json.RawMessage, path string, origin discoverySource) error", count: 1, rationale: "cancellation-aware foreign MCP record traversal"},
	{file: "foreign/records.go", kind: "method", owner: "method *scanner.addMCPMapReplacing", fingerprint: "func(ctx context.Context, catalog *HostCatalog, servers map[string]encoding/json.RawMessage, path string, origin discoverySource) error", count: 1, rationale: "cancellation-aware replacing MCP record traversal"},
	{file: "foreign/records.go", kind: "method", owner: "method *scanner.addMCPMapMode", fingerprint: "func(ctx context.Context, catalog *HostCatalog, servers map[string]encoding/json.RawMessage, path string, origin discoverySource, replace bool) error", count: 1, rationale: "shared cancellation-aware foreign MCP record traversal"},
	{file: "foreign/scan.go", kind: "top-level function", owner: "func runScannerSteps", fingerprint: "func(ctx context.Context, steps ...func(context.Context) error) error", count: 1, rationale: "ordered foreign discovery execution with explicit cancellation propagation"},
	nestedCallableApproval("foreign/scan.go", "func runScannerSteps via parameter steps -> slice element", "func(context.Context) error", "explicitly cancellable foreign discovery step input"),
	{file: "foreign/scan.go", kind: "top-level function", owner: "func checkContext", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "single foreign discovery cancellation check"},
	{file: "foreign/skill.go", kind: "method", owner: "method *scanner.scanSkillRoot", fingerprint: "func(ctx context.Context, catalog *HostCatalog, root string, origin discoverySource) error", count: 1, rationale: "cancellation-aware foreign skill traversal"},
	{file: "foreign/skill.go", kind: "method", owner: "method *scanner.scanSkillEntry", fingerprint: "func(ctx context.Context, catalog *HostCatalog, path string, origin discoverySource) error", count: 1, rationale: "cancellation-aware foreign skill read"},
	{file: "foreign/skill.go", kind: "method", owner: "method *scanner.readFile", fingerprint: "func(ctx context.Context, path string) ([]byte, error)", count: 1, rationale: "bounded cancellation-aware foreign file read"},
	{file: "foreign/skill.go", kind: "method", owner: "method *scanner.readDir", fingerprint: "func(ctx context.Context, path string) ([]os.DirEntry, error)", count: 1, rationale: "bounded cancellation-aware foreign directory read"},
	{file: "foreign/skill.go", kind: "top-level function", owner: "func readAllWithContext", fingerprint: "func(ctx context.Context, reader io.Reader) ([]byte, error)", count: 1, rationale: "bounded cancellation-aware foreign stream read"},
	{file: "foreign/toml.go", kind: "method", owner: "method *scanner.parseTOML", fingerprint: "func(ctx context.Context, path string, data []byte) []tomlSection", count: 1, rationale: "cancellation-aware foreign TOML parsing"},
	{file: "codingtools/bash.go", kind: "method", owner: "method *bashHandler.call", fingerprint: "func(ctx context.Context, sessionID string, args BashArgs) (result map[string]any, err error)", count: 1, rationale: "native typed coding-tool handler with ADK session identity"},
	{file: "codingtools/bash.go", kind: "top-level function", owner: "func bashContextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "codingtools/edit.go", kind: "method", owner: "method *editHandler.call", fingerprint: "func(ctx context.Context, sessionID string, args EditArgs) (result map[string]any, err error)", count: 1, rationale: "native typed coding-tool handler with ADK session identity"},
	{file: "codingtools/edit.go", kind: "method", owner: "method *editHandler.performEdit", fingerprint: "func(ctx context.Context, sessionID string, args EditArgs) (editOperation, error)", count: 1, rationale: "serialized cancellation-aware edit operation"},
	{file: "codingtools/edit.go", kind: "method", owner: "method *editHandler.replaceFile", fingerprint: "func(ctx context.Context, sessionID string, args EditArgs) (editOperation, error)", count: 1, rationale: "descriptor-confined edit replacement setup"},
	{file: "codingtools/edit.go", kind: "method", owner: "method *editHandler.replaceOpenedFile", fingerprint: "func(ctx context.Context, sessionID string, args EditArgs, relative string, parent *os.Root) (editOperation, error)", count: 1, rationale: "descriptor-confined edit replacement leaf"},
	{file: "codingtools/edit.go", kind: "top-level function", owner: "func editContextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "codingtools/edit.go", kind: "top-level function", owner: "func editReadCompleteFile", fingerprint: "func(ctx context.Context, parent *os.Root, name string, maxBytes int64) ([]byte, error)", count: 1, rationale: "bounded file-read leaf helper"},
	{file: "codingtools/find.go", kind: "method", owner: "method *findHandler.call", fingerprint: "func(ctx context.Context, sessionID string, args FindArgs) (result map[string]any, err error)", count: 1, rationale: "native typed coding-tool handler with ADK session identity"},
	{file: "codingtools/find.go", kind: "method", owner: "method *findHandler.collectFindEntries", fingerprint: "func(ctx context.Context, entryType string, base string, matcher github.com/RandomCodeSpace/plasmid/internal/pathglob.Matcher) ([]findEntry, []string, error)", count: 1, rationale: "bounded cancellation-aware find traversal"},
	{file: "codingtools/find.go", kind: "top-level function", owner: "func findContextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "codingtools/grep.go", kind: "method", owner: "method *grepHandler.call", fingerprint: "func(ctx context.Context, sessionID string, args GrepArgs) (result map[string]any, err error)", count: 1, rationale: "native typed coding-tool handler with ADK session identity"},
	{file: "codingtools/grep.go", kind: "method", owner: "method *grepHandler.searchPath", fingerprint: "func(ctx context.Context, args GrepArgs, absolute string, info os.FileInfo, state *grepState) error", count: 1, rationale: "cancellation-aware grep path dispatch"},
	{file: "codingtools/grep.go", kind: "method", owner: "method *grepHandler.searchRegularFile", fingerprint: "func(ctx context.Context, glob string, absolute string, info os.FileInfo, state *grepState) error", count: 1, rationale: "bounded regular-file grep dispatch"},
	{file: "codingtools/grep.go", kind: "method", owner: "method *grepHandler.searchDirectory", fingerprint: "func(ctx context.Context, glob string, absolute string, state *grepState) error", count: 1, rationale: "bounded directory grep traversal"},
	{file: "codingtools/grep.go", kind: "method", owner: "method *grepState.searchFile", fingerprint: "func(ctx context.Context, abs string, relative string, mode os.FileMode, size int64) error", count: 1, rationale: "bounded cancellation-aware grep file scan"},
	{file: "codingtools/grep.go", kind: "method", owner: "method *grepState.collectMatches", fingerprint: "func(ctx context.Context, relative string, lines []grepLine) (bool, error)", count: 1, rationale: "cancellation-aware grep match collection"},
	{file: "codingtools/grep.go", kind: "method", owner: "method *grepHandler.finish", fingerprint: "func(ctx context.Context, sessionID string, maximum int, state grepState, grant int, emitted *int) (map[string]any, error)", count: 1, rationale: "private bounded grep completion operation"},
	{file: "codingtools/grep.go", kind: "top-level function", owner: "func grepContextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "codingtools/grep.go", kind: "top-level function", owner: "func grepLines", fingerprint: "func(ctx context.Context, reader io.Reader) ([]grepLine, int, error)", count: 1, rationale: "cancellation-aware line scanning helper"},
	{file: "codingtools/internal/walk/walk.go", kind: "top-level function", owner: "func Walk", fingerprint: "func(ctx context.Context, filter *Filter, callback func(Entry) error) error", count: 1, rationale: "bounded cancellation-aware filesystem walking leaf"},
	{file: "codingtools/internal/walk/walk.go", kind: "top-level function", owner: "func walk", fingerprint: "func(ctx context.Context, filter *Filter, callback func(Entry) error, warn github.com/RandomCodeSpace/plasmid/warning.Warner) error", count: 1, rationale: "private bounded filesystem walking implementation"},
	{file: "codingtools/internal/walk/walk.go", kind: "top-level function", owner: "func validateWalk", fingerprint: "func(ctx context.Context, filter *Filter, callback func(Entry) error) error", count: 1, rationale: "filesystem walk boundary validation"},
	{file: "codingtools/internal/walk/walk.go", kind: "method", owner: "method *walkState.visit", fingerprint: "func(ctx context.Context, path string, entry io/fs.DirEntry, walkErr error) error", count: 1, rationale: "cancellation-aware filesystem visit callback"},
	{file: "codingtools/internal/walk/walk.go", kind: "method", owner: "method *walkState.stop", fingerprint: "func(ctx context.Context, next error) error", count: 1, rationale: "filesystem walk cancellation and bound settlement"},
	{file: "codingtools/internal/walk/walk.go", kind: "method", owner: "method *walkState.visitRoot", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "bounded selected-root visit"},
	{file: "codingtools/internal/walk/walk.go", kind: "method", owner: "method *walkState.emit", fingerprint: "func(ctx context.Context, _ string, relative string, entry io/fs.DirEntry) error", count: 1, rationale: "bounded cancellation-aware walk emission"},
	{file: "codingtools/list.go", kind: "method", owner: "method *listHandler.call", fingerprint: "func(ctx context.Context, sessionID string, args ListArgs) (result map[string]any, err error)", count: 1, rationale: "native typed coding-tool handler with ADK session identity"},
	{file: "codingtools/list.go", kind: "top-level function", owner: "func collectListEntries", fingerprint: "func(ctx context.Context, absolute string, relative string, args ListArgs) ([]ListEntry, error)", count: 1, rationale: "bounded cancellation-aware list traversal"},
	{file: "codingtools/list.go", kind: "top-level function", owner: "func listContextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "codingtools/native.go", kind: "function literal", owner: "literal in func newNativeTool", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, args T) (map[string]any, error)", count: 1, rationale: "native typed ADK function-tool callback"},
	{file: "codingtools/native.go", kind: "named function type", owner: "type nativeHandler", fingerprint: "func(context.Context, string, T) (map[string]any, error)", count: 1, rationale: "shared generic native coding-tool handler shape"},
	nestedCallableApproval("codingtools/native.go", "func newNativeTool via parameter handler", "func(context.Context, string, T) (map[string]any, error)", "native typed ADK function-tool handler input"),
	{file: "codingtools/read.go", kind: "method", owner: "method *readHandler.call", fingerprint: "func(ctx context.Context, sessionID string, args ReadArgs) (result map[string]any, err error)", count: 1, rationale: "native typed coding-tool handler with ADK session identity"},
	{file: "codingtools/read.go", kind: "method", owner: "method *readHandler.loadReadFile", fingerprint: "func(ctx context.Context, path string) (readFileSnapshot, error)", count: 1, rationale: "bounded cancellation-aware read preparation"},
	{file: "codingtools/read.go", kind: "method", owner: "method *readHandler.renderReadResult", fingerprint: "func(ctx context.Context, snapshot readFileSnapshot, args ReadArgs, grant int) (ReadResult, error)", count: 1, rationale: "bounded cancellation-aware read rendering"},
	{file: "codingtools/read.go", kind: "top-level function", owner: "func contextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "codingtools/read.go", kind: "top-level function", owner: "func readCompleteFile", fingerprint: "func(ctx context.Context, path string, maxBytes int64) ([]byte, os.FileInfo, error)", count: 1, rationale: "bounded cancellation-aware file reader"},
	{file: "codingtools/read.go", kind: "top-level function", owner: "func readOpenedFile", fingerprint: "func(ctx context.Context, file *os.File, size int64, maxBytes int64) ([]byte, error)", count: 1, rationale: "bounded descriptor-backed file reader"},
	{file: "codingtools/read.go", kind: "top-level function", owner: "func renderReadWindow", fingerprint: "func(ctx context.Context, lines []readLine, firstLine int) (string, error)", count: 1, rationale: "cancellation-aware read rendering helper"},
	{file: "codingtools/searchtouch.go", kind: "top-level function", owner: "func publishSearchTouches", fingerprint: "func(ctx context.Context, bus *github.com/RandomCodeSpace/plasmid/workspace.TouchBus, warnings github.com/RandomCodeSpace/plasmid/warning.Warner, sessionID string, paths []string, maximum int)", count: 1, rationale: "framework-free bounded workspace touch publication helper"},
	{file: "codingtools/write.go", kind: "method", owner: "method *writeHandler.call", fingerprint: "func(ctx context.Context, sessionID string, args WriteArgs) (result map[string]any, err error)", count: 1, rationale: "native typed coding-tool handler with ADK session identity"},
	{file: "codingtools/write.go", kind: "method", owner: "method *writeHandler.performWrite", fingerprint: "func(ctx context.Context, sessionID string, path string, data []byte) (writeOperation, error)", count: 1, rationale: "serialized cancellation-aware write operation"},
	{file: "codingtools/write.go", kind: "method", owner: "method *writeHandler.replaceWriteTarget", fingerprint: "func(ctx context.Context, sessionID string, path string, data []byte) (writeOperation, error)", count: 1, rationale: "descriptor-confined write replacement setup"},
	{file: "codingtools/write.go", kind: "method", owner: "method *writeHandler.replaceOpenedWriteTarget", fingerprint: "func(ctx context.Context, sessionID string, relative string, data []byte, parent *os.Root) (writeOperation, error)", count: 1, rationale: "descriptor-confined write replacement leaf"},
	{file: "codingtools/write.go", kind: "top-level function", owner: "func atomicReplaceFile", fingerprint: "func(ctx context.Context, parent *os.Root, name string, data []byte, mode os.FileMode, exists bool) (err error)", count: 1, rationale: "cancellation-aware atomic file replacement leaf"},
	{file: "codingtools/write.go", kind: "top-level function", owner: "func atomicReplaceFileWith", fingerprint: "func(ctx context.Context, parent *os.Root, name string, data []byte, mode os.FileMode, exists bool, options atomicReplaceOptions) (err error)", count: 1, rationale: "testable atomic file replacement implementation"},
	{file: "codingtools/write.go", kind: "top-level function", owner: "func inspectWriteTarget", fingerprint: "func(ctx context.Context, parent *os.Root, name string) ([]byte, os.FileMode, bool, error)", count: 1, rationale: "bounded target inspection helper"},
	{file: "codingtools/write.go", kind: "top-level function", owner: "func writeContextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "sessionstore/sidecar.go", kind: "method", owner: "method *Store.AppendSidecar", fingerprint: "func(ctx context.Context, app string, user string, id string, kind string, value any) error", count: 1, rationale: "durable session sidecar persistence operation"},
	{file: "sessionstore/sidecar.go", kind: "method", owner: "method *Store.LoadSidecar", fingerprint: "func(ctx context.Context, app string, user string, id string, kind string, destination any) (bool, error)", count: 1, rationale: "durable session sidecar lookup operation"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.AppendEvent", fingerprint: "func(ctx context.Context, current google.golang.org/adk/v2/session.Session, event *google.golang.org/adk/v2/session.Event) error", count: 1, rationale: "approved native ADK session.Service event persistence extension point"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.Create", fingerprint: "func(ctx context.Context, req *google.golang.org/adk/v2/session.CreateRequest) (*google.golang.org/adk/v2/session.CreateResponse, error)", count: 1, rationale: "approved native ADK session.Service creation extension point"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.create", fingerprint: "func(ctx context.Context, req *google.golang.org/adk/v2/session.CreateRequest, stateHash string, notices *warningBuffer) (*google.golang.org/adk/v2/session.CreateResponse, error)", count: 1, rationale: "private durable session creation orchestration extracted from the service method"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.startCreate", fingerprint: "func(ctx context.Context, transaction createTransaction) (*google.golang.org/adk/v2/session.CreateResponse, error)", count: 1, rationale: "private durable creation transaction start extracted from the service method"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.resolveCreateID", fingerprint: "func(ctx context.Context, req *google.golang.org/adk/v2/session.CreateRequest, stateHash string) (string, bool, error)", count: 1, rationale: "private cancellation-aware durable creation identity resolution"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.validateAppendInput", fingerprint: "func(ctx context.Context, current google.golang.org/adk/v2/session.Session, event *google.golang.org/adk/v2/session.Event) (*durableSession, bool, error)", count: 1, rationale: "private cancellation-aware event validation extracted from the service method"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.Delete", fingerprint: "func(ctx context.Context, req *google.golang.org/adk/v2/session.DeleteRequest) error", count: 1, rationale: "approved native ADK session.Service deletion extension point"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.Get", fingerprint: "func(ctx context.Context, req *google.golang.org/adk/v2/session.GetRequest) (*google.golang.org/adk/v2/session.GetResponse, error)", count: 1, rationale: "approved native ADK session.Service lookup extension point"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.List", fingerprint: "func(ctx context.Context, req *google.golang.org/adk/v2/session.ListRequest) (*google.golang.org/adk/v2/session.ListResponse, error)", count: 1, rationale: "approved native ADK session.Service listing extension point"},
	{file: "shellexec/executor.go", kind: "method", owner: "method *Executor.Run", fingerprint: "func(ctx context.Context, req Request) (*Result, error)", count: 1, rationale: "bounded shell leaf operation"},
	{file: "shellexec/executor.go", kind: "method", owner: "method *Executor.RunMerged", fingerprint: "func(ctx context.Context, req Request) (Result, error)", count: 1, rationale: "bounded merged-output shell leaf operation"},
	{file: "shellexec/executor.go", kind: "method", owner: "method *Executor.run", fingerprint: "func(ctx context.Context, req Request, merged bool) (*Result, error)", count: 1, rationale: "private bounded shell execution implementation"},
	{file: "shellexec/executor.go", kind: "method", owner: "method *Executor.waitForCommand", fingerprint: "func(ctx context.Context, cmd *os/exec.Cmd, waited <-chan error, timeout *time.Timer) (waitErr error, timedOut bool, killed bool, stopErr error)", count: 1, rationale: "private cancellation-aware shell process wait and termination coordination"},
	{file: "workspace/queue.go", kind: "method", owner: "method *MutationQueue.Do", fingerprint: "func(ctx context.Context, fn func() error) error", count: 1, rationale: "framework-free serialized workspace mutation operation"},
	{file: "workspace/touch.go", kind: "nested callable", owner: "type TouchObserver via method ObserveTouch", fingerprint: "func(context.Context, Touch)", count: 1, rationale: "framework-free workspace touch notification seam"},
	{file: "workspace/touch.go", kind: "method", owner: "method *TouchBus.Publish", fingerprint: "func(ctx context.Context, touch Touch)", count: 1, rationale: "framework-free workspace touch delivery operation"},
	nestedCallableApproval("workspace/touch.go", "type TouchBus via field subscribers -> slice element -> field observer -> method ObserveTouch", "func(context.Context, Touch)", "framework-free touch subscription storage"),
	nestedCallableApproval("workspace/touch.go", "type touchSubscription via field observer -> method ObserveTouch", "func(context.Context, Touch)", "framework-free touch subscription record"),
	nestedCallableApproval("workspace/touch.go", "func NewTouchBus via result 0 -> pointer element -> field subscribers -> slice element -> field observer -> method ObserveTouch", "func(context.Context, Touch)", "framework-free touch bus constructor result"),
	nestedCallableApproval("workspace/touch.go", "method *TouchBus.Subscribe via parameter observer -> method ObserveTouch", "func(context.Context, Touch)", "framework-free touch subscription input"),
	nestedCallableApproval("workspace/touch.go", "variable subscribers in method *TouchBus.Publish via slice element -> field observer -> method ObserveTouch", "func(context.Context, Touch)", "framework-free touch publication snapshot"),
	nestedCallableApproval("workspace/touch.go", "literal in method *TouchBus.Publish via parameter observer -> method ObserveTouch", "func(context.Context, Touch)", "framework-free touch publication callback input"),
}

func nestedCallableApproval(file, owner, fingerprint, rationale string) callableApproval {
	return callableApproval{file: file, kind: "nested callable", owner: owner, fingerprint: fingerprint, count: 1, rationale: rationale}
}

type buildContext struct {
	goos   string
	goarch string
}

func (context buildContext) String() string {
	return context.goos + "/" + context.goarch
}

// Production has unix and non-unix files. Linux and Windows exercise both
// branches; Darwin keeps the other supported Unix host independently covered.
var callableBuildContexts = []buildContext{
	{goos: "linux", goarch: "amd64"},
	{goos: "linux", goarch: "arm64"},
	{goos: "windows", goarch: "amd64"},
	{goos: "windows", goarch: "arm64"},
	{goos: "darwin", goarch: "amd64"},
	{goos: "darwin", goarch: "arm64"},
}

func TestContextTakingCallablesMatchExplicitTypedInventory(t *testing.T) {
	approved := make(map[string]callableApproval, len(approvedContextCallables))
	for _, declaration := range approvedContextCallables {
		key := callableInventoryKey(declaration.file, declaration.kind, declaration.owner, declaration.fingerprint)
		if declaration.rationale == "" || declaration.count < 1 {
			t.Errorf("callable approval %s has invalid evidence", key)
		}
		if _, exists := approved[key]; exists {
			t.Errorf("duplicate callable approval %s", key)
		}
		approved[key] = declaration
	}

	actual, examples, platforms := typedCallableUnion(t, repositoryRoot(t), callableBuildContexts, "./...")
	for key, count := range actual {
		if _, exists := approved[key]; exists {
			continue
		}
		discovered := examples[key]
		t.Errorf("unapproved context-taking %s %s %s at %s:%d for %v (count %d); add an architecture rationale", discovered.kind, discovered.owner, discovered.fingerprint, discovered.file, discovered.line, platforms[key], count)
	}

	for key, approval := range approved {
		if actual[key] != approval.count {
			t.Errorf("callable inventory %s has count %d, want %d", key, actual[key], approval.count)
		}
	}
}

type discoveredCallable struct {
	packagePath string
	file        string
	kind        string
	owner       string
	fingerprint string
	line        int
}

func methodOwner(method *ast.FuncDecl) string {
	receiver := "unknown"
	if method.Recv != nil && len(method.Recv.List) != 0 {
		receiver = receiverName(method.Recv.List[0].Type)
	}
	return "method " + receiver + "." + method.Name.Name
}

func receiverName(expression ast.Expr) string {
	switch receiver := expression.(type) {
	case *ast.Ident:
		return receiver.Name
	case *ast.StarExpr:
		return "*" + receiverName(receiver.X)
	case *ast.IndexExpr:
		return receiverName(receiver.X)
	case *ast.IndexListExpr:
		return receiverName(receiver.X)
	default:
		return "unknown"
	}
}

func callableInventoryKey(file, kind, owner, fingerprint string) string {
	return file + ":" + kind + ":" + owner + ":" + fingerprint
}

func typedCallableUnion(t *testing.T, directory string, buildContexts []buildContext, patterns ...string) (map[string]int, map[string]discoveredCallable, map[string][]string) {
	t.Helper()
	maximumCounts := make(map[string]int)
	examples := make(map[string]discoveredCallable)
	platforms := make(map[string][]string)
	for _, buildContext := range buildContexts {
		contextCounts := make(map[string]int)
		for _, discovered := range loadTypedContextCallables(t, directory, buildContext, patterns...) {
			if discovered.packagePath == legacyLoopImport || discovered.packagePath == legacyADKLoopImport {
				continue
			}
			key := callableInventoryKey(discovered.file, discovered.kind, discovered.owner, discovered.fingerprint)
			contextCounts[key]++
			examples[key] = discovered
		}
		for key, count := range contextCounts {
			if count > maximumCounts[key] {
				maximumCounts[key] = count
			}
			platforms[key] = append(platforms[key], buildContext.String())
		}
	}
	return maximumCounts, examples, platforms
}

func loadTypedContextCallables(t *testing.T, directory string, buildContext buildContext, patterns ...string) []discoveredCallable {
	t.Helper()
	loaded := loadProductionPackages(t, directory, buildContext, append([]string{"context"}, patterns...)...)

	var contextType types.Type
	for _, loadedPackage := range loaded {
		if loadedPackage.PkgPath == "context" && loadedPackage.Types != nil {
			contextObject := loadedPackage.Types.Scope().Lookup("Context")
			if contextObject != nil {
				contextType = contextObject.Type()
			}
		}
	}
	if contextType == nil {
		t.Fatal("loaded package graph has no context.Context type")
	}

	var declarations []discoveredCallable
	for _, loadedPackage := range loaded {
		if loadedPackage.Module == nil || !loadedPackage.Module.Main || loadedPackage.TypesInfo == nil {
			continue
		}
		declarations = append(declarations, collectTypedContextCallables(t, directory, loadedPackage, contextType)...)
	}
	slices.SortFunc(declarations, func(left, right discoveredCallable) int {
		return strings.Compare(callableInventoryKey(left.file, left.kind, left.owner, left.fingerprint), callableInventoryKey(right.file, right.kind, right.owner, right.fingerprint))
	})
	return declarations
}

func loadProductionPackages(t *testing.T, directory string, buildContext buildContext, patterns ...string) []*packages.Package {
	t.Helper()
	configuration := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo |
			packages.NeedImports | packages.NeedDeps | packages.NeedModule,
		Dir: directory,
		Env: buildContextEnvironment(os.Environ(), buildContext),
	}
	loaded, err := packages.Load(configuration, patterns...)
	if err != nil {
		t.Fatalf("load production packages for %s: %v", buildContext, err)
	}
	for _, loadedPackage := range loaded {
		for _, packageError := range loadedPackage.Errors {
			t.Errorf("load %s for %s: %s", loadedPackage.PkgPath, buildContext, packageError)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	return loaded
}

func buildContextEnvironment(base []string, buildContext buildContext) []string {
	environment := make([]string, 0, len(base)+3)
	for _, variable := range base {
		if strings.HasPrefix(variable, "GOOS=") || strings.HasPrefix(variable, "GOARCH=") || strings.HasPrefix(variable, "CGO_ENABLED=") || strings.HasPrefix(variable, "GOPACKAGESDRIVER=") {
			continue
		}
		environment = append(environment, variable)
	}
	return append(environment, "GOOS="+buildContext.goos, "GOARCH="+buildContext.goarch, "CGO_ENABLED=0", "GOPACKAGESDRIVER=off")
}

func collectTypedContextCallables(t *testing.T, root string, loadedPackage *packages.Package, contextType types.Type) []discoveredCallable {
	t.Helper()
	collector := &contextCallableCollector{
		t:             t,
		root:          root,
		loadedPackage: loadedPackage,
		contextType:   contextType,
		qualifier:     packageQualifier(loadedPackage.Types),
	}
	for _, file := range loadedPackage.Syntax {
		collector.parents = nil
		ast.Inspect(file, collector.inspect)
	}
	return collector.declarations
}

type contextCallableCollector struct {
	t             *testing.T
	root          string
	loadedPackage *packages.Package
	contextType   types.Type
	qualifier     types.Qualifier
	parents       []ast.Node
	declarations  []discoveredCallable
}

func (collector *contextCallableCollector) inspect(node ast.Node) bool {
	if node == nil {
		collector.parents = collector.parents[:len(collector.parents)-1]
		return true
	}
	collector.appendNode(node)
	collector.parents = append(collector.parents, node)
	return true
}

func (collector *contextCallableCollector) appendNode(node ast.Node) {
	switch declaration := node.(type) {
	case *ast.FuncDecl:
		collector.appendFunction(declaration)
	case *ast.TypeSpec:
		object, _ := collector.loadedPackage.TypesInfo.Defs[declaration.Name].(*types.TypeName)
		if object != nil {
			collector.append("named function type", "type "+declaration.Name.Name, object.Type(), declaration.Pos())
		}
	case *ast.ValueSpec:
		collector.appendIdentifiers(declaration.Names)
	case *ast.AssignStmt:
		if declaration.Tok == token.DEFINE {
			collector.appendExpressions(declaration.Lhs)
		}
	case *ast.FuncLit:
		collector.append("function literal", "literal in "+enclosingCallableOwner(collector.parents), collector.loadedPackage.TypesInfo.TypeOf(declaration), declaration.Pos())
	}
}

func (collector *contextCallableCollector) appendFunction(declaration *ast.FuncDecl) {
	function, _ := collector.loadedPackage.TypesInfo.Defs[declaration.Name].(*types.Func)
	if function == nil {
		return
	}
	kind := "top-level function"
	owner := "func " + declaration.Name.Name
	if declaration.Recv != nil {
		kind = "method"
		owner = methodOwner(declaration)
	}
	collector.append(kind, owner, function.Type(), declaration.Pos())
}

func (collector *contextCallableCollector) appendExpressions(expressions []ast.Expr) {
	for _, expression := range expressions {
		name, ok := expression.(*ast.Ident)
		if ok {
			collector.appendIdentifiers([]*ast.Ident{name})
		}
	}
}

func (collector *contextCallableCollector) appendIdentifiers(names []*ast.Ident) {
	for _, name := range names {
		variable, _ := collector.loadedPackage.TypesInfo.Defs[name].(*types.Var)
		if variable != nil {
			collector.append("function variable", variableOwner(name.Name, collector.parents), variable.Type(), name.Pos())
		}
	}
}

func (collector *contextCallableCollector) append(kind, owner string, typ types.Type, position token.Pos) {
	location := collector.loadedPackage.Fset.Position(position)
	relativeFile, err := filepath.Rel(collector.root, location.Filename)
	if err != nil {
		collector.t.Errorf("resolve callable path %s: %v", location.Filename, err)
		return
	}
	for _, reachable := range reachableContextSignatures(typ, collector.contextType, collector.loadedPackage.Types) {
		discoveredKind, discoveredOwner := nestedCallableIdentity(kind, owner, reachable.path)
		collector.declarations = append(collector.declarations, discoveredCallable{
			packagePath: collector.loadedPackage.PkgPath,
			file:        filepath.ToSlash(relativeFile),
			kind:        discoveredKind,
			owner:       discoveredOwner,
			fingerprint: types.TypeString(reachable.signature, collector.qualifier),
			line:        location.Line,
		})
	}
}

func nestedCallableIdentity(kind, owner, path string) (string, string) {
	if path == "" {
		return kind, owner
	}
	return "nested callable", owner + " via " + path
}

type reachableSignature struct {
	path      string
	signature *types.Signature
}

func reachableContextSignatures(root, contextType types.Type, ownerPackage *types.Package) []reachableSignature {
	walker := &contextSignatureWalker{
		contextType:  contextType,
		ownerPackage: ownerPackage,
		visiting:     make(map[types.Type]bool),
	}
	walker.visit(root, "")
	return walker.signatures
}

type contextSignatureWalker struct {
	contextType  types.Type
	ownerPackage *types.Package
	visiting     map[types.Type]bool
	signatures   []reachableSignature
}

func (walker *contextSignatureWalker) visit(current types.Type, path string) {
	if current == nil {
		return
	}
	current = types.Unalias(current)
	if walker.visiting[current] {
		return
	}
	walker.visiting[current] = true
	defer delete(walker.visiting, current)
	walker.visitResolved(current, path)
}

func (walker *contextSignatureWalker) visitResolved(current types.Type, path string) {
	switch current := current.(type) {
	case *types.Named:
		if !walker.isForeignNativeType(current) {
			walker.visit(current.Underlying(), path)
		}
	case *types.Signature:
		walker.visitSignature(current, path)
	case *types.Struct:
		walker.visitStruct(current, path)
	case *types.Pointer:
		walker.visit(current.Elem(), appendTypePath(path, "pointer element"))
	case *types.Array:
		walker.visit(current.Elem(), appendTypePath(path, "array element"))
	case *types.Slice:
		walker.visit(current.Elem(), appendTypePath(path, "slice element"))
	case *types.Map:
		walker.visit(current.Key(), appendTypePath(path, "map key"))
		walker.visit(current.Elem(), appendTypePath(path, "map value"))
	case *types.Chan:
		walker.visit(current.Elem(), appendTypePath(path, "channel element"))
	case *types.Interface:
		walker.visitInterface(current, path)
	case *types.TypeParam:
		walker.visit(current.Constraint(), appendTypePath(path, "type constraint"))
	case *types.Union:
		walker.visitUnion(current, path)
	}
}

func (walker *contextSignatureWalker) isForeignNativeType(current *types.Named) bool {
	object := current.Obj()
	return object != nil && object.Pkg() != nil && object.Pkg() != walker.ownerPackage && isNativeFrameworkImport(object.Pkg().Path())
}

func (walker *contextSignatureWalker) visitSignature(current *types.Signature, path string) {
	if signatureAcceptsContext(current, walker.contextType) {
		walker.signatures = append(walker.signatures, reachableSignature{path: path, signature: current})
	}
	walker.visitTuple(current.Params(), path, "parameter")
	walker.visitTuple(current.Results(), path, "result")
}

func (walker *contextSignatureWalker) visitStruct(current *types.Struct, path string) {
	for index := 0; index < current.NumFields(); index++ {
		field := current.Field(index)
		if field.Pkg() != nil && field.Pkg() != walker.ownerPackage && !field.Exported() {
			continue
		}
		walker.visit(field.Type(), appendTypePath(path, "field "+field.Name()))
	}
}

func (walker *contextSignatureWalker) visitInterface(current *types.Interface, path string) {
	current.Complete()
	for index := 0; index < current.NumExplicitMethods(); index++ {
		method := current.ExplicitMethod(index)
		if method.Pkg() == nil || method.Pkg() == walker.ownerPackage || method.Exported() {
			walker.visit(method.Type(), appendTypePath(path, "method "+method.Name()))
		}
	}
	for index := 0; index < current.NumEmbeddeds(); index++ {
		walker.visit(current.EmbeddedType(index), appendTypePath(path, "embedded interface"))
	}
}

func (walker *contextSignatureWalker) visitUnion(current *types.Union, path string) {
	for index := 0; index < current.Len(); index++ {
		walker.visit(current.Term(index).Type(), appendTypePath(path, "union term"))
	}
}

func (walker *contextSignatureWalker) visitTuple(tuple *types.Tuple, path, label string) {
	if tuple == nil {
		return
	}
	for index := 0; index < tuple.Len(); index++ {
		item := tuple.At(index)
		name := item.Name()
		if name == "" {
			name = strconv.Itoa(index)
		}
		walker.visit(item.Type(), appendTypePath(path, label+" "+name))
	}
}

func appendTypePath(path, step string) string {
	if path == "" {
		return step
	}
	return path + " -> " + step
}

func signatureAcceptsContext(signature *types.Signature, contextType types.Type) bool {
	contextInterface, ok := contextType.Underlying().(*types.Interface)
	if !ok {
		return false
	}
	for index := 0; index < signature.Params().Len(); index++ {
		parameterType := types.Unalias(signature.Params().At(index).Type())
		if types.Identical(parameterType, contextType) || types.AssignableTo(parameterType, contextType) || types.Implements(parameterType, contextInterface) {
			return true
		}
	}
	return false
}

func variableOwner(name string, parents []ast.Node) string {
	return "variable " + name + " in " + enclosingCallableOwner(parents)
}

func enclosingCallableOwner(parents []ast.Node) string {
	for index := len(parents) - 1; index >= 0; index-- {
		switch owner := parents[index].(type) {
		case *ast.ValueSpec:
			if len(owner.Names) != 0 {
				return "variable " + owner.Names[0].Name
			}
		case *ast.TypeSpec:
			return "type " + owner.Name.Name
		case *ast.FuncDecl:
			if owner.Recv != nil {
				return methodOwner(owner)
			}
			return "func " + owner.Name.Name
		}
	}
	return "file scope"
}

func TestTypedCallableInventoryResolvesAliasesWrappersAndClosures(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/inventory\n\ngo 1.26.5\n")
	writeTestFile(t, filepath.Join(root, "dep", "dep.go"), `package dep

import "context"

type Ctx = context.Context
type Wrapped context.Context
type ErrorCallback func(Wrapped) error
type Native interface { Invoke(context.Context) error }

var Imported = func(context.Context) error { return nil }
`)
	writeTestFile(t, filepath.Join(root, "subject.go"), `package inventory

import (
	"context"
	"example.com/inventory/dep"
)

type ImportedCallback = dep.ErrorCallback
type Stream func(context.Context) <-chan string
type Holder struct { Callback dep.ErrorCallback }
type NativeAlias = dep.Native
type NativeWrapper dep.Native
type Callback func(context.Context) error
type Nested struct {
	Handlers map[string][]*Callback
	Streams chan [1]Callback
}
type Node struct {
	Next *Node
	Callback Callback
}
type AnonymousHolder struct {
	Handler interface { Handle(context.Context) error }
}

var ImportedAlias = dep.Imported
var Inferred = func(context.Context) {}

func Top(context.Context) {}
func AnonymousParameter(value interface { Call(context.Context) error }) {}
func AnonymousResult() interface { Stream(context.Context) <-chan string } { return nil }

type Worker struct{}
func (Worker) Method(dep.Ctx) error { return nil }

func local() {
	type LocalAlias = dep.Native
	type LocalWrapper dep.Native
	closure := func(dep.Wrapped) error { return nil }
	var _, _ LocalAlias = LocalWrapper(nil), nil
	_ = closure
}

func anonymousLocal() {
	var local interface { Run(context.Context) error }
	_ = local
}
`)
	writeTestFile(t, filepath.Join(root, "platform_linux.go"), `//go:build linux

package inventory

import "context"

func LinuxOnly(context.Context) error { return nil }
`)
	writeTestFile(t, filepath.Join(root, "platform_windows.go"), `//go:build windows

package inventory

import "context"

func WindowsOnly(context.Context) error { return nil }
`)
	writeTestFile(t, filepath.Join(root, "platform_arm64.go"), `//go:build arm64

package inventory

import "context"

func Arm64Only(context.Context) error { return nil }
`)

	_, examples, platforms := typedCallableUnion(t, root, callableBuildContexts, "./...")
	actual := make(map[string]int, len(examples))
	actualPlatforms := make(map[string][]string, len(examples))
	for key, declaration := range examples {
		owner := declaration.kind + ":" + declaration.owner
		actual[owner]++
		actualPlatforms[owner] = platforms[key]
	}
	for _, expected := range []string{
		"named function type:type ErrorCallback",
		"function variable:variable Imported in file scope",
		"function literal:literal in variable Imported",
		"named function type:type ImportedCallback",
		"named function type:type Stream",
		"nested callable:type Holder via field Callback",
		"nested callable:type Nested via field Handlers -> map value -> slice element -> pointer element",
		"nested callable:type Nested via field Streams -> channel element -> array element",
		"nested callable:type Node via field Callback",
		"function variable:variable ImportedAlias in file scope",
		"function variable:variable Inferred in file scope",
		"function literal:literal in variable Inferred",
		"top-level function:func Top",
		"method:method Worker.Method",
		"function variable:variable closure in func local",
		"function literal:literal in func local",
		"top-level function:func LinuxOnly",
		"top-level function:func WindowsOnly",
		"top-level function:func Arm64Only",
	} {
		if actual[expected] == 0 {
			t.Errorf("typed callable inventory missed %s; got %v", expected, actual)
		}
	}
	for owner, expectedPlatforms := range map[string][]string{
		"top-level function:func LinuxOnly":   {"linux/amd64", "linux/arm64"},
		"top-level function:func WindowsOnly": {"windows/amd64", "windows/arm64"},
		"top-level function:func Arm64Only":   {"linux/arm64", "windows/arm64", "darwin/arm64"},
	} {
		if !slices.Equal(actualPlatforms[owner], expectedPlatforms) {
			t.Errorf("%s loaded for %v, want %v", owner, actualPlatforms[owner], expectedPlatforms)
		}
	}

	_, interfaceExamples, _ := typedInterfaceUnion(t, root, callableBuildContexts, "./...")
	interfaces := make(map[string]bool, len(interfaceExamples))
	for _, declaration := range interfaceExamples {
		interfaces[declaration.owner] = true
	}
	for _, expected := range []string{
		"type Native",
		"type NativeAlias",
		"type NativeWrapper",
		"type LocalAlias",
		"type LocalWrapper",
	} {
		if !interfaces[expected] {
			t.Errorf("resolved interface inventory missed %s; got %v", expected, interfaces)
		}
	}

	_, anonymousExamples, _ := typedAnonymousInterfaceUnion(t, root, callableBuildContexts, "./...")
	anonymousInterfaces := make(map[string]bool, len(anonymousExamples))
	for _, declaration := range anonymousExamples {
		anonymousInterfaces[declaration.owner] = true
	}
	for _, expected := range []string{
		"field Handler in type AnonymousHolder",
		"parameter value in func AnonymousParameter",
		"result anonymous in func AnonymousResult",
		"variable local in func anonymousLocal",
	} {
		if !anonymousInterfaces[expected] {
			t.Errorf("resolved anonymous interface inventory missed %s; got %v", expected, anonymousInterfaces)
		}
	}
}

func TestBuildContextEnvironmentDisablesAmbientPackageDriver(t *testing.T) {
	environment := buildContextEnvironment(
		[]string{"KEEP=value", "GOOS=plan9", "GOARCH=386", "CGO_ENABLED=1", "GOPACKAGESDRIVER=ambient"},
		buildContext{goos: "windows", goarch: "arm64"},
	)
	for _, expected := range []string{"KEEP=value", "GOOS=windows", "GOARCH=arm64", "CGO_ENABLED=0", "GOPACKAGESDRIVER=off"} {
		if !slices.Contains(environment, expected) {
			t.Errorf("build environment %v does not contain %q", environment, expected)
		}
	}
	for _, forbidden := range []string{"GOOS=plan9", "GOARCH=386", "CGO_ENABLED=1", "GOPACKAGESDRIVER=ambient"} {
		if slices.Contains(environment, forbidden) {
			t.Errorf("build environment retained ambient override %q", forbidden)
		}
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

var legacyRuntimeInterfaceNames = map[string]bool{
	"Provider":     true,
	"SessionStore": true,
	"Tool":         true,
	"Toolset":      true,
}

func TestLegacyRuntimeInterfacesAreNotAliasedOrWrapped(t *testing.T) {
	root := repositoryRoot(t)
	walkRepositoryGoFiles(t, func(path string, relativeDirectory string, fileSet *token.FileSet, file *ast.File) error {
		if strings.HasSuffix(path, "_test.go") || relativeDirectory == "loop" || relativeDirectory == "adkloop" {
			return nil
		}
		aliases, dotImport := legacyRuntimeImportNames(file)
		ast.Inspect(file, func(node ast.Node) bool {
			typeSpecification, ok := node.(*ast.TypeSpec)
			if !ok || !referencesLegacyRuntimeInterface(typeSpecification.Type, aliases, dotImport) {
				return true
			}
			relativeFile, err := filepath.Rel(root, path)
			if err != nil {
				t.Errorf("resolve legacy runtime wrapper path: %v", err)
				return false
			}
			kind := "defined wrapper"
			if typeSpecification.Assign.IsValid() {
				kind = "alias"
			}
			t.Errorf("%s:%d declares forbidden legacy runtime %s %s", filepath.ToSlash(relativeFile), fileSet.Position(typeSpecification.Pos()).Line, kind, typeSpecification.Name.Name)
			return true
		})
		return nil
	})
}

func legacyRuntimeImportNames(file *ast.File) (map[string]bool, bool) {
	aliases := make(map[string]bool)
	dotImport := false
	for _, specification := range file.Imports {
		importPath, err := strconv.Unquote(specification.Path.Value)
		if err != nil || (importPath != legacyLoopImport && importPath != legacyADKLoopImport) {
			continue
		}
		if specification.Name == nil {
			aliases[filepath.Base(importPath)] = true
			continue
		}
		switch specification.Name.Name {
		case ".":
			dotImport = true
		case "_":
		default:
			aliases[specification.Name.Name] = true
		}
	}
	return aliases, dotImport
}

func referencesLegacyRuntimeInterface(expression ast.Expr, aliases map[string]bool, dotImport bool) bool {
	switch expression := expression.(type) {
	case *ast.ParenExpr:
		return referencesLegacyRuntimeInterface(expression.X, aliases, dotImport)
	case *ast.SelectorExpr:
		packageName, ok := expression.X.(*ast.Ident)
		return ok && aliases[packageName.Name] && legacyRuntimeInterfaceNames[expression.Sel.Name]
	case *ast.Ident:
		return dotImport && legacyRuntimeInterfaceNames[expression.Name]
	default:
		return false
	}
}

func TestLegacyRuntimeAliasAndWrapperDetection(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   int
	}{
		{
			name:   "alias",
			source: "package example\nimport \"github.com/RandomCodeSpace/plasmid/loop\"\ntype Runtime = loop.Provider",
			want:   1,
		},
		{
			name:   "defined wrapper with import alias",
			source: "package example\nimport legacy \"github.com/RandomCodeSpace/plasmid/loop\"\ntype Runtime legacy.Provider",
			want:   1,
		},
		{
			name:   "dot imported wrapper",
			source: "package example\nimport . \"github.com/RandomCodeSpace/plasmid/loop\"\ntype Runtime Toolset",
			want:   1,
		},
		{
			name:   "legacy concrete data remains migration data",
			source: "package example\nimport \"github.com/RandomCodeSpace/plasmid/loop\"\ntype StoredEvent loop.Event",
			want:   0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, test.name+".go", test.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			aliases, dotImport := legacyRuntimeImportNames(file)
			got := 0
			ast.Inspect(file, func(node ast.Node) bool {
				typeSpecification, ok := node.(*ast.TypeSpec)
				if ok && referencesLegacyRuntimeInterface(typeSpecification.Type, aliases, dotImport) {
					got++
				}
				return true
			})
			if got != test.want {
				t.Errorf("found %d legacy runtime aliases or wrappers, want %d", got, test.want)
			}
		})
	}
}

type goFileVisitor func(path string, relativeDirectory string, fileSet *token.FileSet, file *ast.File) error

func walkRepositoryGoFiles(t *testing.T, visit goFileVisitor) {
	t.Helper()
	root := repositoryRoot(t)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		relativeDirectory, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		return visit(path, filepath.ToSlash(relativeDirectory), fileSet, file)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate architecture boundary test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

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

const zeroImportDeletionOwner = "E08 / issue #24: Build the native Harness checkpoint and delete loop bridges"

type legacyImportMaximum struct {
	legacyPackage string
	importers     []string
}

// The native Harness checkpoint deleted both legacy packages. These zero
// maximums remain as a permanent no-reintroduction gate.
var legacyImportMaximums = []legacyImportMaximum{
	{
		legacyPackage: "github.com/plasmid-dev/plasmid/loop",
		importers:     []string{},
	},
	{
		legacyPackage: "github.com/plasmid-dev/plasmid/adkloop",
		importers:     []string{},
	},
}

func TestDeletedLoopBridgesStayAbsent(t *testing.T) {
	root := repositoryRoot(t)
	for _, directory := range []string{"loop", "adkloop"} {
		if _, err := os.Stat(filepath.Join(root, directory)); !os.IsNotExist(err) {
			t.Errorf("deleted legacy package %q exists or could not be checked: %v", directory, err)
		}
	}
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
	"plugins":      "compiled-plugin native tools and callbacks",
	"sessionstore": "native ADK session.Service implementation",
	"skills":       "native ADK skill toolset",
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
		file:        "sessionstore/log.go",
		owner:       "type warningLogger",
		fingerprint: "interface{warn(code string, path string, line int, message string)}",
		count:       1,
		rationale:   "narrow internal warning emission seam for session recovery",
	},
	{
		file:        "warning/warning.go",
		owner:       "type Sink",
		fingerprint: "interface{Warn(Warning)}",
		count:       1,
		rationale:   "shared framework-free structured warning destination",
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
			if discovered.packagePath == "github.com/plasmid-dev/plasmid/loop" || discovered.packagePath == "github.com/plasmid-dev/plasmid/adkloop" {
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
		qualifier := func(imported *types.Package) string {
			if imported == loadedPackage.Types {
				return ""
			}
			return imported.Path()
		}
		for _, file := range loadedPackage.Syntax {
			ast.Inspect(file, func(node ast.Node) bool {
				declaration, ok := node.(*ast.TypeSpec)
				if !ok {
					return true
				}
				object, _ := loadedPackage.TypesInfo.Defs[declaration.Name].(*types.TypeName)
				if object == nil {
					return true
				}
				resolved := types.Unalias(object.Type())
				interfaceType, ok := resolved.Underlying().(*types.Interface)
				if !ok {
					return true
				}
				location := loadedPackage.Fset.Position(declaration.Pos())
				relativeFile, err := filepath.Rel(directory, location.Filename)
				if err != nil {
					t.Errorf("resolve interface path %s: %v", location.Filename, err)
					return false
				}
				declarations = append(declarations, discoveredInterface{
					packagePath: loadedPackage.PkgPath,
					file:        filepath.ToSlash(relativeFile),
					owner:       "type " + declaration.Name.Name,
					fingerprint: types.TypeString(interfaceType.Complete(), qualifier),
					line:        location.Line,
				})
				return true
			})
		}
	}
	return declarations
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
			if discovered.packagePath == "github.com/plasmid-dev/plasmid/loop" || discovered.packagePath == "github.com/plasmid-dev/plasmid/adkloop" {
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
		qualifier := func(imported *types.Package) string {
			if imported == loadedPackage.Types {
				return ""
			}
			return imported.Path()
		}
		for _, file := range loadedPackage.Syntax {
			var parents []ast.Node
			ast.Inspect(file, func(node ast.Node) bool {
				if node == nil {
					parents = parents[:len(parents)-1]
					return true
				}
				interfaceNode, ok := node.(*ast.InterfaceType)
				if ok && !isNamedInterfaceSyntax(interfaceNode, parents) {
					resolved := loadedPackage.TypesInfo.TypeOf(interfaceNode)
					if resolved == nil {
						parents = append(parents, node)
						return true
					}
					if resolvedInterface, ok := resolved.Underlying().(*types.Interface); ok {
						location := loadedPackage.Fset.Position(interfaceNode.Pos())
						relativeFile, err := filepath.Rel(directory, location.Filename)
						if err != nil {
							t.Errorf("resolve anonymous interface path %s: %v", location.Filename, err)
							return false
						}
						declarations = append(declarations, discoveredInterface{
							packagePath: loadedPackage.PkgPath,
							file:        filepath.ToSlash(relativeFile),
							owner:       anonymousInterfaceOwner(parents),
							fingerprint: types.TypeString(resolvedInterface.Complete(), qualifier),
							line:        location.Line,
						})
					}
				}
				parents = append(parents, node)
				return true
			})
		}
	}
	return declarations
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
			name := "anonymous"
			if len(declaration.Names) != 0 {
				name = declaration.Names[0].Name
			}
			kind := "field"
			if index > 0 {
				if fields, ok := parents[index-1].(*ast.FieldList); ok {
					for outer := index - 2; outer >= 0; outer-- {
						if function, ok := parents[outer].(*ast.FuncType); ok {
							switch fields {
							case function.Params:
								kind = "parameter"
							case function.Results:
								kind = "result"
							}
							break
						}
					}
				}
			}
			return kind + " " + name + " in " + enclosingCallableOwner(parents[:index])
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
	nestedCallableApproval("harness.go", "variable processor in func New via method ProcessRequest", "func(google.golang.org/adk/v2/agent.Context, *google.golang.org/adk/v2/model.LLMRequest) error", "confirmation wrapper preserves source request processor"),
	{file: "harness.go", kind: "method", owner: "method instructionProvider.Provide", fingerprint: "func(ctx google.golang.org/adk/v2/agent.ReadonlyContext) (string, error)", count: 1, rationale: "native ADK dynamic context and LSP instruction composition"},
	{file: "harness.go", kind: "function literal", owner: "literal in func New", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, current google.golang.org/adk/v2/tool.Tool, args map[string]any) (map[string]any, error)", count: 1, rationale: "native ADK argument-aware before-tool policy callback"},
	{file: "config/load.go", kind: "top-level function", owner: "func Load", fingerprint: "func(ctx context.Context, options Options) (Result, error)", count: 1, rationale: "cancellation-aware versioned configuration entry point"},
	{file: "contextresolver/commands.go", kind: "top-level function", owner: "func expandCommands", fingerprint: "func(ctx context.Context, source string, path string, trust TrustLevel, options commandOptions, executor *github.com/plasmid-dev/plasmid/shellexec.Executor, sink github.com/plasmid-dev/plasmid/warning.Sink) string", count: 1, rationale: "trust-gated bounded prompt command expansion leaf"},
	{file: "contextresolver/commands.go", kind: "top-level function", owner: "func expandCommandsWithBudget", fingerprint: "func(ctx context.Context, source string, path string, trust TrustLevel, options commandOptions, executor *github.com/plasmid-dev/plasmid/shellexec.Executor, sink github.com/plasmid-dev/plasmid/warning.Sink, budget *commandDocumentBudget) string", count: 1, rationale: "trust-gated prompt command expansion sharing source-document time and output budgets across imports"},
	{file: "contextresolver/discovery.go", kind: "method", owner: "method *Resolver.discover", fingerprint: "func(ctx context.Context) ([]document, error)", count: 1, rationale: "bounded cancellation-aware instruction discovery"},
	{file: "contextresolver/discovery.go", kind: "method", owner: "method *Resolver.candidates", fingerprint: "func(ctx context.Context, state *discoveryState) ([]candidate, error)", count: 1, rationale: "cancellation-aware instruction candidate collection sharing the discovery budget"},
	{file: "contextresolver/discovery.go", kind: "method", owner: "method *Resolver.loadDocument", fingerprint: "func(ctx context.Context, source candidate, state *discoveryState) (document, bool)", count: 1, rationale: "bounded fail-soft instruction loading"},
	{file: "contextresolver/discovery.go", kind: "method", owner: "method *Resolver.expandImports", fingerprint: "func(ctx context.Context, sourcePath string, body string, host github.com/plasmid-dev/plasmid/internal/syntax.Host, trust TrustLevel, depth int, state *discoveryState) importExpansion", count: 1, rationale: "confined cancellation-aware Claude import expansion with policy provenance"},
	{file: "contextresolver/discovery.go", kind: "method", owner: "method *Resolver.loadImport", fingerprint: "func(ctx context.Context, parent string, requested string, trust TrustLevel, depth int, state *discoveryState) importExpansion", count: 1, rationale: "bounded confined instruction import loading with source policy and trust"},
	{file: "contextresolver/discovery.go", kind: "top-level function", owner: "func readBoundedAt", fingerprint: "func(ctx context.Context, rootPath string, relative string, maximum int) ([]byte, bool, error)", count: 1, rationale: "descriptor-confined nonblocking cancellation-aware file reader"},
	{file: "contextresolver/resolver.go", kind: "method", owner: "method *Resolver.StartSession", fingerprint: "func(ctx context.Context, sessionID string) error", count: 1, rationale: "session-scoped immutable instruction snapshot construction"},
	{file: "contextresolver/resolver.go", kind: "method", owner: "method *Resolver.startSession", fingerprint: "func(ctx context.Context, sessionID string) error", count: 1, rationale: "shared session snapshot construction beneath synchronized public operations"},
	{file: "contextresolver/resolver.go", kind: "method", owner: "method *Resolver.Instructions", fingerprint: "func(ctx context.Context, sessionID string, invocationID string) (string, error)", count: 1, rationale: "native instruction assembly entry point with turn scope recording"},
	{file: "contextresolver/resolver.go", kind: "method", owner: "method *Resolver.assemble", fingerprint: "func(ctx context.Context, sessionID string, view *sessionView) (string, github.com/plasmid-dev/plasmid/internal/syntax.ToolPolicy, error)", count: 1, rationale: "bounded session-view prompt assembly"},
	{file: "contextresolver/resolver.go", kind: "method", owner: "method *Resolver.ObserveTouch", fingerprint: "func(_ context.Context, touch github.com/plasmid-dev/plasmid/workspace.Touch)", count: 1, rationale: "framework-free lazy instruction activation observer"},
	{file: "foreign/claude.go", kind: "top-level function", owner: "func ScanClaude", fingerprint: "func(ctx context.Context, options Options) (HostCatalog, error)", count: 1, rationale: "bounded cancellation-aware Claude metadata discovery entry point"},
	{file: "foreign/codex.go", kind: "top-level function", owner: "func ScanCodex", fingerprint: "func(ctx context.Context, options Options) (HostCatalog, error)", count: 1, rationale: "bounded cancellation-aware Codex metadata discovery entry point"},
	{file: "foreign/copilot.go", kind: "top-level function", owner: "func ScanCopilot", fingerprint: "func(ctx context.Context, options Options) (HostCatalog, error)", count: 1, rationale: "bounded cancellation-aware Copilot metadata discovery entry point"},
	{file: "foreign/scan.go", kind: "top-level function", owner: "func Scan", fingerprint: "func(ctx context.Context, options Options) (Catalog, error)", count: 1, rationale: "bounded cancellation-aware combined foreign discovery entry point"},
	{file: "foreign/scan.go", kind: "top-level function", owner: "func newScanner", fingerprint: "func(ctx context.Context, host Host, options Options) (*scanner, error)", count: 1, rationale: "shared cancellation-aware foreign scanner construction"},
	{file: "codingtools/bash.go", kind: "method", owner: "method *bashHandler.call", fingerprint: "func(ctx context.Context, sessionID string, rawArgs map[string]any) (result map[string]any, err error)", count: 1, rationale: "native coding-tool handler with ADK session identity"},
	{file: "codingtools/bash.go", kind: "top-level function", owner: "func bashContextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "codingtools/edit.go", kind: "method", owner: "method *editHandler.call", fingerprint: "func(ctx context.Context, sessionID string, rawArgs map[string]any) (result map[string]any, err error)", count: 1, rationale: "native coding-tool handler with ADK session identity"},
	{file: "codingtools/edit.go", kind: "top-level function", owner: "func editContextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "codingtools/edit.go", kind: "top-level function", owner: "func editReadCompleteFile", fingerprint: "func(ctx context.Context, parent *os.Root, name string, maxBytes int64) ([]byte, error)", count: 1, rationale: "bounded file-read leaf helper"},
	{file: "codingtools/find.go", kind: "method", owner: "method *findHandler.call", fingerprint: "func(ctx context.Context, sessionID string, rawArgs map[string]any) (result map[string]any, err error)", count: 1, rationale: "native coding-tool handler with ADK session identity"},
	{file: "codingtools/find.go", kind: "top-level function", owner: "func findContextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "codingtools/grep.go", kind: "method", owner: "method *grepHandler.call", fingerprint: "func(ctx context.Context, sessionID string, rawArgs map[string]any) (result map[string]any, err error)", count: 1, rationale: "native coding-tool handler with ADK session identity"},
	{file: "codingtools/grep.go", kind: "method", owner: "method *grepHandler.finish", fingerprint: "func(ctx context.Context, sessionID string, maximum int, state grepState, grant int, emitted *int) (map[string]any, error)", count: 1, rationale: "private bounded grep completion operation"},
	{file: "codingtools/grep.go", kind: "top-level function", owner: "func grepContextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "codingtools/grep.go", kind: "top-level function", owner: "func grepLines", fingerprint: "func(ctx context.Context, reader io.Reader) ([]grepLine, int, error)", count: 1, rationale: "cancellation-aware line scanning helper"},
	{file: "codingtools/internal/walk/walk.go", kind: "top-level function", owner: "func Walk", fingerprint: "func(ctx context.Context, filter *Filter, callback func(Entry) error) error", count: 1, rationale: "bounded cancellation-aware filesystem walking leaf"},
	{file: "codingtools/internal/walk/walk.go", kind: "top-level function", owner: "func walk", fingerprint: "func(ctx context.Context, filter *Filter, callback func(Entry) error, warn github.com/plasmid-dev/plasmid/warning.Sink) error", count: 1, rationale: "private bounded filesystem walking implementation"},
	{file: "codingtools/list.go", kind: "method", owner: "method *listHandler.call", fingerprint: "func(ctx context.Context, sessionID string, rawArgs map[string]any) (result map[string]any, err error)", count: 1, rationale: "native coding-tool handler with ADK session identity"},
	{file: "codingtools/list.go", kind: "top-level function", owner: "func listContextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "codingtools/native.go", kind: "function literal", owner: "literal in func newNativeTool", fingerprint: "func(ctx google.golang.org/adk/v2/agent.Context, args map[string]any) (map[string]any, error)", count: 1, rationale: "native ADK function-tool callback"},
	{file: "codingtools/native.go", kind: "named function type", owner: "type nativeHandler", fingerprint: "func(context.Context, string, map[string]any) (map[string]any, error)", count: 1, rationale: "shared native coding-tool handler shape"},
	nestedCallableApproval("codingtools/native.go", "func newNativeTool via parameter handler", "func(context.Context, string, map[string]any) (map[string]any, error)", "native ADK function-tool handler input"),
	{file: "codingtools/read.go", kind: "method", owner: "method *readHandler.call", fingerprint: "func(ctx context.Context, sessionID string, rawArgs map[string]any) (result map[string]any, err error)", count: 1, rationale: "native coding-tool handler with ADK session identity"},
	{file: "codingtools/read.go", kind: "top-level function", owner: "func contextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "codingtools/read.go", kind: "top-level function", owner: "func readCompleteFile", fingerprint: "func(ctx context.Context, path string, maxBytes int64) ([]byte, os.FileInfo, error)", count: 1, rationale: "bounded cancellation-aware file reader"},
	{file: "codingtools/read.go", kind: "top-level function", owner: "func renderReadWindow", fingerprint: "func(ctx context.Context, lines []readLine, firstLine int) (string, error)", count: 1, rationale: "cancellation-aware read rendering helper"},
	{file: "codingtools/searchtouch.go", kind: "top-level function", owner: "func publishSearchTouches", fingerprint: "func(ctx context.Context, bus *github.com/plasmid-dev/plasmid/workspace.TouchBus, warnings github.com/plasmid-dev/plasmid/warning.Sink, sessionID string, paths []string, maximum int)", count: 1, rationale: "framework-free bounded workspace touch publication helper"},
	{file: "codingtools/write.go", kind: "method", owner: "method *writeHandler.call", fingerprint: "func(ctx context.Context, sessionID string, rawArgs map[string]any) (result map[string]any, err error)", count: 1, rationale: "native coding-tool handler with ADK session identity"},
	{file: "codingtools/write.go", kind: "top-level function", owner: "func atomicReplaceFile", fingerprint: "func(ctx context.Context, parent *os.Root, name string, data []byte, mode os.FileMode, exists bool) (err error)", count: 1, rationale: "cancellation-aware atomic file replacement leaf"},
	{file: "codingtools/write.go", kind: "top-level function", owner: "func atomicReplaceFileWith", fingerprint: "func(ctx context.Context, parent *os.Root, name string, data []byte, mode os.FileMode, exists bool, options atomicReplaceOptions) (err error)", count: 1, rationale: "testable atomic file replacement implementation"},
	{file: "codingtools/write.go", kind: "top-level function", owner: "func inspectWriteTarget", fingerprint: "func(ctx context.Context, parent *os.Root, name string) ([]byte, os.FileMode, bool, error)", count: 1, rationale: "bounded target inspection helper"},
	{file: "codingtools/write.go", kind: "top-level function", owner: "func writeContextError", fingerprint: "func(ctx context.Context) error", count: 1, rationale: "leaf cancellation error normalization"},
	{file: "sessionstore/sidecar.go", kind: "method", owner: "method *Store.AppendSidecar", fingerprint: "func(ctx context.Context, app string, user string, id string, kind string, value any) error", count: 1, rationale: "durable session sidecar persistence operation"},
	{file: "sessionstore/sidecar.go", kind: "method", owner: "method *Store.LoadSidecar", fingerprint: "func(ctx context.Context, app string, user string, id string, kind string, destination any) (bool, error)", count: 1, rationale: "durable session sidecar lookup operation"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.AppendEvent", fingerprint: "func(ctx context.Context, current google.golang.org/adk/v2/session.Session, event *google.golang.org/adk/v2/session.Event) error", count: 1, rationale: "approved native ADK session.Service event persistence extension point"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.Create", fingerprint: "func(ctx context.Context, req *google.golang.org/adk/v2/session.CreateRequest) (*google.golang.org/adk/v2/session.CreateResponse, error)", count: 1, rationale: "approved native ADK session.Service creation extension point"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.Delete", fingerprint: "func(ctx context.Context, req *google.golang.org/adk/v2/session.DeleteRequest) error", count: 1, rationale: "approved native ADK session.Service deletion extension point"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.Get", fingerprint: "func(ctx context.Context, req *google.golang.org/adk/v2/session.GetRequest) (*google.golang.org/adk/v2/session.GetResponse, error)", count: 1, rationale: "approved native ADK session.Service lookup extension point"},
	{file: "sessionstore/store.go", kind: "method", owner: "method *Store.List", fingerprint: "func(ctx context.Context, req *google.golang.org/adk/v2/session.ListRequest) (*google.golang.org/adk/v2/session.ListResponse, error)", count: 1, rationale: "approved native ADK session.Service listing extension point"},
	{file: "shellexec/executor.go", kind: "method", owner: "method *Executor.Run", fingerprint: "func(ctx context.Context, req Request) (*Result, error)", count: 1, rationale: "bounded shell leaf operation"},
	{file: "shellexec/executor.go", kind: "method", owner: "method *Executor.RunMerged", fingerprint: "func(ctx context.Context, req Request) (Result, error)", count: 1, rationale: "bounded merged-output shell leaf operation"},
	{file: "shellexec/executor.go", kind: "method", owner: "method *Executor.run", fingerprint: "func(ctx context.Context, req Request, merged bool) (*Result, error)", count: 1, rationale: "private bounded shell execution implementation"},
	{file: "workspace/queue.go", kind: "method", owner: "method *MutationQueue.Do", fingerprint: "func(ctx context.Context, fn func() error) error", count: 1, rationale: "framework-free serialized workspace mutation operation"},
	{file: "workspace/queue.go", kind: "method", owner: "method *MutationQueue.do", fingerprint: "func(ctx context.Context, fn func() error, beforeWait func() error, afterAcquire func() error) error", count: 1, rationale: "private serialized workspace mutation implementation"},
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
			if discovered.packagePath == "github.com/plasmid-dev/plasmid/loop" || discovered.packagePath == "github.com/plasmid-dev/plasmid/adkloop" {
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
	var declarations []discoveredCallable
	qualifier := func(imported *types.Package) string {
		if imported == loadedPackage.Types {
			return ""
		}
		return imported.Path()
	}
	for _, file := range loadedPackage.Syntax {
		var parents []ast.Node
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil {
				parents = parents[:len(parents)-1]
				return true
			}
			appendDeclaration := func(kind, owner string, typ types.Type, position token.Pos) {
				location := loadedPackage.Fset.Position(position)
				relativeFile, err := filepath.Rel(root, location.Filename)
				if err != nil {
					t.Errorf("resolve callable path %s: %v", location.Filename, err)
					return
				}
				for _, reachable := range reachableContextSignatures(typ, contextType, loadedPackage.Types) {
					discoveredKind := kind
					discoveredOwner := owner
					if reachable.path != "" {
						discoveredKind = "nested callable"
						discoveredOwner += " via " + reachable.path
					}
					declarations = append(declarations, discoveredCallable{
						packagePath: loadedPackage.PkgPath,
						file:        filepath.ToSlash(relativeFile),
						kind:        discoveredKind,
						owner:       discoveredOwner,
						fingerprint: types.TypeString(reachable.signature, qualifier),
						line:        location.Line,
					})
				}
			}

			switch declaration := node.(type) {
			case *ast.FuncDecl:
				function, _ := loadedPackage.TypesInfo.Defs[declaration.Name].(*types.Func)
				if function != nil {
					kind := "top-level function"
					owner := "func " + declaration.Name.Name
					if declaration.Recv != nil {
						kind = "method"
						owner = methodOwner(declaration)
					}
					appendDeclaration(kind, owner, function.Type(), declaration.Pos())
				}
			case *ast.TypeSpec:
				object, _ := loadedPackage.TypesInfo.Defs[declaration.Name].(*types.TypeName)
				if object != nil {
					appendDeclaration("named function type", "type "+declaration.Name.Name, object.Type(), declaration.Pos())
				}
			case *ast.ValueSpec:
				for _, name := range declaration.Names {
					variable, _ := loadedPackage.TypesInfo.Defs[name].(*types.Var)
					if variable != nil {
						appendDeclaration("function variable", variableOwner(name.Name, parents), variable.Type(), name.Pos())
					}
				}
			case *ast.AssignStmt:
				if declaration.Tok == token.DEFINE {
					for _, expression := range declaration.Lhs {
						name, ok := expression.(*ast.Ident)
						if !ok {
							continue
						}
						variable, _ := loadedPackage.TypesInfo.Defs[name].(*types.Var)
						if variable != nil {
							appendDeclaration("function variable", variableOwner(name.Name, parents), variable.Type(), name.Pos())
						}
					}
				}
			case *ast.FuncLit:
				appendDeclaration("function literal", "literal in "+enclosingCallableOwner(parents), loadedPackage.TypesInfo.TypeOf(declaration), declaration.Pos())
			}
			parents = append(parents, node)
			return true
		})
	}
	return declarations
}

type reachableSignature struct {
	path      string
	signature *types.Signature
}

func reachableContextSignatures(root, contextType types.Type, ownerPackage *types.Package) []reachableSignature {
	var signatures []reachableSignature
	visiting := make(map[types.Type]bool)
	var visit func(types.Type, string)
	var visitTuple func(*types.Tuple, string, string)
	visit = func(current types.Type, path string) {
		if current == nil {
			return
		}
		current = types.Unalias(current)
		if visiting[current] {
			return
		}
		visiting[current] = true
		defer delete(visiting, current)
		switch current := current.(type) {
		case *types.Named:
			// Native framework named types are dependency contracts, not local
			// callable surfaces. The enclosing local signature is inventoried
			// before parameters and results are traversed, so a direct
			// agent.Context parameter remains visible without recursively
			// approving the entire ADK object graph.
			if object := current.Obj(); object != nil && object.Pkg() != nil && object.Pkg() != ownerPackage && isNativeFrameworkImport(object.Pkg().Path()) {
				return
			}
			visit(current.Underlying(), path)
		case *types.Signature:
			if signatureAcceptsContext(current, contextType) {
				signatures = append(signatures, reachableSignature{path: path, signature: current})
			}
			visitTuple(current.Params(), path, "parameter")
			visitTuple(current.Results(), path, "result")
		case *types.Struct:
			for index := 0; index < current.NumFields(); index++ {
				field := current.Field(index)
				if field.Pkg() != nil && field.Pkg() != ownerPackage && !field.Exported() {
					continue
				}
				visit(field.Type(), appendTypePath(path, "field "+field.Name()))
			}
		case *types.Pointer:
			visit(current.Elem(), appendTypePath(path, "pointer element"))
		case *types.Array:
			visit(current.Elem(), appendTypePath(path, "array element"))
		case *types.Slice:
			visit(current.Elem(), appendTypePath(path, "slice element"))
		case *types.Map:
			visit(current.Key(), appendTypePath(path, "map key"))
			visit(current.Elem(), appendTypePath(path, "map value"))
		case *types.Chan:
			visit(current.Elem(), appendTypePath(path, "channel element"))
		case *types.Interface:
			current.Complete()
			for index := 0; index < current.NumExplicitMethods(); index++ {
				method := current.ExplicitMethod(index)
				if method.Pkg() != nil && method.Pkg() != ownerPackage && !method.Exported() {
					continue
				}
				visit(method.Type(), appendTypePath(path, "method "+method.Name()))
			}
			for index := 0; index < current.NumEmbeddeds(); index++ {
				visit(current.EmbeddedType(index), appendTypePath(path, "embedded interface"))
			}
		case *types.TypeParam:
			visit(current.Constraint(), appendTypePath(path, "type constraint"))
		case *types.Union:
			for index := 0; index < current.Len(); index++ {
				visit(current.Term(index).Type(), appendTypePath(path, "union term"))
			}
		}
	}
	visitTuple = func(tuple *types.Tuple, path, label string) {
		if tuple == nil {
			return
		}
		for index := 0; index < tuple.Len(); index++ {
			item := tuple.At(index)
			name := item.Name()
			if name == "" {
				name = strconv.Itoa(index)
			}
			visit(item.Type(), appendTypePath(path, label+" "+name))
		}
	}
	visit(root, "")
	return signatures
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
	writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/inventory\n\ngo 1.26.6\n")
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
		if err != nil || (importPath != "github.com/plasmid-dev/plasmid/loop" && importPath != "github.com/plasmid-dev/plasmid/adkloop") {
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
			source: "package example\nimport \"github.com/plasmid-dev/plasmid/loop\"\ntype Runtime = loop.Provider",
			want:   1,
		},
		{
			name:   "defined wrapper with import alias",
			source: "package example\nimport legacy \"github.com/plasmid-dev/plasmid/loop\"\ntype Runtime legacy.Provider",
			want:   1,
		},
		{
			name:   "dot imported wrapper",
			source: "package example\nimport . \"github.com/plasmid-dev/plasmid/loop\"\ntype Runtime Toolset",
			want:   1,
		},
		{
			name:   "legacy concrete data remains migration data",
			source: "package example\nimport \"github.com/plasmid-dev/plasmid/loop\"\ntype StoredEvent loop.Event",
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

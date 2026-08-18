// Package skills projects immutable extension catalogs into native ADK tools.
package skills

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/plasmid-dev/plasmid/contextresolver"
	"github.com/plasmid-dev/plasmid/extensions"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/warning"
)

// Config supplies the framework-free catalog and scope owners.
type Config struct {
	Catalogs   *extensions.Store
	Contexts   *contextresolver.Resolver
	ProjectDir string
	Warnings   warning.Sink
	Output     outputlimit.Policy
	Budget     *outputlimit.Budget
}

// Toolset owns the native list/load/resource tools.
type Toolset struct {
	config Config
	tools  []tool.Tool
}

// Format prevents activation configuration retained by the catalog store from
// appearing in diagnostic formatting.
func (*Toolset) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "skills.Toolset{redacted}")
}

// LogValue prevents structured logging from reflecting catalog internals.
func (*Toolset) LogValue() slog.Value {
	return slog.StringValue("skills.Toolset{redacted}")
}

// New validates dependencies and constructs all three fixed native tools.
func New(config Config) (*Toolset, error) {
	if config.Catalogs == nil || config.Contexts == nil || config.ProjectDir == "" {
		return nil, errors.New("construct skill toolset: catalogs, contexts, and project directory are required")
	}
	if config.Warnings == nil {
		config.Warnings = warning.SlogSink{}
	}
	if config.Output == (outputlimit.Policy{}) {
		config.Output = outputlimit.Defaults()
	}
	if _, err := outputlimit.NewWriter(config.Output); err != nil {
		return nil, fmt.Errorf("construct skill toolset: invalid output policy: %w", err)
	}
	if config.Budget == nil {
		config.Budget = outputlimit.NewBudget(outputlimit.DefaultPerSession)
	}
	set := &Toolset{config: config}
	list, err := functiontool.New[struct{}, map[string]any](functiontool.Config{
		Name: "list_skills", Description: "List installed skills available to the model.",
		InputSchema: objectSchema(nil, nil), OutputSchema: &jsonschema.Schema{Type: "object"},
	}, set.list)
	if err != nil {
		return nil, err
	}
	load, err := functiontool.New[loadArgs, map[string]any](functiontool.Config{
		Name: "load_skill", Description: "Load and expand one installed skill.",
		InputSchema: objectSchema(map[string]*jsonschema.Schema{
			"name": {Type: "string"}, "arguments": {Type: "string"},
		}, []string{"name"}), OutputSchema: &jsonschema.Schema{Type: "object"},
	}, set.load)
	if err != nil {
		return nil, err
	}
	resource, err := functiontool.New[resourceArgs, map[string]any](functiontool.Config{
		Name: "load_skill_resource", Description: "Load one confined UTF-8 resource from an installed skill.",
		InputSchema: objectSchema(map[string]*jsonschema.Schema{
			"name": {Type: "string"}, "path": {Type: "string"},
		}, []string{"name", "path"}), OutputSchema: &jsonschema.Schema{Type: "object"},
	}, set.loadResource)
	if err != nil {
		return nil, err
	}
	set.tools = []tool.Tool{list, load, resource}
	return set, nil
}

// Name implements native ADK tool.Toolset.
func (*Toolset) Name() string { return "skills" }

// Tools returns the fixed utility tools after ensuring the session snapshot exists.
func (s *Toolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	if err := s.config.Catalogs.StartSession(ctx, ctx.SessionID()); err != nil {
		return nil, err
	}
	catalog, ok := s.config.Catalogs.Snapshot(ctx.SessionID())
	if !ok || len(catalog.Skills()) == 0 {
		return []tool.Tool{}, nil
	}
	return append([]tool.Tool(nil), s.tools...), nil
}

// ProcessRequest refreshes path-scoped skill tools after synchronous workspace
// touches within the same native ADK turn.
func (s *Toolset) ProcessRequest(ctx agent.Context, request *model.LLMRequest) error {
	resolved, err := s.Tools(ctx)
	if err != nil {
		return err
	}
	available := make(map[string]tool.Tool, len(resolved))
	for _, value := range resolved {
		available[value.Name()] = value
		request.Tools[value.Name()] = value
	}
	for _, value := range s.tools {
		if _, ok := available[value.Name()]; !ok {
			delete(request.Tools, value.Name())
		}
	}
	return nil
}

func (s *Toolset) list(ctx agent.Context, _ struct{}) (map[string]any, error) {
	reservation := s.config.Budget.Reserve(ctx.SessionID(), s.config.Output.MaxBytes)
	emitted := 0
	defer func() { s.config.Budget.Consume(ctx.SessionID(), reservation.ID, emitted) }()
	catalog, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	items := catalog.Skills()
	result := make([]map[string]any, len(items))
	for index, item := range items {
		result[index] = map[string]any{
			"name": item.Name, "qualified_names": append([]string(nil), item.QualifiedNames...),
			"description": item.Description,
		}
	}
	bounded, emitted, err := s.boundResult(map[string]any{"skills": result}, reservation.Grant)
	return bounded, err
}

type loadArgs struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (s *Toolset) load(ctx agent.Context, args loadArgs) (map[string]any, error) {
	reservation := s.config.Budget.Reserve(ctx.SessionID(), s.config.Output.MaxBytes)
	emitted := 0
	defer func() { s.config.Budget.Consume(ctx.SessionID(), reservation.ID, emitted) }()
	catalog, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	loaded, err := catalog.LoadSkill(ctx, args.Name, true)
	if err != nil {
		return nil, err
	}
	for _, notice := range loaded.Warnings {
		s.config.Warnings.Warn(notice)
	}
	root := loaded.Root
	body, err := s.config.Contexts.Expand(ctx, contextresolver.Expansion{
		Source: loaded.Body, Path: loaded.SelectedProvenance.SourcePath, Trust: contextresolver.ExtensionTrust(loaded.SelectedProvenance),
		Arguments: args.Arguments, Declared: loaded.Arguments, SessionID: ctx.SessionID(),
		SkillDir: root, ProjectDir: s.config.ProjectDir, PluginRoot: loaded.PluginRoot,
		PluginData: loaded.PluginData, Effort: "normal",
	})
	if err != nil {
		return nil, err
	}
	policy := contextresolver.ExtensionPolicy(loaded.AllowedTools, loaded.DeniedTools, loaded.Restricted)
	if err := s.config.Contexts.IntersectPolicy(ctx.SessionID(), ctx.InvocationID(), policy); err != nil {
		return nil, err
	}
	bounded, emitted, err := s.boundResult(map[string]any{
		"name": loaded.Skill.Name, "qualified_names": loaded.Skill.QualifiedNames,
		"content": body, "warnings": stableWarnings(loaded.Warnings),
	}, reservation.Grant)
	return bounded, err
}

type resourceArgs struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (s *Toolset) loadResource(ctx agent.Context, args resourceArgs) (map[string]any, error) {
	reservation := s.config.Budget.Reserve(ctx.SessionID(), s.config.Output.MaxBytes)
	emitted := 0
	defer func() { s.config.Budget.Consume(ctx.SessionID(), reservation.ID, emitted) }()
	catalog, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	content, err := catalog.LoadSkillResource(ctx, args.Name, args.Path, true)
	if err != nil {
		return nil, err
	}
	bounded, emitted, err := s.boundResult(map[string]any{"name": args.Name, "path": args.Path, "content": content}, reservation.Grant)
	return bounded, err
}

func (s *Toolset) boundResult(projected map[string]any, grant int) (map[string]any, int, error) {
	return outputlimit.BoundJSON(projected, grant, s.config.Output, func(text string, report outputlimit.Report) map[string]any {
		return map[string]any{"output": text, "truncated": true, "truncation": report}
	})
}

func (s *Toolset) catalog(ctx context.Context) (extensions.Catalog, error) {
	readonly, ok := ctx.(agent.ReadonlyContext)
	if !ok {
		return extensions.Catalog{}, errors.New("skill invocation lacks native ADK context")
	}
	if err := s.config.Catalogs.StartSession(ctx, readonly.SessionID()); err != nil {
		return extensions.Catalog{}, err
	}
	catalog, ok := s.config.Catalogs.Snapshot(readonly.SessionID())
	if !ok {
		return extensions.Catalog{}, errors.New("skill catalog snapshot is unavailable")
	}
	return catalog, nil
}

func stableWarnings(values []warning.Warning) []map[string]any {
	result := make([]map[string]any, len(values))
	for index, value := range values {
		result[index] = map[string]any{"code": value.Code, "path": value.Path, "line": value.Line}
	}
	return result
}

func objectSchema(properties map[string]*jsonschema.Schema, required []string) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object", Properties: properties, Required: required,
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
}

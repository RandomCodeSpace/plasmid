// Package extensions owns immutable, framework-free extension catalog snapshots.
package extensions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/internal/pathglob"
	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/warning"
)

const defaultMaxResourceBytes = 1 << 20

var (
	ErrNotFound  = errors.New("extension not found")
	ErrAmbiguous = errors.New("extension name is ambiguous")
	ErrUntrusted = errors.New("extension is not trusted for invocation")
	ErrInactive  = errors.New("extension is not active for the current session")
	ErrChanged   = errors.New("extension source changed after snapshot")
	ErrResource  = errors.New("invalid skill resource")
	ErrClosed    = errors.New("extension store is closed")
)

// ToolPattern is the framework-free public projection of a tool policy row.
type ToolPattern struct {
	Tool     string `json:"tool"`
	Argument string `json:"argument"`
}

// Metadata is one deterministic Agent Skills metadata entry.
type Metadata struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Provenance identifies one retained source without activation secrets.
type Provenance struct {
	Host           string `json:"host"`
	Scope          string `json:"scope"`
	SourcePath     string `json:"source_path"`
	PluginID       string `json:"plugin_id"`
	PluginVersion  string `json:"plugin_version"`
	Enabled        bool   `json:"enabled"`
	Trusted        bool   `json:"trusted"`
	Classification string `json:"classification"`
}

// Skill is safe list metadata. Bodies and resources are never retained here.
type Skill struct {
	Name           string       `json:"name"`
	QualifiedNames []string     `json:"qualified_names"`
	Description    string       `json:"description"`
	License        string       `json:"license"`
	Compatibility  string       `json:"compatibility"`
	Metadata       []Metadata   `json:"metadata"`
	Globs          []string     `json:"globs"`
	Provenance     []Provenance `json:"provenance"`
	ModelInvocable bool         `json:"model_invocable"`
	UserInvocable  bool         `json:"user_invocable"`
}

// Template is safe template list metadata.
type Template struct {
	Name           string       `json:"name"`
	QualifiedNames []string     `json:"qualified_names"`
	Provenance     []Provenance `json:"provenance"`
	ModelInvocable bool         `json:"model_invocable"`
	UserInvocable  bool         `json:"user_invocable"`
}

// MCPServer is a secret-free activation descriptor.
type MCPServer struct {
	Name           string       `json:"name"`
	QualifiedNames []string     `json:"qualified_names"`
	Transport      string       `json:"transport"`
	Foreign        bool         `json:"foreign"`
	Allowed        bool         `json:"allowed"`
	Provenance     []Provenance `json:"provenance"`
}

// Instruction and CompiledPlugin complete the normalized catalog kinds.
type Instruction struct {
	Name       string       `json:"name"`
	Provenance []Provenance `json:"provenance"`
}

type CompiledPlugin struct {
	Name       string       `json:"name"`
	Provenance []Provenance `json:"provenance"`
}

// LoadedSkill is one digest-verified lazy body and invocation policy.
type LoadedSkill struct {
	Skill              Skill
	SelectedName       string
	SelectedProvenance Provenance
	Root               string
	PluginRoot         string
	PluginData         string
	Body               string
	Arguments          []string
	Globs              []string
	AllowedTools       []ToolPattern
	DeniedTools        []ToolPattern
	Restricted         bool
	Warnings           []warning.Warning
}

// LoadedTemplate is one digest-verified lazy template and invocation policy.
type LoadedTemplate struct {
	Template           Template
	SelectedName       string
	SelectedProvenance Provenance
	Root               string
	PluginRoot         string
	PluginData         string
	Body               string
	Arguments          []string
	AllowedTools       []ToolPattern
	DeniedTools        []ToolPattern
	Restricted         bool
	Warnings           []warning.Warning
}

type documentSpec struct {
	name, description, license, compatibility string
	metadata                                  []Metadata
	arguments                                 []string
	globs                                     []string
	allowed, denied                           []ToolPattern
	restricted                                bool
	userInvocable, modelInvocable             bool
}

type sourceRef struct {
	alias, root, relative, path, digest string
	pluginRoot, pluginData              string
	provenance                          Provenance
	rootInfo                            os.FileInfo
	userInvocable, modelInvocable       bool
}

type skillRecord struct {
	public  Skill
	spec    documentSpec
	digest  string
	sources map[string][]sourceRef
}

type templateRecord struct {
	public  Template
	spec    documentSpec
	digest  string
	sources map[string][]sourceRef
}

// Catalog is immutable after publication by Store.StartSession.
type Catalog struct {
	skills           []skillRecord
	templates        []templateRecord
	mcpServers       []MCPServer
	mcpLookup        map[string][]string
	mcpAllowed       map[string]bool
	resolveMCP       func(string) (config.MCPServer, error)
	instructions     []Instruction
	plugins          []CompiledPlugin
	skillLookup      map[string][]int
	templateLookup   map[string][]int
	warnings         []warning.Warning
	maxResourceBytes int64
	skillMatchers    []pathglob.Matcher
	activeSkills     []bool
}

func (c Catalog) Skills() []Skill {
	result := make([]Skill, 0, len(c.skills))
	for index, record := range c.skills {
		if record.public.ModelInvocable && c.skillActive(index) {
			result = append(result, cloneSkill(record.public))
		}
	}
	return result
}

func (c Catalog) AllSkills() []Skill {
	result := make([]Skill, len(c.skills))
	for index, record := range c.skills {
		result[index] = cloneSkill(record.public)
	}
	return result
}

func (c Catalog) Templates() []Template {
	result := make([]Template, len(c.templates))
	for index, record := range c.templates {
		result[index] = cloneTemplate(record.public)
	}
	return result
}

func (c Catalog) MCPServers() []MCPServer {
	result := make([]MCPServer, len(c.mcpServers))
	for index, value := range c.mcpServers {
		result[index] = cloneMCPServer(value)
	}
	return result
}

// AllowedMCPNames returns exact qualified activation names in stable order.
func (c Catalog) AllowedMCPNames() []string {
	result := make([]string, 0, len(c.mcpAllowed))
	for name, allowed := range c.mcpAllowed {
		if allowed {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return result
}

// ResolveMCP returns one consent-gated runtime descriptor. Secrets remain out
// of list records and warnings.
func (c Catalog) ResolveMCP(name string) (config.MCPServer, error) {
	aliases := c.mcpLookup[name]
	if strings.Contains(name, ":") {
		aliases = []string{name}
	}
	aliases = sortedUnique(aliases)
	if len(aliases) == 0 {
		return config.MCPServer{}, ErrNotFound
	}
	if len(aliases) != 1 {
		return config.MCPServer{}, ErrAmbiguous
	}
	alias := aliases[0]
	if !c.mcpAllowed[alias] {
		return config.MCPServer{}, ErrUntrusted
	}
	if c.resolveMCP == nil {
		return config.MCPServer{}, ErrNotFound
	}
	return c.resolveMCP(alias)
}

func (c Catalog) Instructions() []Instruction {
	result := make([]Instruction, len(c.instructions))
	for index, value := range c.instructions {
		result[index] = Instruction{Name: value.Name, Provenance: append([]Provenance(nil), value.Provenance...)}
	}
	return result
}

func (c Catalog) CompiledPlugins() []CompiledPlugin {
	result := make([]CompiledPlugin, len(c.plugins))
	for index, value := range c.plugins {
		result[index] = CompiledPlugin{Name: value.Name, Provenance: append([]Provenance(nil), value.Provenance...)}
	}
	return result
}
func (c Catalog) Warnings() []warning.Warning { return append([]warning.Warning(nil), c.warnings...) }

// LoadSkill bounded-reads and digest-verifies the selected snapshot source.
func (c Catalog) LoadSkill(ctx context.Context, name string, model bool) (LoadedSkill, error) {
	index, err := resolveIndex(name, c.skillLookup)
	if err != nil {
		return LoadedSkill{}, fmt.Errorf("load skill %q: %w", name, err)
	}
	record := c.skills[index]
	if model && !c.skillActive(index) {
		return LoadedSkill{}, fmt.Errorf("load skill %q: %w", name, ErrInactive)
	}
	source, err := selectSource(name, record.sources, model)
	if err != nil {
		return LoadedSkill{}, fmt.Errorf("load skill %q: %w", name, err)
	}
	data, err := readConfined(ctx, source.root, source.relative, c.maximumBytes(), source.rootInfo)
	if err != nil {
		return LoadedSkill{}, fmt.Errorf("load skill %q: %w", name, err)
	}
	if digest(data) != source.digest {
		return LoadedSkill{}, fmt.Errorf("load skill %q: %w", name, ErrChanged)
	}
	document, notices := syntax.ParseDocument(string(data), source.path, syntax.Host(source.provenance.Host))
	if document.Name != record.spec.name {
		return LoadedSkill{}, fmt.Errorf("load skill %q: %w", name, ErrChanged)
	}
	return LoadedSkill{
		Skill: cloneSkill(record.public), SelectedName: source.alias, SelectedProvenance: source.provenance, Root: source.root,
		PluginRoot: source.pluginRoot, PluginData: source.pluginData, Body: document.Body,
		Arguments:    append([]string(nil), document.Arguments...),
		Globs:        append([]string(nil), document.Globs...),
		AllowedTools: projectPatterns(document.AllowedTools), DeniedTools: projectPatterns(document.DeniedTools),
		Restricted: document.RestrictsTools(), Warnings: append([]warning.Warning(nil), notices...),
	}, nil
}

// LoadTemplate bounded-reads and digest-verifies the selected template source.
func (c Catalog) LoadTemplate(ctx context.Context, name string, model bool) (LoadedTemplate, error) {
	index, err := resolveIndex(name, c.templateLookup)
	if err != nil {
		return LoadedTemplate{}, fmt.Errorf("load template %q: %w", name, err)
	}
	record := c.templates[index]
	source, err := selectSource(name, record.sources, model)
	if err != nil {
		return LoadedTemplate{}, fmt.Errorf("load template %q: %w", name, err)
	}
	data, err := readConfined(ctx, source.root, source.relative, c.maximumBytes(), source.rootInfo)
	if err != nil {
		return LoadedTemplate{}, fmt.Errorf("load template %q: %w", name, err)
	}
	if digest(data) != source.digest {
		return LoadedTemplate{}, fmt.Errorf("load template %q: %w", name, ErrChanged)
	}
	document, notices := syntax.ParseTemplate(string(data), source.path, syntax.Host(source.provenance.Host), record.spec.name)
	return LoadedTemplate{
		Template: cloneTemplate(record.public), SelectedName: source.alias, SelectedProvenance: source.provenance, Root: source.root,
		PluginRoot: source.pluginRoot, PluginData: source.pluginData, Body: document.Body,
		Arguments:    append([]string(nil), document.Arguments...),
		AllowedTools: projectPatterns(document.AllowedTools), DeniedTools: projectPatterns(document.DeniedTools),
		Restricted: document.RestrictsTools(), Warnings: append([]warning.Warning(nil), notices...),
	}, nil
}

// LoadSkillResource reads one confined regular UTF-8 resource on demand.
func (c Catalog) LoadSkillResource(ctx context.Context, name, resource string, model bool) (string, error) {
	index, err := resolveIndex(name, c.skillLookup)
	if err != nil {
		return "", err
	}
	record := c.skills[index]
	if model && !c.skillActive(index) {
		return "", fmt.Errorf("load skill resource %q from %q: %w", resource, name, ErrInactive)
	}
	source, err := selectSource(name, record.sources, model)
	if err != nil {
		return "", err
	}
	if distinctRootsForName(record.sources, name, model) != 1 {
		return "", fmt.Errorf("load skill resource %q from %q: %w", resource, name, ErrAmbiguous)
	}
	clean := filepath.Clean(filepath.FromSlash(resource))
	if resource == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("load skill resource %q: %w", resource, ErrResource)
	}
	maximum := c.maxResourceBytes
	if maximum <= 0 {
		maximum = defaultMaxResourceBytes
	}
	data, err := readConfined(ctx, source.root, clean, maximum, source.rootInfo)
	if err != nil {
		return "", fmt.Errorf("load skill resource %q: %w", resource, err)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("load skill resource %q: %w", resource, ErrResource)
	}
	return string(data), nil
}

func (c Catalog) skillActive(index int) bool {
	return len(c.activeSkills) == 0 || index >= len(c.activeSkills) || c.activeSkills[index]
}

func (c Catalog) maximumBytes() int64 {
	if c.maxResourceBytes > 0 {
		return c.maxResourceBytes
	}
	return defaultMaxResourceBytes
}

func resolveIndex(name string, lookup map[string][]int) (int, error) {
	indices := lookup[name]
	if len(indices) == 0 {
		return 0, ErrNotFound
	}
	if len(indices) != 1 {
		return 0, ErrAmbiguous
	}
	return indices[0], nil
}

func selectSource(name string, sources map[string][]sourceRef, model bool) (sourceRef, error) {
	aliases := make([]string, 0, len(sources))
	if strings.Contains(name, ":") {
		aliases = append(aliases, name)
	} else {
		for alias := range sources {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
	}
	found := false
	for _, alias := range aliases {
		for _, source := range sources[alias] {
			found = true
			if model && !source.modelInvocable {
				continue
			}
			if !model && !source.userInvocable {
				continue
			}
			return source, nil
		}
	}
	if !found {
		return sourceRef{}, ErrNotFound
	}
	return sourceRef{}, ErrUntrusted
}

func distinctRoots(sources map[string][]sourceRef, model bool) int {
	roots := make(map[string]bool)
	for _, values := range sources {
		for _, source := range values {
			if (model && source.modelInvocable) || (!model && source.userInvocable) {
				roots[source.root] = true
			}
		}
	}
	return len(roots)
}

func distinctRootsForName(sources map[string][]sourceRef, name string, model bool) int {
	if !strings.Contains(name, ":") {
		return distinctRoots(sources, model)
	}
	return distinctRoots(map[string][]sourceRef{name: sources[name]}, model)
}

func readConfined(ctx context.Context, rootPath, relative string, maximum int64, expectedRoot os.FileInfo) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if expectedRoot != nil {
		directory, openErr := root.Open(".")
		if openErr != nil {
			return nil, openErr
		}
		actual, statErr := directory.Stat()
		directory.Close()
		if statErr != nil {
			return nil, statErr
		}
		if !os.SameFile(expectedRoot, actual) {
			return nil, ErrChanged
		}
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrResource
	}
	data, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: file}, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, ErrResource
	}
	return data, ctx.Err()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return n, contextErr
	}
	return n, err
}

func projectPatterns(values []syntax.ToolPattern) []ToolPattern {
	result := make([]ToolPattern, len(values))
	for index, value := range values {
		result[index] = ToolPattern{Tool: value.Tool, Argument: value.Argument}
	}
	return result
}

func cloneSkill(value Skill) Skill {
	value.QualifiedNames = append([]string(nil), value.QualifiedNames...)
	value.Metadata = append([]Metadata(nil), value.Metadata...)
	value.Globs = append([]string(nil), value.Globs...)
	value.Provenance = append([]Provenance(nil), value.Provenance...)
	return value
}

func cloneTemplate(value Template) Template {
	value.QualifiedNames = append([]string(nil), value.QualifiedNames...)
	value.Provenance = append([]Provenance(nil), value.Provenance...)
	return value
}

func cloneMCPServer(value MCPServer) MCPServer {
	value.QualifiedNames = append([]string(nil), value.QualifiedNames...)
	value.Provenance = append([]Provenance(nil), value.Provenance...)
	return value
}

func cloneMCPConfig(value config.MCPServer) config.MCPServer {
	value.Args = append([]string(nil), value.Args...)
	value.Env = cloneStrings(value.Env)
	value.Headers = cloneStrings(value.Headers)
	return value
}

func cloneStrings(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

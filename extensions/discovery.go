package extensions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/foreign"
	"github.com/plasmid-dev/plasmid/internal/foreignactivation"
	"github.com/plasmid-dev/plasmid/internal/pathglob"
	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

func discover(ctx context.Context, options Options) (Catalog, error) {
	builder := catalogBuilder{catalog: Catalog{
		skillLookup: make(map[string][]int), templateLookup: make(map[string][]int),
		mcpLookup: make(map[string][]string), mcpAllowed: make(map[string]bool),
		maxResourceBytes: options.MaxResourceBytes,
		instructions:     cloneInstructions(options.Instructions),
		plugins:          cloneCompiledPlugins(options.CompiledPlugins),
	}, activations: make(map[string][]config.MCPServer)}
	for _, root := range options.SkillRoots {
		if err := builder.scanConfiguredRoot(ctx, root, options); err != nil {
			return Catalog{}, err
		}
	}
	hostScans := []struct {
		enabled bool
		scan    func(context.Context, foreign.Options, *foreignactivation.Vault) (foreign.HostCatalog, error)
	}{{options.Claude, foreign.ScanClaudeWithActivations}, {options.Codex, foreign.ScanCodexWithActivations}, {options.Copilot, foreign.ScanCopilotWithActivations}}
	for _, host := range hostScans {
		if !host.enabled {
			continue
		}
		vault := &foreignactivation.Vault{}
		view, err := host.scan(ctx, options.Foreign, vault)
		if err != nil {
			return Catalog{}, err
		}
		builder.addForeign(view, vault, options.MCP)
	}
	builder.addConfiguredMCP(options.MCP.Servers)
	builder.finish()
	activations := builder.activations
	builder.catalog.resolveMCP = func(alias string) (config.MCPServer, error) {
		values := activations[alias]
		if len(values) == 0 {
			return config.MCPServer{}, ErrNotFound
		}
		if len(values) != 1 {
			return config.MCPServer{}, ErrAmbiguous
		}
		return cloneMCPConfig(values[0]), nil
	}
	for _, notice := range builder.catalog.warnings {
		options.WarningSink.Warn(notice)
	}
	return builder.catalog, nil
}

type catalogBuilder struct {
	catalog     Catalog
	activations map[string][]config.MCPServer
	entries     int
	truncated   bool
}

func (b *catalogBuilder) scanConfiguredRoot(ctx context.Context, rootPath string, options Options) error {
	root, err := os.OpenRoot(rootPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		b.warn(warning.WarnForeignIndexUnreadable, rootPath, "configured skill root is unreadable")
		return nil
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	remaining := options.MaxEntries - b.entries
	if remaining <= 0 {
		if !b.truncated {
			b.warn(warning.WarnForeignScanTruncated, rootPath, "configured extension scan entry limit reached")
			b.truncated = true
		}
		directory.Close()
		return nil
	}
	entries, err := directory.ReadDir(remaining + 1)
	directory.Close()
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(entries) > remaining {
		// File.ReadDir returns directory order, which differs across filesystems.
		// A partial sample therefore cannot select a portable subset. Reject the
		// over-budget root as a unit instead of publishing whichever entries the
		// host happened to return first.
		entries = nil
		b.entries = options.MaxEntries
		if !b.truncated {
			b.warn(warning.WarnForeignScanTruncated, rootPath, "configured extension scan entry limit reached")
			b.truncated = true
		}
	}
	b.entries += len(entries)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return b.scanConfiguredEntries(rootPath, entries, options, configuredScan{
		contextError: ctx.Err,
		read: func(rootPath, relative string, maximum int64) ([]byte, error) {
			return readConfined(ctx, rootPath, relative, maximum, nil)
		},
	})
}

type configuredScan struct {
	contextError func() error
	read         func(string, string, int64) ([]byte, error)
}

func (b *catalogBuilder) scanConfiguredEntries(rootPath string, entries []os.DirEntry, options Options, scan configuredScan) error {
	trusted := !canonicalWithin(rootPath, options.WorkingDir) || options.Foreign.ProjectTrusted
	for _, entry := range entries {
		if err := scan.contextError(); err != nil {
			return err
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		relative := filepath.Join(entry.Name(), "SKILL.md")
		maximum := options.MaxResourceBytes
		if maximum <= 0 {
			maximum = defaultMaxResourceBytes
		}
		data, err := scan.read(rootPath, relative, maximum)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			b.warn(warning.WarnForeignIndexUnreadable, filepath.Join(rootPath, relative), "configured skill is unreadable")
			continue
		}
		path := filepath.Join(rootPath, relative)
		document, notices := syntax.ParseDocument(string(data), filepath.ToSlash(path), syntax.HostPlasmid)
		b.catalog.warnings = append(b.catalog.warnings, notices...)
		if document.Name == "" || document.Description == "" {
			continue
		}
		provenance := Provenance{Host: "plasmid", Scope: "configured", SourcePath: filepath.ToSlash(path), Enabled: true, Trusted: trusted, Classification: "documented"}
		skillRoot := filepath.Join(rootPath, entry.Name())
		rootInfo, err := rootIdentity(skillRoot)
		if err != nil {
			b.warn(warning.WarnForeignIndexUnreadable, skillRoot, "configured skill root is unreadable")
			continue
		}
		source := sourceRef{
			alias: qualify("plasmid", "configured", document.Name), root: skillRoot, relative: "SKILL.md",
			path: filepath.ToSlash(path), digest: digest(data), provenance: provenance, rootInfo: rootInfo,
			userInvocable:  document.Exposure.UserInvocable,
			modelInvocable: document.Exposure.Allows(syntax.InvocationModel, canonicalWithin(rootPath, options.WorkingDir), trusted),
		}
		b.addSkill(specFromDocument(document), source)
	}
	return nil
}

func (b *catalogBuilder) addForeign(view foreign.HostCatalog, vault *foreignactivation.Vault, mcpConfig config.MCP) {
	b.catalog.warnings = append(b.catalog.warnings, view.Warnings...)
	b.addForeignSkills(view.Skills)
	b.addForeignTemplates(view.Templates)
	b.addForeignMCP(view.MCPServers, vault, mcpConfig)
}

func (b *catalogBuilder) addForeignSkills(skills []foreign.Skill) {
	for _, item := range skills {
		if len(item.Provenance) == 0 {
			continue
		}
		spec := documentSpec{
			name: item.Name, description: item.Description, license: item.License, compatibility: item.Compatibility,
			globs:     append([]string(nil), item.Globs...),
			arguments: append([]string(nil), item.Arguments...), allowed: projectForeignPatterns(item.Permissions.Allowed),
			denied: projectForeignPatterns(item.Permissions.Denied), restricted: item.RestrictsTools,
			userInvocable: item.UserInvocable, modelInvocable: item.ModelInvocable,
		}
		for _, value := range item.Metadata {
			spec.metadata = append(spec.metadata, Metadata{Name: value.Name, Value: value.Value})
		}
		for _, provenance := range item.Provenance {
			converted := convertProvenance(provenance)
			root := filepath.Dir(filepath.FromSlash(provenance.SourcePath))
			rootInfo, err := rootIdentity(root)
			if err != nil {
				b.warn(warning.WarnForeignIndexUnreadable, root, "foreign skill root is unreadable")
				continue
			}
			repository := provenance.Scope == foreign.ScopeProject || provenance.Scope == foreign.ScopeLocal
			source := sourceRef{
				alias: qualify(string(provenance.Host), string(provenance.Scope), item.QualifiedName), root: root, relative: filepath.Base(provenance.SourcePath),
				path: provenance.SourcePath, digest: item.SourceDigest(), provenance: converted, rootInfo: rootInfo,
				pluginRoot: provenance.PluginRoot, pluginData: provenance.PluginData,
				userInvocable:  provenance.Enabled && item.UserInvocable,
				modelInvocable: provenance.Enabled && item.ModelInvocable && (!repository || converted.Trusted),
			}
			b.addSkill(spec, source)
		}
	}
}

func (b *catalogBuilder) addForeignTemplates(templates []foreign.Template) {
	for _, item := range templates {
		if len(item.Provenance) == 0 {
			continue
		}
		spec := documentSpec{name: item.Name, description: item.Name, arguments: append([]string(nil), item.Arguments...), allowed: projectForeignPatterns(item.Permissions.Allowed), denied: projectForeignPatterns(item.Permissions.Denied), restricted: item.RestrictsTools, userInvocable: item.UserInvocable, modelInvocable: item.ModelInvocable}
		for _, provenance := range item.Provenance {
			converted := convertProvenance(provenance)
			repository := provenance.Scope == foreign.ScopeProject || provenance.Scope == foreign.ScopeLocal
			root := filepath.Dir(filepath.FromSlash(provenance.SourcePath))
			rootInfo, err := rootIdentity(root)
			if err != nil {
				b.warn(warning.WarnForeignIndexUnreadable, root, "foreign template root is unreadable")
				continue
			}
			source := sourceRef{
				alias: qualify(string(provenance.Host), string(provenance.Scope), item.QualifiedName), root: root, relative: filepath.Base(provenance.SourcePath),
				path: provenance.SourcePath, digest: item.SourceDigest(), provenance: converted, rootInfo: rootInfo,
				pluginRoot: provenance.PluginRoot, pluginData: provenance.PluginData,
				userInvocable:  provenance.Enabled && item.UserInvocable,
				modelInvocable: provenance.Enabled && item.ModelInvocable && (!repository || converted.Trusted),
			}
			b.addTemplate(spec, source)
		}
	}
}

func (b *catalogBuilder) addForeignMCP(servers []foreign.MCPServer, vault *foreignactivation.Vault, mcpConfig config.MCP) {
	allow := make(map[string]bool, len(mcpConfig.AllowForeign))
	for _, name := range mcpConfig.AllowForeign {
		allow[name] = true
	}
	for _, item := range servers {
		provenance := make([]Provenance, len(item.Provenance))
		aliases := make([]string, len(item.Provenance))
		allowed := false
		for index, source := range item.Provenance {
			provenance[index] = convertProvenance(source)
			aliases[index] = qualify(string(source.Host), string(source.Scope), item.QualifiedName)
			repository := source.Scope == foreign.ScopeProject || source.Scope == foreign.ScopeLocal
			authorized := source.Enabled && (!repository || provenance[index].Trusted) && (mcpConfig.InheritForeign || allow[aliases[index]])
			b.catalog.mcpAllowed[aliases[index]] = authorized
			allowed = allowed || authorized
		}
		b.catalog.mcpServers = append(b.catalog.mcpServers, MCPServer{Name: item.Name, QualifiedNames: sortedUnique(aliases), Transport: item.Transport, Foreign: true, Allowed: allowed, Provenance: provenance})
		if activation, ok := foreign.TransferMCPActivation(item, vault); ok {
			runtime := config.MCPServer{ID: activation.ID, Transport: config.MCPTransport(activation.Transport), Command: activation.Command, Args: append([]string(nil), activation.Args...), Env: cloneStrings(activation.Env), URL: activation.URL, Headers: cloneStrings(activation.Headers)}
			for _, alias := range aliases {
				b.catalog.mcpLookup[item.Name] = append(b.catalog.mcpLookup[item.Name], alias)
				b.catalog.mcpLookup[alias] = []string{alias}
				b.activations[alias] = append(b.activations[alias], runtime)
			}
		}
	}
}

func (b *catalogBuilder) addConfiguredMCP(values []config.MCPServer) {
	for _, item := range values {
		alias := qualify("plasmid", "configured", item.ID)
		transport := string(item.Transport)
		b.catalog.mcpServers = append(b.catalog.mcpServers, MCPServer{Name: item.ID, QualifiedNames: []string{alias}, Transport: transport, Allowed: true, Provenance: []Provenance{{Host: "plasmid", Scope: "configured", Enabled: true, Trusted: true, Classification: "documented"}}})
		b.catalog.mcpLookup[item.ID] = append(b.catalog.mcpLookup[item.ID], alias)
		b.catalog.mcpLookup[alias] = []string{alias}
		b.activations[alias] = append(b.activations[alias], item)
		b.catalog.mcpAllowed[alias] = true
	}
}

func (b *catalogBuilder) addSkill(spec documentSpec, source sourceRef) {
	for index := range b.catalog.skills {
		record := &b.catalog.skills[index]
		if record.spec.name == spec.name && record.digest == source.digest {
			record.sources[source.alias] = append(record.sources[source.alias], source)
			record.public.QualifiedNames = sortedUnique(append(record.public.QualifiedNames, source.alias))
			record.public.Provenance = append(record.public.Provenance, source.provenance)
			record.public.ModelInvocable = record.public.ModelInvocable || source.modelInvocable
			record.public.UserInvocable = record.public.UserInvocable || source.userInvocable
			return
		}
	}
	record := skillRecord{spec: spec, digest: source.digest, sources: map[string][]sourceRef{source.alias: {source}}}
	record.public = Skill{Name: spec.name, QualifiedNames: []string{source.alias}, Description: spec.description, License: spec.license, Compatibility: spec.compatibility, Metadata: append([]Metadata(nil), spec.metadata...), Globs: append([]string(nil), spec.globs...), Provenance: []Provenance{source.provenance}, ModelInvocable: source.modelInvocable, UserInvocable: source.userInvocable}
	b.catalog.skills = append(b.catalog.skills, record)
}

func (b *catalogBuilder) addTemplate(spec documentSpec, source sourceRef) {
	for index := range b.catalog.templates {
		record := &b.catalog.templates[index]
		if record.spec.name == spec.name && record.digest == source.digest {
			record.sources[source.alias] = append(record.sources[source.alias], source)
			record.public.QualifiedNames = sortedUnique(append(record.public.QualifiedNames, source.alias))
			record.public.Provenance = append(record.public.Provenance, source.provenance)
			record.public.ModelInvocable = record.public.ModelInvocable || source.modelInvocable
			record.public.UserInvocable = record.public.UserInvocable || source.userInvocable
			return
		}
	}
	record := templateRecord{spec: spec, digest: source.digest, sources: map[string][]sourceRef{source.alias: {source}}}
	record.public = Template{Name: spec.name, QualifiedNames: []string{source.alias}, Provenance: []Provenance{source.provenance}, ModelInvocable: source.modelInvocable, UserInvocable: source.userInvocable}
	b.catalog.templates = append(b.catalog.templates, record)
}

func (b *catalogBuilder) finish() {
	b.finishSkills()
	b.finishTemplates()
	sort.SliceStable(b.catalog.mcpServers, func(i, j int) bool { return b.catalog.mcpServers[i].Name < b.catalog.mcpServers[j].Name })
}

func (b *catalogBuilder) finishSkills() {
	sort.SliceStable(b.catalog.skills, func(i, j int) bool {
		if b.catalog.skills[i].spec.name != b.catalog.skills[j].spec.name {
			return b.catalog.skills[i].spec.name < b.catalog.skills[j].spec.name
		}
		return b.catalog.skills[i].digest < b.catalog.skills[j].digest
	})
	b.catalog.skillLookup = make(map[string][]int)
	b.catalog.skillMatchers = make([]pathglob.Matcher, len(b.catalog.skills))
	b.catalog.activeSkills = make([]bool, len(b.catalog.skills))
	for index, record := range b.catalog.skills {
		if len(record.spec.globs) == 0 {
			b.catalog.activeSkills[index] = true
		} else {
			b.catalog.skillMatchers[index], _ = pathglob.Compile(record.spec.globs)
		}
		b.catalog.skillLookup[record.spec.name] = append(b.catalog.skillLookup[record.spec.name], index)
		for alias := range record.sources {
			b.catalog.skillLookup[alias] = append(b.catalog.skillLookup[alias], index)
		}
	}
	warnedSkills := make(map[string]bool)
	for _, record := range b.catalog.skills {
		if len(b.catalog.skillLookup[record.spec.name]) > 1 && !warnedSkills[record.spec.name] {
			b.warn(warning.WarnForeignAmbiguousName, record.spec.name, "unqualified skill name requires qualification")
			warnedSkills[record.spec.name] = true
		}
	}
}

func (b *catalogBuilder) finishTemplates() {
	sort.SliceStable(b.catalog.templates, func(i, j int) bool {
		if b.catalog.templates[i].spec.name != b.catalog.templates[j].spec.name {
			return b.catalog.templates[i].spec.name < b.catalog.templates[j].spec.name
		}
		return b.catalog.templates[i].digest < b.catalog.templates[j].digest
	})
	b.catalog.templateLookup = make(map[string][]int)
	for index, record := range b.catalog.templates {
		b.catalog.templateLookup[record.spec.name] = append(b.catalog.templateLookup[record.spec.name], index)
		for alias := range record.sources {
			b.catalog.templateLookup[alias] = append(b.catalog.templateLookup[alias], index)
		}
	}
	warnedTemplates := make(map[string]bool)
	for _, record := range b.catalog.templates {
		if len(b.catalog.templateLookup[record.spec.name]) > 1 && !warnedTemplates[record.spec.name] {
			b.warn(warning.WarnForeignAmbiguousName, record.spec.name, "unqualified template name requires qualification")
			warnedTemplates[record.spec.name] = true
		}
	}
}

func specFromDocument(document syntax.Document) documentSpec {
	spec := documentSpec{name: document.Name, description: document.Description, license: document.License, compatibility: document.Compatibility, arguments: append([]string(nil), document.Arguments...), globs: append([]string(nil), document.Globs...), allowed: projectPatterns(document.AllowedTools), denied: projectPatterns(document.DeniedTools), restricted: document.RestrictsTools(), userInvocable: document.Exposure.UserInvocable, modelInvocable: document.Exposure.ModelInvocable}
	for _, value := range document.Metadata {
		spec.metadata = append(spec.metadata, Metadata{Name: value.Name, Value: value.Value})
	}
	return spec
}

func projectForeignPatterns(values []foreign.ToolPattern) []ToolPattern {
	result := make([]ToolPattern, len(values))
	for index, value := range values {
		result[index] = ToolPattern{Tool: value.Tool, Argument: value.Argument}
	}
	return result
}
func convertProvenance(value foreign.Provenance) Provenance {
	trusted := value.Trust == foreign.TrustTrusted || (value.Scope != foreign.ScopeProject && value.Scope != foreign.ScopeLocal)
	return Provenance{Host: string(value.Host), Scope: string(value.Scope), SourcePath: value.SourcePath, PluginID: value.PluginID, PluginVersion: value.PluginVersion, Enabled: value.Enabled, Trusted: trusted, Classification: string(value.Classification)}
}
func qualify(host, scope, name string) string { return host + ":" + scope + ":" + name }
func digest(data []byte) string               { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func canonicalWithin(path, root string) bool {
	return workspace.ContainsCanonical(root, path)
}

func rootIdentity(path string) (os.FileInfo, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return directory.Stat()
}
func sortedUnique(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return append([]string(nil), result...)
}
func (b *catalogBuilder) warn(code, path, message string) {
	b.catalog.warnings = append(b.catalog.warnings, warning.Warning{Code: code, Source: "extensions", Path: filepath.ToSlash(path), Message: message})
}

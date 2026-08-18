package foreign

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plasmid-dev/plasmid/internal/foreignactivation"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

// ScanCodex discovers Codex extension metadata without activating it.
func ScanCodex(ctx context.Context, options Options) (HostCatalog, error) {
	return ScanCodexWithActivations(ctx, options, nil)
}

// ScanCodexWithActivations transfers runtime descriptors into an internal
// capability while keeping the returned normalized catalog secret-free.
func ScanCodexWithActivations(ctx context.Context, options Options, vault *foreignactivation.Vault) (HostCatalog, error) {
	s, err := newScanner(ctx, HostCodex, options)
	if err != nil {
		return HostCatalog{}, err
	}
	s.activationVault = vault
	catalog := HostCatalog{Host: HostCodex}
	steps := make([]func(context.Context) error, 0)
	for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
		steps = append(steps, func(scanCtx context.Context) error {
			return s.scanSkillRoot(scanCtx, &catalog, filepath.Join(directory, agentsDirectory, "skills"), source(ScopeProject, ClassificationDocumented, "", "", true))
		})
	}
	if s.options.HomeDir != "" {
		steps = append(steps, func(scanCtx context.Context) error {
			return s.scanSkillRoot(scanCtx, &catalog, filepath.Join(s.options.HomeDir, agentsDirectory, "skills"), source(ScopeUser, ClassificationDocumented, "", "", true))
		})
	}
	steps = append(steps,
		func(scanCtx context.Context) error {
			return s.scanSkillRoot(scanCtx, &catalog, s.options.AdminSkillsDir, source(ScopeAdmin, ClassificationDocumented, "", "", true))
		},
		func(scanCtx context.Context) error {
			return s.scanSkillRoot(scanCtx, &catalog, filepath.Join(s.options.CodexHome, "skills"), source(ScopeUser, ClassificationCompatibility, "", "", true))
		},
		func(scanCtx context.Context) error {
			return s.scanTemplateRoot(scanCtx, &catalog, filepath.Join(s.options.CodexHome, "prompts"), ".md", source(ScopeUser, ClassificationCompatibility, "", "", true))
		},
	)
	configs := []struct {
		path  string
		scope Scope
	}{}
	if s.options.ProjectTrusted {
		configs = append(configs, struct {
			path  string
			scope Scope
		}{filepath.Join(s.options.RepositoryRoot, codexDirectory, codexConfigurationFile), ScopeProject})
	} else {
		s.warnIfUntrustedFile(ctx, filepath.Join(s.options.RepositoryRoot, codexDirectory, codexConfigurationFile))
	}
	configs = append(configs, struct {
		path  string
		scope Scope
	}{filepath.Join(s.options.CodexHome, codexConfigurationFile), ScopeUser})
	pluginEnabled := make(map[string]bool)
	for _, config := range configs {
		steps = append(steps, func(scanCtx context.Context) error {
			return s.scanCodexConfig(scanCtx, &catalog, config.path, config.scope, pluginEnabled)
		})
	}
	marketplaces := []struct {
		path           string
		root           string
		scope          Scope
		classification Classification
	}{
		{filepath.Join(s.options.RepositoryRoot, agentsDirectory, "plugins", marketplaceFileName), s.options.RepositoryRoot, ScopeProject, ClassificationDocumented},
		{filepath.Join(s.options.HomeDir, agentsDirectory, "plugins", marketplaceFileName), s.options.HomeDir, ScopeUser, ClassificationDocumented},
		{filepath.Join(s.options.RepositoryRoot, ".claude-plugin", marketplaceFileName), s.options.RepositoryRoot, ScopeProject, ClassificationCompatibility},
	}
	for _, marketplace := range marketplaces {
		if marketplace.root != "" {
			steps = append(steps, func(scanCtx context.Context) error {
				return s.scanCodexMarketplace(scanCtx, &catalog, marketplace.path, marketplace.root, marketplace.scope, marketplace.classification, pluginEnabled)
			})
		}
	}
	steps = append(steps, checkContext)
	if err := runScannerSteps(ctx, steps...); err != nil {
		return HostCatalog{}, err
	}
	return s.finish(catalog), nil
}

func (s *scanner) warnIfUntrustedFile(ctx context.Context, path string) {
	if err := checkContext(ctx); err != nil {
		return
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		s.addWarning(warning.WarnForeignProjectUntrusted, path, "repository configuration skipped because the project is untrusted")
	}
}

func (s *scanner) scanCodexConfig(ctx context.Context, catalog *HostCatalog, path string, scope Scope, pluginEnabled map[string]bool) error {
	data, err := s.readFile(ctx, path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.addReadWarning(err, path, warning.WarnForeignIndexUnreadable, "Codex configuration is unreadable")
		return nil
	}
	sections := s.parseTOML(ctx, path, data)
	if err := checkContext(ctx); err != nil {
		return err
	}
	for _, section := range sections {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if codexMCPSection(section) {
			s.scanCodexMCPSection(catalog, path, scope, section)
			continue
		}
		s.scanCodexPluginSetting(path, section, pluginEnabled)
	}
	return nil
}

func codexMCPSection(section tomlSection) bool {
	return len(section.path) == 2 && section.path[0] == "mcp_servers"
}

func (s *scanner) scanCodexMCPSection(catalog *HostCatalog, path string, scope Scope, section tomlSection) {
	transport, valid := s.codexMCPTransport(path, section.values)
	if !valid {
		return
	}
	enabled, valid := s.codexMCPEnabled(path, section.values)
	if !valid {
		return
	}
	headers := tomlStringMap(section.values, "http_headers")
	if len(headers) == 0 {
		headers = tomlStringMap(section.values, "headers")
	}
	name := section.path[1]
	s.addMCPRecord(catalog, MCPServer{
		Name: name, QualifiedName: name, Transport: transport, Inert: true,
		Provenance: []Provenance{s.provenance(scope, path, "", "", enabled, ClassificationDocumented)},
		activationKey: s.captureActivation(foreignactivation.Descriptor{
			ID: name, Transport: transport, Command: tomlString(section.values, "command"), URL: tomlString(section.values, "url"),
			Args: tomlStrings(section.values, "args"), Env: tomlStringMap(section.values, "env"), Headers: headers,
		}),
	}, false)
}

func (s *scanner) codexMCPTransport(path string, values map[string]tomlScalar) (string, bool) {
	if strings.TrimSpace(tomlString(values, "url")) != "" {
		return mcpTransportHTTP, true
	}
	if strings.TrimSpace(tomlString(values, "command")) != "" {
		return mcpTransportStdio, true
	}
	s.addWarning(warning.WarnForeignEntryShapeUnknown, path, "MCP server entry lacks command or URL")
	return "", false
}

func (s *scanner) codexMCPEnabled(path string, values map[string]tomlScalar) (bool, bool) {
	raw, present := values["enabled"]
	if !present {
		return true, true
	}
	return s.tomlBoolean(path, raw)
}

func (s *scanner) scanCodexPluginSetting(path string, section tomlSection, pluginEnabled map[string]bool) {
	if len(section.path) != 2 || section.path[0] != "plugins" {
		return
	}
	raw, present := section.values["enabled"]
	if !present {
		return
	}
	enabled, valid := s.tomlBoolean(path, raw)
	if !valid {
		return
	}
	if _, alreadySet := pluginEnabled[section.path[1]]; !alreadySet {
		pluginEnabled[section.path[1]] = enabled
	}
}

func (s *scanner) tomlBoolean(path string, value tomlScalar) (bool, bool) {
	if value.kind == tomlScalarBoolean {
		return value.value == "true", true
	}
	if value.kind != tomlScalarInvalid {
		s.addWarningLine(warning.WarnForeignTOMLUnsupported, path, value.line, "TOML enabled value must be boolean")
	}
	return false, false
}

type codexMarketplace struct {
	Name    string `json:"name"`
	Plugins []struct {
		Name    string          `json:"name"`
		Version string          `json:"version"`
		Source  json.RawMessage `json:"source"`
	} `json:"plugins"`
}

func (s *scanner) scanCodexMarketplace(ctx context.Context, catalog *HostCatalog, path, root string, scope Scope, classification Classification, pluginEnabled map[string]bool) error {
	data, readErr := s.readFile(ctx, path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil
		}
		s.addReadWarning(readErr, path, warning.WarnForeignIndexUnreadable, "Codex marketplace index is unreadable")
		return nil
	}
	var manifest codexMarketplace
	if json.Unmarshal(data, &manifest) != nil || strings.TrimSpace(manifest.Name) == "" {
		s.addWarning(warning.WarnForeignManifestInvalid, path, "Codex marketplace index is invalid")
		return nil
	}
	sort.Slice(manifest.Plugins, func(i, j int) bool { return manifest.Plugins[i].Name < manifest.Plugins[j].Name })
	marketRoot, rootErr := workspace.NewRoot(root)
	if rootErr != nil {
		s.addWarning(warning.WarnForeignInstallPathMissing, root, "Codex marketplace root is unavailable")
		return nil
	}
	for _, plugin := range manifest.Plugins {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if !s.consumeEntry(path) {
			return nil
		}
		pluginRoot, ok := s.codexLocalPluginRoot(marketRoot, path, plugin.Source)
		if !ok {
			continue
		}
		pluginID := plugin.Name + "@" + manifest.Name
		if err := s.scanCodexPlugin(ctx, catalog, codexPluginInput{
			root: pluginRoot, identifier: pluginID, indexVersion: plugin.Version,
			enabled: pluginEnabled[pluginID], scope: scope, classification: classification,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) codexLocalPluginRoot(root *workspace.Root, sourcePath string, raw json.RawMessage) (string, bool) {
	var source struct {
		Source string `json:"source"`
		Path   string `json:"path"`
	}
	if json.Unmarshal(raw, &source) != nil || source.Source != "local" || !strings.HasPrefix(filepath.ToSlash(source.Path), "./") {
		s.addWarning(warning.WarnForeignEntryShapeUnknown, sourcePath, "Codex plugin source is not a confined local path")
		return "", false
	}
	resolved, err := root.Resolve(filepath.FromSlash(source.Path))
	if err != nil {
		s.addWarning(warning.WarnForeignPathEscape, filepath.Join(root.Dir(), source.Path), "Codex plugin source escapes its marketplace")
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		s.addWarning(warning.WarnForeignInstallPathMissing, resolved, "Codex plugin root is unavailable")
		return "", false
	}
	return resolved, true
}

type codexPluginInput struct {
	root           string
	identifier     string
	indexVersion   string
	enabled        bool
	scope          Scope
	classification Classification
}

func (input codexPluginInput) origin(version string) discoverySource {
	return source(input.scope, input.classification, input.identifier, version, input.enabled)
}

func (s *scanner) scanCodexPlugin(ctx context.Context, catalog *HostCatalog, input codexPluginInput) error {
	manifest, ok := s.loadPluginManifest(ctx, input.root, []string{".codex-plugin/plugin.json"}, true)
	if !ok {
		return nil
	}
	version := manifest.version
	if version == "" {
		version = input.indexVersion
	}
	origin := input.origin(version)
	for _, path := range s.componentPaths(input.root, manifest.fields["skills"], []string{"./skills"}, true) {
		if err := s.scanSkillRoot(ctx, catalog, path, origin); err != nil {
			return err
		}
	}
	for _, path := range s.componentPaths(input.root, manifest.fields["commands"], nil, true) {
		if err := s.scanTemplateRoot(ctx, catalog, path, ".md", origin); err != nil {
			return err
		}
	}
	for _, path := range s.componentPaths(input.root, manifest.fields["mcpServers"], []string{"./.mcp.json"}, true) {
		if err := s.scanCodexPluginMCP(ctx, catalog, path, origin); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) scanCodexPluginMCP(ctx context.Context, catalog *HostCatalog, path string, origin discoverySource) error {
	data, err := s.readFile(ctx, path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.addReadWarning(err, path, warning.WarnForeignIndexUnreadable, "Codex plugin MCP declaration is unreadable")
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Codex plugin MCP declaration shape is invalid")
		return nil
	}
	servers := object
	if raw, found := object["mcp_servers"]; found {
		var wrapped map[string]json.RawMessage
		if json.Unmarshal(raw, &wrapped) != nil || wrapped == nil {
			s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Codex plugin mcp_servers wrapper is invalid")
			return nil
		}
		servers = wrapped
	}
	return s.addMCPMap(ctx, catalog, servers, path, origin)
}

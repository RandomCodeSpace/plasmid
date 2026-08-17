package foreign

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plasmid-dev/plasmid/warning"
)

var copilotManifestOrder = []string{
	".plugin/plugin.json",
	"plugin.json",
	".github/plugin/plugin.json",
	".claude-plugin/plugin.json",
}

// ScanCopilot discovers GitHub Copilot extension metadata without activating it.
func ScanCopilot(ctx context.Context, options Options) (HostCatalog, error) {
	s, err := newScanner(ctx, HostCopilot, options)
	if err != nil {
		return HostCatalog{}, err
	}
	catalog := HostCatalog{Host: HostCopilot}
	for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
		for _, relative := range []string{filepath.Join(".github", "skills"), filepath.Join(".agents", "skills"), filepath.Join(".claude", "skills")} {
			if err := s.scanSkillRoot(&catalog, filepath.Join(directory, relative), ScopeProject, ClassificationDocumented, "", "", true); err != nil {
				return HostCatalog{}, err
			}
		}
	}
	if s.options.HomeDir != "" {
		for _, root := range []string{filepath.Join(s.options.HomeDir, ".copilot", "skills"), filepath.Join(s.options.HomeDir, ".agents", "skills")} {
			if err := s.scanSkillRoot(&catalog, root, ScopeUser, ClassificationDocumented, "", "", true); err != nil {
				return HostCatalog{}, err
			}
		}
	}
	for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
		if err := s.scanTemplateRoot(&catalog, filepath.Join(directory, ".claude", "commands"), ".md", ScopeProject, ClassificationDocumented, "", "", true); err != nil {
			return HostCatalog{}, err
		}
	}
	if s.options.HomeDir != "" {
		if err := s.scanTemplateRoot(&catalog, filepath.Join(s.options.HomeDir, ".claude", "commands"), ".md", ScopeUser, ClassificationDocumented, "", "", true); err != nil {
			return HostCatalog{}, err
		}
	}
	if err := s.scanCopilotPreview(&catalog); err != nil {
		return HostCatalog{}, err
	}
	if err := s.scanCopilotMCP(&catalog); err != nil {
		return HostCatalog{}, err
	}
	if err := s.scanCopilotPlugins(&catalog); err != nil {
		return HostCatalog{}, err
	}
	if err := s.check(); err != nil {
		return HostCatalog{}, err
	}
	return s.finish(catalog), nil
}

func (s *scanner) scanCopilotPreview(catalog *HostCatalog) error {
	for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
		root := filepath.Join(directory, ".github", "prompts")
		entries, err := s.readDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			s.addReadWarning(err, root, warning.WarnForeignIndexUnreadable, "Copilot preview prompt root is unreadable")
			continue
		}
		if !s.options.EnableCopilotPreview {
			for _, entry := range entries {
				if !s.consumeEntry(root) {
					return nil
				}
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".prompt.md") {
					s.addWarning(warning.WarnForeignEcosystemDisabled, root, "Copilot IDE preview prompts are disabled")
					break
				}
			}
			continue
		}
		if err := s.scanTemplateRoot(catalog, root, ".prompt.md", ScopeProject, ClassificationPreview, "", "", true); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) scanCopilotMCP(catalog *HostCatalog) error {
	if s.options.ProjectTrusted {
		for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
			if err := s.scanCopilotMCPFile(catalog, filepath.Join(directory, ".mcp.json"), ScopeProject, ClassificationDocumented, "", "", true, true, false); err != nil {
				return err
			}
			if err := s.scanCopilotMCPFile(catalog, filepath.Join(directory, ".github", "mcp.json"), ScopeProject, ClassificationDocumented, "", "", true, true, false); err != nil {
				return err
			}
		}
	} else {
		for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
			s.warnIfUntrustedFile(filepath.Join(directory, ".mcp.json"))
			s.warnIfUntrustedFile(filepath.Join(directory, ".github", "mcp.json"))
		}
	}
	if s.options.HomeDir != "" {
		return s.scanCopilotMCPFile(catalog, filepath.Join(s.options.HomeDir, ".copilot", "mcp-config.json"), ScopeUser, ClassificationDocumented, "", "", true, false, false)
	}
	return nil
}

func (s *scanner) scanCopilotPlugins(catalog *HostCatalog) error {
	if s.options.HomeDir == "" {
		return nil
	}
	root := filepath.Join(s.options.HomeDir, ".copilot", "installed-plugins")
	groups, err := s.readDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.addReadWarning(err, root, warning.WarnForeignIndexUnreadable, "Copilot installed-plugin root is unreadable")
		return nil
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name() < groups[j].Name() })
	for _, group := range groups {
		if err := s.check(); err != nil {
			return err
		}
		if !s.consumeEntry(root) {
			return nil
		}
		if !group.IsDir() || group.Type()&os.ModeSymlink != 0 {
			continue
		}
		groupRoot := filepath.Join(root, group.Name())
		plugins, readErr := s.readDir(groupRoot)
		if readErr != nil {
			s.addReadWarning(readErr, groupRoot, warning.WarnForeignIndexUnreadable, "Copilot plugin group is unreadable")
			continue
		}
		sort.Slice(plugins, func(i, j int) bool { return plugins[i].Name() < plugins[j].Name() })
		for _, plugin := range plugins {
			if err := s.check(); err != nil {
				return err
			}
			if !s.consumeEntry(groupRoot) {
				return nil
			}
			if !plugin.IsDir() || plugin.Type()&os.ModeSymlink != 0 {
				continue
			}
			if err := s.scanCopilotPlugin(catalog, filepath.Join(groupRoot, plugin.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *scanner) scanCopilotPlugin(catalog *HostCatalog, root string) error {
	manifest, ok := s.loadPluginManifest(root, copilotManifestOrder, true)
	if !ok {
		return nil
	}
	for _, path := range s.componentPaths(root, manifest.fields["skills"], []string{"skills"}, false) {
		if err := s.scanSkillRoot(catalog, path, ScopeUser, ClassificationDocumented, manifest.name, manifest.version, true); err != nil {
			return err
		}
	}
	for _, path := range s.componentPaths(root, manifest.fields["commands"], nil, false) {
		if err := s.scanTemplateRoot(catalog, path, ".md", ScopeUser, ClassificationDocumented, manifest.name, manifest.version, true); err != nil {
			return err
		}
	}
	raw := manifest.fields["mcpServers"]
	if len(raw) == 0 {
		return nil
	}
	var inline map[string]json.RawMessage
	if json.Unmarshal(raw, &inline) == nil && inline != nil {
		return s.addMCPMapReplacing(catalog, inline, manifest.path, ScopeUser, ClassificationDocumented, manifest.name, manifest.version, true)
	}
	for _, path := range s.componentPaths(root, raw, nil, false) {
		if err := s.scanCopilotMCPFile(catalog, path, ScopeUser, ClassificationDocumented, manifest.name, manifest.version, true, true, true); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) scanCopilotMCPFile(catalog *HostCatalog, path string, scope Scope, classification Classification, pluginID, pluginVersion string, enabled, allowBare, replace bool) error {
	data, err := s.readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.addReadWarning(err, path, warning.WarnForeignIndexUnreadable, "Copilot MCP declaration is unreadable")
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Copilot MCP declaration shape is invalid")
		return nil
	}
	var servers map[string]json.RawMessage
	if raw, found := object["mcpServers"]; found {
		if json.Unmarshal(raw, &servers) != nil || servers == nil {
			s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Copilot mcpServers wrapper is invalid")
			return nil
		}
	} else if allowBare {
		servers = object
	} else {
		s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Copilot user MCP declaration requires an mcpServers object")
		return nil
	}
	if replace {
		return s.addMCPMapReplacing(catalog, servers, path, scope, classification, pluginID, pluginVersion, enabled)
	}
	return s.addMCPMap(catalog, servers, path, scope, classification, pluginID, pluginVersion, enabled)
}

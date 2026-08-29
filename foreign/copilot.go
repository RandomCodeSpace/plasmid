package foreign

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RandomCodeSpace/plasmid/internal/foreignactivation"
	"github.com/RandomCodeSpace/plasmid/warning"
)

var copilotManifestOrder = []string{
	".plugin/plugin.json",
	pluginManifestName,
	".github/plugin/plugin.json",
	".claude-plugin/plugin.json",
}

// ScanCopilot discovers GitHub Copilot extension metadata without activating it.
func ScanCopilot(ctx context.Context, options Options) (HostCatalog, error) {
	return ScanCopilotWithActivations(ctx, options, nil)
}

// ScanCopilotWithActivations transfers runtime descriptors into an internal
// capability while keeping the returned normalized catalog secret-free.
func ScanCopilotWithActivations(ctx context.Context, options Options, vault *foreignactivation.Vault) (HostCatalog, error) {
	s, err := newScanner(ctx, HostCopilot, options)
	if err != nil {
		return HostCatalog{}, err
	}
	s.activationVault = vault
	catalog := HostCatalog{Host: HostCopilot}
	steps := make([]func(context.Context) error, 0)
	for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
		for _, relative := range []string{filepath.Join(githubDirectory, "skills"), filepath.Join(agentsDirectory, "skills"), filepath.Join(claudeDirectory, "skills")} {
			steps = append(steps, func(scanCtx context.Context) error {
				return s.scanSkillRoot(scanCtx, &catalog, filepath.Join(directory, relative), source(ScopeProject, ClassificationDocumented, "", "", true))
			})
		}
	}
	if s.options.HomeDir != "" {
		for _, root := range []string{filepath.Join(s.options.HomeDir, copilotDirectory, "skills"), filepath.Join(s.options.HomeDir, agentsDirectory, "skills")} {
			steps = append(steps, func(scanCtx context.Context) error {
				return s.scanSkillRoot(scanCtx, &catalog, root, source(ScopeUser, ClassificationDocumented, "", "", true))
			})
		}
	}
	for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
		steps = append(steps, func(scanCtx context.Context) error {
			return s.scanTemplateRoot(scanCtx, &catalog, filepath.Join(directory, claudeDirectory, "commands"), ".md", source(ScopeProject, ClassificationDocumented, "", "", true))
		})
	}
	if s.options.HomeDir != "" {
		steps = append(steps, func(scanCtx context.Context) error {
			return s.scanTemplateRoot(scanCtx, &catalog, filepath.Join(s.options.HomeDir, claudeDirectory, "commands"), ".md", source(ScopeUser, ClassificationDocumented, "", "", true))
		})
	}
	steps = append(steps,
		func(scanCtx context.Context) error { return s.scanCopilotPreview(scanCtx, &catalog) },
		func(scanCtx context.Context) error { return s.scanCopilotMCP(scanCtx, &catalog) },
		func(scanCtx context.Context) error { return s.scanCopilotPlugins(scanCtx, &catalog) },
		checkContext,
	)
	if err := runScannerSteps(ctx, steps...); err != nil {
		return HostCatalog{}, err
	}
	return s.finish(catalog), nil
}

func (s *scanner) scanCopilotPreview(ctx context.Context, catalog *HostCatalog) error {
	for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
		root := filepath.Join(directory, githubDirectory, "prompts")
		if err := s.scanCopilotPreviewRoot(ctx, catalog, root); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) scanCopilotPreviewRoot(ctx context.Context, catalog *HostCatalog, root string) error {
	entries, err := s.readDir(ctx, root)
	if err != nil {
		if !os.IsNotExist(err) {
			s.addReadWarning(err, root, warning.WarnForeignIndexUnreadable, "Copilot preview prompt root is unreadable")
		}
		return nil
	}
	if s.options.EnableCopilotPreview {
		return s.scanTemplateRoot(ctx, catalog, root, ".prompt.md", source(ScopeProject, ClassificationPreview, "", "", true))
	}
	s.warnCopilotPreviewDisabled(root, entries)
	return nil
}

func (s *scanner) warnCopilotPreviewDisabled(root string, entries []os.DirEntry) {
	for _, entry := range entries {
		if !s.consumeEntry(root) {
			return
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".prompt.md") {
			s.addWarning(warning.WarnForeignEcosystemDisabled, root, "Copilot IDE preview prompts are disabled")
			return
		}
	}
}

func (s *scanner) scanCopilotMCP(ctx context.Context, catalog *HostCatalog) error {
	if s.options.ProjectTrusted {
		for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
			if err := s.scanCopilotMCPFile(ctx, catalog, filepath.Join(directory, ".mcp.json"), source(ScopeProject, ClassificationDocumented, "", "", true), copilotMCPOptions{allowBare: true}); err != nil {
				return err
			}
			if err := s.scanCopilotMCPFile(ctx, catalog, filepath.Join(directory, githubDirectory, "mcp.json"), source(ScopeProject, ClassificationDocumented, "", "", true), copilotMCPOptions{allowBare: true}); err != nil {
				return err
			}
		}
	} else {
		for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
			s.warnIfUntrustedFile(ctx, filepath.Join(directory, ".mcp.json"))
			s.warnIfUntrustedFile(ctx, filepath.Join(directory, githubDirectory, "mcp.json"))
		}
	}
	if s.options.HomeDir != "" {
		return s.scanCopilotMCPFile(ctx, catalog, filepath.Join(s.options.HomeDir, copilotDirectory, "mcp-config.json"), source(ScopeUser, ClassificationDocumented, "", "", true), copilotMCPOptions{})
	}
	return nil
}

func (s *scanner) scanCopilotPlugins(ctx context.Context, catalog *HostCatalog) error {
	if s.options.HomeDir == "" {
		return nil
	}
	root := filepath.Join(s.options.HomeDir, copilotDirectory, "installed-plugins")
	groups, err := s.readDir(ctx, root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.addReadWarning(err, root, warning.WarnForeignIndexUnreadable, "Copilot installed-plugin root is unreadable")
		return nil
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name() < groups[j].Name() })
	for _, group := range groups {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if !s.consumeEntry(root) {
			return nil
		}
		if !group.IsDir() || group.Type()&os.ModeSymlink != 0 {
			continue
		}
		if err := s.scanCopilotPluginGroup(ctx, catalog, filepath.Join(root, group.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) scanCopilotPluginGroup(ctx context.Context, catalog *HostCatalog, root string) error {
	plugins, err := s.readDir(ctx, root)
	if err != nil {
		s.addReadWarning(err, root, warning.WarnForeignIndexUnreadable, "Copilot plugin group is unreadable")
		return nil
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Name() < plugins[j].Name() })
	for _, plugin := range plugins {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if !s.consumeEntry(root) {
			return nil
		}
		if plugin.IsDir() && plugin.Type()&os.ModeSymlink == 0 {
			if err := s.scanCopilotPlugin(ctx, catalog, filepath.Join(root, plugin.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *scanner) scanCopilotPlugin(ctx context.Context, catalog *HostCatalog, root string) error {
	manifest, ok := s.loadPluginManifest(ctx, root, copilotManifestOrder, true)
	if !ok {
		return nil
	}
	for _, path := range s.componentPaths(root, manifest.fields["skills"], []string{"skills"}, false) {
		if err := s.scanSkillRoot(ctx, catalog, path, source(ScopeUser, ClassificationDocumented, manifest.name, manifest.version, true)); err != nil {
			return err
		}
	}
	for _, path := range s.componentPaths(root, manifest.fields["commands"], nil, false) {
		if err := s.scanTemplateRoot(ctx, catalog, path, ".md", source(ScopeUser, ClassificationDocumented, manifest.name, manifest.version, true)); err != nil {
			return err
		}
	}
	raw := manifest.fields["mcpServers"]
	if len(raw) == 0 {
		return nil
	}
	var inline map[string]json.RawMessage
	if json.Unmarshal(raw, &inline) == nil && inline != nil {
		return s.addMCPMapReplacing(ctx, catalog, inline, manifest.path, source(ScopeUser, ClassificationDocumented, manifest.name, manifest.version, true))
	}
	for _, path := range s.componentPaths(root, raw, nil, false) {
		if err := s.scanCopilotMCPFile(ctx, catalog, path, source(ScopeUser, ClassificationDocumented, manifest.name, manifest.version, true), copilotMCPOptions{allowBare: true, replace: true}); err != nil {
			return err
		}
	}
	return nil
}

type copilotMCPOptions struct {
	allowBare bool
	replace   bool
}

func (s *scanner) scanCopilotMCPFile(ctx context.Context, catalog *HostCatalog, path string, origin discoverySource, options copilotMCPOptions) error {
	data, err := s.readFile(ctx, path)
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
	} else if options.allowBare {
		servers = object
	} else {
		s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Copilot user MCP declaration requires an mcpServers object")
		return nil
	}
	if options.replace {
		return s.addMCPMapReplacing(ctx, catalog, servers, path, origin)
	}
	return s.addMCPMap(ctx, catalog, servers, path, origin)
}

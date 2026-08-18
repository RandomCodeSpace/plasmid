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

// ScanClaude discovers Claude Code extensions without activating them.
func ScanClaude(ctx context.Context, options Options) (HostCatalog, error) {
	return ScanClaudeWithActivations(ctx, options, nil)
}

// ScanClaudeWithActivations transfers runtime descriptors into an internal
// capability while keeping the returned normalized catalog secret-free.
func ScanClaudeWithActivations(ctx context.Context, options Options, vault *foreignactivation.Vault) (HostCatalog, error) {
	scanner, err := newScanner(ctx, HostClaude, options)
	if err != nil {
		return HostCatalog{}, err
	}
	scanner.activationVault = vault
	catalog := HostCatalog{Host: HostClaude}
	steps := make([]func(context.Context) error, 0)
	for _, directory := range ancestorDirectories(scanner.options.WorkingDir, scanner.options.RepositoryRoot) {
		steps = append(steps, func(scanCtx context.Context) error {
			return scanner.scanSkillRoot(scanCtx, &catalog, filepath.Join(directory, ".claude", "skills"), source(ScopeProject, ClassificationDocumented, "", "", true))
		})
	}
	if scanner.options.HomeDir != "" {
		steps = append(steps, func(scanCtx context.Context) error {
			return scanner.scanSkillRoot(scanCtx, &catalog, filepath.Join(scanner.options.HomeDir, ".claude", "skills"), source(ScopeUser, ClassificationDocumented, "", "", true))
		})
	}
	steps = append(steps, func(scanCtx context.Context) error {
		return scanner.scanTemplateRoot(scanCtx, &catalog, filepath.Join(scanner.options.RepositoryRoot, ".claude", "commands"), ".md", source(ScopeProject, ClassificationDocumented, "", "", true))
	})
	if scanner.options.HomeDir != "" {
		steps = append(steps, func(scanCtx context.Context) error {
			return scanner.scanTemplateRoot(scanCtx, &catalog, filepath.Join(scanner.options.HomeDir, ".claude", "commands"), ".md", source(ScopeUser, ClassificationDocumented, "", "", true))
		})
	}
	steps = append(steps,
		func(scanCtx context.Context) error { return scanner.scanClaudeMCP(scanCtx, &catalog) },
		func(scanCtx context.Context) error { return scanner.scanClaudePlugins(scanCtx, &catalog) },
		checkContext,
	)
	if err := runScannerSteps(ctx, steps...); err != nil {
		return HostCatalog{}, err
	}
	return scanner.finish(catalog), nil
}

type claudeInstall struct {
	Scope       string `json:"scope"`
	InstallPath string `json:"installPath"`
	Version     string `json:"version"`
	LastUpdated string `json:"lastUpdated"`
}

type claudePluginIndex struct {
	Version int                        `json:"version"`
	Plugins map[string][]claudeInstall `json:"plugins"`
}

func (s *scanner) scanClaudePlugins(ctx context.Context, catalog *HostCatalog) error {
	if s.options.HomeDir == "" {
		return nil
	}
	pluginsRoot := filepath.Join(s.options.HomeDir, ".claude", "plugins")
	path := filepath.Join(pluginsRoot, "installed_plugins.json")
	index, ok := s.loadClaudePluginIndex(ctx, path)
	if !ok {
		return nil
	}
	confinedPlugins, err := workspace.NewRoot(pluginsRoot)
	if err != nil {
		s.addWarning(warning.WarnForeignIndexUnreadable, pluginsRoot, "Claude plugin root is unavailable")
		return nil
	}
	return s.scanClaudePluginIndex(ctx, catalog, path, confinedPlugins, index)
}

func (s *scanner) loadClaudePluginIndex(ctx context.Context, path string) (claudePluginIndex, bool) {
	data, err := s.readFile(ctx, path)
	if err != nil {
		if os.IsNotExist(err) {
			return claudePluginIndex{}, false
		}
		s.addReadWarning(err, path, warning.WarnForeignIndexUnreadable, "Claude plugin index is unreadable")
		return claudePluginIndex{}, false
	}
	var index claudePluginIndex
	if json.Unmarshal(data, &index) != nil || index.Plugins == nil {
		s.addWarning(warning.WarnForeignEntryShapeUnknown, path, "Claude plugin index shape is invalid")
		return claudePluginIndex{}, false
	}
	if index.Version != 2 {
		s.addWarning(warning.WarnForeignIndexUnsupportedVersion, path, "Claude plugin index version is unsupported")
		return claudePluginIndex{}, false
	}
	return index, true
}

func (s *scanner) scanClaudePluginIndex(ctx context.Context, catalog *HostCatalog, path string, confinedPlugins *workspace.Root, index claudePluginIndex) error {
	enabled := s.claudeEnabledPlugins(ctx)
	identifiers := make([]string, 0, len(index.Plugins))
	for identifier := range index.Plugins {
		identifiers = append(identifiers, identifier)
	}
	sort.Strings(identifiers)
	for _, identifier := range identifiers {
		for _, entry := range sortClaudeInstalls(index.Plugins[identifier]) {
			if err := checkContext(ctx); err != nil {
				return err
			}
			if !s.consumeEntry(path) {
				return nil
			}
			if err := s.scanClaudePluginInstall(ctx, catalog, path, confinedPlugins, identifier, entry, enabled[identifier]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *scanner) scanClaudePluginInstall(ctx context.Context, catalog *HostCatalog, indexPath string, confinedPlugins *workspace.Root, identifier string, entry claudeInstall, enabled bool) error {
	root, ok := s.claudePluginRoot(indexPath, confinedPlugins, entry.InstallPath)
	if !ok {
		return nil
	}
	manifest, hasManifest := s.loadPluginManifest(ctx, root, []string{".claude-plugin/plugin.json"}, false)
	version := entry.Version
	if hasManifest && version == "" {
		version = manifest.version
	}
	origin := source(claudeScope(entry.Scope), ClassificationCompatibility, identifier, version, enabled)
	if err := s.scanClaudePluginComponents(ctx, catalog, root, manifest, hasManifest, origin); err != nil {
		return err
	}
	return s.scanClaudePluginMCP(ctx, catalog, root, manifest, hasManifest, origin)
}

func (s *scanner) claudePluginRoot(indexPath string, confinedPlugins *workspace.Root, installPath string) (string, bool) {
	if !filepath.IsAbs(installPath) {
		s.addWarning(warning.WarnForeignInstallPathRelative, indexPath, "Claude plugin install path is relative")
		return "", false
	}
	confinedPath, err := confinedPlugins.Resolve(installPath)
	if err != nil {
		s.addWarning(warning.WarnForeignPathEscape, installPath, "Claude plugin install path escapes its root")
		return "", false
	}
	root, err := canonicalDirectory(confinedPath)
	if err != nil {
		s.addWarning(warning.WarnForeignInstallPathMissing, installPath, "Claude plugin install path is unavailable")
		return "", false
	}
	return root, true
}

func (s *scanner) scanClaudePluginComponents(ctx context.Context, catalog *HostCatalog, root string, manifest pluginManifest, hasManifest bool, origin discoverySource) error {
	var skillsRaw, commandsRaw json.RawMessage
	if hasManifest {
		skillsRaw = manifest.fields["skills"]
		commandsRaw = manifest.fields["commands"]
	}
	steps := make([]func(context.Context) error, 0)
	for _, path := range s.componentPaths(root, skillsRaw, []string{"skills"}, false) {
		steps = append(steps, func(scanCtx context.Context) error { return s.scanSkillRoot(scanCtx, catalog, path, origin) })
	}
	for _, path := range s.componentPaths(root, commandsRaw, []string{"commands"}, false) {
		steps = append(steps, func(scanCtx context.Context) error { return s.scanTemplateRoot(scanCtx, catalog, path, ".md", origin) })
	}
	return runScannerSteps(ctx, steps...)
}

func (s *scanner) scanClaudePluginMCP(ctx context.Context, catalog *HostCatalog, root string, manifest pluginManifest, hasManifest bool, origin discoverySource) error {
	if !hasManifest || len(manifest.fields["mcpServers"]) == 0 {
		return s.scanClaudeMCPFile(ctx, catalog, filepath.Join(root, ".mcp.json"), origin)
	}
	raw := manifest.fields["mcpServers"]
	var inline map[string]json.RawMessage
	if json.Unmarshal(raw, &inline) == nil && inline != nil {
		return s.addMCPMap(ctx, catalog, inline, manifest.path, origin)
	}
	steps := make([]func(context.Context) error, 0)
	for _, path := range s.componentPaths(root, raw, nil, false) {
		steps = append(steps, func(scanCtx context.Context) error {
			return s.scanClaudeMCPFile(scanCtx, catalog, path, origin)
		})
	}
	return runScannerSteps(ctx, steps...)
}

func claudeScopeRank(value string) int {
	switch claudeScope(value) {
	case ScopeLocal:
		return 0
	case ScopeProject:
		return 1
	default:
		return 2
	}
}

type claudeInstallSortKey struct {
	install      claudeInstall
	scopeRank    int
	version      []string
	versionValid bool
}

func sortClaudeInstalls(installs []claudeInstall) []claudeInstall {
	keys := make([]claudeInstallSortKey, 0, len(installs))
	for _, install := range installs {
		version, valid := parseClaudeNumericVersion(install.Version)
		keys = append(keys, claudeInstallSortKey{install: install, scopeRank: claudeScopeRank(install.Scope), version: version, versionValid: valid})
	}
	sort.SliceStable(keys, func(i, j int) bool {
		left, right := keys[i], keys[j]
		if left.scopeRank != right.scopeRank {
			return left.scopeRank < right.scopeRank
		}
		if left.install.LastUpdated != right.install.LastUpdated {
			return left.install.LastUpdated > right.install.LastUpdated
		}
		if left.versionValid != right.versionValid {
			return left.versionValid
		}
		if left.versionValid {
			if comparison := compareClaudeVersionParts(left.version, right.version); comparison != 0 {
				return comparison > 0
			}
		}
		return left.install.InstallPath < right.install.InstallPath
	})
	result := make([]claudeInstall, len(keys))
	for index, key := range keys {
		result[index] = key.install
	}
	return result
}

func compareClaudeVersionParts(left, right []string) int {
	length := max(len(left), len(right))
	for index := 0; index < length; index++ {
		leftPart, rightPart := "0", "0"
		if index < len(left) {
			leftPart = left[index]
		}
		if index < len(right) {
			rightPart = right[index]
		}
		if comparison := compareNumericText(leftPart, rightPart); comparison != 0 {
			return comparison
		}
	}
	return 0
}

func parseClaudeNumericVersion(value string) ([]string, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	if value == "" || strings.ContainsAny(value, "+-") {
		return nil, false
	}
	parts := strings.Split(value, ".")
	for _, part := range parts {
		if !numericText(part) {
			return nil, false
		}
	}
	return parts, true
}

func numericText(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func compareNumericText(left, right string) int {
	left = strings.TrimLeft(left, "0")
	right = strings.TrimLeft(right, "0")
	if left == "" {
		left = "0"
	}
	if right == "" {
		right = "0"
	}
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return strings.Compare(left, right)
}

func (s *scanner) claudeEnabledPlugins(ctx context.Context) map[string]bool {
	result := make(map[string]bool)
	paths := []string{filepath.Join(s.options.HomeDir, ".claude", "settings.json")}
	for _, directory := range reverseStrings(ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot)) {
		paths = append(paths, filepath.Join(directory, ".claude", "settings.json"), filepath.Join(directory, ".claude", "settings.local.json"))
	}
	for _, path := range paths {
		data, err := s.readFile(ctx, path)
		if err != nil {
			if !os.IsNotExist(err) && checkContext(ctx) == nil {
				s.addReadWarning(err, path, warning.WarnForeignIndexUnreadable, "Claude settings are unreadable")
			}
			continue
		}
		var settings struct {
			Enabled map[string]bool `json:"enabledPlugins"`
		}
		if json.Unmarshal(data, &settings) != nil {
			s.addWarning(warning.WarnForeignEntryShapeUnknown, path, "Claude settings shape is invalid")
			continue
		}
		for identifier, value := range settings.Enabled {
			result[identifier] = value
		}
	}
	return result
}

func (s *scanner) scanClaudeMCP(ctx context.Context, catalog *HostCatalog) error {
	path := filepath.Join(s.options.HomeDir, ".claude.json")
	root, err := s.loadClaudeMCPRoot(ctx, path)
	if err != nil {
		return err
	}
	return runScannerSteps(ctx,
		func(scanCtx context.Context) error { return s.scanClaudeLocalMCP(scanCtx, catalog, path, root) },
		func(scanCtx context.Context) error { return s.scanClaudeProjectMCP(scanCtx, catalog) },
		func(scanCtx context.Context) error { return s.scanClaudeUserMCP(scanCtx, catalog, path, root) },
	)
}

func (s *scanner) loadClaudeMCPRoot(ctx context.Context, path string) (map[string]json.RawMessage, error) {
	if s.options.HomeDir == "" {
		return nil, nil
	}
	data, err := s.readFile(ctx, path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		if contextErr := checkContext(ctx); contextErr != nil {
			return nil, contextErr
		}
		s.addReadWarning(err, path, warning.WarnForeignIndexUnreadable, "Claude MCP configuration is unreadable")
		return nil, nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Claude MCP configuration shape is invalid")
		return nil, nil
	}
	return root, nil
}

func (s *scanner) scanClaudeLocalMCP(ctx context.Context, catalog *HostCatalog, path string, root map[string]json.RawMessage) error {
	var projects map[string]json.RawMessage
	if raw := root["projects"]; len(raw) != 0 && json.Unmarshal(raw, &projects) != nil {
		s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Claude project MCP map is invalid")
		return nil
	}
	for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
		if err := s.scanClaudeLocalProject(ctx, catalog, path, projects, directory); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) scanClaudeLocalProject(ctx context.Context, catalog *HostCatalog, path string, projects map[string]json.RawMessage, directory string) error {
	raw, present := projects[directory]
	if !present {
		raw, present = projects[filepath.ToSlash(directory)]
	}
	if !present {
		return nil
	}
	var project map[string]json.RawMessage
	if json.Unmarshal(raw, &project) != nil || project == nil {
		s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Claude local project entry is invalid")
		return nil
	}
	servers, found, valid := claudeMCPServers(project)
	if found && !valid {
		s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Claude local MCP wrapper is invalid")
		return nil
	}
	if !found {
		return nil
	}
	return s.addMCPMap(ctx, catalog, servers, path, source(ScopeLocal, ClassificationDocumented, "", "", true))
}

func (s *scanner) scanClaudeProjectMCP(ctx context.Context, catalog *HostCatalog) error {
	path := filepath.Join(s.options.RepositoryRoot, ".mcp.json")
	if !s.options.ProjectTrusted {
		s.warnIfUntrustedFile(ctx, path)
		return nil
	}
	return s.scanClaudeMCPFile(ctx, catalog, path, source(ScopeProject, ClassificationDocumented, "", "", true))
}

func (s *scanner) scanClaudeUserMCP(ctx context.Context, catalog *HostCatalog, path string, root map[string]json.RawMessage) error {
	servers, found, valid := claudeMCPServers(root)
	if found && !valid {
		s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Claude user MCP wrapper is invalid")
		return nil
	}
	if !found {
		return nil
	}
	return s.addMCPMap(ctx, catalog, servers, path, source(ScopeUser, ClassificationDocumented, "", "", true))
}

func (s *scanner) scanClaudeMCPFile(ctx context.Context, catalog *HostCatalog, path string, origin discoverySource) error {
	data, err := s.readFile(ctx, path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.addReadWarning(err, path, warning.WarnForeignIndexUnreadable, "Claude MCP declaration is unreadable")
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Claude MCP declaration shape is invalid")
		return nil
	}
	servers, found, valid := claudeMCPServers(object)
	if !found || !valid {
		s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Claude MCP declaration requires an mcpServers object")
		return nil
	}
	return s.addMCPMap(ctx, catalog, servers, path, origin)
}

func claudeMCPServers(object map[string]json.RawMessage) (map[string]json.RawMessage, bool, bool) {
	raw, found := object["mcpServers"]
	if !found {
		return nil, false, false
	}
	var servers map[string]json.RawMessage
	if json.Unmarshal(raw, &servers) != nil || servers == nil {
		return nil, true, false
	}
	return servers, true, true
}

func claudeScope(value string) Scope {
	switch strings.ToLower(value) {
	case "local":
		return ScopeLocal
	case "project":
		return ScopeProject
	default:
		return ScopeUser
	}
}

func reverseStrings(values []string) []string {
	result := append([]string(nil), values...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}

func ancestorDirectories(workingDir, repositoryRoot string) []string {
	var result []string
	for current := workingDir; ; current = filepath.Dir(current) {
		result = append(result, current)
		if current == repositoryRoot {
			return result
		}
	}
}

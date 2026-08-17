package foreign

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

// ScanClaude discovers Claude Code extensions without activating them.
func ScanClaude(ctx context.Context, options Options) (HostCatalog, error) {
	scanner, err := newScanner(ctx, HostClaude, options)
	if err != nil {
		return HostCatalog{}, err
	}
	catalog := HostCatalog{Host: HostClaude}
	for _, directory := range ancestorDirectories(scanner.options.WorkingDir, scanner.options.RepositoryRoot) {
		if err := scanner.scanSkillRoot(&catalog, filepath.Join(directory, ".claude", "skills"), ScopeProject, ClassificationDocumented, "", "", true); err != nil {
			return HostCatalog{}, err
		}
	}
	if scanner.options.HomeDir != "" {
		if err := scanner.scanSkillRoot(&catalog, filepath.Join(scanner.options.HomeDir, ".claude", "skills"), ScopeUser, ClassificationDocumented, "", "", true); err != nil {
			return HostCatalog{}, err
		}
	}
	if err := scanner.scanTemplateRoot(&catalog, filepath.Join(scanner.options.RepositoryRoot, ".claude", "commands"), ".md", ScopeProject, ClassificationDocumented, "", "", true); err != nil {
		return HostCatalog{}, err
	}
	if scanner.options.HomeDir != "" {
		if err := scanner.scanTemplateRoot(&catalog, filepath.Join(scanner.options.HomeDir, ".claude", "commands"), ".md", ScopeUser, ClassificationDocumented, "", "", true); err != nil {
			return HostCatalog{}, err
		}
	}
	if err := scanner.scanClaudeMCP(&catalog); err != nil {
		return HostCatalog{}, err
	}
	if err := scanner.scanClaudePlugins(&catalog); err != nil {
		return HostCatalog{}, err
	}
	if err := scanner.check(); err != nil {
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

func (s *scanner) scanClaudePlugins(catalog *HostCatalog) error {
	if s.options.HomeDir == "" {
		return nil
	}
	pluginsRoot := filepath.Join(s.options.HomeDir, ".claude", "plugins")
	path := filepath.Join(pluginsRoot, "installed_plugins.json")
	data, err := s.readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.addReadWarning(err, path, warning.WarnForeignIndexUnreadable, "Claude plugin index is unreadable")
		return nil
	}
	var index struct {
		Version int                        `json:"version"`
		Plugins map[string][]claudeInstall `json:"plugins"`
	}
	if json.Unmarshal(data, &index) != nil || index.Plugins == nil {
		s.addWarning(warning.WarnForeignEntryShapeUnknown, path, "Claude plugin index shape is invalid")
		return nil
	}
	if index.Version != 2 {
		s.addWarning(warning.WarnForeignIndexUnsupportedVersion, path, "Claude plugin index version is unsupported")
		return nil
	}
	confinedPlugins, confinementErr := workspace.NewRoot(pluginsRoot)
	if confinementErr != nil {
		s.addWarning(warning.WarnForeignIndexUnreadable, pluginsRoot, "Claude plugin root is unavailable")
		return nil
	}
	enabled := s.claudeEnabledPlugins()
	identifiers := make([]string, 0, len(index.Plugins))
	for identifier := range index.Plugins {
		identifiers = append(identifiers, identifier)
	}
	sort.Strings(identifiers)
	for _, identifier := range identifiers {
		entries := sortClaudeInstalls(index.Plugins[identifier])
		for _, entry := range entries {
			if err := s.check(); err != nil {
				return err
			}
			if !s.consumeEntry(path) {
				return nil
			}
			if !filepath.IsAbs(entry.InstallPath) {
				s.addWarning(warning.WarnForeignInstallPathRelative, path, "Claude plugin install path is relative")
				continue
			}
			confinedPath, resolveErr := confinedPlugins.Resolve(entry.InstallPath)
			if resolveErr != nil {
				s.addWarning(warning.WarnForeignPathEscape, entry.InstallPath, "Claude plugin install path escapes its root")
				continue
			}
			root, rootErr := canonicalDirectory(confinedPath)
			if rootErr != nil {
				s.addWarning(warning.WarnForeignInstallPathMissing, entry.InstallPath, "Claude plugin install path is unavailable")
				continue
			}
			scope := claudeScope(entry.Scope)
			manifest, hasManifest := s.loadPluginManifest(root, []string{".claude-plugin/plugin.json"}, false)
			pluginID := identifier
			version := entry.Version
			if hasManifest {
				if manifest.name != "" {
					pluginID = identifier
				}
				if version == "" {
					version = manifest.version
				}
			}
			var skillsRaw, commandsRaw, mcpRaw json.RawMessage
			if hasManifest {
				skillsRaw = manifest.fields["skills"]
				commandsRaw = manifest.fields["commands"]
				mcpRaw = manifest.fields["mcpServers"]
			}
			for _, skills := range s.componentPaths(root, skillsRaw, []string{"skills"}, false) {
				if err := s.scanSkillRoot(catalog, skills, scope, ClassificationCompatibility, pluginID, version, enabled[identifier]); err != nil {
					return err
				}
			}
			for _, commands := range s.componentPaths(root, commandsRaw, []string{"commands"}, false) {
				if err := s.scanTemplateRoot(catalog, commands, ".md", scope, ClassificationCompatibility, pluginID, version, enabled[identifier]); err != nil {
					return err
				}
			}
			if len(mcpRaw) != 0 && hasManifest {
				var inline map[string]json.RawMessage
				if json.Unmarshal(mcpRaw, &inline) == nil && inline != nil {
					if err := s.addMCPMap(catalog, inline, manifest.path, scope, ClassificationCompatibility, pluginID, version, enabled[identifier]); err != nil {
						return err
					}
				} else {
					for _, mcpPath := range s.componentPaths(root, mcpRaw, nil, false) {
						if err := s.scanClaudeMCPFile(catalog, mcpPath, scope, ClassificationCompatibility, pluginID, version, enabled[identifier]); err != nil {
							return err
						}
					}
				}
			} else if err := s.scanClaudeMCPFile(catalog, filepath.Join(root, ".mcp.json"), scope, ClassificationCompatibility, pluginID, version, enabled[identifier]); err != nil {
				return err
			}
		}
	}
	return nil
}

func claudeScopeRank(value string) int {
	switch claudeScope(value) {
	case ScopeLocal:
		return 0
	case ScopeProject:
		return 1
	case ScopeUser:
		return 2
	default:
		return 3
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

func (s *scanner) claudeEnabledPlugins() map[string]bool {
	result := make(map[string]bool)
	paths := []string{filepath.Join(s.options.HomeDir, ".claude", "settings.json")}
	for _, directory := range reverseStrings(ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot)) {
		paths = append(paths, filepath.Join(directory, ".claude", "settings.json"), filepath.Join(directory, ".claude", "settings.local.json"))
	}
	for _, path := range paths {
		data, err := s.readFile(path)
		if err != nil {
			if !os.IsNotExist(err) && s.check() == nil {
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

func (s *scanner) scanClaudeMCP(catalog *HostCatalog) error {
	path := filepath.Join(s.options.HomeDir, ".claude.json")
	var root map[string]json.RawMessage
	if s.options.HomeDir != "" {
		data, err := s.readFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				if contextErr := s.check(); contextErr != nil {
					return contextErr
				}
				s.addReadWarning(err, path, warning.WarnForeignIndexUnreadable, "Claude MCP configuration is unreadable")
			}
		} else if json.Unmarshal(data, &root) != nil || root == nil {
			s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Claude MCP configuration shape is invalid")
			root = nil
		}
	}
	if root != nil {
		var projects map[string]json.RawMessage
		if raw := root["projects"]; len(raw) != 0 && json.Unmarshal(raw, &projects) != nil {
			s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Claude project MCP map is invalid")
		}
		for _, directory := range ancestorDirectories(s.options.WorkingDir, s.options.RepositoryRoot) {
			raw, present := projects[directory]
			if !present {
				raw, present = projects[filepath.ToSlash(directory)]
			}
			if !present {
				continue
			}
			var project map[string]json.RawMessage
			if json.Unmarshal(raw, &project) != nil || project == nil {
				s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Claude local project entry is invalid")
				continue
			}
			servers, found, valid := claudeMCPServers(project)
			if found && !valid {
				s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Claude local MCP wrapper is invalid")
				continue
			}
			if found {
				if err := s.addMCPMap(catalog, servers, path, ScopeLocal, ClassificationDocumented, "", "", true); err != nil {
					return err
				}
			}
		}
	}
	projectMCP := filepath.Join(s.options.RepositoryRoot, ".mcp.json")
	if s.options.ProjectTrusted {
		if err := s.scanClaudeMCPFile(catalog, projectMCP, ScopeProject, ClassificationDocumented, "", "", true); err != nil {
			return err
		}
	} else {
		s.warnIfUntrustedFile(projectMCP)
	}
	if root != nil {
		servers, found, valid := claudeMCPServers(root)
		if found && !valid {
			s.addWarning(warning.WarnForeignMCPShapeUnknown, path, "Claude user MCP wrapper is invalid")
		} else if found {
			if err := s.addMCPMap(catalog, servers, path, ScopeUser, ClassificationDocumented, "", "", true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *scanner) scanClaudeMCPFile(catalog *HostCatalog, path string, scope Scope, classification Classification, pluginID, pluginVersion string, enabled bool) error {
	data, err := s.readFile(path)
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
	return s.addMCPMap(catalog, servers, path, scope, classification, pluginID, pluginVersion, enabled)
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
		parent := filepath.Dir(current)
		if parent == current {
			return result
		}
	}
}

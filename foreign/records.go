package foreign

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plasmid-dev/plasmid/warning"
)

func (s *scanner) scanTemplateRoot(catalog *HostCatalog, root, suffix string, scope Scope, classification Classification, pluginID, pluginVersion string, enabled bool) error {
	if err := s.check(); err != nil {
		return err
	}
	if s.truncated {
		return nil
	}
	entries, err := s.readDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.addReadWarning(err, root, warning.WarnForeignIndexUnreadable, "template root is unreadable")
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := s.check(); err != nil {
			return err
		}
		if !s.consumeEntry(root) {
			return nil
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		data, readErr := s.readFile(path)
		if readErr != nil {
			s.addReadWarning(readErr, path, warning.WarnForeignIndexUnreadable, "template is unreadable")
			continue
		}
		name := strings.TrimSuffix(entry.Name(), suffix)
		record := Template{
			Name: name, QualifiedName: qualify(pluginID, name), Body: string(data),
			Provenance: []Provenance{s.provenance(scope, path, pluginID, pluginVersion, enabled, classification)},
		}
		s.addTemplate(catalog, record)
	}
	return nil
}

func (s *scanner) addTemplate(catalog *HostCatalog, record Template) {
	key := record.QualifiedName
	if _, shadowed := s.seenSkills[key]; shadowed {
		s.addWarning(warning.WarnForeignDuplicateTemplate, record.Provenance[0].SourcePath, "legacy template shadowed by skill")
		return
	}
	if _, duplicate := s.seenTemplates[key]; duplicate {
		s.addWarning(warning.WarnForeignDuplicateTemplate, record.Provenance[0].SourcePath, "lower-precedence template name dropped")
		return
	}
	position := len(catalog.Templates)
	catalog.Templates = append(catalog.Templates, record)
	s.seenTemplates[key] = position
}

func (s *scanner) addMCPMap(catalog *HostCatalog, servers map[string]json.RawMessage, path string, scope Scope, classification Classification, pluginID, pluginVersion string, enabled bool) error {
	return s.addMCPMapMode(catalog, servers, path, scope, classification, pluginID, pluginVersion, enabled, false)
}

func (s *scanner) addMCPMapReplacing(catalog *HostCatalog, servers map[string]json.RawMessage, path string, scope Scope, classification Classification, pluginID, pluginVersion string, enabled bool) error {
	return s.addMCPMapMode(catalog, servers, path, scope, classification, pluginID, pluginVersion, enabled, true)
}

func (s *scanner) addMCPMapMode(catalog *HostCatalog, servers map[string]json.RawMessage, path string, scope Scope, classification Classification, pluginID, pluginVersion string, enabled, replace bool) error {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := s.check(); err != nil {
			return err
		}
		if !s.consumeEntry(path) {
			return nil
		}
		var declaration map[string]json.RawMessage
		if strings.TrimSpace(name) == "" || json.Unmarshal(servers[name], &declaration) != nil || declaration == nil {
			s.addWarning(warning.WarnForeignEntryShapeUnknown, path, "MCP server entry is invalid")
			continue
		}
		transport, valid := mcpTransport(declaration)
		if !valid {
			s.addWarning(warning.WarnForeignEntryShapeUnknown, path, "MCP server entry lacks a valid command or URL")
			continue
		}
		record := MCPServer{
			Name: name, QualifiedName: qualify(pluginID, name), Transport: transport, Inert: true,
			Provenance: []Provenance{s.provenance(scope, path, pluginID, pluginVersion, enabled, classification)},
		}
		s.addMCPRecord(catalog, record, replace)
	}
	return nil
}

func mcpTransport(declaration map[string]json.RawMessage) (string, bool) {
	var declaredType string
	if raw, present := declaration["type"]; present && json.Unmarshal(raw, &declaredType) != nil {
		return "", false
	}
	var command, url string
	_ = json.Unmarshal(declaration["command"], &command)
	_ = json.Unmarshal(declaration["url"], &url)
	switch strings.ToLower(strings.TrimSpace(declaredType)) {
	case "":
		if strings.TrimSpace(url) != "" && strings.TrimSpace(command) == "" {
			return "http", true
		}
		return "stdio", strings.TrimSpace(command) != "" && strings.TrimSpace(url) == ""
	case "stdio":
		return "stdio", strings.TrimSpace(command) != "" && strings.TrimSpace(url) == ""
	case "http", "sse", "streamable-http":
		return strings.ToLower(strings.TrimSpace(declaredType)), strings.TrimSpace(url) != "" && strings.TrimSpace(command) == ""
	default:
		return "", false
	}
}

func (s *scanner) addMCPRecord(catalog *HostCatalog, record MCPServer, replace bool) {
	path := record.Provenance[0].SourcePath
	if position, duplicate := s.seenMCP[record.Name]; duplicate {
		if replace {
			catalog.MCPServers[position] = record
			s.addWarning(warning.WarnForeignDuplicateMCPServer, path, "higher-precedence MCP server replaced an earlier entry")
			s.addWarning(warning.WarnForeignMCPInert, path, "foreign MCP server remains inert")
			return
		}
		s.addWarning(warning.WarnForeignDuplicateMCPServer, path, "lower-precedence MCP server dropped")
		return
	}
	s.seenMCP[record.Name] = len(catalog.MCPServers)
	catalog.MCPServers = append(catalog.MCPServers, record)
	s.addWarning(warning.WarnForeignMCPInert, path, "foreign MCP server remains inert")
}

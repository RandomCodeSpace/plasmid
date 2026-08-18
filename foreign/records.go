package foreign

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plasmid-dev/plasmid/internal/foreignactivation"
	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/warning"
)

const (
	mcpTransportHTTP  = "http"
	mcpTransportStdio = "stdio"
)

func (s *scanner) scanTemplateRoot(ctx context.Context, catalog *HostCatalog, root, suffix string, origin discoverySource) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if s.truncated {
		return nil
	}
	entries, err := s.readDir(ctx, root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.addReadWarning(err, root, warning.WarnForeignIndexUnreadable, "template root is unreadable")
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if !s.consumeEntry(root) {
			return nil
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if err := s.scanTemplateEntry(ctx, catalog, path, strings.TrimSuffix(entry.Name(), suffix), origin); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) scanTemplateEntry(ctx context.Context, catalog *HostCatalog, path, name string, origin discoverySource) error {
	data, err := s.readFile(ctx, path)
	if err != nil {
		s.addReadWarning(err, path, warning.WarnForeignIndexUnreadable, "template is unreadable")
		return nil
	}
	document, notices := syntax.ParseTemplate(string(data), filepath.ToSlash(path), syntax.Host(s.host), name)
	s.warnings = append(s.warnings, notices...)
	s.addTemplate(catalog, Template{
		Name: name, QualifiedName: qualify(origin.pluginID, name),
		Arguments: append([]string(nil), document.Arguments...), Permissions: inertPermissions(document.AllowedTools, document.DeniedTools),
		UserInvocable: document.Exposure.UserInvocable, ModelInvocable: document.Exposure.ModelInvocable,
		RestrictsTools: document.RestrictsTools(),
		Provenance:     []Provenance{s.provenance(origin.scope, path, origin.pluginID, origin.pluginVersion, origin.enabled, origin.classification)},
		sourceDigest:   fmt.Sprintf("%x", sha256.Sum256(data)),
	})
	return nil
}

func inertPermissions(allowed, denied []syntax.ToolPattern) InertPermissions {
	result := InertPermissions{Allowed: make([]ToolPattern, len(allowed)), Denied: make([]ToolPattern, len(denied))}
	for index, pattern := range allowed {
		result.Allowed[index] = ToolPattern{Tool: pattern.Tool, Argument: pattern.Argument}
	}
	for index, pattern := range denied {
		result.Denied[index] = ToolPattern{Tool: pattern.Tool, Argument: pattern.Argument}
	}
	return result
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

func (s *scanner) addMCPMap(ctx context.Context, catalog *HostCatalog, servers map[string]json.RawMessage, path string, origin discoverySource) error {
	return s.addMCPMapMode(ctx, catalog, servers, path, origin, false)
}

func (s *scanner) addMCPMapReplacing(ctx context.Context, catalog *HostCatalog, servers map[string]json.RawMessage, path string, origin discoverySource) error {
	return s.addMCPMapMode(ctx, catalog, servers, path, origin, true)
}

func (s *scanner) addMCPMapMode(ctx context.Context, catalog *HostCatalog, servers map[string]json.RawMessage, path string, origin discoverySource, replace bool) error {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := checkContext(ctx); err != nil {
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
			Name: name, QualifiedName: qualify(origin.pluginID, name), Transport: transport, Inert: true,
			Provenance:    []Provenance{s.provenance(origin.scope, path, origin.pluginID, origin.pluginVersion, origin.enabled, origin.classification)},
			activationKey: s.captureActivation(activationFromJSON(name, transport, declaration)),
		}
		s.addMCPRecord(catalog, record, replace)
	}
	return nil
}

func activationFromJSON(name, transport string, declaration map[string]json.RawMessage) foreignactivation.Descriptor {
	result := foreignactivation.Descriptor{ID: name, Transport: transport}
	_ = json.Unmarshal(declaration["command"], &result.Command)
	_ = json.Unmarshal(declaration["args"], &result.Args)
	_ = json.Unmarshal(declaration["env"], &result.Env)
	_ = json.Unmarshal(declaration["url"], &result.URL)
	_ = json.Unmarshal(declaration["headers"], &result.Headers)
	return result
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
			return mcpTransportHTTP, true
		}
		return mcpTransportStdio, strings.TrimSpace(command) != "" && strings.TrimSpace(url) == ""
	case "stdio":
		return mcpTransportStdio, strings.TrimSpace(command) != "" && strings.TrimSpace(url) == ""
	case "http", "sse", "streamable-http":
		return mcpTransportHTTP, strings.TrimSpace(url) != "" && strings.TrimSpace(command) == ""
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

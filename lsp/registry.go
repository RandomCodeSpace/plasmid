// Package lsp provides framework-free language-server lifecycle leaves.
package lsp

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/RandomCodeSpace/plasmid/warning"
)

var serverIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Server describes one language-server executable and the files it owns.
type Server struct {
	ID          string
	Command     string
	Args        []string
	Extensions  []string
	RootMarkers []string
	Disabled    bool
}

// Registry is an immutable, deterministic language-server registry.
type Registry struct {
	servers []Server
}

// DefaultRegistry returns the built-in registry.
func DefaultRegistry() Registry {
	return Registry{servers: []Server{{
		ID:          "gopls",
		Command:     "gopls",
		Extensions:  []string{".go"},
		RootMarkers: []string{"go.work", "go.mod", ".git"},
	}}}
}

// MergeRegistry overlays entries on the built-ins by server ID. Invalid
// entries are skipped and reported without invalidating unrelated servers.
func MergeRegistry(entries []Server, warnings warning.Warner) Registry {
	if warnings == nil {
		warnings = warning.SlogSink{}
	}
	byID := make(map[string]Server)
	for _, server := range DefaultRegistry().servers {
		byID[server.ID] = cloneServer(server)
	}
	seen := make(map[string]struct{}, len(entries))
	for index, entry := range entries {
		entry.ID = strings.TrimSpace(entry.ID)
		if _, duplicate := seen[entry.ID]; duplicate && entry.ID != "" {
			warnings.Warn(warning.Warning{
				Code: warning.WarnLSPConfigDuplicateServer, Source: "lsp.registry",
				Path: entry.ID, Line: index + 1, Message: "duplicate server entry; later entry wins",
			})
		}
		seen[entry.ID] = struct{}{}

		merged := entry
		if base, exists := byID[entry.ID]; exists {
			merged = mergeServer(base, entry)
		}
		normalized, ok := normalizeServer(merged)
		if !ok {
			warnings.Warn(warning.Warning{
				Code: warning.WarnLSPConfigInvalidServer, Source: "lsp.registry",
				Path: entry.ID, Line: index + 1, Message: "invalid server entry was ignored",
			})
			continue
		}
		byID[normalized.ID] = normalized
	}

	servers := make([]Server, 0, len(byID))
	for _, server := range byID {
		servers = append(servers, cloneServer(server))
	}
	slices.SortFunc(servers, func(left, right Server) int { return strings.Compare(left.ID, right.ID) })
	return Registry{servers: servers}
}

func mergeServer(base, overlay Server) Server {
	result := cloneServer(base)
	result.Disabled = overlay.Disabled
	if overlay.Command != "" {
		result.Command = overlay.Command
	}
	if overlay.Args != nil {
		result.Args = append([]string(nil), overlay.Args...)
	}
	if overlay.Extensions != nil {
		result.Extensions = append([]string(nil), overlay.Extensions...)
	}
	if overlay.RootMarkers != nil {
		result.RootMarkers = append([]string(nil), overlay.RootMarkers...)
	}
	return result
}

func normalizeServer(server Server) (Server, bool) {
	server.ID = strings.TrimSpace(server.ID)
	server.Command = strings.TrimSpace(server.Command)
	if !serverIDPattern.MatchString(server.ID) || server.Command == "" || strings.ContainsRune(server.Command, 0) {
		return Server{}, false
	}
	server.Args = append([]string(nil), server.Args...)
	for _, argument := range server.Args {
		if strings.ContainsRune(argument, 0) {
			return Server{}, false
		}
	}
	server.Extensions = normalizeStrings(server.Extensions, true)
	if len(server.Extensions) == 0 {
		return Server{}, false
	}
	for _, extension := range server.Extensions {
		if extension == "." || !strings.HasPrefix(extension, ".") || strings.ContainsAny(extension, `/\\`) || strings.ContainsRune(extension, 0) {
			return Server{}, false
		}
	}
	server.RootMarkers = normalizeOrderedStrings(server.RootMarkers)
	for _, marker := range server.RootMarkers {
		if marker == "." || filepath.IsAbs(marker) || filepath.Base(marker) != marker || strings.ContainsAny(marker, `/\\`) || strings.ContainsRune(marker, 0) {
			return Server{}, false
		}
	}
	return server, true
}

func normalizeStrings(values []string, lower bool) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if lower {
			value = strings.ToLower(value)
		}
		if value != "" {
			result = append(result, value)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func normalizeOrderedStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func cloneServer(server Server) Server {
	server.Args = append([]string(nil), server.Args...)
	server.Extensions = append([]string(nil), server.Extensions...)
	server.RootMarkers = append([]string(nil), server.RootMarkers...)
	return server
}

// Server returns a defensive copy of the named server.
func (registry Registry) Server(id string) (Server, bool) {
	index, found := slices.BinarySearchFunc(registry.servers, id, func(server Server, id string) int {
		return strings.Compare(server.ID, id)
	})
	if !found {
		return Server{}, false
	}
	return cloneServer(registry.servers[index]), true
}

// Servers returns defensive copies in server-ID order.
func (registry Registry) Servers() []Server {
	servers := make([]Server, len(registry.servers))
	for index, server := range registry.servers {
		servers[index] = cloneServer(server)
	}
	return servers
}

// Match returns enabled servers that own the path's extension.
func (registry Registry) Match(path string) []Server {
	extension := strings.ToLower(filepath.Ext(path))
	var matches []Server
	for _, server := range registry.servers {
		if !server.Disabled && slices.Contains(server.Extensions, extension) {
			matches = append(matches, cloneServer(server))
		}
	}
	return matches
}

package foreign

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

type pluginManifest struct {
	path    string
	name    string
	version string
	fields  map[string]json.RawMessage
}

func (s *scanner) loadPluginManifest(root string, candidates []string, required bool) (pluginManifest, bool) {
	for _, relative := range candidates {
		path := filepath.Join(root, filepath.FromSlash(relative))
		data, err := s.readFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			s.addReadWarning(err, path, warning.WarnForeignManifestInvalid, "plugin manifest is unreadable")
			return pluginManifest{}, false
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields == nil {
			s.addWarning(warning.WarnForeignManifestInvalid, path, "plugin manifest is invalid")
			return pluginManifest{}, false
		}
		var name, version string
		_ = json.Unmarshal(fields["name"], &name)
		_ = json.Unmarshal(fields["version"], &version)
		if strings.TrimSpace(name) == "" {
			s.addWarning(warning.WarnForeignManifestInvalid, path, "plugin manifest name is missing")
			return pluginManifest{}, false
		}
		return pluginManifest{path: path, name: name, version: version, fields: fields}, true
	}
	if required {
		s.addWarning(warning.WarnForeignManifestMissing, root, "required plugin manifest is missing")
	}
	return pluginManifest{}, false
}

func (s *scanner) componentPaths(root string, raw json.RawMessage, defaults []string, requireDot bool) []string {
	values := defaults
	if len(raw) != 0 {
		values = nil
		var single string
		if json.Unmarshal(raw, &single) == nil {
			values = []string{single}
		} else if json.Unmarshal(raw, &values) != nil {
			s.addWarning(warning.WarnForeignManifestInvalid, root, "plugin component path has an invalid shape")
			return nil
		}
	}
	pluginRoot, err := workspace.NewRoot(root)
	if err != nil {
		s.addWarning(warning.WarnForeignInstallPathMissing, root, "plugin root is unavailable")
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if resolved, ok := s.componentPath(pluginRoot, root, value, requireDot); ok {
			result = append(result, resolved)
		}
	}
	return result
}

func (s *scanner) componentPath(pluginRoot *workspace.Root, root, value string, requireDot bool) (string, bool) {
	if strings.TrimSpace(value) == "" || filepath.IsAbs(value) || (requireDot && value != "." && !strings.HasPrefix(filepath.ToSlash(value), "./")) {
		s.addWarning(warning.WarnForeignPathEscape, root, "plugin component path is not a confined relative path")
		return "", false
	}
	resolved, err := pluginRoot.Resolve(filepath.FromSlash(value))
	if err == nil {
		return resolved, true
	}
	path := filepath.Join(root, value)
	if errors.Is(err, workspace.ErrOutsideRoot) {
		s.addWarning(warning.WarnForeignPathEscape, path, "plugin component path escapes its root")
	} else {
		s.addWarning(warning.WarnForeignManifestInvalid, path, "plugin component path is invalid")
	}
	return "", false
}

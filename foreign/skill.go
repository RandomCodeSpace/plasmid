package foreign

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

func (s *scanner) scanSkillRoot(catalog *HostCatalog, root string, origin discoverySource) error {
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
		s.addReadWarning(err, root, warning.WarnForeignIndexUnreadable, "skill root is unreadable")
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
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if err := s.scanSkillEntry(catalog, filepath.Join(root, entry.Name(), "SKILL.md"), origin); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) scanSkillEntry(catalog *HostCatalog, path string, origin discoverySource) error {
	data, err := s.readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.addWarning(warning.WarnForeignSkillMissingMarkdown, path, "skill is missing SKILL.md")
		} else {
			s.addReadWarning(err, path, warning.WarnForeignIndexUnreadable, "skill markdown is unreadable")
		}
		return nil
	}
	document, notices := syntax.ParseDocument(string(data), filepath.ToSlash(path), syntax.Host(s.host))
	s.warnings = append(s.warnings, notices...)
	if document.Name == "" || document.Description == "" {
		return nil
	}
	s.addSkill(catalog, s.skillRecord(document, path, origin), data)
	return nil
}

func (s *scanner) skillRecord(document syntax.Document, path string, origin discoverySource) Skill {
	permissions := inertPermissions(document.AllowedTools, document.DeniedTools)
	if len(permissions.Allowed) > 0 || len(permissions.Denied) > 0 {
		s.addWarning(warning.WarnForeignPermissionInert, path, "foreign tool permissions are inert")
	}
	metadata := make([]MetadataEntry, len(document.Metadata))
	for index, item := range document.Metadata {
		metadata[index] = MetadataEntry{Name: item.Name, Value: item.Value}
	}
	return Skill{
		Name: document.Name, QualifiedName: qualify(origin.pluginID, document.Name), Description: document.Description, License: document.License,
		Compatibility: document.Compatibility, Metadata: metadata, Permissions: permissions,
		Arguments: append([]string(nil), document.Arguments...), Globs: append([]string(nil), document.Globs...),
		UserInvocable: document.Exposure.UserInvocable, ModelInvocable: document.Exposure.ModelInvocable,
		RestrictsTools: document.RestrictsTools(),
		Provenance:     []Provenance{s.provenance(origin.scope, path, origin.pluginID, origin.pluginVersion, origin.enabled, origin.classification)},
	}
}

func (s *scanner) addSkill(catalog *HostCatalog, record Skill, data []byte) {
	canonical, err := filepath.EvalSymlinks(record.Provenance[0].SourcePath)
	if err != nil {
		canonical = record.Provenance[0].SourcePath
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(data))
	record.sourceDigest = fingerprint
	record.realPaths = []string{filepath.ToSlash(filepath.Clean(canonical))}
	identity := record.QualifiedName + ":"
	for _, key := range []string{"path:" + identity + filepath.Clean(canonical), "bytes:" + identity + fingerprint} {
		if position, ok := s.skillSources[key]; ok {
			catalog.Skills[position].Provenance = append(catalog.Skills[position].Provenance, record.Provenance...)
			catalog.Skills[position].realPaths = append(catalog.Skills[position].realPaths, record.realPaths...)
			return
		}
	}
	if _, duplicate := s.seenSkills[record.QualifiedName]; duplicate {
		s.addWarning(warning.WarnForeignDuplicateSkill, record.Provenance[0].SourcePath, "lower-precedence skill name dropped")
		return
	}
	position := len(catalog.Skills)
	catalog.Skills = append(catalog.Skills, record)
	s.seenSkills[record.QualifiedName] = position
	s.skillSources["path:"+identity+filepath.Clean(canonical)] = position
	s.skillSources["bytes:"+identity+fingerprint] = position
}

func qualify(pluginID, name string) string {
	if pluginID == "" {
		return name
	}
	return pluginID + ":" + name
}

func (s *scanner) readFile(path string) ([]byte, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	if !s.pathAllowed(path) {
		return nil, fmt.Errorf("read foreign file %q: %w", path, workspace.ErrOutsideRoot)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(&contextReader{scanner: s, reader: file}, s.options.MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.options.MaxFileBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", s.options.MaxFileBytes)
	}
	return data, nil
}

func (s *scanner) readDir(path string) ([]os.DirEntry, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	if !s.pathAllowed(path) {
		return nil, fmt.Errorf("read foreign directory %q: %w", path, workspace.ErrOutsideRoot)
	}
	directory, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	limit := s.options.MaxEntries - s.entries + 1
	entries, err := directory.ReadDir(limit)
	if err == io.EOF {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if contextErr := s.check(); contextErr != nil {
		return nil, contextErr
	}
	return entries, nil
}

type contextReader struct {
	scanner *scanner
	reader  io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.scanner.check(); err != nil {
		return 0, err
	}
	count, err := r.reader.Read(buffer)
	if contextErr := r.scanner.check(); contextErr != nil {
		return count, contextErr
	}
	return count, err
}

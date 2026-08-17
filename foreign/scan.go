package foreign

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

const (
	defaultMaxEntries   = 4096
	defaultMaxFileBytes = int64(1 << 20)
)

// Scan discovers all supported hosts while retaining their independent
// precedence. Unqualified names shared by hosts are reported as ambiguous.
func Scan(ctx context.Context, options Options) (Catalog, error) {
	claude, err := ScanClaude(ctx, options)
	if err != nil {
		return Catalog{}, err
	}
	codex, err := ScanCodex(ctx, options)
	if err != nil {
		return Catalog{}, err
	}
	copilot, err := ScanCopilot(ctx, options)
	if err != nil {
		return Catalog{}, err
	}
	catalog := Catalog{Hosts: []HostCatalog{claude, codex, copilot}, Skills: []Skill{}, Warnings: []warning.Warning{}}
	for _, host := range catalog.Hosts {
		for _, skill := range host.Skills {
			mergeCatalogSkill(&catalog.Skills, skill)
		}
	}
	sort.SliceStable(catalog.Skills, func(i, j int) bool {
		if catalog.Skills[i].Name != catalog.Skills[j].Name {
			return catalog.Skills[i].Name < catalog.Skills[j].Name
		}
		return catalog.Skills[i].QualifiedName < catalog.Skills[j].QualifiedName
	})
	nameRecords := make(map[string][]Skill)
	for _, skill := range catalog.Skills {
		nameRecords[skill.Name] = append(nameRecords[skill.Name], skill)
	}
	names := make([]string, 0, len(nameRecords))
	for name, records := range nameRecords {
		if len(records) > 1 && recordsSpanHosts(records) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		values := catalogSkillHosts(nameRecords[name])
		catalog.Warnings = append(catalog.Warnings, warning.Warning{
			Code: warning.WarnForeignAmbiguousName, Source: "foreign", Path: name,
			Message: "unqualified skill name is ambiguous across hosts: " + strings.Join(values, ", "),
		})
	}
	return catalog, nil
}

func mergeCatalogSkill(skills *[]Skill, candidate Skill) {
	for index := range *skills {
		existing := &(*skills)[index]
		if existing.QualifiedName != candidate.QualifiedName || !sameCatalogSkillSource(*existing, candidate) {
			continue
		}
		existing.Provenance = append(existing.Provenance, candidate.Provenance...)
		existing.realPaths = append(existing.realPaths, candidate.realPaths...)
		sortProvenance(existing.Provenance)
		return
	}
	*skills = append(*skills, candidate)
}

func sameCatalogSkillSource(left, right Skill) bool {
	for _, leftPath := range left.realPaths {
		for _, rightPath := range right.realPaths {
			if leftPath == rightPath {
				return true
			}
		}
	}
	return left.sourceDigest != "" && left.sourceDigest == right.sourceDigest && skillHasNoPlugin(left) && skillHasNoPlugin(right)
}

func skillHasNoPlugin(skill Skill) bool {
	for _, provenance := range skill.Provenance {
		if provenance.PluginID != "" {
			return false
		}
	}
	return true
}

func recordsSpanHosts(records []Skill) bool {
	hosts := catalogSkillHosts(records)
	return len(hosts) > 1
}

func catalogSkillHosts(records []Skill) []string {
	seen := make(map[Host]bool)
	for _, record := range records {
		for _, provenance := range record.Provenance {
			seen[provenance.Host] = true
		}
	}
	hosts := make([]string, 0, len(seen))
	for host := range seen {
		hosts = append(hosts, string(host))
	}
	sort.Strings(hosts)
	return hosts
}

type scanner struct {
	ctx           context.Context
	options       Options
	host          Host
	entries       int
	truncated     bool
	warnings      []warning.Warning
	seenSkills    map[string]int
	skillSources  map[string]int
	seenTemplates map[string]int
	seenMCP       map[string]int
	allowedRoots  []pathBoundary
}

type pathBoundary struct {
	lexical string
	real    string
}

func newScanner(ctx context.Context, host Host, options Options) (*scanner, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.WorkingDir == "" {
		var err error
		options.WorkingDir, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	workingDir, err := canonicalDirectory(options.WorkingDir)
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}
	options.WorkingDir = workingDir
	if options.HomeDir == "" {
		options.HomeDir, _ = os.UserHomeDir()
	}
	if options.RepositoryRoot == "" {
		options.RepositoryRoot = findRepositoryRoot(workingDir)
	}
	root, err := canonicalDirectory(options.RepositoryRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	relative, err := filepath.Rel(root, workingDir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("working directory is outside repository root")
	}
	options.RepositoryRoot = root
	if options.MaxEntries <= 0 {
		options.MaxEntries = defaultMaxEntries
	}
	if options.MaxFileBytes <= 0 {
		options.MaxFileBytes = defaultMaxFileBytes
	}
	if options.CodexHome == "" && options.HomeDir != "" {
		options.CodexHome = filepath.Join(options.HomeDir, ".codex")
	}
	if options.AdminSkillsDir == "" {
		options.AdminSkillsDir = filepath.FromSlash("/etc/codex/skills")
	}
	allowedRoots := []pathBoundary{{lexical: options.RepositoryRoot, real: options.RepositoryRoot}}
	for _, candidate := range []string{options.HomeDir, options.CodexHome, options.AdminSkillsDir} {
		if candidate == "" {
			continue
		}
		if root, rootErr := canonicalDirectory(candidate); rootErr == nil {
			absolute, absoluteErr := filepath.Abs(candidate)
			if absoluteErr == nil {
				allowedRoots = append(allowedRoots, pathBoundary{lexical: filepath.Clean(absolute), real: root})
			}
			if absoluteErr != nil || filepath.Clean(absolute) != root {
				allowedRoots = append(allowedRoots, pathBoundary{lexical: root, real: root})
			}
		}
	}
	sort.SliceStable(allowedRoots, func(i, j int) bool { return len(allowedRoots[i].lexical) > len(allowedRoots[j].lexical) })
	return &scanner{
		ctx: ctx, options: options, host: host,
		seenSkills: make(map[string]int), skillSources: make(map[string]int),
		seenTemplates: make(map[string]int), seenMCP: make(map[string]int), allowedRoots: allowedRoots,
	}, nil
}

func canonicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return filepath.Clean(resolved), nil
}

func findRepositoryRoot(workingDir string) string {
	for current := workingDir; ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return workingDir
		}
	}
}

func (s *scanner) check() error {
	return s.ctx.Err()
}

func (s *scanner) consumeEntry(path string) bool {
	if s.truncated || s.entries >= s.options.MaxEntries {
		if !s.truncated {
			s.addWarning(warning.WarnForeignScanTruncated, path, "foreign scan entry limit reached")
			s.truncated = true
		}
		return false
	}
	s.entries++
	return true
}

func (s *scanner) pathAllowed(path string) bool {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return os.IsNotExist(err)
	}
	for _, boundary := range s.allowedRoots {
		lexicalRelative, lexicalErr := filepath.Rel(boundary.lexical, absolute)
		if lexicalErr != nil || lexicalRelative == ".." || strings.HasPrefix(lexicalRelative, ".."+string(filepath.Separator)) {
			continue
		}
		realRelative, realErr := filepath.Rel(boundary.real, resolved)
		return realErr == nil && realRelative != ".." && !strings.HasPrefix(realRelative, ".."+string(filepath.Separator))
	}
	return false
}

func (s *scanner) addReadWarning(err error, path, code, message string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if errors.Is(err, workspace.ErrOutsideRoot) {
		s.addWarning(warning.WarnForeignPathEscape, path, "foreign source path escapes its configured root")
		return
	}
	s.addWarning(code, path, message)
}

func (s *scanner) addWarning(code, path, message string) {
	s.addWarningLine(code, path, 0, message)
}

func (s *scanner) addWarningLine(code, path string, line int, message string) {
	s.warnings = append(s.warnings, warning.Warning{
		Code: code, Source: "foreign", Path: filepath.ToSlash(path), Line: line, Message: message,
	})
}

func (s *scanner) provenance(scope Scope, path, pluginID, pluginVersion string, enabled bool, classification Classification) Provenance {
	trust := TrustUnknown
	if scope == ScopeProject || scope == ScopeLocal {
		trust = TrustUntrusted
		if s.options.ProjectTrusted {
			trust = TrustTrusted
		}
	}
	return Provenance{
		Host: s.host, Scope: scope, SourcePath: filepath.ToSlash(filepath.Clean(path)), PluginID: pluginID,
		PluginVersion: pluginVersion, Enabled: enabled, Trust: trust, Classification: classification,
	}
}

func (s *scanner) finish(catalog HostCatalog) HostCatalog {
	sort.SliceStable(catalog.Skills, func(i, j int) bool { return catalog.Skills[i].Name < catalog.Skills[j].Name })
	sort.SliceStable(catalog.Templates, func(i, j int) bool { return catalog.Templates[i].Name < catalog.Templates[j].Name })
	sort.SliceStable(catalog.MCPServers, func(i, j int) bool { return catalog.MCPServers[i].Name < catalog.MCPServers[j].Name })
	for index := range catalog.Skills {
		sortProvenance(catalog.Skills[index].Provenance)
	}
	for index := range catalog.Templates {
		sortProvenance(catalog.Templates[index].Provenance)
	}
	for index := range catalog.MCPServers {
		sortProvenance(catalog.MCPServers[index].Provenance)
	}
	catalog.Warnings = append([]warning.Warning(nil), s.warnings...)
	if catalog.Skills == nil {
		catalog.Skills = []Skill{}
	}
	if catalog.Templates == nil {
		catalog.Templates = []Template{}
	}
	if catalog.MCPServers == nil {
		catalog.MCPServers = []MCPServer{}
	}
	if catalog.Warnings == nil {
		catalog.Warnings = []warning.Warning{}
	}
	return catalog
}

func sortProvenance(values []Provenance) {
	sort.SliceStable(values, func(i, j int) bool {
		left, right := values[i], values[j]
		if left.Host != right.Host {
			return left.Host < right.Host
		}
		if left.Scope != right.Scope {
			return left.Scope < right.Scope
		}
		if left.PluginID != right.PluginID {
			return left.PluginID < right.PluginID
		}
		return left.SourcePath < right.SourcePath
	})
}

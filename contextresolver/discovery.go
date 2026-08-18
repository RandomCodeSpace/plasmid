package contextresolver

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plasmid-dev/plasmid/internal/pathglob"
	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

const rootScope = 1000

const (
	agentInstructions   = "AGENT.md"
	agentsInstructions  = "AGENTS.md"
	claudeInstructions  = "CLAUDE.md"
	copilotInstructions = "copilot-instructions.md"
)

type candidate struct {
	host        syntax.Host
	path        string
	prefix      string
	forceScoped bool
	scope       int
	trust       TrustLevel
}

type candidateCollector struct {
	resolver     *Resolver
	contextError func() error
	state        *discoveryState
	rootDir      string
	result       []candidate
}

func (c *candidateCollector) add(path string, host syntax.Host, scope int, trust TrustLevel, prefix string, scoped bool) {
	if !c.resolver.hostEnabled(host) {
		return
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return
	}
	if trust != TrustUser {
		trust = c.resolver.repositorySourceTrust(path)
	}
	c.result = append(c.result, candidate{path: path, host: host, scope: scope, trust: trust, prefix: prefix, forceScoped: scoped})
}

type discoveryState struct {
	content          map[[32]byte]string
	real             map[string]bool
	realDocument     map[string]int
	stack            map[string]bool
	discoveryEntries int
	budgetWarned     bool
	documentIndex    map[[32]byte]int
	nextIndex        int
	duplicateIndex   int
	duplicateSource  *InstructionProvenance
}

type importExpansion struct {
	parts  []documentPart
	policy syntax.ToolPolicy
}

func (r *Resolver) discover(ctx context.Context) ([]document, error) {
	state := &discoveryState{content: make(map[[32]byte]string), real: make(map[string]bool), realDocument: make(map[string]int), stack: make(map[string]bool), documentIndex: make(map[[32]byte]int), duplicateIndex: -1}
	candidates, err := r.candidates(ctx, state)
	if err != nil {
		return nil, err
	}
	var documents []document
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		state.nextIndex = len(documents)
		state.duplicateIndex = -1
		state.duplicateSource = nil
		item, ok := r.loadDocument(ctx, candidate, state)
		if ok {
			documents = append(documents, item)
		} else if state.duplicateIndex >= 0 && state.duplicateIndex < len(documents) && state.duplicateSource != nil {
			documents[state.duplicateIndex].provenance = append(documents[state.duplicateIndex].provenance, *state.duplicateSource)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(documents, func(i, j int) bool {
		if documents[i].scope != documents[j].scope {
			return documents[i].scope < documents[j].scope
		}
		return documents[i].displayPath < documents[j].displayPath
	})
	return documents, nil
}

func (r *Resolver) candidates(ctx context.Context, state *discoveryState) ([]candidate, error) {
	rootDir := r.options.Root.Dir()
	collector := candidateCollector{resolver: r, contextError: ctx.Err, state: state, rootDir: rootDir}

	if r.options.HomeDir != "" {
		for _, source := range []struct {
			path string
			host syntax.Host
		}{
			{filepath.Join(r.options.HomeDir, ".claude", claudeInstructions), syntax.HostClaude},
			{filepath.Join(r.options.HomeDir, ".codex", agentsInstructions), syntax.HostCodex},
			{filepath.Join(r.options.HomeDir, ".github", copilotInstructions), syntax.HostCopilot},
		} {
			collector.add(source.path, source.host, 0, TrustUser, "", false)
		}
	}

	ancestors := pathAncestors(rootDir)
	for index, directory := range ancestors {
		scope := index + 1
		if directory == rootDir {
			scope = rootScope
		}
		for _, name := range []string{agentInstructions, agentsInstructions, claudeInstructions} {
			host := syntax.HostCodex
			if name == claudeInstructions {
				host = syntax.HostClaude
			}
			collector.add(filepath.Join(directory, name), host, scope, TrustUntrusted, "", false)
		}
	}
	for _, source := range []struct {
		path string
		host syntax.Host
	}{
		{filepath.Join(rootDir, ".claude", claudeInstructions), syntax.HostClaude},
		{filepath.Join(rootDir, ".codex", agentsInstructions), syntax.HostCodex},
		{filepath.Join(rootDir, ".github", copilotInstructions), syntax.HostCopilot},
	} {
		collector.add(source.path, source.host, rootScope, TrustUntrusted, "", false)
	}
	if err := filepath.WalkDir(rootDir, collector.visit); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return collector.result, nil
}

func (c *candidateCollector) visit(path string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return c.handleWalkError(path, entry)
	}
	if err := c.contextError(); err != nil {
		return err
	}
	if !c.resolver.takeDiscoveryEntry(c.state, ".") {
		return fs.SkipAll
	}
	if entry.IsDir() {
		return c.visitDirectory(path, entry)
	}
	c.visitFile(path, entry)
	return nil
}

func (c *candidateCollector) handleWalkError(path string, entry fs.DirEntry) error {
	c.resolver.options.WarningSink.Warn(contextWarning(warning.WarnContextReadError, displayPath(c.rootDir, path), "instruction discovery entry could not be read"))
	if entry != nil && entry.IsDir() {
		return filepath.SkipDir
	}
	return nil
}

func (c *candidateCollector) visitDirectory(path string, entry fs.DirEntry) error {
	if path != c.rootDir && (entry.Type()&os.ModeSymlink != 0 || entry.Name() == ".git" || entry.Name() == ".plasmid") {
		return filepath.SkipDir
	}
	return nil
}

func (c *candidateCollector) visitFile(path string, entry fs.DirEntry) {
	if entry.Type()&os.ModeSymlink != 0 {
		return
	}
	relative := displayPath(c.rootDir, path)
	if filepath.Dir(path) == c.rootDir || relative == ".claude/CLAUDE.md" || relative == ".codex/AGENTS.md" || relative == ".github/copilot-instructions.md" {
		return
	}
	base := entry.Name()
	directory := filepath.ToSlash(filepath.Dir(relative))
	depth := strings.Count(directory, "/") + 1
	switch {
	case base == agentInstructions || base == agentsInstructions || base == claudeInstructions:
		host := syntax.HostCodex
		if base == claudeInstructions {
			host = syntax.HostClaude
		}
		c.add(path, host, rootScope+depth, TrustUntrusted, directory, false)
	case strings.HasPrefix(relative, ".github/instructions/") && strings.HasSuffix(base, ".instructions.md"):
		c.add(path, syntax.HostCopilot, rootScope+1, TrustUntrusted, "", true)
	case strings.HasPrefix(relative, ".claude/rules/") && strings.HasSuffix(base, ".md"):
		c.add(path, syntax.HostClaude, rootScope+1, TrustUntrusted, "", false)
	}
}

func (r *Resolver) loadDocument(ctx context.Context, source candidate, state *discoveryState) (document, bool) {
	real, err := filepath.EvalSymlinks(source.path)
	if err != nil {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextReadError, source.path, "instruction source could not be resolved"))
		return document{}, false
	}
	real, err = filepath.Abs(real)
	if err != nil {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextReadError, source.path, "instruction source could not be resolved"))
		return document{}, false
	}
	if state.real[real] {
		if index, ok := state.realDocument[real]; ok {
			provenance := instructionProvenance(source)
			state.duplicateIndex = index
			state.duplicateSource = &provenance
		}
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextDedupDropped, displayPath(r.options.Root.Dir(), source.path), "duplicate real instruction path dropped"))
		return document{}, false
	}
	data, truncated, err := readBoundedAt(ctx, filepath.Dir(real), filepath.Base(real), r.options.MaxFileBytes)
	if err != nil {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextReadError, displayPath(r.options.Root.Dir(), source.path), "instruction source could not be read"))
		return document{}, false
	}
	display := displayPath(r.options.Root.Dir(), source.path)
	if truncated {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextFileTruncated, display, "instruction source exceeded the per-file byte budget"))
	}
	instruction, notices := syntax.ParseInstruction(string(data), display, source.host)
	for _, notice := range notices {
		r.options.WarningSink.Warn(notice)
	}
	var matcher pathglob.Matcher
	if len(instruction.Globs) != 0 {
		matcher, _ = pathglob.Compile(instruction.Globs)
	}
	if (source.forceScoped || instruction.PathScopeDeclared) && matcher == nil {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextGlobUnsupported, display, "path-scoped instruction has no valid path glob and was dropped"))
		return document{}, false
	}
	hash := sha256.Sum256(data)
	if previous, duplicate := state.content[hash]; duplicate {
		provenance := instructionProvenance(source)
		state.duplicateIndex = state.documentIndex[hash]
		state.duplicateSource = &provenance
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextDedupDropped, display, "byte-identical instruction content dropped; first source retained at "+previous))
		return document{}, false
	}
	state.real[real] = true
	state.realDocument[real] = state.nextIndex
	state.content[hash] = display
	state.documentIndex[hash] = state.nextIndex
	state.stack[real] = true
	imports := r.expandImports(ctx, real, instruction.Body, source.host, source.trust, 0, state)
	delete(state.stack, real)
	return document{
		parts: imports.parts, displayPath: display, matcher: matcher, policy: instruction.Policy.Intersect(imports.policy),
		prefix: source.prefix, scope: source.scope, provenance: []InstructionProvenance{instructionProvenance(source)},
	}, true
}

func instructionProvenance(source candidate) InstructionProvenance {
	scope := "project"
	if source.trust == TrustUser {
		scope = "user"
	}
	return InstructionProvenance{
		Host: string(source.host), Scope: scope, SourcePath: filepath.ToSlash(source.path),
		Enabled: true, Trusted: source.trust != TrustUntrusted, Classification: "documented",
	}
}

func (r *Resolver) expandImports(ctx context.Context, sourcePath, body string, host syntax.Host, trust TrustLevel, depth int, state *discoveryState) importExpansion {
	regions := syntax.ScanCodeRegions(body)
	result := importExpansion{parts: make([]documentPart, 0, 1), policy: syntax.NewToolPolicy(nil, nil)}
	var current strings.Builder
	display := displayPath(r.options.Root.Dir(), sourcePath)
	flush := func() {
		if current.Len() == 0 {
			return
		}
		result.parts = appendDocumentPart(result.parts, documentPart{body: current.String(), displayPath: display, trust: trust})
		current.Reset()
	}
	lineStart := 0
	for lineStart < len(body) {
		lineEnd := strings.IndexByte(body[lineStart:], '\n')
		next := len(body)
		if lineEnd >= 0 {
			lineEnd += lineStart
			next = lineEnd + 1
		} else {
			lineEnd = len(body)
		}
		line := body[lineStart:lineEnd]
		trimmed := strings.TrimSpace(line)
		if isImportDirective(trimmed, line, lineStart, regions) {
			r.expandImportDirective(importDirective{
				sourcePath: sourcePath, body: body, line: line, requested: trimmed[1:], host: host,
				trust: trust, depth: depth, next: next, state: state, current: &current, result: &result, flush: flush,
				load: func(parent, requested string, trust TrustLevel, depth int, state *discoveryState) importExpansion {
					return r.loadImport(ctx, parent, requested, trust, depth, state)
				},
			})
		} else {
			current.WriteString(body[lineStart:next])
		}
		lineStart = next
	}
	flush()
	return result
}

func isImportDirective(trimmed, line string, lineStart int, regions []syntax.CodeRegion) bool {
	return strings.HasPrefix(trimmed, "@") && len(trimmed) > 1 && !strings.ContainsAny(trimmed[1:], " \t") &&
		!syntax.IsCodeOffset(regions, lineStart+strings.Index(line, "@"))
}

type importDirective struct {
	sourcePath, body, line, requested string
	host                              syntax.Host
	trust                             TrustLevel
	depth, next                       int
	state                             *discoveryState
	current                           *strings.Builder
	result                            *importExpansion
	flush                             func()
	load                              func(string, string, TrustLevel, int, *discoveryState) importExpansion
}

func (r *Resolver) expandImportDirective(directive importDirective) {
	if directive.host != syntax.HostClaude {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextImportNotClaude, displayPath(r.options.Root.Dir(), directive.sourcePath), "import directive ignored outside Claude syntax"))
		directive.current.WriteString(directive.line)
	} else {
		directive.flush()
		imported := directive.load(directive.sourcePath, directive.requested, directive.trust, directive.depth+1, directive.state)
		directive.result.parts = append(directive.result.parts, imported.parts...)
		directive.result.policy = directive.result.policy.Intersect(imported.policy)
	}
	if directive.next < len(directive.body) || strings.HasSuffix(directive.body, "\n") {
		directive.current.WriteByte('\n')
	}
}

func (r *Resolver) loadImport(ctx context.Context, parent, requested string, trust TrustLevel, depth int, state *discoveryState) importExpansion {
	empty := importExpansion{policy: syntax.NewToolPolicy(nil, nil)}
	display := displayPath(r.options.Root.Dir(), parent)
	if depth > r.options.MaxImportDepth {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextImportDepth, display, "instruction import depth exceeded"))
		return empty
	}
	if err := ctx.Err(); err != nil {
		return empty
	}
	if !r.takeDiscoveryEntry(state, display) {
		return empty
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(parent), filepath.FromSlash(requested)))
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextImportMissing, display, "instruction import could not be resolved"))
		return empty
	}
	approvedRoot, relative, approved := rootedPath(r.approvedImportRoots(trust), real)
	if !approved {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextImportEscape, display, "instruction import escaped approved roots"))
		return empty
	}
	if state.stack[real] {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextImportCycle, display, "instruction import cycle detected"))
		return empty
	}
	if state.real[real] {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextDedupDropped, displayPath(r.options.Root.Dir(), real), "duplicate real instruction path dropped"))
		return empty
	}
	data, truncated, err := readBoundedAt(ctx, approvedRoot, relative, r.options.MaxFileBytes)
	if err != nil {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextImportMissing, display, "instruction import could not be read"))
		return empty
	}
	if truncated {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextFileTruncated, displayPath(r.options.Root.Dir(), real), "instruction import exceeded the per-file byte budget"))
	}
	state.real[real] = true
	importDisplay := displayPath(r.options.Root.Dir(), real)
	hash := sha256.Sum256(data)
	if previous, duplicate := state.content[hash]; duplicate {
		r.options.WarningSink.Warn(contextWarning(warning.WarnContextDedupDropped, importDisplay, "byte-identical instruction content dropped; first source retained at "+previous))
		return empty
	}
	state.content[hash] = importDisplay
	state.stack[real] = true
	instruction, notices := syntax.ParseInstruction(string(data), importDisplay, syntax.HostClaude)
	for _, notice := range notices {
		r.options.WarningSink.Warn(notice)
	}
	importTrust := r.importTrust(real, trust)
	result := r.expandImports(ctx, real, instruction.Body, syntax.HostClaude, importTrust, depth, state)
	delete(state.stack, real)
	result.policy = instruction.Policy.Intersect(result.policy)
	return result
}

func appendDocumentPart(parts []documentPart, next documentPart) []documentPart {
	if next.body == "" {
		return parts
	}
	if len(parts) != 0 {
		last := &parts[len(parts)-1]
		if last.displayPath == next.displayPath && last.trust == next.trust {
			last.body += next.body
			return parts
		}
	}
	return append(parts, next)
}

func (r *Resolver) takeDiscoveryEntry(state *discoveryState, path string) bool {
	if state.discoveryEntries >= r.options.MaxDiscoveryEntries {
		if !state.budgetWarned {
			state.budgetWarned = true
			r.options.WarningSink.Warn(contextWarning(warning.WarnContextDiscoveryTruncated, path, "instruction discovery entry budget exhausted"))
		}
		return false
	}
	state.discoveryEntries++
	return true
}

func readBoundedAt(ctx context.Context, rootPath, relative string, maximum int) ([]byte, bool, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	file, err := root.OpenFile(relative, os.O_RDONLY|nonBlockingOpenFlag, 0)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("instruction source is not a regular file")
	}
	reader := bufio.NewReader(io.LimitReader(file, int64(maximum)+1))
	var data []byte
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		count, readErr := reader.Read(buffer)
		data = append(data, buffer[:count]...)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, false, readErr
		}
	}
	truncated := len(data) > maximum
	if truncated {
		data = data[:maximum]
	}
	return data, truncated, nil
}

func (r *Resolver) approvedImportRoots(trust TrustLevel) []string {
	roots := []string{r.options.Root.Dir()}
	if trust == TrustUser && r.options.HomeDir != "" {
		roots = append(roots, r.options.HomeDir)
	}
	roots = append(roots, r.options.ImportRoots...)
	result := make([]string, 0, len(roots))
	for _, root := range roots {
		real, err := filepath.EvalSymlinks(root)
		if err == nil {
			result = append(result, filepath.Clean(real))
		}
	}
	return result
}

func (r *Resolver) repositorySourceTrust(path string) TrustLevel {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return TrustUntrusted
	}
	root := r.options.Root.Dir()
	for _, trusted := range r.options.TrustedRoots {
		real, err := filepath.EvalSymlinks(trusted)
		if err == nil && workspace.Contains(real, root) && workspace.Contains(real, realPath) {
			return TrustRepository
		}
	}
	return TrustUntrusted
}

func (r *Resolver) importTrust(path string, parent TrustLevel) TrustLevel {
	if parent == TrustUser && r.options.HomeDir != "" && workspace.Contains(r.options.HomeDir, path) {
		return TrustUser
	}
	return r.repositorySourceTrust(path)
}

func (r *Resolver) hostEnabled(host syntax.Host) bool {
	if r.options.Hosts == nil {
		return true
	}
	switch host {
	case syntax.HostClaude:
		return r.options.Hosts.Claude
	case syntax.HostCodex:
		return r.options.Hosts.Codex
	case syntax.HostCopilot:
		return r.options.Hosts.Copilot
	default:
		return false
	}
}

func pathAncestors(path string) []string {
	path = filepath.Clean(path)
	var reversed []string
	for current := path; ; current = filepath.Dir(current) {
		reversed = append(reversed, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	result := make([]string, len(reversed))
	for index := range reversed {
		result[len(reversed)-1-index] = reversed[index]
	}
	return result
}

func displayPath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err == nil && workspace.Contains(root, path) {
		return filepath.ToSlash(relative)
	}
	return filepath.ToSlash(path)
}

func rootedPath(roots []string, path string) (string, string, bool) {
	for _, root := range roots {
		if workspace.Contains(root, path) {
			relative, err := filepath.Rel(root, path)
			if err == nil {
				return root, relative, true
			}
		}
	}
	return "", "", false
}

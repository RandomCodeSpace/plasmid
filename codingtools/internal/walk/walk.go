// Package walk provides deterministic, filtered directory traversal.
package walk

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/RandomCodeSpace/plasmid/internal/pathglob"
	"github.com/RandomCodeSpace/plasmid/warning"
	"github.com/RandomCodeSpace/plasmid/workspace"
)

const (
	defaultMaxVisited = 100000
	defaultMaxResults = 20000
	gitignoreName     = ".gitignore"
)

// ErrWalkTruncated reports that a configured visit or result cap stopped traversal.
var ErrWalkTruncated = errors.New("walk truncated")

// Filter controls directory traversal. A zero visit or result cap selects the
// package default. A negative depth is unlimited; depth zero visits only the root.
type Filter struct {
	Root             *workspace.Root
	WarningSink      warning.Warner
	IncludeGlobs     []string
	ExcludeGlobs     []string
	SkipHidden       bool
	SkipVCS          bool
	RespectGitignore bool
	FollowSymlinks   bool
	MaxDepth         int
	MaxVisited       int
	MaxResults       int
}

// Entry is one callback-visible descendant of the walk root.
type Entry struct {
	Path      string
	IsDir     bool
	IsSymlink bool
	Size      int64
	ModTime   time.Time
	Mode      fs.FileMode
}

// Walk traverses descendants of Filter.Root in lexical order. Symlinks are
// reported but never descended, including when FollowSymlinks is true.
func Walk(ctx context.Context, filter *Filter, callback func(Entry) error) error {
	warnings := defaultWarningSink
	if filter != nil && filter.WarningSink != nil {
		warnings = filter.WarningSink
	}
	return walk(ctx, filter, callback, warnings)
}

func walk(ctx context.Context, filter *Filter, callback func(Entry) error, warn warning.Warner) error {
	if err := validateWalk(ctx, filter, callback); err != nil {
		return err
	}
	include, err := compileGlobs("include", filter.IncludeGlobs)
	if err != nil {
		return err
	}
	exclude, err := compileGlobs("exclude", filter.ExcludeGlobs)
	if err != nil {
		return err
	}

	state := walkState{
		filter: filter, callback: callback, warn: warn, root: filter.Root.Dir(), include: include, exclude: exclude,
		maxVisited: defaultedLimit(filter.MaxVisited, defaultMaxVisited),
		maxResults: defaultedLimit(filter.MaxResults, defaultMaxResults),
		rules:      make(map[string][]ignoreRule),
	}
	return filepath.WalkDir(state.root, func(path string, entry fs.DirEntry, walkErr error) error {
		return state.visit(ctx, path, entry, walkErr)
	})
}

type walkState struct {
	filter                 *Filter
	callback               func(Entry) error
	warn                   warning.Warner
	root                   string
	include, exclude       pathglob.Matcher
	maxVisited, maxResults int
	visited, results       int
	rules                  map[string][]ignoreRule
}

func validateWalk(ctx context.Context, filter *Filter, callback func(Entry) error) error {
	if ctx == nil {
		return errors.New("walk context is nil")
	}
	if filter == nil {
		return errors.New("walk filter is nil")
	}
	if filter.Root == nil {
		return errors.New("walk root is nil")
	}
	if callback == nil {
		return errors.New("walk callback is nil")
	}
	return ctx.Err()
}

func defaultedLimit(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func (state *walkState) visit(ctx context.Context, path string, entry fs.DirEntry, walkErr error) error {
	if err := firstError(ctx.Err(), walkErr); err != nil {
		return err
	}
	state.visited++
	relative := state.filter.Root.Rel(path)
	if relative == "." {
		return state.visitRoot(ctx)
	}
	if skip, action := state.filterEntry(path, relative, entry); skip {
		return state.stop(ctx, action)
	}
	return state.emit(ctx, path, relative, entry)
}

func firstError(values ...error) error {
	for _, err := range values {
		if err != nil {
			return err
		}
	}
	return nil
}

func (state *walkState) stop(ctx context.Context, next error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.maxVisited > 0 && state.visited >= state.maxVisited {
		return ErrWalkTruncated
	}
	return next
}

func (state *walkState) visitRoot(ctx context.Context) error {
	if state.filter.RespectGitignore {
		rules := loadIgnoreFile(state.root, filepath.Join(state.root, ".git", "info", "exclude"), ".git/info/exclude", ".", state.warn)
		rules = append(rules, loadIgnoreFile(state.root, filepath.Join(state.root, gitignoreName), gitignoreName, ".", state.warn)...)
		state.rules[state.root] = rules
	}
	if state.filter.MaxDepth == 0 {
		return state.stop(ctx, fs.SkipDir)
	}
	return state.stop(ctx, nil)
}

func (state *walkState) filterEntry(path, relative string, entry fs.DirEntry) (bool, error) {
	directory := entry.IsDir()
	name := entry.Name()
	if name == ".git" || state.filter.SkipVCS && (name == ".hg" || name == ".svn") {
		return true, skipDirectory(directory)
	}
	if state.filter.SkipHidden && strings.HasPrefix(name, ".") {
		return true, skipDirectory(directory)
	}
	parentRules := state.rules[filepath.Dir(path)]
	if state.filter.RespectGitignore && ignoredBy(parentRules, relative, directory) {
		return true, skipDirectory(directory)
	}
	if matchPath(state.exclude, relative, directory) {
		return true, skipDirectory(directory)
	}
	if directory && state.filter.RespectGitignore {
		ignorePath := filepath.Join(path, gitignoreName)
		rules := append([]ignoreRule(nil), parentRules...)
		rules = append(rules, loadIgnoreFile(state.root, ignorePath, ignoreDisplayPath(state.root, ignorePath), relative, state.warn)...)
		state.rules[path] = rules
	}
	return false, nil
}

func skipDirectory(directory bool) error {
	if directory {
		return fs.SkipDir
	}
	return nil
}

func (state *walkState) emit(ctx context.Context, _ string, relative string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	directory := entry.IsDir()
	if !directory && len(state.filter.IncludeGlobs) != 0 && !matchPath(state.include, relative, false) {
		return state.stop(ctx, nil)
	}
	value := Entry{
		Path: filepath.ToSlash(relative), IsDir: directory, IsSymlink: info.Mode()&fs.ModeSymlink != 0,
		Size: info.Size(), ModTime: info.ModTime(), Mode: info.Mode(),
	}
	if err := state.callback(value); err != nil {
		return err
	}
	state.results++
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.maxResults > 0 && state.results >= state.maxResults {
		return ErrWalkTruncated
	}
	depth := strings.Count(relative, "/") + 1
	if directory && state.filter.MaxDepth >= 0 && depth == state.filter.MaxDepth {
		return state.stop(ctx, fs.SkipDir)
	}
	return state.stop(ctx, nil)
}

func compileGlobs(kind string, patterns []string) (pathglob.Matcher, error) {
	matcher, compileErrors := pathglob.Compile(patterns)
	if len(compileErrors) == 0 {
		return matcher, nil
	}
	return nil, fmt.Errorf("compile %s globs: %w", kind, errors.Join(compileErrors...))
}

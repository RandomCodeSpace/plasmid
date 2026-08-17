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

	"github.com/plasmid-dev/plasmid/internal/pathglob"
	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/workspace"
)

const (
	defaultMaxVisited = 100000
	defaultMaxResults = 20000
)

// ErrWalkTruncated reports that a configured visit or result cap stopped traversal.
var ErrWalkTruncated = errors.New("walk truncated")

// Filter controls directory traversal. A zero visit or result cap selects the
// package default. A negative depth is unlimited; depth zero visits only the root.
type Filter struct {
	Root             *workspace.Root
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
	return walk(ctx, filter, callback, defaultWarningSink)
}

func walk(ctx context.Context, filter *Filter, callback func(Entry) error, warn loop.WarningSink) error {
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
	if err := ctx.Err(); err != nil {
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

	maxVisited := filter.MaxVisited
	if maxVisited == 0 {
		maxVisited = defaultMaxVisited
	}
	maxResults := filter.MaxResults
	if maxResults == 0 {
		maxResults = defaultMaxResults
	}

	root := filter.Root.Dir()
	rulesByDirectory := make(map[string][]ignoreRule)
	visited := 0
	results := 0

	return filepath.WalkDir(root, func(path string, dirEntry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}

		visited++
		hitVisitedCap := maxVisited > 0 && visited >= maxVisited
		stop := func(next error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if hitVisitedCap {
				return ErrWalkTruncated
			}
			return next
		}

		relPath := filter.Root.Rel(path)
		if relPath == "." {
			if filter.RespectGitignore {
				rules := loadIgnoreFile(
					root,
					filepath.Join(root, ".git", "info", "exclude"),
					".git/info/exclude",
					".",
					warn,
				)
				rules = append(rules, loadIgnoreFile(
					root,
					filepath.Join(root, ".gitignore"),
					".gitignore",
					".",
					warn,
				)...)
				rulesByDirectory[root] = rules
			}
			if filter.MaxDepth == 0 {
				return stop(fs.SkipDir)
			}
			return stop(nil)
		}

		depth := strings.Count(relPath, "/") + 1
		isDirectory := dirEntry.IsDir()
		if filter.MaxDepth >= 0 && depth > filter.MaxDepth {
			if isDirectory {
				return stop(fs.SkipDir)
			}
			return stop(nil)
		}

		name := dirEntry.Name()
		if name == ".git" || filter.SkipVCS && (name == ".hg" || name == ".svn") {
			if isDirectory {
				return stop(fs.SkipDir)
			}
			return stop(nil)
		}
		if filter.SkipHidden && strings.HasPrefix(name, ".") {
			if isDirectory {
				return stop(fs.SkipDir)
			}
			return stop(nil)
		}

		parentRules := rulesByDirectory[filepath.Dir(path)]
		if filter.RespectGitignore && ignoredBy(parentRules, relPath, isDirectory) {
			if isDirectory {
				return stop(fs.SkipDir)
			}
			return stop(nil)
		}
		if matchPath(exclude, relPath, isDirectory) {
			if isDirectory {
				return stop(fs.SkipDir)
			}
			return stop(nil)
		}

		if isDirectory && filter.RespectGitignore {
			directoryRules := append([]ignoreRule(nil), parentRules...)
			directoryRules = append(directoryRules, loadIgnoreFile(
				root,
				filepath.Join(path, ".gitignore"),
				ignoreDisplayPath(root, filepath.Join(path, ".gitignore")),
				relPath,
				warn,
			)...)
			rulesByDirectory[path] = directoryRules
		}

		info, err := dirEntry.Info()
		if err != nil {
			return err
		}
		isSymlink := info.Mode()&fs.ModeSymlink != 0
		if !isDirectory && len(filter.IncludeGlobs) != 0 && !matchPath(include, relPath, false) {
			return stop(nil)
		}

		entry := Entry{
			Path:      filepath.ToSlash(relPath),
			IsDir:     isDirectory,
			IsSymlink: isSymlink,
			Size:      info.Size(),
			ModTime:   info.ModTime(),
			Mode:      info.Mode(),
		}
		if err := callback(entry); err != nil {
			return err
		}
		results++
		if err := ctx.Err(); err != nil {
			return err
		}
		if maxResults > 0 && results >= maxResults {
			return ErrWalkTruncated
		}
		if isDirectory && filter.MaxDepth >= 0 && depth == filter.MaxDepth {
			return stop(fs.SkipDir)
		}
		return stop(nil)
	})
}

func compileGlobs(kind string, patterns []string) (pathglob.Matcher, error) {
	matcher, compileErrors := pathglob.Compile(patterns)
	if len(compileErrors) == 0 {
		return matcher, nil
	}
	return nil, fmt.Errorf("compile %s globs: %w", kind, errors.Join(compileErrors...))
}

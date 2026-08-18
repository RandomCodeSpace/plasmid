package walk

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/plasmid-dev/plasmid/internal/pathglob"
	"github.com/plasmid-dev/plasmid/warning"
)

type ignoreRule struct {
	base    string
	matcher pathglob.Matcher
	negated bool
}

func loadIgnoreFile(root, path, displayPath, base string, warn warning.Sink) []ignoreRule {
	file, err := openRegularFileWithoutSymlinks(root, path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			warn.Warn(warning.Warning{
				Code:    warning.WarnWalkUnreadableIgnore,
				Source:  "walk",
				Path:    displayPath,
				Message: err.Error(),
			})
		}
		return nil
	}
	defer func() { _ = file.Close() }()
	return parseIgnore(file, displayPath, base, warn)
}

func parseIgnore(reader io.Reader, source, base string, warn warning.Sink) []ignoreRule {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var rules []ignoreRule
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue
		}

		negated := strings.HasPrefix(line, "!")
		pattern := line
		if negated {
			pattern = pattern[1:]
		}
		if !strings.HasPrefix(pattern, "/") && hasNonTrailingPathSeparator(pattern) {
			pattern = "/" + pattern
		}
		matcher, err := pathglob.CompileOne(pattern)
		if err != nil {
			warn.Warn(warning.Warning{
				Code:    warning.WarnWalkInvalidIgnorePattern,
				Source:  "walk",
				Path:    source,
				Line:    lineNumber,
				Message: err.Error(),
			})
			continue
		}
		rules = append(rules, ignoreRule{base: base, matcher: matcher, negated: negated})
	}
	if err := scanner.Err(); err != nil {
		warn.Warn(warning.Warning{
			Code:    warning.WarnWalkUnreadableIgnore,
			Source:  "walk",
			Path:    source,
			Line:    lineNumber + 1,
			Message: err.Error(),
		})
	}
	return rules
}

func openRegularFileWithoutSymlinks(root, path string) (*os.File, error) {
	relative, _ := filepath.Rel(root, path)
	scopedRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = scopedRoot.Close() }()

	pathInfo, err := validateIgnorePath(scopedRoot, root, relative)
	if err != nil {
		return nil, err
	}
	return openValidatedIgnoreFile(scopedRoot, relative, path, pathInfo)
}

func validateIgnorePath(scopedRoot *os.Root, root, relative string) (os.FileInfo, error) {
	components := strings.Split(relative, string(filepath.Separator))
	current := ""
	var pathInfo os.FileInfo
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := scopedRoot.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("ignore path %q is a symbolic link", filepath.Join(root, current))
		}
		if index != len(components)-1 {
			if !info.IsDir() {
				return nil, fmt.Errorf("ignore path component %q is not a directory", filepath.Join(root, current))
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("ignore file %q is not a regular file", filepath.Join(root, current))
		}
		pathInfo = info
	}
	return pathInfo, nil
}

func openValidatedIgnoreFile(scopedRoot *os.Root, relative, path string, pathInfo os.FileInfo) (*os.File, error) {
	file, err := scopedRoot.Open(relative)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("ignore file %q changed while opening", path)
	}
	return file, nil
}

func hasNonTrailingPathSeparator(pattern string) bool {
	for index := 0; index < len(pattern); index++ {
		if pattern[index] == '\\' && index+1 < len(pattern) {
			if pattern[index+1] == '/' {
				return true
			}
			index++
			continue
		}
		if pattern[index] == '/' && index != len(pattern)-1 {
			return true
		}
	}
	return false
}

func ignoredBy(rules []ignoreRule, relPath string, isDir bool) bool {
	ignored := false
	for _, rule := range rules {
		candidate, ok := relativeToRuleBase(relPath, rule.base)
		if !ok {
			continue
		}
		if !matchPath(rule.matcher, candidate, isDir) {
			continue
		}
		ignored = !rule.negated
	}
	return ignored
}

func relativeToRuleBase(relPath, base string) (string, bool) {
	if base == "." || base == "" {
		return relPath, true
	}
	prefix := base + "/"
	if !strings.HasPrefix(relPath, prefix) {
		return "", false
	}
	return strings.TrimPrefix(relPath, prefix), true
}

func matchPath(matcher pathglob.Matcher, relPath string, isDir bool) bool {
	if matcher.Match(relPath) {
		return true
	}
	return isDir && matcher.Match(strings.TrimSuffix(relPath, "/")+"/")
}

var defaultWarningSink warning.Sink = warning.SlogSink{}

func ignoreDisplayPath(root, path string) string {
	relative, _ := filepath.Rel(root, path)
	return filepath.ToSlash(relative)
}

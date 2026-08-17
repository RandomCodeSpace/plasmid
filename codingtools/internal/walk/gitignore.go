package walk

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/plasmid-dev/plasmid/internal/pathglob"
	"github.com/plasmid-dev/plasmid/loop"
)

type ignoreRule struct {
	base    string
	matcher pathglob.Matcher
	negated bool
}

func loadIgnoreFile(root, path, displayPath, base string, warn loop.WarningSink) []ignoreRule {
	file, err := openRegularFileWithoutSymlinks(root, path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			warn.Warn(loop.Warning{
				Code:    loop.WarnWalkUnreadableIgnore,
				Source:  "walk",
				Path:    displayPath,
				Message: err.Error(),
			})
		}
		return nil
	}
	defer file.Close()
	return parseIgnore(file, displayPath, base, warn)
}

func parseIgnore(reader io.Reader, source, base string, warn loop.WarningSink) []ignoreRule {
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
			warn.Warn(loop.Warning{
				Code:    loop.WarnWalkInvalidIgnorePattern,
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
		warn.Warn(loop.Warning{
			Code:    loop.WarnWalkUnreadableIgnore,
			Source:  "walk",
			Path:    source,
			Line:    lineNumber + 1,
			Message: err.Error(),
		})
	}
	return rules
}

func openRegularFileWithoutSymlinks(root, path string) (*os.File, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("ignore file %q is outside root", path)
	}
	scopedRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer scopedRoot.Close()

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

	file, err := scopedRoot.Open(relative)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		file.Close()
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
	if matcher == nil {
		return false
	}
	if matcher.Match(relPath) {
		return true
	}
	return isDir && matcher.Match(strings.TrimSuffix(relPath, "/")+"/")
}

type slogWarningSink struct {
	logger *slog.Logger
}

func (s slogWarningSink) Warn(warning loop.Warning) {
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn(warning.String())
}

var defaultWarningSink loop.WarningSink = slogWarningSink{}

func ignoreDisplayPath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Sprintf("%s", filepath.ToSlash(path))
	}
	return filepath.ToSlash(relative)
}

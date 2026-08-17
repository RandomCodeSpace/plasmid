// Package pathglob provides deterministic matching for slash-separated paths.
package pathglob

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	ErrEmptyPattern     = errors.New("path glob is empty")
	ErrUnsupportedBrace = errors.New("path glob brace expansion is unsupported")
)

// Matcher matches slash-separated, root-relative paths.
type Matcher interface {
	Match(relPath string) bool
	Patterns() []string
}

type matcher struct {
	patterns []string
	rules    []rule
}

type rule struct {
	negative bool
	regexp   *regexp.Regexp
}

// Compile compiles patterns and returns a matcher containing every valid rule.
// Invalid rules are omitted and returned in input order.
func Compile(patterns []string) (Matcher, []error) {
	compiled := &matcher{patterns: append([]string(nil), patterns...)}
	var compileErrors []error
	for _, pattern := range patterns {
		rule, err := compileRule(pattern)
		if err != nil {
			compileErrors = append(compileErrors, fmt.Errorf("pattern %q: %w", pattern, err))
			continue
		}
		compiled.rules = append(compiled.rules, rule)
	}
	return compiled, compileErrors
}

// CompileOne compiles one pattern.
func CompileOne(pattern string) (Matcher, error) {
	compiled, compileErrors := Compile([]string{pattern})
	if len(compileErrors) != 0 {
		return nil, compileErrors[0]
	}
	return compiled, nil
}

// SplitList splits a comma-separated pattern list, trimming and omitting empty items.
func SplitList(value string) []string {
	parts := strings.Split(value, ",")
	patterns := make([]string, 0, len(parts))
	for _, part := range parts {
		if pattern := strings.TrimSpace(part); pattern != "" {
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

func (m *matcher) Match(relPath string) bool {
	relPath = strings.TrimPrefix(relPath, "./")
	relPath = strings.TrimPrefix(relPath, "/")
	matched := false
	for _, rule := range m.rules {
		if rule.regexp.MatchString(relPath) {
			matched = !rule.negative
		}
	}
	return matched
}

func (m *matcher) Patterns() []string {
	return append([]string(nil), m.patterns...)
}

func compileRule(pattern string) (rule, error) {
	if pattern == "" {
		return rule{}, ErrEmptyPattern
	}

	negative := false
	if pattern[0] == '!' {
		negative = true
		pattern = pattern[1:]
		if pattern == "" {
			return rule{}, ErrEmptyPattern
		}
	}

	anchored := strings.HasPrefix(pattern, "/")
	if anchored {
		pattern = pattern[1:]
	}
	directoryOnly := hasUnescapedSuffix(pattern, '/')
	if directoryOnly {
		pattern = pattern[:len(pattern)-1]
	}
	if pattern == "" {
		return rule{}, ErrEmptyPattern
	}

	body, err := globRegexp(pattern)
	if err != nil {
		return rule{}, err
	}
	prefix := "(?:^|.*/)"
	if anchored {
		prefix = "^"
	}
	suffix := "$"
	if directoryOnly {
		suffix = "(?:/.*)$"
	}
	compiled, err := regexp.Compile(prefix + body + suffix)
	if err != nil {
		return rule{}, fmt.Errorf("invalid character class: %w", err)
	}
	return rule{negative: negative, regexp: compiled}, nil
}

func globRegexp(pattern string) (string, error) {
	var expression strings.Builder
	for index := 0; index < len(pattern); {
		switch pattern[index] {
		case '\\':
			if index+1 == len(pattern) {
				expression.WriteString(regexp.QuoteMeta("\\"))
				index++
				continue
			}
			expression.WriteString(regexp.QuoteMeta(pattern[index+1 : index+2]))
			index += 2
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				index += 2
				for index < len(pattern) && pattern[index] == '*' {
					index++
				}
				if index < len(pattern) && pattern[index] == '/' {
					expression.WriteString("(?:.*/)?")
					index++
				} else {
					expression.WriteString(".*")
				}
				continue
			}
			expression.WriteString("[^/]*")
			index++
		case '?':
			expression.WriteString("[^/]")
			index++
		case '[':
			end, class, err := characterClass(pattern, index)
			if err != nil {
				return "", err
			}
			expression.WriteString(class)
			index = end
		case '{', '}':
			return "", ErrUnsupportedBrace
		default:
			expression.WriteString(regexp.QuoteMeta(pattern[index : index+1]))
			index++
		}
	}
	return expression.String(), nil
}

func characterClass(pattern string, start int) (int, string, error) {
	index := start + 1
	if index >= len(pattern) {
		return 0, "", errors.New("unterminated character class")
	}
	var class strings.Builder
	class.WriteByte('[')
	if pattern[index] == '!' {
		class.WriteByte('^')
		index++
	}
	contentStart := index
	for index < len(pattern) {
		if pattern[index] == '/' {
			return 0, "", errors.New("character class cannot contain a path separator")
		}
		if pattern[index] == '\\' && index+1 < len(pattern) {
			class.WriteByte('\\')
			class.WriteByte(pattern[index+1])
			index += 2
			continue
		}
		if pattern[index] == ']' {
			if index == contentStart {
				return 0, "", errors.New("empty character class")
			}
			class.WriteByte(']')
			return index + 1, class.String(), nil
		}
		class.WriteByte(pattern[index])
		index++
	}
	return 0, "", errors.New("unterminated character class")
}

func hasUnescapedSuffix(value string, suffix byte) bool {
	if len(value) == 0 || value[len(value)-1] != suffix {
		return false
	}
	backslashes := 0
	for index := len(value) - 2; index >= 0 && value[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 == 0
}

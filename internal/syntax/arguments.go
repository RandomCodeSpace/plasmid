package syntax

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// NamedArgument is one deterministic name/value pair.
type NamedArgument struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Arguments is one parsed invocation. Positionals are one-based when used by
// substitution.
type Arguments struct {
	Raw         string          `json:"raw"`
	Declared    []string        `json:"declared"`
	Positionals []string        `json:"positionals"`
	Named       []NamedArgument `json:"named"`
}

// ParseArguments tokenizes shell-like quoting without performing expansion.
// Only declared names are interpreted as name=value arguments.
func ParseArguments(source string, declared []string) (Arguments, error) {
	arguments := Arguments{
		Raw: source, Declared: append([]string(nil), declared...),
		Positionals: []string{}, Named: []NamedArgument{},
	}
	declaredSet := make(map[string]bool, len(declared))
	for _, name := range declared {
		if !validArgumentName(name) {
			return Arguments{}, fmt.Errorf("invalid declared argument %q", name)
		}
		if declaredSet[name] {
			return Arguments{}, fmt.Errorf("duplicate declared argument %q", name)
		}
		declaredSet[name] = true
	}
	tokens, err := splitArguments(source)
	if err != nil {
		return Arguments{}, err
	}
	seen := make(map[string]bool)
	for _, token := range tokens {
		name, value, hasValue := strings.Cut(token, "=")
		if hasValue && declaredSet[name] {
			if seen[name] {
				return Arguments{}, fmt.Errorf("duplicate named argument %q", name)
			}
			seen[name] = true
			arguments.Named = append(arguments.Named, NamedArgument{Name: name, Value: value})
			continue
		}
		arguments.Positionals = append(arguments.Positionals, token)
	}
	return arguments, nil
}

func splitArguments(source string) ([]string, error) {
	scanner := argumentScanner{}
	for _, character := range source {
		scanner.consume(character)
	}
	if scanner.escaped {
		return nil, errors.New("argument list ends with an escape")
	}
	if scanner.quote != 0 {
		return nil, errors.New("argument list has an unterminated quote")
	}
	scanner.flush()
	return scanner.tokens, nil
}

type argumentScanner struct {
	tokens  []string
	token   strings.Builder
	started bool
	quote   rune
	escaped bool
}

func (s *argumentScanner) consume(character rune) {
	if s.escaped {
		s.write(character)
		s.escaped = false
		return
	}
	if character == '\\' && s.quote != '\'' {
		s.escaped = true
		s.started = true
		return
	}
	if s.quote != 0 {
		s.consumeQuoted(character)
		return
	}
	if character == '\'' || character == '"' {
		s.quote = character
		s.started = true
		return
	}
	if unicode.IsSpace(character) {
		s.flush()
		return
	}
	s.write(character)
}

func (s *argumentScanner) consumeQuoted(character rune) {
	if character == s.quote {
		s.quote = 0
		s.started = true
		return
	}
	s.write(character)
}

func (s *argumentScanner) write(character rune) {
	s.token.WriteRune(character)
	s.started = true
}

func (s *argumentScanner) flush() {
	if !s.started {
		return
	}
	s.tokens = append(s.tokens, s.token.String())
	s.token.Reset()
	s.started = false
}

func (a Arguments) namedValue(name string) (string, bool) {
	for _, argument := range a.Named {
		if argument.Name == name {
			return argument.Value, true
		}
	}
	return "", false
}

func (a Arguments) declared(name string) bool {
	for _, declared := range a.Declared {
		if declared == name {
			return true
		}
	}
	return false
}

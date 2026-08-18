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
	var tokens []string
	var token strings.Builder
	started := false
	var quote rune
	escaped := false
	flush := func() {
		if started {
			tokens = append(tokens, token.String())
			token.Reset()
			started = false
		}
	}
	for _, character := range source {
		if escaped {
			token.WriteRune(character)
			started = true
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
				started = true
			} else {
				token.WriteRune(character)
				started = true
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			started = true
			continue
		}
		if unicode.IsSpace(character) {
			flush()
			continue
		}
		token.WriteRune(character)
		started = true
	}
	if escaped {
		return nil, errors.New("argument list ends with an escape")
	}
	if quote != 0 {
		return nil, errors.New("argument list has an unterminated quote")
	}
	flush()
	return tokens, nil
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

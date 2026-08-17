package syntax

import (
	"strconv"
	"strings"

	"github.com/plasmid-dev/plasmid/warning"
)

// Variables are the only harness values available to substitution. Process
// environment variables are deliberately absent.
type Variables struct {
	SessionID  string `json:"session_id"`
	SkillDir   string `json:"skill_dir"`
	ProjectDir string `json:"project_dir"`
	PluginRoot string `json:"plugin_root"`
	PluginData string `json:"plugin_data"`
	Effort     string `json:"effort"`
}

// Substitutions contains all deterministic expansion inputs.
type Substitutions struct {
	Arguments Arguments
	Variables Variables
}

// Substitute expands arguments and explicit harness variables in one
// non-recursive pass. Unresolved tokens remain byte-for-byte intact.
func Substitute(source, path string, values Substitutions) (string, []warning.Warning) {
	var output strings.Builder
	var warnings []warning.Warning
	line := 1
	for index := 0; index < len(source); {
		if source[index] != '$' {
			output.WriteByte(source[index])
			if source[index] == '\n' {
				line++
			}
			index++
			continue
		}
		name, end, kind := substitutionToken(source, index)
		if kind == substitutionNone {
			output.WriteByte(source[index])
			index++
			continue
		}
		replacement, found, missingArgument := resolveSubstitution(name, kind, values)
		if found {
			output.WriteString(replacement)
		} else {
			output.WriteString(source[index:end])
			code := warning.WarnSyntaxUnresolvedVariable
			if missingArgument {
				code = warning.WarnSyntaxMissingArgument
			}
			warnings = append(warnings, syntaxWarning(code, path, line, "substitution is unresolved"))
		}
		index = end
	}
	return output.String(), warnings
}

type substitutionKind uint8

const (
	substitutionNone substitutionKind = iota
	substitutionArguments
	substitutionPositional
	substitutionNamed
)

func substitutionToken(source string, start int) (string, int, substitutionKind) {
	remaining := source[start:]
	if strings.HasPrefix(remaining, "$ARGUMENTS") && tokenBoundary(source, start+len("$ARGUMENTS")) {
		return "ARGUMENTS", start + len("$ARGUMENTS"), substitutionArguments
	}
	if start+1 < len(source) && source[start+1] >= '0' && source[start+1] <= '9' {
		end := start + 2
		for end < len(source) && source[end] >= '0' && source[end] <= '9' {
			end++
		}
		return source[start+1 : end], end, substitutionPositional
	}
	if strings.HasPrefix(remaining, "${") {
		close := strings.IndexByte(remaining[2:], '}')
		if close >= 0 {
			end := start + 2 + close + 1
			name := source[start+2 : end-1]
			if validVariableName(name) {
				return name, end, substitutionNamed
			}
		}
	}
	return "", start + 1, substitutionNone
}

func resolveSubstitution(name string, kind substitutionKind, values Substitutions) (string, bool, bool) {
	switch kind {
	case substitutionArguments:
		return values.Arguments.Raw, true, false
	case substitutionPositional:
		position, err := strconv.Atoi(name)
		if err == nil && position > 0 && position <= len(values.Arguments.Positionals) {
			return values.Arguments.Positionals[position-1], true, false
		}
		return "", false, true
	case substitutionNamed:
		if value, ok := values.Arguments.namedValue(name); ok {
			return value, true, false
		}
		if values.Arguments.declared(name) {
			return "", false, true
		}
		return values.Variables.lookup(name)
	default:
		return "", false, false
	}
}

func (v Variables) lookup(name string) (string, bool, bool) {
	values := map[string]string{
		"SESSION_ID":               v.SessionID,
		"CLAUDE_SESSION_ID":        v.SessionID,
		"SKILL_DIR":                v.SkillDir,
		"PROJECT_DIR":              v.ProjectDir,
		"CLAUDE_PROJECT_DIR":       v.ProjectDir,
		"PLUGIN_ROOT":              v.PluginRoot,
		"CLAUDE_PLUGIN_ROOT":       v.PluginRoot,
		"PLUGIN_DATA":              v.PluginData,
		"CLAUDE_PLUGIN_DATA":       v.PluginData,
		"EFFORT":                   v.Effort,
		"CLAUDE_CODE_EFFORT_LEVEL": v.Effort,
	}
	value, ok := values[name]
	return value, ok && value != "", false
}

func tokenBoundary(source string, index int) bool {
	return index >= len(source) || !isVariableCharacter(source[index])
}

func validVariableName(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if !isVariableCharacter(value[index]) {
			return false
		}
	}
	return true
}

func isVariableCharacter(character byte) bool {
	return (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
		(character >= '0' && character <= '9') || character == '_'
}

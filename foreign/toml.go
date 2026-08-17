package foreign

import (
	"strconv"
	"strings"

	"github.com/plasmid-dev/plasmid/warning"
)

type tomlSection struct {
	path   []string
	values map[string]tomlScalar
}

type tomlScalarKind uint8

const (
	tomlScalarInvalid tomlScalarKind = iota
	tomlScalarString
	tomlScalarBoolean
)

type tomlScalar struct {
	kind  tomlScalarKind
	value string
	line  int
}

func (s *scanner) parseTOML(path string, data []byte) []tomlSection {
	sections := []tomlSection{}
	current := -1
	for lineIndex, rawLine := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if err := s.check(); err != nil {
			return sections
		}
		line := strings.TrimSpace(stripTOMLComment(rawLine))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && !strings.HasPrefix(line, "[[") {
			pathParts, ok := splitTOMLPath(strings.TrimSpace(line[1 : len(line)-1]))
			if !ok {
				s.addWarningLine(warning.WarnForeignTOMLUnsupported, path, lineIndex+1, "TOML section is unsupported")
				current = -1
				continue
			}
			sections = append(sections, tomlSection{path: pathParts, values: make(map[string]tomlScalar)})
			current = len(sections) - 1
			continue
		}
		if current < 0 {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" {
			s.addWarningLine(warning.WarnForeignTOMLUnsupported, path, lineIndex+1, "TOML entry is unsupported")
			continue
		}
		decoded, valid := decodeTOMLScalar(value)
		if !valid {
			if key == "command" || key == "url" || key == "enabled" {
				s.addWarningLine(warning.WarnForeignTOMLUnsupported, path, lineIndex+1, "TOML typed value is unsupported")
				sections[current].values[key] = tomlScalar{kind: tomlScalarInvalid, line: lineIndex + 1}
			}
			continue
		}
		decoded.line = lineIndex + 1
		if (key == "command" || key == "url") && decoded.kind != tomlScalarString {
			s.addWarningLine(warning.WarnForeignTOMLUnsupported, path, lineIndex+1, "TOML identity value must be a string")
			decoded.kind = tomlScalarInvalid
		}
		sections[current].values[key] = decoded
	}
	return sections
}

func stripTOMLComment(value string) string {
	var quote byte
	for index := 0; index < len(value); index++ {
		character := value[index]
		if quote != 0 {
			if quote == '"' && character == '\\' {
				index++
				continue
			}
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '"' || character == '\'' {
			quote = character
			continue
		}
		if character == '#' {
			return value[:index]
		}
	}
	return value
}

func splitTOMLPath(value string) ([]string, bool) {
	var result []string
	var part strings.Builder
	var quote byte
	flush := func() bool {
		text := strings.TrimSpace(part.String())
		part.Reset()
		if text == "" {
			return false
		}
		if text[0] == '"' || text[0] == '\'' {
			decoded, ok := decodeTOMLScalar(text)
			if !ok || decoded.kind != tomlScalarString {
				return false
			}
			text = decoded.value
		}
		result = append(result, text)
		return true
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if quote != 0 {
			part.WriteByte(character)
			if quote == '"' && character == '\\' && index+1 < len(value) {
				index++
				part.WriteByte(value[index])
				continue
			}
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '"' || character == '\'' {
			quote = character
			part.WriteByte(character)
			continue
		}
		if character == '.' {
			if !flush() {
				return nil, false
			}
			continue
		}
		part.WriteByte(character)
	}
	if quote != 0 || !flush() {
		return nil, false
	}
	return result, true
}

func decodeTOMLScalar(value string) (tomlScalar, bool) {
	if value == "true" || value == "false" {
		return tomlScalar{kind: tomlScalarBoolean, value: value}, true
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return tomlScalar{kind: tomlScalarString, value: value[1 : len(value)-1]}, true
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		decoded, err := strconv.Unquote(value)
		return tomlScalar{kind: tomlScalarString, value: decoded}, err == nil
	}
	return tomlScalar{}, false
}

func tomlString(values map[string]tomlScalar, key string) string {
	value := values[key]
	if value.kind != tomlScalarString {
		return ""
	}
	return value.value
}

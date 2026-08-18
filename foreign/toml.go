package foreign

import (
	"context"
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
	tomlScalarStringArray
	tomlScalarStringMap
)

type tomlScalar struct {
	kind  tomlScalarKind
	value string
	list  []string
	items map[string]string
	line  int
}

func (s *scanner) parseTOML(ctx context.Context, path string, data []byte) []tomlSection {
	parser := tomlParser{scanner: s, path: path, current: -1}
	for lineIndex, rawLine := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if err := checkContext(ctx); err != nil {
			return parser.sections
		}
		line := strings.TrimSpace(stripTOMLComment(rawLine))
		parser.consumeLine(line, lineIndex+1)
	}
	return parser.sections
}

type tomlParser struct {
	scanner  *scanner
	path     string
	sections []tomlSection
	current  int
}

func (p *tomlParser) consumeLine(line string, number int) {
	if line == "" {
		return
	}
	if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && !strings.HasPrefix(line, "[[") {
		p.beginSection(line, number)
		return
	}
	if p.current >= 0 {
		p.addEntry(line, number)
	}
}

func (p *tomlParser) beginSection(line string, number int) {
	path, ok := splitTOMLPath(strings.TrimSpace(line[1 : len(line)-1]))
	if !ok {
		p.scanner.addWarningLine(warning.WarnForeignTOMLUnsupported, p.path, number, "TOML section is unsupported")
		p.current = -1
		return
	}
	p.sections = append(p.sections, tomlSection{path: path, values: make(map[string]tomlScalar)})
	p.current = len(p.sections) - 1
}

func (p *tomlParser) addEntry(line string, number int) {
	key, value, ok := strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		p.scanner.addWarningLine(warning.WarnForeignTOMLUnsupported, p.path, number, "TOML entry is unsupported")
		return
	}
	decoded, valid := decodeTOMLScalar(strings.TrimSpace(value))
	if !valid {
		p.addInvalidEntry(key, number)
		return
	}
	decoded.line = number
	if (key == "command" || key == "url") && decoded.kind != tomlScalarString {
		p.scanner.addWarningLine(warning.WarnForeignTOMLUnsupported, p.path, number, "TOML identity value must be a string")
		decoded.kind = tomlScalarInvalid
	}
	p.sections[p.current].values[key] = decoded
}

func (p *tomlParser) addInvalidEntry(key string, number int) {
	if key != "command" && key != "url" && key != "enabled" {
		return
	}
	p.scanner.addWarningLine(warning.WarnForeignTOMLUnsupported, p.path, number, "TOML typed value is unsupported")
	p.sections[p.current].values[key] = tomlScalar{kind: tomlScalarInvalid, line: number}
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
	parser := tomlPathParser{}
	for index := 0; index < len(value); index++ {
		index = parser.consume(value, index)
		if !parser.valid {
			return nil, false
		}
	}
	if parser.quote != 0 || !parser.flush() {
		return nil, false
	}
	return parser.result, true
}

type tomlPathParser struct {
	result []string
	part   strings.Builder
	quote  byte
	valid  bool
}

func (p *tomlPathParser) consume(value string, index int) int {
	p.valid = true
	character := value[index]
	if p.quote != 0 {
		p.part.WriteByte(character)
		if p.quote == '"' && character == '\\' && index+1 < len(value) {
			index++
			p.part.WriteByte(value[index])
		} else if character == p.quote {
			p.quote = 0
		}
		return index
	}
	switch character {
	case '"', '\'':
		p.quote = character
		p.part.WriteByte(character)
	case '.':
		p.valid = p.flush()
	default:
		p.part.WriteByte(character)
	}
	return index
}

func (p *tomlPathParser) flush() bool {
	text := strings.TrimSpace(p.part.String())
	p.part.Reset()
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
	p.result = append(p.result, text)
	return true
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
	if len(value) >= 2 && value[0] == '[' && value[len(value)-1] == ']' {
		return decodeTOMLArray(value[1 : len(value)-1])
	}
	if len(value) >= 2 && value[0] == '{' && value[len(value)-1] == '}' {
		return decodeTOMLMap(value[1 : len(value)-1])
	}
	return tomlScalar{}, false
}

func decodeTOMLArray(value string) (tomlScalar, bool) {
	parts, ok := splitTOMLValues(value)
	if !ok {
		return tomlScalar{}, false
	}
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		decoded, valid := decodeTOMLScalar(part)
		if !valid || decoded.kind != tomlScalarString {
			return tomlScalar{}, false
		}
		result = append(result, decoded.value)
	}
	return tomlScalar{kind: tomlScalarStringArray, list: result}, true
}

func decodeTOMLMap(value string) (tomlScalar, bool) {
	parts, ok := splitTOMLValues(value)
	if !ok {
		return tomlScalar{}, false
	}
	result := make(map[string]string, len(parts))
	for _, part := range parts {
		key, item, valid := decodeTOMLMapItem(part)
		if !valid {
			return tomlScalar{}, false
		}
		result[key] = item
	}
	return tomlScalar{kind: tomlScalarStringMap, items: result}, true
}

func decodeTOMLMapItem(part string) (string, string, bool) {
	key, raw, found := strings.Cut(part, "=")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, "\"") || strings.HasPrefix(key, "'") {
		decoded, valid := decodeTOMLScalar(key)
		if !valid || decoded.kind != tomlScalarString {
			return "", "", false
		}
		key = decoded.value
	}
	decoded, valid := decodeTOMLScalar(strings.TrimSpace(raw))
	return key, decoded.value, key != "" && valid && decoded.kind == tomlScalarString
}

func splitTOMLValues(value string) ([]string, bool) {
	if strings.TrimSpace(value) == "" {
		return []string{}, true
	}
	splitter := tomlValueSplitter{value: value}
	for index := 0; index < len(value); index++ {
		index = splitter.consume(index)
		if !splitter.valid {
			return nil, false
		}
	}
	if splitter.quote != 0 || !splitter.appendPart(len(value)) {
		return nil, false
	}
	return splitter.result, true
}

type tomlValueSplitter struct {
	value  string
	result []string
	quote  byte
	start  int
	valid  bool
}

func (s *tomlValueSplitter) consume(index int) int {
	s.valid = true
	character := s.value[index]
	if s.quote != 0 {
		if s.quote == '"' && character == '\\' && index+1 < len(s.value) {
			return index + 1
		}
		if character == s.quote {
			s.quote = 0
		}
		return index
	}
	switch character {
	case '"', '\'':
		s.quote = character
	case ',':
		s.valid = s.appendPart(index)
		s.start = index + 1
	}
	return index
}

func (s *tomlValueSplitter) appendPart(end int) bool {
	part := strings.TrimSpace(s.value[s.start:end])
	if part == "" {
		return false
	}
	s.result = append(s.result, part)
	return true
}

func tomlString(values map[string]tomlScalar, key string) string {
	value := values[key]
	if value.kind != tomlScalarString {
		return ""
	}
	return value.value
}

func tomlStrings(values map[string]tomlScalar, key string) []string {
	value := values[key]
	if value.kind != tomlScalarStringArray {
		return nil
	}
	return append([]string(nil), value.list...)
}

func tomlStringMap(values map[string]tomlScalar, key string) map[string]string {
	value := values[key]
	if value.kind != tomlScalarStringMap {
		return nil
	}
	return cloneStringMap(value.items)
}

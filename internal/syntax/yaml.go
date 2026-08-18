// Package syntax owns Plasmid's framework-free syntax document primitives.
package syntax

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// YAMLKind identifies a value in the supported YAML subset.
type YAMLKind uint8

const (
	YAMLScalar YAMLKind = iota + 1
	YAMLSequence
	YAMLMapping
)

// YAMLValue is a value in the supported YAML subset. Mapping order and
// duplicate keys are retained so document projection can warn deterministically.
type YAMLValue struct {
	Kind     YAMLKind    `json:"kind"`
	Line     int         `json:"line"`
	Scalar   string      `json:"scalar"`
	Sequence []YAMLValue `json:"sequence"`
	Mapping  []YAMLField `json:"mapping"`
}

// YAMLField is one ordered mapping entry.
type YAMLField struct {
	Name  string    `json:"name"`
	Line  int       `json:"line"`
	Value YAMLValue `json:"value"`
}

// YAMLError reports a stable one-based source line.
type YAMLError struct {
	Line    int
	Message string
}

func (e *YAMLError) Error() string {
	return fmt.Sprintf("line %d: %s", e.Line, e.Message)
}

type yamlLine struct {
	blank  bool
	indent int
	line   int
	text   string
	raw    string
}

// ParseYAML parses the intentionally small YAML subset used by syntax
// frontmatter. It supports ordered mappings, nested mappings, scalar
// sequences, flow scalar sequences, quoted scalars, and literal or folded
// block scalars. Aliases, tags, flow mappings, and complex keys are rejected.
func ParseYAML(source string) (YAMLValue, error) {
	lines, err := tokenizeYAML(source)
	if err != nil {
		return YAMLValue{}, err
	}
	start := skipYAMLBlank(lines, 0)
	if start == len(lines) {
		return YAMLValue{Kind: YAMLMapping, Line: 1, Mapping: []YAMLField{}}, nil
	}
	if lines[start].indent != 0 {
		return YAMLValue{}, yamlError(lines[start], "root mapping must start at column 1")
	}
	value, next, err := parseYAMLBlock(lines, start, 0, 0)
	if err != nil {
		return YAMLValue{}, err
	}
	if value.Kind != YAMLMapping {
		return YAMLValue{}, yamlError(lines[0], "root value must be a mapping")
	}
	next = skipYAMLBlank(lines, next)
	if next != len(lines) {
		return YAMLValue{}, yamlError(lines[next], "unexpected indentation")
	}
	return value, nil
}

func tokenizeYAML(source string) ([]yamlLine, error) {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	source = strings.ReplaceAll(source, "\r", "\n")
	rawLines := strings.Split(source, "\n")
	lines := make([]yamlLine, 0, len(rawLines))
	for index, raw := range rawLines {
		indent := 0
		for indent < len(raw) && raw[indent] == ' ' {
			indent++
		}
		if indent < len(raw) && raw[indent] == '\t' {
			return nil, &YAMLError{Line: index + 1, Message: "tab indentation is unsupported"}
		}
		text := strings.TrimSpace(stripYAMLComment(raw[indent:]))
		lines = append(lines, yamlLine{blank: text == "", indent: indent, line: index + 1, text: text, raw: raw})
	}
	return lines, nil
}

func stripYAMLComment(value string) string {
	var quote byte
	for index := 0; index < len(value); index++ {
		character := value[index]
		if next, quoted := advanceYAMLQuote(value, index, &quote); quoted {
			index = next
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == '#' && (index == 0 || value[index-1] == ' ' || value[index-1] == '\t') {
			return strings.TrimRight(value[:index], " \t")
		}
	}
	return value
}

func advanceYAMLQuote(value string, index int, quote *byte) (int, bool) {
	if *quote == 0 {
		return index, false
	}
	character := value[index]
	if *quote == '"' && character == '\\' {
		return index + 1, true
	}
	if character != *quote {
		return index, true
	}
	if *quote == '\'' && index+1 < len(value) && value[index+1] == '\'' {
		return index + 1, true
	}
	*quote = 0
	return index, true
}

func parseYAMLBlock(lines []yamlLine, start, indent, depth int) (YAMLValue, int, error) {
	start = skipYAMLBlank(lines, start)
	if start >= len(lines) {
		return YAMLValue{}, start, errors.New("empty YAML block")
	}
	if depth > 32 {
		return YAMLValue{}, start, yamlError(lines[start], "mapping nesting exceeds 32 levels")
	}
	if lines[start].indent != indent {
		return YAMLValue{}, start, yamlError(lines[start], "inconsistent indentation")
	}
	if strings.HasPrefix(lines[start].text, "-") {
		return parseYAMLSequence(lines, start, indent)
	}
	return parseYAMLMapping(lines, start, indent, depth)
}

func parseYAMLMapping(lines []yamlLine, start, indent, depth int) (YAMLValue, int, error) {
	value := YAMLValue{Kind: YAMLMapping, Line: lines[start].line, Mapping: []YAMLField{}}
	index := start
	for {
		index = skipYAMLBlank(lines, index)
		if index >= len(lines) || lines[index].indent < indent {
			break
		}
		if lines[index].indent > indent {
			return YAMLValue{}, index, yamlError(lines[index], "unexpected indentation")
		}
		line := lines[index]
		name, raw, err := parseYAMLMappingHeader(line)
		if err != nil {
			return YAMLValue{}, index, err
		}
		field := YAMLField{Name: name, Line: line.line}
		index++
		field.Value, index, err = parseYAMLMappingValue(lines, index, indent, depth, line, raw)
		if err != nil {
			return YAMLValue{}, index, err
		}
		value.Mapping = append(value.Mapping, field)
	}
	return value, index, nil
}

func parseYAMLMappingHeader(line yamlLine) (string, string, error) {
	if strings.HasPrefix(line.text, "-") {
		return "", "", yamlError(line, "cannot mix sequence and mapping entries")
	}
	name, raw, ok := splitYAMLField(line.text)
	if !ok {
		return "", "", yamlError(line, "mapping entry must use key: value syntax")
	}
	if err := validateYAMLKey(name); err != nil {
		return "", "", yamlError(line, err.Error())
	}
	return name, raw, nil
}

func parseYAMLMappingValue(lines []yamlLine, index, indent, depth int, line yamlLine, raw string) (YAMLValue, int, error) {
	if raw == "|" || raw == ">" {
		block, next, err := parseYAMLBlockScalar(lines, index, indent, raw == ">")
		return YAMLValue{Kind: YAMLScalar, Line: line.line, Scalar: block}, next, err
	}
	if raw != "" {
		scalar, err := parseYAMLScalar(raw, line.line)
		return scalar, index, err
	}
	childStart := skipYAMLBlank(lines, index)
	if childStart >= len(lines) || lines[childStart].indent <= indent {
		return YAMLValue{Kind: YAMLScalar, Line: line.line}, index, nil
	}
	return parseYAMLBlock(lines, childStart, lines[childStart].indent, depth+1)
}

func parseYAMLSequence(lines []yamlLine, start, indent int) (YAMLValue, int, error) {
	value := YAMLValue{Kind: YAMLSequence, Line: lines[start].line, Sequence: []YAMLValue{}}
	index := start
	for {
		index = skipYAMLBlank(lines, index)
		if index >= len(lines) || lines[index].indent < indent {
			break
		}
		if lines[index].indent > indent {
			return YAMLValue{}, index, yamlError(lines[index], "nested sequence entries are unsupported")
		}
		scalar, err := parseYAMLSequenceEntry(lines[index])
		if err != nil {
			return YAMLValue{}, index, err
		}
		value.Sequence = append(value.Sequence, scalar)
		index++
		if next := skipYAMLBlank(lines, index); next < len(lines) && lines[next].indent > indent {
			return YAMLValue{}, next, yamlError(lines[next], "nested sequence entries are unsupported")
		}
	}
	return value, index, nil
}

func parseYAMLSequenceEntry(line yamlLine) (YAMLValue, error) {
	if line.text != "-" && !strings.HasPrefix(line.text, "- ") {
		return YAMLValue{}, yamlError(line, "cannot mix mapping and sequence entries")
	}
	raw := strings.TrimSpace(strings.TrimPrefix(line.text, "-"))
	if raw == "" {
		return YAMLValue{}, yamlError(line, "empty sequence entries are unsupported")
	}
	return parseYAMLScalar(raw, line.line)
}

func parseYAMLBlockScalar(lines []yamlLine, start, parentIndent int, folded bool) (string, int, error) {
	firstContent := firstYAMLBlockContent(lines, start)
	if firstContent >= len(lines) || lines[firstContent].indent <= parentIndent {
		return "", start, nil
	}
	indent := lines[firstContent].indent
	var values []string
	index := start
	for index < len(lines) {
		raw, stop, err := yamlBlockScalarLine(lines[index], parentIndent, indent)
		if err != nil {
			return "", index, err
		}
		if stop {
			break
		}
		values = append(values, raw)
		index++
	}
	values = trimYAMLBlockTrailingBlank(values)
	if !folded {
		return strings.Join(values, "\n") + "\n", index, nil
	}
	return foldYAMLLines(values) + "\n", index, nil
}

func firstYAMLBlockContent(lines []yamlLine, start int) int {
	for start < len(lines) && strings.TrimSpace(lines[start].raw) == "" {
		start++
	}
	return start
}

func yamlBlockScalarLine(line yamlLine, parentIndent, indent int) (string, bool, error) {
	physical := strings.TrimSpace(line.raw)
	if physical != "" && line.indent <= parentIndent {
		return "", true, nil
	}
	if physical != "" && line.indent < indent {
		return "", false, yamlError(line, "block scalar indentation is inconsistent")
	}
	if len(line.raw) < indent {
		return "", false, nil
	}
	return line.raw[indent:], false, nil
}

func trimYAMLBlockTrailingBlank(values []string) []string {
	for len(values) > 0 && values[len(values)-1] == "" {
		values = values[:len(values)-1]
	}
	return values
}

func foldYAMLLines(values []string) string {
	var output strings.Builder
	for index, value := range values {
		if index > 0 {
			if value == "" || values[index-1] == "" {
				output.WriteByte('\n')
			} else {
				output.WriteByte(' ')
			}
		}
		output.WriteString(value)
	}
	return output.String()
}

func skipYAMLBlank(lines []yamlLine, index int) int {
	for index < len(lines) && lines[index].blank {
		index++
	}
	return index
}

func splitYAMLField(value string) (string, string, bool) {
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
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == ':' {
			return strings.TrimSpace(value[:index]), strings.TrimSpace(value[index+1:]), true
		}
	}
	return "", "", false
}

func validateYAMLKey(value string) error {
	if value == "" {
		return errors.New("mapping key is empty")
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return fmt.Errorf("unsupported mapping key %q", value)
	}
	return nil
}

func parseYAMLScalar(raw string, line int) (YAMLValue, error) {
	value := YAMLValue{Kind: YAMLScalar, Line: line}
	if strings.HasPrefix(raw, "[") {
		sequence, err := parseYAMLFlowSequence(raw, line)
		if err != nil {
			return YAMLValue{}, err
		}
		return sequence, nil
	}
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "&") || strings.HasPrefix(raw, "*") || strings.HasPrefix(raw, "!") {
		return YAMLValue{}, &YAMLError{Line: line, Message: "flow mappings, anchors, aliases, and tags are unsupported"}
	}
	scalar, err := unquoteYAMLScalar(raw)
	if err != nil {
		return YAMLValue{}, &YAMLError{Line: line, Message: err.Error()}
	}
	value.Scalar = scalar
	return value, nil
}

func parseYAMLFlowSequence(raw string, line int) (YAMLValue, error) {
	if !strings.HasSuffix(raw, "]") {
		return YAMLValue{}, &YAMLError{Line: line, Message: "unterminated flow sequence"}
	}
	inner := strings.TrimSpace(raw[1 : len(raw)-1])
	value := YAMLValue{Kind: YAMLSequence, Line: line, Sequence: []YAMLValue{}}
	if inner == "" {
		return value, nil
	}
	parts, err := splitYAMLFlowItems(inner)
	if err != nil {
		return YAMLValue{}, &YAMLError{Line: line, Message: err.Error()}
	}
	for _, part := range parts {
		scalar, err := unquoteYAMLScalar(strings.TrimSpace(part))
		if err != nil {
			return YAMLValue{}, &YAMLError{Line: line, Message: err.Error()}
		}
		value.Sequence = append(value.Sequence, YAMLValue{Kind: YAMLScalar, Line: line, Scalar: scalar})
	}
	return value, nil
}

func splitYAMLFlowItems(value string) ([]string, error) {
	scanner := yamlFlowScanner{value: value}
	for index := 0; index < len(value); index++ {
		next, err := scanner.consume(index)
		if err != nil {
			return nil, err
		}
		index = next
	}
	if scanner.quote != 0 {
		return nil, errors.New("unterminated quoted scalar")
	}
	if err := scanner.append(len(value)); err != nil {
		return nil, err
	}
	return scanner.parts, nil
}

type yamlFlowScanner struct {
	value string
	parts []string
	start int
	quote byte
}

func (s *yamlFlowScanner) consume(index int) (int, error) {
	character := s.value[index]
	if s.quote != 0 {
		if s.quote == '"' && character == '\\' {
			return index + 1, nil
		}
		if character == s.quote {
			s.quote = 0
		}
		return index, nil
	}
	if character == '\'' || character == '"' {
		s.quote = character
		return index, nil
	}
	if character == ',' {
		return index, s.append(index)
	}
	return index, nil
}

func (s *yamlFlowScanner) append(end int) error {
	part := strings.TrimSpace(s.value[s.start:end])
	if part == "" {
		return errors.New("empty flow sequence entry")
	}
	s.parts = append(s.parts, part)
	s.start = end + 1
	return nil
}

func unquoteYAMLScalar(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value[0] == '"' {
		return unquoteYAMLDoubleScalar(value)
	}
	if value[0] == '\'' {
		return unquoteYAMLSingleScalar(value)
	}
	if strings.ContainsAny(value, "[]{}") {
		return "", errors.New("unsupported flow syntax")
	}
	return value, nil
}

func unquoteYAMLDoubleScalar(value string) (string, error) {
	if len(value) < 2 || value[len(value)-1] != '"' {
		return "", errors.New("unterminated double-quoted scalar")
	}
	decoded, err := strconv.Unquote(value)
	if err != nil {
		return "", fmt.Errorf("invalid double-quoted scalar: %w", err)
	}
	return decoded, nil
}

func unquoteYAMLSingleScalar(value string) (string, error) {
	if len(value) < 2 || value[len(value)-1] != '\'' {
		return "", errors.New("unterminated single-quoted scalar")
	}
	for index := 1; index < len(value)-1; index++ {
		if value[index] != '\'' {
			continue
		}
		if index+1 >= len(value)-1 || value[index+1] != '\'' {
			return "", errors.New("single quotes inside a scalar must be doubled")
		}
		index++
	}
	return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), nil
}

func yamlError(line yamlLine, message string) error {
	return &YAMLError{Line: line.line, Message: message}
}

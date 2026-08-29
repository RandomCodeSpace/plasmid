package syntax

import (
	"errors"
	"fmt"
	"strings"

	"github.com/RandomCodeSpace/plasmid/internal/pathglob"
)

// ToolPattern matches a tool name and, when present, its serialized argument.
type ToolPattern struct {
	Tool     string `json:"tool"`
	Argument string `json:"argument"`
}

// ParseToolPatterns parses whitespace-separated tool patterns. Valid patterns
// are retained when siblings are malformed; errors remain in source order.
func ParseToolPatterns(source string) ([]ToolPattern, []error) {
	parts := splitToolPatterns(source)
	patterns := make([]ToolPattern, 0, len(parts))
	var parseErrors []error
	for _, part := range parts {
		pattern, err := ParseToolPattern(part)
		if err != nil {
			parseErrors = append(parseErrors, err)
			continue
		}
		patterns = append(patterns, pattern)
	}
	if len(parts) == 0 {
		parseErrors = append(parseErrors, errors.New("tool pattern list is empty"))
	}
	return patterns, parseErrors
}

// ParseToolPattern parses Tool or Tool(argument-pattern).
func ParseToolPattern(source string) (ToolPattern, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return ToolPattern{}, errors.New("tool pattern is empty")
	}
	pattern := ToolPattern{Tool: source}
	if open := strings.IndexByte(source, '('); open >= 0 {
		if !strings.HasSuffix(source, ")") || strings.Contains(source[open+1:len(source)-1], "(") || strings.Contains(source[open+1:len(source)-1], ")") {
			return ToolPattern{}, fmt.Errorf("invalid tool pattern %q", source)
		}
		pattern.Tool = source[:open]
		pattern.Argument = source[open+1 : len(source)-1]
		if pattern.Argument == "" {
			return ToolPattern{}, fmt.Errorf("tool pattern %q has an empty argument", source)
		}
	} else if strings.ContainsRune(source, ')') {
		return ToolPattern{}, fmt.Errorf("invalid tool pattern %q", source)
	}
	if !validToolName(pattern.Tool) {
		return ToolPattern{}, fmt.Errorf("invalid tool name %q", pattern.Tool)
	}
	if strings.ContainsAny(pattern.Argument, "\r\n") {
		return ToolPattern{}, fmt.Errorf("tool pattern %q contains a newline", source)
	}
	return pattern, nil
}

// ToolPolicy applies deny-wins matching. Intersect retains each policy as an
// independent layer, so a request must satisfy every nested scope.
type ToolPolicy struct {
	layers []toolPolicyLayer
}

type toolPolicyLayer struct {
	allowed  []ToolPattern
	denied   []ToolPattern
	restrict bool
}

// NewToolPolicy constructs one policy layer using defensive copies.
func NewToolPolicy(allowed, denied []ToolPattern) ToolPolicy {
	return ToolPolicy{layers: []toolPolicyLayer{{
		allowed:  append([]ToolPattern(nil), allowed...),
		denied:   append([]ToolPattern(nil), denied...),
		restrict: len(allowed) != 0,
	}}}
}

func newRestrictedToolPolicy(allowed, denied []ToolPattern) ToolPolicy {
	return ToolPolicy{layers: []toolPolicyLayer{{
		allowed:  append([]ToolPattern(nil), allowed...),
		denied:   append([]ToolPattern(nil), denied...),
		restrict: true,
	}}}
}

// NewRestrictedToolPolicy constructs an allow-list layer even when the list is
// empty, preserving explicit deny-all declarations across package seams.
func NewRestrictedToolPolicy(allowed, denied []ToolPattern) ToolPolicy {
	return newRestrictedToolPolicy(allowed, denied)
}

// Intersect combines nested policies without widening either one.
func (p ToolPolicy) Intersect(other ToolPolicy) ToolPolicy {
	layers := clonePolicyLayers(p.layers)
	layers = append(layers, clonePolicyLayers(other.layers)...)
	return ToolPolicy{layers: layers}
}

// Allows reports whether every nested layer allows a tool invocation.
func (p ToolPolicy) Allows(tool, argument string) bool {
	for _, layer := range p.layers {
		if layer.denies(tool, argument) {
			return false
		}
	}
	for _, layer := range p.layers {
		if layer.restrict && !layer.allows(tool, argument) {
			return false
		}
	}
	return true
}

func (l toolPolicyLayer) denies(tool, argument string) bool {
	for _, pattern := range l.denied {
		if pattern.matches(tool, argument) {
			return true
		}
	}
	return false
}

func (l toolPolicyLayer) allows(tool, argument string) bool {
	for _, pattern := range l.allowed {
		if pattern.matches(tool, argument) {
			return true
		}
	}
	return false
}

// Visible reports whether a tool name has any invocation permitted by every
// layer. Argument-specific denies do not hide an otherwise usable tool.
func (p ToolPolicy) Visible(tool string) bool {
	tool = canonicalToolName(tool)
	for _, layer := range p.layers {
		if layer.hides(tool) {
			return false
		}
	}
	for _, layer := range p.layers {
		if layer.restrict && !layer.shows(tool) {
			return false
		}
	}
	return true
}

func (l toolPolicyLayer) hides(tool string) bool {
	for _, pattern := range l.denied {
		if pattern.Argument == "" && pattern.matches(tool, "") {
			return true
		}
	}
	return false
}

func (l toolPolicyLayer) shows(tool string) bool {
	for _, pattern := range l.allowed {
		patternTool := pattern.Tool
		if !strings.ContainsAny(patternTool, "*?") {
			patternTool = canonicalToolName(patternTool)
		}
		if pathglob.MatchString(patternTool, tool) {
			return true
		}
	}
	return false
}

// Allowed returns a defensive copy of the first policy layer's allow list.
func (p ToolPolicy) Allowed() []ToolPattern {
	if len(p.layers) == 0 {
		return []ToolPattern{}
	}
	return append([]ToolPattern(nil), p.layers[0].allowed...)
}

// Denied returns a defensive copy of the first policy layer's deny list.
func (p ToolPolicy) Denied() []ToolPattern {
	if len(p.layers) == 0 {
		return []ToolPattern{}
	}
	return append([]ToolPattern(nil), p.layers[0].denied...)
}

func (p ToolPattern) matches(tool, argument string) bool {
	patternTool := p.Tool
	if !strings.ContainsAny(patternTool, "*?") {
		patternTool = canonicalToolName(patternTool)
	}
	if !pathglob.MatchString(patternTool, canonicalToolName(tool)) {
		return false
	}
	return p.Argument == "" || argumentPatternMatch(p.Argument, argument)
}

func argumentPatternMatch(pattern, value string) bool {
	if prefix, ok := strings.CutSuffix(pattern, ":*"); ok {
		return value == prefix || strings.HasPrefix(value, prefix+" ")
	}
	return pathglob.MatchString(pattern, value)
}

func splitToolPatterns(source string) []string {
	var parts []string
	for index := 0; index < len(source); {
		index = skipToolPatternSpace(source, index)
		if index == len(source) {
			break
		}
		start := index
		index = toolPatternEnd(source, index)
		parts = append(parts, source[start:index])
	}
	return parts
}

func skipToolPatternSpace(source string, index int) int {
	for index < len(source) && isToolPatternSpace(source[index]) {
		index++
	}
	return index
}

func toolPatternEnd(source string, index int) int {
	depth := 0
	for index < len(source) {
		if source[index] == '(' {
			depth++
		}
		if source[index] == ')' && depth > 0 {
			depth--
		}
		if depth == 0 && isToolPatternSpace(source[index]) {
			break
		}
		index++
	}
	return index
}

func validToolName(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("_.*:-", character) {
			continue
		}
		return false
	}
	return true
}

func isToolPatternSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\n' || character == '\r'
}

func clonePolicyLayers(layers []toolPolicyLayer) []toolPolicyLayer {
	cloned := make([]toolPolicyLayer, len(layers))
	for index, layer := range layers {
		cloned[index] = toolPolicyLayer{
			allowed:  append([]ToolPattern(nil), layer.allowed...),
			denied:   append([]ToolPattern(nil), layer.denied...),
			restrict: layer.restrict,
		}
	}
	return cloned
}

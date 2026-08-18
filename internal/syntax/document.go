package syntax

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/plasmid-dev/plasmid/internal/pathglob"
	"github.com/plasmid-dev/plasmid/warning"
)

// Host identifies the source syntax projected into a Document.
type Host string

const (
	HostPortable Host = "portable"
	HostPlasmid  Host = "plasmid"
	HostClaude   Host = "claude"
	HostCodex    Host = "codex"
	HostCopilot  Host = "copilot"
)

// FieldStatus records whether Plasmid honors or deliberately ignores a field.
type FieldStatus string

const (
	FieldCore        FieldStatus = "core"
	FieldSupported   FieldStatus = "supported"
	FieldUnsupported FieldStatus = "unsupported"

	fieldName                   = "name"
	fieldDescription            = "description"
	fieldAllowedTools           = "allowed-tools"
	fieldDeniedTools            = "disallowed-tools"
	fieldArguments              = "arguments"
	fieldGlobs                  = "globs"
	fieldArgumentHint           = "argument-hint"
	fieldDisableModelInvocation = "disable-model-invocation"
	frontmatterFence            = "---"
	byteOrderMark               = "\ufeff"
	documentNotInvocable        = "document is not invocable"
)

// FieldRule is one ordered support-matrix row.
type FieldRule struct {
	Name     string      `json:"name"`
	Required bool        `json:"required"`
	Status   FieldStatus `json:"status"`
}

var commonFieldRules = []FieldRule{
	{Name: fieldName, Required: true, Status: FieldCore},
	{Name: fieldDescription, Required: true, Status: FieldCore},
	{Name: "license", Status: FieldCore},
	{Name: "compatibility", Status: FieldCore},
	{Name: "metadata", Status: FieldCore},
	{Name: fieldAllowedTools, Status: FieldCore},
}

var claudeFieldRules = []FieldRule{
	{Name: "arguments", Status: FieldSupported},
	{Name: fieldDeniedTools, Status: FieldSupported},
	{Name: fieldGlobs, Status: FieldSupported},
	{Name: fieldArgumentHint, Status: FieldSupported},
	{Name: fieldDisableModelInvocation, Status: FieldSupported},
	{Name: "user-invocable", Status: FieldSupported},
	{Name: "model", Status: FieldUnsupported},
	{Name: "effort", Status: FieldUnsupported},
	{Name: "context", Status: FieldUnsupported},
	{Name: "agent", Status: FieldUnsupported},
	{Name: "background", Status: FieldUnsupported},
	{Name: "hooks", Status: FieldUnsupported},
}

// SupportMatrix returns a defensive ordered field-support matrix for a host.
func SupportMatrix(host Host) []FieldRule {
	rules := slices.Clone(commonFieldRules)
	if host == HostClaude || host == HostPlasmid {
		rules = append(rules, claudeFieldRules...)
	}
	return rules
}

// MetadataEntry retains deterministic metadata order.
type MetadataEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Document is the normalized, framework-free syntax document model.
type Document struct {
	Host          Host            `json:"host"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	License       string          `json:"license"`
	Compatibility string          `json:"compatibility"`
	Metadata      []MetadataEntry `json:"metadata"`
	ArgumentHint  string          `json:"argument_hint"`
	Arguments     []string        `json:"arguments"`
	AllowedTools  []ToolPattern   `json:"allowed_tools"`
	DeniedTools   []ToolPattern   `json:"denied_tools"`
	Globs         []string        `json:"globs"`
	Exposure      Exposure        `json:"exposure"`
	Body          string          `json:"body"`
	policy        ToolPolicy
	restrictTools bool
}

// ParseDocument projects one frontmatter document. Malformed and unsupported
// entries become stable warnings; successfully parsed independent entries are
// retained.
func ParseDocument(source, path string, host Host) (Document, []warning.Warning) {
	document := newDocument(host)
	header, body, err := splitFrontmatter(source)
	document.Body = body
	if !validHost(host) {
		document.Exposure = Exposure{}
		return document, []warning.Warning{
			syntaxWarning(warning.WarnSyntaxInvalidFrontmatter, path, 1, "syntax host is invalid"),
			syntaxWarning(warning.WarnSyntaxDocumentNotInvocable, path, 1, documentNotInvocable),
		}
	}
	if err != nil {
		document.Exposure = Exposure{}
		return document, []warning.Warning{
			syntaxWarning(warning.WarnSyntaxInvalidFrontmatter, path, 1, "frontmatter delimiters are invalid"),
			syntaxWarning(warning.WarnSyntaxDocumentNotInvocable, path, 1, documentNotInvocable),
		}
	}
	entries := parseFrontmatterEntries(header)
	rules, ruleByName := documentRules(host)
	seen, valid, warnings := projectDocumentEntries(&document, entries, ruleByName, path)
	warnings = append(warnings, missingRequiredWarnings(rules, seen, path)...)
	warnings = append(warnings, validateDocumentIdentity(&document, entries, valid, path)...)
	compileDocumentPolicy(&document)
	return document, warnings
}

func newDocument(host Host) Document {
	return Document{
		Host: host, Metadata: []MetadataEntry{}, Arguments: []string{}, AllowedTools: []ToolPattern{},
		DeniedTools: []ToolPattern{}, Globs: []string{}, Exposure: DefaultExposure(),
		policy: newRestrictedToolPolicy(nil, nil),
	}
}

func documentRules(host Host) ([]FieldRule, map[string]FieldRule) {
	rules := SupportMatrix(host)
	ruleByName := make(map[string]FieldRule, len(rules))
	for _, rule := range rules {
		ruleByName[rule.Name] = rule
	}
	return rules, ruleByName
}

func projectDocumentEntries(document *Document, entries []frontmatterEntry, rules map[string]FieldRule, path string) (map[string]bool, map[string]bool, []warning.Warning) {
	seen := make(map[string]bool)
	valid := make(map[string]bool)
	var warnings []warning.Warning
	for _, entry := range entries {
		warnings = append(warnings, projectDocumentEntry(document, entry, rules, path, seen, valid)...)
	}
	return seen, valid, warnings
}

func projectDocumentEntry(document *Document, entry frontmatterEntry, rules map[string]FieldRule, path string, seen, valid map[string]bool) []warning.Warning {
	line := entry.line + 1
	if entry.name == "" {
		return []warning.Warning{syntaxWarning(warning.WarnSyntaxInvalidFrontmatter, path, line, "frontmatter entry is invalid")}
	}
	rule, known := rules[entry.name]
	if !known {
		return []warning.Warning{syntaxWarning(warning.WarnSyntaxUnknownField, path, line, "frontmatter field is unknown")}
	}
	if seen[entry.name] {
		return []warning.Warning{syntaxWarning(warning.WarnSyntaxDuplicateField, path, line, "frontmatter field is duplicated")}
	}
	seen[entry.name] = true
	if rule.Status == FieldUnsupported {
		return []warning.Warning{syntaxWarning(warning.WarnSyntaxUnsupportedField, path, line, "frontmatter field is unsupported")}
	}
	if entry.err != nil {
		if entry.name == fieldAllowedTools {
			document.restrictTools = true
		}
		return []warning.Warning{syntaxWarning(warning.WarnSyntaxInvalidField, path, line, "frontmatter field is invalid")}
	}
	warnings := projectDocumentField(document, entry.field, path, line)
	if len(warnings) == 0 {
		valid[entry.name] = true
	}
	return warnings
}

func missingRequiredWarnings(rules []FieldRule, seen map[string]bool, path string) []warning.Warning {
	var warnings []warning.Warning
	for _, rule := range rules {
		if rule.Required && !seen[rule.Name] {
			warnings = append(warnings, syntaxWarning(warning.WarnSyntaxInvalidField, path, 1, "required frontmatter field is missing"))
		}
	}
	return warnings
}

func validateDocumentIdentity(document *Document, entries []frontmatterEntry, valid map[string]bool, path string) []warning.Warning {
	var warnings []warning.Warning
	if !validDocumentName(document.Name) && valid[fieldName] {
		warnings = append(warnings, syntaxWarning(warning.WarnSyntaxInvalidField, path, entryLine(entries, fieldName)+1, "frontmatter name is invalid"))
	}
	if strings.TrimSpace(document.Description) == "" && valid[fieldDescription] {
		warnings = append(warnings, syntaxWarning(warning.WarnSyntaxInvalidField, path, entryLine(entries, fieldDescription)+1, "frontmatter description is invalid"))
	}
	if !validDocumentName(document.Name) || strings.TrimSpace(document.Description) == "" {
		document.Exposure = Exposure{}
		warnings = append(warnings, syntaxWarning(warning.WarnSyntaxDocumentNotInvocable, path, 1, documentNotInvocable))
	}
	return warnings
}

func compileDocumentPolicy(document *Document) {
	if document.restrictTools {
		document.policy = newRestrictedToolPolicy(document.AllowedTools, document.DeniedTools)
	} else {
		document.policy = NewToolPolicy(nil, document.DeniedTools)
	}
}

// ToolPolicy returns the compiled policy while preserving the distinction
// between an absent allow-list and a configured list with no valid patterns.
func (d Document) ToolPolicy() ToolPolicy {
	return ToolPolicy{layers: clonePolicyLayers(d.policy.layers)}
}

// RestrictsTools reports whether an allow-list was declared, including an
// explicitly empty or wholly invalid declaration that must deny all tools.
func (d Document) RestrictsTools() bool { return d.restrictTools }

// ParseTemplate projects optional template frontmatter through the same parser
// as Agent Skills while retaining the filename as the template identity.
func ParseTemplate(source, path string, host Host, identity string) (Document, []warning.Warning) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return Document{}, []warning.Warning{syntaxWarning(warning.WarnSyntaxDocumentNotInvocable, path, 1, "template identity is empty")}
	}
	header, body, err := splitFrontmatter(source)
	if err == nil && header == "" {
		document := Document{Host: host, Name: identity, Description: identity, Body: body, Exposure: DefaultExposure(), policy: NewToolPolicy(nil, nil)}
		return document, nil
	}
	if err != nil {
		normalized := strings.TrimPrefix(strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\r", "\n"), byteOrderMark)
		if normalized == frontmatterFence || strings.HasPrefix(normalized, frontmatterFence+"\n") {
			document := Document{Host: host, Name: identity, Description: identity, Body: source, Exposure: Exposure{}, policy: NewToolPolicy(nil, nil)}
			return document, []warning.Warning{
				syntaxWarning(warning.WarnSyntaxInvalidFrontmatter, path, 1, "frontmatter delimiters are invalid"),
				syntaxWarning(warning.WarnSyntaxDocumentNotInvocable, path, 1, "template is not invocable"),
			}
		}
		document := Document{Host: host, Name: identity, Description: identity, Body: source, Exposure: DefaultExposure(), policy: NewToolPolicy(nil, nil)}
		return document, nil
	}
	hasName, hasDescription := false, false
	for _, entry := range parseFrontmatterEntries(header) {
		hasName = hasName || entry.name == fieldName
		hasDescription = hasDescription || entry.name == fieldDescription
	}
	var normalized strings.Builder
	normalized.WriteString("---\n")
	if !hasName {
		normalized.WriteString("name: ")
		normalized.WriteString(identity)
		normalized.WriteByte('\n')
	}
	if !hasDescription {
		normalized.WriteString("description: ")
		normalized.WriteString(identity)
		normalized.WriteByte('\n')
	}
	normalized.WriteString(header)
	normalized.WriteString("\n---\n")
	normalized.WriteString(body)
	document, notices := ParseDocument(normalized.String(), path, host)
	document.Name = identity
	return document, notices
}

func projectDocumentField(document *Document, field YAMLField, path string, line int) []warning.Warning {
	switch field.Name {
	case fieldName, fieldDescription, "license", "compatibility", fieldArgumentHint:
		return projectScalarDocumentField(document, field, path, line)
	case "metadata":
		return projectDocumentMetadata(document, field, path, line)
	case fieldArguments, fieldGlobs:
		return projectDocumentList(document, field, path, line)
	case fieldAllowedTools, fieldDeniedTools:
		return projectDocumentTools(document, field, path, line)
	case fieldDisableModelInvocation, "user-invocable":
		return projectDocumentExposure(document, field, path, line)
	}
	return nil
}

func invalidDocumentField(path string, line int) []warning.Warning {
	return []warning.Warning{syntaxWarning(warning.WarnSyntaxInvalidField, path, line, "frontmatter field has an invalid type")}
}

func projectScalarDocumentField(document *Document, field YAMLField, path string, line int) []warning.Warning {
	value, ok := yamlString(field.Value)
	if !ok {
		return invalidDocumentField(path, line)
	}
	switch field.Name {
	case fieldName:
		document.Name = value
	case fieldDescription:
		document.Description = value
	case "license":
		document.License = value
	case "compatibility":
		document.Compatibility = value
	case fieldArgumentHint:
		document.ArgumentHint = value
	}
	return nil
}

func projectDocumentMetadata(document *Document, field YAMLField, path string, line int) []warning.Warning {
	if field.Value.Kind != YAMLMapping {
		return invalidDocumentField(path, line)
	}
	seen := make(map[string]bool)
	var warnings []warning.Warning
	for _, entry := range field.Value.Mapping {
		if seen[entry.Name] {
			warnings = append(warnings, syntaxWarning(warning.WarnSyntaxDuplicateField, path, entry.Line+1, "metadata field is duplicated"))
			continue
		}
		seen[entry.Name] = true
		value, ok := yamlString(entry.Value)
		if !ok {
			warnings = append(warnings, syntaxWarning(warning.WarnSyntaxInvalidField, path, entry.Line+1, "metadata field is invalid"))
			continue
		}
		document.Metadata = append(document.Metadata, MetadataEntry{Name: entry.Name, Value: value})
	}
	return warnings
}

func projectDocumentList(document *Document, field YAMLField, path string, line int) []warning.Warning {
	values, ok := yamlScalarItems(field.Value)
	if !ok {
		return invalidDocumentField(path, line)
	}
	if field.Name == fieldArguments {
		return projectDocumentArguments(document, values, path)
	}
	return projectDocumentGlobs(document, values, path)
}

func projectDocumentArguments(document *Document, values []yamlScalarItem, path string) []warning.Warning {
	seen := make(map[string]bool)
	var warnings []warning.Warning
	for _, item := range values {
		if !validArgumentName(item.value) {
			warnings = append(warnings, syntaxWarning(warning.WarnSyntaxInvalidField, path, item.line+1, "argument name is invalid"))
			continue
		}
		if seen[item.value] {
			warnings = append(warnings, syntaxWarning(warning.WarnSyntaxDuplicateArgument, path, item.line+1, "argument name is duplicated"))
			continue
		}
		seen[item.value] = true
		document.Arguments = append(document.Arguments, item.value)
	}
	return warnings
}

func projectDocumentGlobs(document *Document, values []yamlScalarItem, path string) []warning.Warning {
	document.Globs = []string{}
	var warnings []warning.Warning
	for _, item := range values {
		if _, err := pathglob.CompileOne(item.value); err != nil {
			warnings = append(warnings, syntaxWarning(warning.WarnSyntaxInvalidGlob, path, item.line+1, "path glob is invalid"))
			continue
		}
		document.Globs = append(document.Globs, item.value)
	}
	return warnings
}

func projectDocumentTools(document *Document, field YAMLField, path string, line int) []warning.Warning {
	values, ok := yamlScalarItems(field.Value)
	if !ok {
		if field.Name == fieldAllowedTools {
			document.restrictTools = true
		}
		return invalidDocumentField(path, line)
	}
	patterns, warnings := projectToolPatterns(values, path)
	if field.Name == fieldAllowedTools {
		document.AllowedTools = patterns
		document.restrictTools = true
	} else {
		document.DeniedTools = patterns
	}
	return warnings
}

func projectToolPatterns(values []yamlScalarItem, path string) ([]ToolPattern, []warning.Warning) {
	patterns := []ToolPattern{}
	var warnings []warning.Warning
	for _, item := range values {
		parsed, parseErrors := ParseToolPatterns(item.value)
		for range parseErrors {
			warnings = append(warnings, syntaxWarning(warning.WarnSyntaxInvalidToolPattern, path, item.line+1, "tool pattern is invalid"))
		}
		patterns = append(patterns, parsed...)
	}
	return patterns, warnings
}

func projectDocumentExposure(document *Document, field YAMLField, path string, line int) []warning.Warning {
	value, ok := yamlBool(field.Value)
	if !ok {
		return invalidDocumentField(path, line)
	}
	if field.Name == fieldDisableModelInvocation {
		document.Exposure.ModelInvocable = !value
	} else {
		document.Exposure.UserInvocable = value
	}
	return nil
}

func splitFrontmatter(source string) (string, string, error) {
	source = strings.TrimPrefix(source, byteOrderMark)
	source = strings.ReplaceAll(source, "\r\n", "\n")
	source = strings.ReplaceAll(source, "\r", "\n")
	lines := strings.Split(source, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return "", source, errors.New("frontmatter must start with --- on the first line")
	}
	for index := 1; index < len(lines); index++ {
		if lines[index] == "---" || lines[index] == "..." {
			return strings.Join(lines[1:index], "\n"), strings.Join(lines[index+1:], "\n"), nil
		}
	}
	return "", source, errors.New("frontmatter closing delimiter is missing")
}

type frontmatterEntry struct {
	err   error
	field YAMLField
	line  int
	name  string
}

func parseFrontmatterEntries(header string) []frontmatterEntry {
	lines := strings.Split(header, "\n")
	var entries []frontmatterEntry
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		entries = append(entries, parseFrontmatterEntry(lines[start:end], start))
	}
	for index, raw := range lines {
		if strings.TrimSpace(stripYAMLComment(raw)) == "" {
			continue
		}
		if leadingSpaces(raw) == 0 {
			flush(index)
			start = index
		} else if start < 0 {
			start = index
		}
	}
	flush(len(lines))
	return entries
}

func parseFrontmatterEntry(lines []string, offset int) frontmatterEntry {
	entry := frontmatterEntry{line: offset + 1}
	for index, raw := range lines {
		text := strings.TrimSpace(stripYAMLComment(raw))
		if text == "" {
			continue
		}
		entry.line = offset + index + 1
		if name, _, ok := splitYAMLField(text); ok {
			entry.name = name
		}
		break
	}
	value, err := ParseYAML(strings.Join(lines, "\n"))
	if err != nil {
		entry.err = err
		var yamlErr *YAMLError
		if errors.As(err, &yamlErr) {
			entry.line = offset + yamlErr.Line
		}
		return entry
	}
	if len(value.Mapping) != 1 {
		entry.err = errors.New("frontmatter chunk does not contain one field")
		return entry
	}
	entry.field = value.Mapping[0]
	shiftYAMLLines(&entry.field.Value, offset)
	entry.field.Line += offset
	entry.name = entry.field.Name
	entry.line = entry.field.Line
	return entry
}

func shiftYAMLLines(value *YAMLValue, offset int) {
	value.Line += offset
	for index := range value.Sequence {
		shiftYAMLLines(&value.Sequence[index], offset)
	}
	for index := range value.Mapping {
		value.Mapping[index].Line += offset
		shiftYAMLLines(&value.Mapping[index].Value, offset)
	}
}

type yamlScalarItem struct {
	line  int
	value string
}

func yamlScalarItems(value YAMLValue) ([]yamlScalarItem, bool) {
	if value.Kind == YAMLScalar {
		return []yamlScalarItem{{line: value.Line, value: value.Scalar}}, true
	}
	if value.Kind != YAMLSequence {
		return nil, false
	}
	items := make([]yamlScalarItem, 0, len(value.Sequence))
	for _, item := range value.Sequence {
		if item.Kind != YAMLScalar {
			return nil, false
		}
		items = append(items, yamlScalarItem{line: item.Line, value: item.Scalar})
	}
	return items, true
}

func yamlString(value YAMLValue) (string, bool) {
	return value.Scalar, value.Kind == YAMLScalar
}

func yamlBool(value YAMLValue) (bool, bool) {
	if value.Kind != YAMLScalar {
		return false, false
	}
	parsed, err := strconv.ParseBool(value.Scalar)
	return parsed, err == nil && (value.Scalar == "true" || value.Scalar == "false")
}

func validDocumentName(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] == '-' || value[len(value)-1] == '-' || strings.Contains(value, "--") {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validArgumentName(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '_' || (index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
}

func validHost(host Host) bool {
	switch host {
	case HostPortable, HostPlasmid, HostClaude, HostCodex, HostCopilot:
		return true
	default:
		return false
	}
}

func entryLine(entries []frontmatterEntry, name string) int {
	for _, entry := range entries {
		if entry.name == name {
			return entry.line
		}
	}
	return 0
}

func syntaxWarning(code, path string, line int, message string) warning.Warning {
	return warning.Warning{Code: code, Source: "syntax", Path: path, Line: line, Message: message}
}

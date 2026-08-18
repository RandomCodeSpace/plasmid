package syntax

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/plasmid-dev/plasmid/internal/pathglob"
	"github.com/plasmid-dev/plasmid/warning"
)

// ErrToolDenied identifies a turn-scoped tool policy rejection.
var ErrToolDenied = errors.New("tool denied by active instruction policy")

const fieldApplyTo = "applyTo"

// Instruction is the normalized syntax owned by an instruction file.
type Instruction struct {
	Body              string     `json:"body"`
	Globs             []string   `json:"globs"`
	PathScopeDeclared bool       `json:"path_scope_declared"`
	Policy            ToolPolicy `json:"-"`
}

// ParseInstruction projects the supported instruction frontmatter subset.
// Files without frontmatter are returned unchanged.
func ParseInstruction(source, path string, host Host) (Instruction, []warning.Warning) {
	result := Instruction{Body: source, Globs: []string{}, Policy: NewToolPolicy(nil, nil)}
	if !strings.HasPrefix(strings.TrimPrefix(source, "\ufeff"), "---\n") &&
		!strings.HasPrefix(strings.TrimPrefix(source, "\ufeff"), "---\r\n") {
		return result, nil
	}
	header, body, err := splitFrontmatter(source)
	if err != nil {
		result.PathScopeDeclared = malformedInstructionPathScopeDeclared(source)
		result.Policy = malformedInstructionToolPolicy(source)
		return result, []warning.Warning{syntaxWarning(warning.WarnContextFrontmatterUnsupported, path, 1, "instruction frontmatter is invalid")}
	}
	result.Body = body
	document := Document{policy: NewToolPolicy(nil, nil), AllowedTools: []ToolPattern{}, DeniedTools: []ToolPattern{}}
	notices := projectInstructionEntries(&result, &document, parseFrontmatterEntries(header), path)
	compileDocumentPolicy(&document)
	result.Policy = document.ToolPolicy()
	return result, notices
}

func projectInstructionEntries(result *Instruction, document *Document, entries []frontmatterEntry, path string) []warning.Warning {
	seen := make(map[string]bool)
	var notices []warning.Warning
	for _, entry := range entries {
		line := entry.line + 1
		if entry.name == "" {
			notices = append(notices, syntaxWarning(warning.WarnContextFrontmatterUnsupported, path, line, "instruction frontmatter entry is invalid"))
			continue
		}
		if entry.name == fieldAllowedTools {
			document.restrictTools = true
		}
		if instructionPathField(entry.name) {
			result.PathScopeDeclared = true
		}
		if entry.err != nil {
			notices = append(notices, syntaxWarning(warning.WarnContextFrontmatterUnsupported, path, line, "instruction frontmatter entry is invalid"))
			continue
		}
		if seen[entry.name] {
			notices = append(notices, syntaxWarning(warning.WarnContextFrontmatterUnsupported, path, line, "instruction frontmatter entry is duplicated"))
			continue
		}
		seen[entry.name] = true
		switch entry.name {
		case fieldApplyTo, "paths", fieldGlobs:
			notices = append(notices, projectInstructionPaths(result, entry, path, line)...)
		case fieldAllowedTools, fieldDeniedTools:
			notices = append(notices, projectDocumentField(document, entry.field, path, line)...)
		default:
			notices = append(notices, syntaxWarning(warning.WarnContextFrontmatterUnsupported, path, line, "instruction frontmatter field is unsupported"))
		}
	}
	return notices
}

func projectInstructionPaths(result *Instruction, entry frontmatterEntry, path string, line int) []warning.Warning {
	items, ok := yamlScalarItems(entry.field.Value)
	if !ok {
		return []warning.Warning{syntaxWarning(warning.WarnContextFrontmatterUnsupported, path, line, "instruction path scope is invalid")}
	}
	var notices []warning.Warning
	for _, item := range items {
		for _, pattern := range instructionPatterns(entry.name, item.value) {
			if _, err := pathglob.CompileOne(pattern); err != nil {
				notices = append(notices, syntaxWarning(warning.WarnContextGlobInvalid, path, item.line+1, "instruction path glob is invalid"))
				continue
			}
			result.Globs = append(result.Globs, pattern)
		}
	}
	return notices
}

func instructionPatterns(name, value string) []string {
	if name == fieldApplyTo {
		return pathglob.SplitList(value)
	}
	return []string{value}
}

func instructionPathField(name string) bool {
	return name == fieldApplyTo || name == "paths" || name == fieldGlobs
}

func malformedInstructionPathScopeDeclared(source string) bool {
	source = strings.TrimPrefix(source, "\ufeff")
	source = strings.ReplaceAll(source, "\r\n", "\n")
	source = strings.ReplaceAll(source, "\r", "\n")
	_, header, _ := strings.Cut(source, "\n")
	for _, entry := range parseFrontmatterEntries(header) {
		if instructionPathField(entry.name) {
			return true
		}
	}
	return false
}

func malformedInstructionToolPolicy(source string) ToolPolicy {
	source = strings.TrimPrefix(source, "\ufeff")
	source = strings.ReplaceAll(source, "\r\n", "\n")
	source = strings.ReplaceAll(source, "\r", "\n")
	_, header, _ := strings.Cut(source, "\n")
	document := Document{policy: NewToolPolicy(nil, nil)}
	for _, entry := range parseFrontmatterEntries(header) {
		if entry.name == fieldAllowedTools {
			document.restrictTools = true
		}
		if entry.err == nil && (entry.name == fieldAllowedTools || entry.name == fieldDeniedTools) {
			_ = projectDocumentField(&document, entry.field, "", entry.line+1)
		}
	}
	compileDocumentPolicy(&document)
	return document.policy
}

// CommandDirective is one executable prompt command region.
type CommandDirective struct {
	Start        int    `json:"start"`
	End          int    `json:"end"`
	ContentStart int    `json:"content_start"`
	ContentEnd   int    `json:"content_end"`
	Line         int    `json:"line"`
	Command      string `json:"command"`
}

// ScanCommandDirectives recognizes !`inline` and ```! fenced commands while
// leaving ordinary Markdown code untouched.
func ScanCommandDirectives(source string) []CommandDirective {
	regions := ScanCodeRegions(source)
	result := make([]CommandDirective, 0)
	for _, region := range regions {
		switch region.Kind {
		case CodeRegionInline:
			if region.Start == 0 || source[region.Start-1] != '!' || escapedMarkdownByte(source, region.Start-1) {
				continue
			}
			result = append(result, CommandDirective{
				Start: region.Start - 1, End: region.End, ContentStart: region.ContentStart,
				ContentEnd: region.ContentEnd, Line: lineAtOffset(source, region.Start-1),
				Command: source[region.ContentStart:region.ContentEnd],
			})
		case CodeRegionFence:
			opening := source[region.Start:region.ContentStart]
			if !fenceCommandMarker(opening) {
				continue
			}
			result = append(result, CommandDirective{
				Start: region.Start, End: region.End, ContentStart: region.ContentStart,
				ContentEnd: region.ContentEnd, Line: lineAtOffset(source, region.Start),
				Command: source[region.ContentStart:region.ContentEnd],
			})
		}
	}
	return result
}

func fenceCommandMarker(opening string) bool {
	opening = strings.TrimSpace(opening)
	index := 0
	for index < len(opening) && opening[index] == ' ' {
		index++
	}
	if index >= len(opening) || (opening[index] != '`' && opening[index] != '~') {
		return false
	}
	marker := opening[index]
	for index < len(opening) && opening[index] == marker {
		index++
	}
	return strings.TrimSpace(opening[index:]) == "!"
}

func lineAtOffset(source string, offset int) int {
	return strings.Count(source[:offset], "\n") + 1
}

// NativeToolInvocation maps supported host policy names to Plasmid's native
// wire names and selects the host-compatible argument string.
func NativeToolInvocation(name string, args map[string]any) (string, string) {
	name = canonicalToolName(name)
	argumentField := map[string]string{
		"read": "path", "write": "path", "edit": "path", "bash": "command",
		"grep": "pattern", "find": "glob", "ls": "path",
	}[name]
	if argumentField != "" {
		if value, ok := args[argumentField].(string); ok {
			return name, value
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return name, "{}"
	}
	return name, string(encoded)
}

func canonicalToolName(name string) string {
	switch strings.ToLower(name) {
	case "read":
		return "read"
	case "write":
		return "write"
	case "edit":
		return "edit"
	case "bash":
		return "bash"
	case "grep":
		return "grep"
	case "glob", "find":
		return "find"
	case "ls", "list":
		return "ls"
	default:
		return name
	}
}

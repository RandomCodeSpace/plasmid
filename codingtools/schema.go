package codingtools

import "encoding/json"

// Schemas are canonical JSON rather than reflected Go types because their
// bytes are a cross-language contract. Accessors return independent copies.
const (
	readSchemaJSON  = `{"additionalProperties":false,"description":"Read a numbered line window from a workspace text file.","properties":{"limit":{"default":2000,"description":"Maximum number of source lines to return; defaults to 2000.","minimum":1,"type":"integer"},"offset":{"default":1,"description":"One-based first source line; defaults to 1.","minimum":1,"type":"integer"},"path":{"description":"Workspace-relative file path to read.","type":"string"}},"required":["path"],"type":"object"}`
	writeSchemaJSON = `{"additionalProperties":false,"description":"Create or completely replace a workspace text file.","properties":{"content":{"description":"Complete text content to write without truncation.","type":"string"},"path":{"description":"Workspace-relative destination path; replacing an existing file requires a prior read.","type":"string"}},"required":["path","content"],"type":"object"}`
	editSchemaJSON  = `{"additionalProperties":false,"description":"Apply one deterministic text replacement to a previously read file.","properties":{"new_text":{"description":"Replacement text; an empty value deletes the match.","type":"string"},"old_text":{"description":"Text to match using the documented deterministic tiers.","type":"string"},"path":{"description":"Workspace-relative file path; editing requires a prior read.","type":"string"},"replace_all":{"default":false,"description":"Replace every match; defaults to false so ambiguous matches are refused.","type":"boolean"}},"required":["path","old_text","new_text"],"type":"object"}`
	bashSchemaJSON  = `{"additionalProperties":false,"description":"Run one fresh non-interactive shell command from a workspace-relative initial directory with host-process authority.","properties":{"command":{"description":"Shell command passed to the resolved shell with -c.","type":"string"},"dir":{"default":".","description":"Workspace-relative initial directory; defaults to the workspace root and paths outside it are rejected.","type":"string"},"timeout_ms":{"default":120000,"description":"Timeout in milliseconds; defaults to 120000 and is capped by host policy.","minimum":1,"type":"integer"}},"required":["command"],"type":"object"}`
	grepSchemaJSON  = `{"additionalProperties":false,"description":"Search workspace text files with bounded deterministic results.","properties":{"case_insensitive":{"default":false,"description":"Match without case distinctions; defaults to false.","type":"boolean"},"context_lines":{"default":0,"description":"Lines of context before and after each match; defaults to 0.","minimum":0,"type":"integer"},"glob":{"description":"Optional slash-separated glob restricting searched files.","type":"string"},"literal":{"default":false,"description":"Treat pattern as literal text instead of a regular expression; defaults to false.","type":"boolean"},"max_results":{"default":200,"description":"Maximum matches to return; defaults to 200.","minimum":1,"type":"integer"},"path":{"default":".","description":"Workspace-relative file or directory to search; defaults to the workspace root.","type":"string"},"pattern":{"description":"Portable regular expression or literal text to find.","type":"string"}},"required":["pattern"],"type":"object"}`
	findSchemaJSON  = `{"additionalProperties":false,"description":"Find workspace entries matching a slash-separated glob.","properties":{"glob":{"description":"Required slash-separated glob used to select entries.","type":"string"},"max_results":{"default":200,"description":"Maximum paths to return; defaults to 200.","minimum":1,"type":"integer"},"path":{"default":".","description":"Workspace-relative directory to search; defaults to the workspace root.","type":"string"},"sort_by":{"default":"path","description":"Sort paths ascending or modification times descending; defaults to path.","enum":["path","modified"],"type":"string"},"type":{"default":"any","description":"Entry type to include; defaults to any.","enum":["file","dir","symlink","any"],"type":"string"}},"required":["glob"],"type":"object"}`
	listSchemaJSON  = `{"additionalProperties":false,"description":"List workspace directory entries with bounded depth and count.","properties":{"max_depth":{"default":1,"description":"Maximum traversal depth; defaults to 1.","minimum":1,"type":"integer"},"max_results":{"default":20000,"description":"Maximum entries to return; defaults to 20000.","minimum":1,"type":"integer"},"path":{"default":".","description":"Workspace-relative directory to list; defaults to the workspace root.","type":"string"},"show_hidden":{"default":false,"description":"Include dot-prefixed entries; defaults to false.","type":"boolean"}},"required":[],"type":"object"}`
)

var (
	readSchema  = json.RawMessage(readSchemaJSON)
	writeSchema = json.RawMessage(writeSchemaJSON)
	editSchema  = json.RawMessage(editSchemaJSON)
	bashSchema  = json.RawMessage(bashSchemaJSON)
	grepSchema  = json.RawMessage(grepSchemaJSON)
	findSchema  = json.RawMessage(findSchemaJSON)
	listSchema  = json.RawMessage(listSchemaJSON)
)

// ReadInputSchema returns an independent copy of the read input schema.
func ReadInputSchema() json.RawMessage { return cloneSchema(readSchema) }

// WriteInputSchema returns an independent copy of the write input schema.
func WriteInputSchema() json.RawMessage { return cloneSchema(writeSchema) }

// EditInputSchema returns an independent copy of the edit input schema.
func EditInputSchema() json.RawMessage { return cloneSchema(editSchema) }

// BashInputSchema returns an independent copy of the bash input schema.
func BashInputSchema() json.RawMessage { return cloneSchema(bashSchema) }

// GrepInputSchema returns an independent copy of the grep input schema.
func GrepInputSchema() json.RawMessage { return cloneSchema(grepSchema) }

// FindInputSchema returns an independent copy of the find input schema.
func FindInputSchema() json.RawMessage { return cloneSchema(findSchema) }

// ListInputSchema returns an independent copy of the ls input schema.
func ListInputSchema() json.RawMessage { return cloneSchema(listSchema) }

func cloneSchema(schema json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), schema...)
}

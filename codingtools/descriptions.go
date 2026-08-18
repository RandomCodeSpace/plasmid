package codingtools

const (
	// ReadDescription is the model-facing read tool description.
	ReadDescription = "Read a UTF-8 text file with one-based line offsets and numbered output. paths are relative to the working directory; paths outside it are rejected. Binary and oversized files are refused. Long output is truncated with the shared truncation marker and a structured report."

	// WriteDescription is the model-facing write tool description.
	WriteDescription = "Create a text file or replace its complete contents. Overwriting an existing file requires a prior successful read, and the write is refused if the file changed since that read. paths are relative to the working directory; paths outside it are rejected. The returned unified diff is truncated with the shared truncation marker and a structured report; file contents are not truncated."

	// EditDescription is the model-facing edit tool description.
	EditDescription = "Replace text using exact, trailing-whitespace, then uniform-indentation matching. Editing requires a prior successful read and is refused if the file changed since that read; ambiguous matches require more context or replace_all. paths are relative to the working directory; paths outside it are rejected. The returned unified diff is truncated with the shared truncation marker and a structured report; file contents are not truncated."

	// BashDescription is the model-facing bash tool description.
	BashDescription = "Run one fresh, non-interactive shell command with host-process authority. The optional dir selects an initial working directory inside the workspace and defaults to the workspace root; only that initial directory is sandbox-checked. The command itself is not confined by the file-tool sandbox; hosts requiring isolation must confine the host process. Commands have a bounded timeout, and stdout and stderr are independently truncated with the shared truncation marker and structured reports. A nonzero exit is returned as a result, not a tool failure."

	// GrepDescription is the model-facing grep tool description.
	GrepDescription = "Search text files with the portable regular-expression subset or literal matching, optional glob filtering, case folding, and surrounding lines. paths are relative to the working directory; paths outside it are rejected. Results are sorted by path and line, binary and oversized files are counted as skipped, and result or line limits set truncated rather than silently dropping the condition."

	// FindDescription is the model-facing find tool description.
	FindDescription = "Find workspace entries by glob, with optional type and sort controls. paths are relative to the working directory; paths outside it are rejected. Returned paths are slash-separated and relative, never absolute. Results are deterministic and truncated at max_results, with the truncated field reporting the limit."

	// ListDescription is the model-facing ls tool description.
	ListDescription = "List directory entries with type, size, and RFC3339 modification time, with optional hidden entries and depth control. paths are relative to the working directory; paths outside it are rejected. Results use slash-separated relative paths, sort directories first and names deterministically, and are truncated at max_results with the truncated field reporting the limit."
)

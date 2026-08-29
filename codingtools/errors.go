package codingtools

import (
	"errors"

	"github.com/RandomCodeSpace/plasmid/codingtools/internal/textmatch"
	"github.com/RandomCodeSpace/plasmid/shellexec"
	"github.com/RandomCodeSpace/plasmid/workspace"
)

var (
	// File sentinels preserve the workspace contract's errors.Is identity.
	ErrPathOutsideRoot = workspace.ErrOutsideRoot
	ErrFileNotFound    = workspace.ErrNotFound
	ErrIsDirectory     = workspace.ErrIsDirectory
	ErrBinaryFile      = workspace.ErrBinaryFile
	ErrFileTooLarge    = workspace.ErrTooLarge
	ErrNeverRead       = workspace.ErrNeverRead
	ErrStaleRead       = workspace.ErrStaleRead

	// Edit sentinels preserve the deterministic matcher's errors.Is identity.
	ErrNoMatch        = textmatch.ErrNoMatch
	ErrAmbiguousMatch = textmatch.ErrAmbiguousMatch

	// ErrNoShell preserves the shell executor's errors.Is identity.
	ErrNoShell = shellexec.ErrNoShell

	// ErrUnsupportedPattern classifies search syntax outside the portable
	// regexp and glob subset. Handlers add the rejected construct and remedy.
	ErrUnsupportedPattern = errors.New("coding tools: unsupported pattern")
)

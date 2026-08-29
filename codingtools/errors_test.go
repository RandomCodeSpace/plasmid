package codingtools

import (
	"errors"
	"testing"

	"github.com/RandomCodeSpace/plasmid/codingtools/internal/textmatch"
	"github.com/RandomCodeSpace/plasmid/shellexec"
	"github.com/RandomCodeSpace/plasmid/workspace"
)

func TestSentinelIdentity(t *testing.T) {
	tests := []struct {
		name   string
		public error
		owner  error
	}{
		{"path outside root", ErrPathOutsideRoot, workspace.ErrOutsideRoot},
		{"file not found", ErrFileNotFound, workspace.ErrNotFound},
		{"directory", ErrIsDirectory, workspace.ErrIsDirectory},
		{"binary", ErrBinaryFile, workspace.ErrBinaryFile},
		{"too large", ErrFileTooLarge, workspace.ErrTooLarge},
		{"never read", ErrNeverRead, workspace.ErrNeverRead},
		{"stale read", ErrStaleRead, workspace.ErrStaleRead},
		{"no match", ErrNoMatch, textmatch.ErrNoMatch},
		{"ambiguous match", ErrAmbiguousMatch, textmatch.ErrAmbiguousMatch},
		{"no shell", ErrNoShell, shellexec.ErrNoShell},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.public != test.owner || !errors.Is(test.public, test.owner) || !errors.Is(test.owner, test.public) {
				t.Fatalf("public sentinel %v does not preserve owner identity %v", test.public, test.owner)
			}
		})
	}
	if ErrUnsupportedPattern == nil {
		t.Fatal("ErrUnsupportedPattern is nil")
	}
}

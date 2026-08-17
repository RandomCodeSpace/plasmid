//go:build !unix && !windows

package lsp

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

func configureProcessTree(*exec.Cmd) error {
	return fmt.Errorf("LSP process-tree cleanup is unsupported on %s", runtime.GOOS)
}

func attachProcessTree(*os.Process) (processTree, error) {
	return nil, fmt.Errorf("LSP process-tree cleanup is unsupported on %s", runtime.GOOS)
}

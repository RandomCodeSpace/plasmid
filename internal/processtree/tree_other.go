//go:build !unix && !windows

package processtree

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

func configure(*exec.Cmd) error {
	return fmt.Errorf("process-tree cleanup is unsupported on %s", runtime.GOOS)
}

func attach(*os.Process) (Terminator, error) {
	return nil, fmt.Errorf("process-tree cleanup is unsupported on %s", runtime.GOOS)
}

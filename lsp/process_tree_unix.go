//go:build unix

package lsp

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

type unixProcessTree struct {
	pid int
}

func configureProcessTree(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func attachProcessTree(process *os.Process) (processTree, error) {
	return unixProcessTree{pid: process.Pid}, nil
}

func (tree unixProcessTree) terminate() error {
	err := syscall.Kill(-tree.pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

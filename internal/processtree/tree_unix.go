//go:build unix

package processtree

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

type unixTree struct{ pid int }

func configure(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func attach(process *os.Process) (Tree, error) { return unixTree{pid: process.Pid}, nil }

func (tree unixTree) Terminate() error {
	err := syscall.Kill(-tree.pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

//go:build !unix

package shellexec

import (
	"os"
	"os/exec"
)

func configureProcess(*exec.Cmd) {
	// Non-Unix platforms have no portable process-group configuration here.
}

func signalProcessGroup(process *os.Process, force bool) error {
	if force {
		return process.Kill()
	}
	if err := process.Signal(os.Interrupt); err == nil {
		return nil
	}
	return process.Kill()
}

func processSignal(state *os.ProcessState) string { return "" }

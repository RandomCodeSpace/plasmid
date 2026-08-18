// Package processtree owns operating-system process descendant containment.
package processtree

import (
	"os"
	"os/exec"
)

// Terminator owns a process and every descendant it creates.
type Terminator interface {
	Terminate() error
}

// Tree is retained for source compatibility.
type Tree = Terminator

// Configure prepares a command so no descendant can escape before Attach.
func Configure(command *exec.Cmd) error { return configure(command) }

// Attach takes ownership of a started command's process tree.
func Attach(process *os.Process) (Tree, error) { return attach(process) }

package lsp

import (
	"os"
	"os/exec"

	"github.com/RandomCodeSpace/plasmid/internal/processtree"
)

type processTree struct{ processtree.Terminator }

func (tree processTree) terminate() error { return tree.Terminate() }

func configureProcessTree(command *exec.Cmd) error { return processtree.Configure(command) }

func attachProcessTree(process *os.Process) (processTree, error) {
	tree, err := processtree.Attach(process)
	return processTree{Terminator: tree}, err
}

package lsp

import (
	"os"
	"os/exec"

	"github.com/plasmid-dev/plasmid/internal/processtree"
)

type processTree struct{ processtree.Tree }

func (tree processTree) terminate() error { return tree.Terminate() }

func configureProcessTree(command *exec.Cmd) error { return processtree.Configure(command) }

func attachProcessTree(process *os.Process) (processTree, error) {
	tree, err := processtree.Attach(process)
	return processTree{Tree: tree}, err
}

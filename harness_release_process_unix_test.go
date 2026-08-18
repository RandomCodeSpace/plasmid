//go:build unix

package plasmid_test

import (
	"errors"
	"syscall"
)

func releaseProcessExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

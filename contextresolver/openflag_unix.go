//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package contextresolver

import "syscall"

const nonBlockingOpenFlag = syscall.O_NONBLOCK

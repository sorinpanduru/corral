//go:build unix

package osfile

import "syscall"

// openFlags: non-blocking so a FIFO cannot block open, and no controlling
// terminal can be acquired from a tty path.
const openFlags = syscall.O_NONBLOCK | syscall.O_NOCTTY

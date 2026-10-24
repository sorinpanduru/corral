//go:build unix

package markerscan

import (
	"os"
	"syscall"
)

// redirectFDs saves fds 1 and 2, points them at the given pipe write ends and
// returns a func that restores them. Process-wide; not parallel-safe.
func redirectFDs(out, errw *os.File) (restore func(), err error) {
	saved1, err := syscall.Dup(1)
	if err != nil {
		return nil, err
	}
	saved2, err := syscall.Dup(2)
	if err != nil {
		syscall.Close(saved1)
		return nil, err
	}
	syscall.CloseOnExec(saved1)
	syscall.CloseOnExec(saved2)
	if err := dup2(int(out.Fd()), 1); err != nil {
		syscall.Close(saved1)
		syscall.Close(saved2)
		return nil, err
	}
	if err := dup2(int(errw.Fd()), 2); err != nil {
		_ = dup2(saved1, 1)
		syscall.Close(saved1)
		syscall.Close(saved2)
		return nil, err
	}
	return func() {
		_ = dup2(saved1, 1)
		_ = dup2(saved2, 2)
		syscall.Close(saved1)
		syscall.Close(saved2)
	}, nil
}

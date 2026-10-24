//go:build !linux

package upstreamnet

import (
	"net"
	"syscall"
)

func userTimeoutControl(_, _ string, _ syscall.RawConn) error { return nil }

// Peek is not supported off Linux: it always reports PeekNone. Callers must
// fall back to a zero-deadline read at checkout and treat data or an error as
// a fault (ADR-0018).
func Peek(net.Conn) (PeekState, error) { return PeekNone, nil }

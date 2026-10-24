//go:build !linux

package upstreamnet

import (
	"net"
	"syscall"
)

func userTimeoutControl(_, _ string, _ syscall.RawConn) error { return nil }

// Peek is not supported off Linux: it reports PeekNone. Callers fall back to
// checking at checkout (ADR-0018).
func Peek(net.Conn) (PeekState, error) { return PeekNone, nil }

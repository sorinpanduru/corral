//go:build linux

package upstreamnet

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func userTimeoutControl(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(UserTimeout.Milliseconds()))
	}); err != nil {
		return err
	}
	return serr
}

// Peek looks at the socket without consuming data and without blocking. On a
// TLS connection it looks at the underlying TCP socket, so any TLS record
// counts as data. A locally closed connection returns an error.
func Peek(c net.Conn) (PeekState, error) {
	for {
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		c = u.NetConn()
	}
	sc, ok := c.(syscall.Conn)
	if !ok {
		return PeekNone, nil
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return PeekNone, err
	}
	var (
		n    int
		serr error
		buf  [1]byte
	)
	if err := rc.Read(func(fd uintptr) bool {
		n, _, serr = unix.Recvfrom(int(fd), buf[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
		return true
	}); err != nil {
		return PeekNone, err
	}
	switch {
	case serr == nil && n > 0:
		return PeekData, nil
	case serr == nil:
		return PeekClosed, nil
	case serr == unix.EAGAIN || serr == unix.EINTR:
		return PeekNone, nil
	case serr == unix.ECONNRESET || serr == unix.EPIPE || serr == unix.ETIMEDOUT:
		return PeekClosed, nil
	}
	return PeekNone, serr
}

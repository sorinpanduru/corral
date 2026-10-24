//go:build linux

package upstreamnet_test

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/upstreamnet"
)

func TestConstants_FR28(t *testing.T) {
	if upstreamnet.KeepAliveIdle != 30*time.Second || upstreamnet.KeepAliveInterval != 10*time.Second ||
		upstreamnet.KeepAliveCount != 3 || upstreamnet.UserTimeout != 60*time.Second {
		t.Fatal("keepalive or user-timeout constants differ from FR-28")
	}
}

func sockopt(t *testing.T, c net.Conn, level, opt int) int {
	t.Helper()
	rc, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var v int
	var serr error
	if err := rc.Control(func(fd uintptr) { v, serr = unix.GetsockoptInt(int(fd), level, opt) }); err != nil {
		t.Fatal(err)
	}
	if serr != nil {
		t.Fatal(serr)
	}
	return v
}

// AC-14, FR-28: after Dial the socket carries the fixed keepalive values and
// TCP_USER_TIMEOUT.
func TestDialSocketOptions_AC14(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := upstreamnet.Dial(ctx, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, ok := c.(*net.TCPConn); !ok {
		t.Fatalf("Dial returned %T, want *net.TCPConn", c)
	}
	for _, o := range []struct {
		name       string
		level, opt int
		want       int
	}{
		{"SO_KEEPALIVE", unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1},
		{"TCP_KEEPIDLE", unix.IPPROTO_TCP, unix.TCP_KEEPIDLE, 30},
		{"TCP_KEEPINTVL", unix.IPPROTO_TCP, unix.TCP_KEEPINTVL, 10},
		{"TCP_KEEPCNT", unix.IPPROTO_TCP, unix.TCP_KEEPCNT, 3},
		{"TCP_USER_TIMEOUT", unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, 60000},
	} {
		if got := sockopt(t, c, o.level, o.opt); got != o.want {
			t.Errorf("%s = %d, want %d", o.name, got, o.want)
		}
	}
}

// Dial failures classify through errclass: a refused port is
// network/ECONNREFUSED, an expired context is a timeout.
func TestDialFailures_FR28(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = upstreamnet.Dial(ctx, addr)
	if err == nil {
		t.Fatal("Dial to a closed port succeeded")
	}
	if c, d := errclass.Classify(err); c != errclass.ClassNetwork || d != errclass.DetailECONNREFUSED {
		t.Fatalf("Classify = (%s, %s), want network/ECONNREFUSED", c, d)
	}

	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if _, err := upstreamnet.Dial(done, addr); err == nil {
		t.Fatal("Dial with a canceled context succeeded")
	}
	expired, c2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer c2()
	_, err = upstreamnet.Dial(expired, "127.0.0.1:1")
	if err == nil {
		t.Fatal("Dial with an expired context succeeded")
	}
	if c, _ := errclass.Classify(err); c != errclass.ClassTimeout {
		t.Fatalf("expired context classified %s, want timeout", c)
	}
}

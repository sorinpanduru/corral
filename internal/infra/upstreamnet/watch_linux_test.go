//go:build linux

package upstreamnet_test

import (
	"net"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/upstreamnet"
)

// FR-45: a peer close is detected within 100 ms.
func TestWatchCloseDetectsPeerClose_FR45(t *testing.T) {
	c, s := tcpPair(t)
	stop := make(chan struct{})
	defer close(stop)
	ch := upstreamnet.WatchClose(c, stop)
	if closedWithin(ch, 50*time.Millisecond) {
		t.Fatal("reported close on a live connection")
	}
	s.Close()
	if !closedWithin(ch, 100*time.Millisecond+400*time.Millisecond) {
		t.Fatal("peer close not detected")
	}
}

// FR-45: pending data stops the watch without consuming the byte, and is not
// reported as a close.
func TestWatchClosePendingDataNotConsumed_FR45(t *testing.T) {
	c, s := tcpPair(t)
	stop := make(chan struct{})
	defer close(stop)
	ch := upstreamnet.WatchClose(c, stop)
	if _, err := s.Write([]byte{0x42}); err != nil {
		t.Fatal(err)
	}
	if closedWithin(ch, 300*time.Millisecond) {
		t.Fatal("data was reported as a close")
	}
	buf := make([]byte, 1)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(buf)
	if err != nil || n != 1 || buf[0] != 0x42 {
		t.Fatalf("byte not readable after the watch: n=%d err=%v", n, err)
	}
}

// FR-45: stop ends the watcher; a later close is not reported.
func TestWatchCloseStop_FR45(t *testing.T) {
	c, s := tcpPair(t)
	stop := make(chan struct{})
	ch := upstreamnet.WatchClose(c, stop)
	close(stop)
	time.Sleep(100 * time.Millisecond)
	s.Close()
	if closedWithin(ch, 300*time.Millisecond) {
		t.Fatal("a stopped watcher reported a close")
	}
}

// FR-62: Peek tells none, data and closed apart on plaintext, never consumes,
// and returns at once.
func TestPeekPlain_FR62(t *testing.T) {
	c, s := tcpPair(t)
	begin := time.Now()
	st, err := upstreamnet.Peek(c)
	if err != nil || st != upstreamnet.PeekNone {
		t.Fatalf("quiet socket: %v %v", st, err)
	}
	if time.Since(begin) > 100*time.Millisecond {
		t.Fatal("Peek blocked on a quiet socket")
	}
	if _, err := s.Write([]byte("xyz")); err != nil {
		t.Fatal(err)
	}
	waitPeek(t, c, upstreamnet.PeekData)
	// Peeking twice still shows data: nothing consumed.
	waitPeek(t, c, upstreamnet.PeekData)
	buf := make([]byte, 3)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := c.Read(buf); err != nil || string(buf[:n]) != "xyz" {
		t.Fatalf("data consumed by Peek: n=%d err=%v", n, err)
	}
	c.SetReadDeadline(time.Time{})
	if st, _ := upstreamnet.Peek(c); st != upstreamnet.PeekNone {
		t.Fatalf("after reading everything: %v", st)
	}
	s.Close()
	waitPeek(t, c, upstreamnet.PeekClosed)
}

func waitPeek(t *testing.T, c net.Conn, want upstreamnet.PeekState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var st upstreamnet.PeekState
	for time.Now().Before(deadline) {
		var err error
		st, err = upstreamnet.Peek(c)
		if err != nil {
			t.Fatalf("Peek error: %v", err)
		}
		if st == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Peek = %v, want %v", st, want)
}

// Data followed by a close reports data first (the data is unread), and a
// peek on a closed local connection is an error.
func TestPeekDataBeforeClose_FR62(t *testing.T) {
	c, s := tcpPair(t)
	s.Write([]byte{1})
	s.Close()
	time.Sleep(50 * time.Millisecond)
	st, err := upstreamnet.Peek(c)
	if err != nil || st != upstreamnet.PeekData {
		t.Fatalf("Peek = %v, %v; want data (unread byte precedes the FIN)", st, err)
	}
	c.Close()
	if _, err := upstreamnet.Peek(c); err == nil {
		t.Fatal("Peek on a locally closed connection returned no error")
	}
}

// FR-62: on a TLS connection Peek looks at the TCP socket: any TLS record is
// data, a TCP close is closed.
func TestPeekTLS_FR62(t *testing.T) {
	cli, srv := tlsPair(t)
	if st, err := upstreamnet.Peek(cli); err != nil || st != upstreamnet.PeekNone {
		t.Fatalf("quiet TLS conn: %v %v", st, err)
	}
	if _, err := srv.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	waitPeek(t, cli, upstreamnet.PeekData)
	buf := make([]byte, 2)
	cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := cli.Read(buf); err != nil || string(buf[:n]) != "hi" {
		t.Fatalf("TLS data consumed or broken by Peek: n=%d err=%v", n, err)
	}
	cli.SetReadDeadline(time.Time{})
	srv.NetConn().Close()
	waitPeek(t, cli, upstreamnet.PeekClosed)
}

// FR-62: WatchReadable reports data and close, on plaintext and TLS, without
// consuming.
func TestWatchReadable_FR62(t *testing.T) {
	recv := func(t *testing.T, ch <-chan upstreamnet.PeekState) upstreamnet.PeekState {
		t.Helper()
		select {
		case st := <-ch:
			return st
		case <-time.After(2 * time.Second):
			t.Fatal("WatchReadable reported nothing")
			return upstreamnet.PeekNone
		}
	}
	t.Run("plain data", func(t *testing.T) {
		c, s := tcpPair(t)
		stop := make(chan struct{})
		defer close(stop)
		ch := upstreamnet.WatchReadable(c, stop)
		s.Write([]byte{7})
		if st := recv(t, ch); st != upstreamnet.PeekData {
			t.Fatalf("got %v, want data", st)
		}
		buf := make([]byte, 1)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if n, err := c.Read(buf); err != nil || n != 1 || buf[0] != 7 {
			t.Fatal("byte consumed by the watcher")
		}
	})
	t.Run("plain close", func(t *testing.T) {
		c, s := tcpPair(t)
		stop := make(chan struct{})
		defer close(stop)
		ch := upstreamnet.WatchReadable(c, stop)
		s.Close()
		if st := recv(t, ch); st != upstreamnet.PeekClosed {
			t.Fatalf("got %v, want closed", st)
		}
	})
	t.Run("tls data", func(t *testing.T) {
		cli, srv := tlsPair(t)
		stop := make(chan struct{})
		defer close(stop)
		ch := upstreamnet.WatchReadable(cli, stop)
		srv.Write([]byte("x"))
		if st := recv(t, ch); st != upstreamnet.PeekData {
			t.Fatalf("got %v, want data", st)
		}
	})
	t.Run("tls close", func(t *testing.T) {
		cli, srv := tlsPair(t)
		stop := make(chan struct{})
		defer close(stop)
		ch := upstreamnet.WatchReadable(cli, stop)
		srv.NetConn().Close()
		if st := recv(t, ch); st != upstreamnet.PeekClosed {
			t.Fatalf("got %v, want closed", st)
		}
	})
	t.Run("stop reports nothing", func(t *testing.T) {
		c, s := tcpPair(t)
		stop := make(chan struct{})
		ch := upstreamnet.WatchReadable(c, stop)
		close(stop)
		time.Sleep(100 * time.Millisecond)
		s.Write([]byte{1})
		select {
		case st, ok := <-ch:
			if ok && st != upstreamnet.PeekNone {
				t.Fatalf("stopped watcher reported %v", st)
			}
		case <-time.After(300 * time.Millisecond):
		}
	})
}

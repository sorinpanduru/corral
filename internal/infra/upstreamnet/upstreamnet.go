// Package upstreamnet holds the socket-level helpers for upstream and client
// connections: the dialer with fixed keepalive values, TLS configuration
// builders that always verify, and non-consuming liveness peeks (SPEC-002
// FR-28, FR-41, FR-44, FR-45, FR-62; ADR-0018, ADR-0019).
//
// Compliance: certificate and key material is taken as bytes from the caller
// (the settings layer reads files), is never logged, and never appears in
// errors, which are constant sentinels.
//
// Concurrency precondition for Peek, WatchClose and WatchReadable: nothing may
// read from the connection while a watcher runs. Peek never consumes data, but
// a concurrent reader would race with it for the same bytes and could make a
// close or data event be missed or misreported. Stop the watcher (close its
// stop channel) before reading.
//
// Off Linux, Peek always returns PeekNone: the watchers never report anything.
// Callers (the upstream connection pool) must then fall back to a zero-deadline
// read at checkout and treat returned data or an error as a fault, discarding
// the connection (ADR-0018).
package upstreamnet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"time"
)

// Fixed socket values (FR-28).
const (
	KeepAliveIdle     = 30 * time.Second
	KeepAliveInterval = 10 * time.Second
	KeepAliveCount    = 3
	UserTimeout       = 60 * time.Second
)

// Constant-text errors; they never carry input bytes.
var (
	ErrBadCA      = errors.New("upstreamnet: no usable CA certificate in the supplied PEM")
	ErrBadKeyPair = errors.New("upstreamnet: certificate and key could not be loaded as a pair")
)

// Dial connects to addr over TCP with keepalive and TCP_USER_TIMEOUT set. The
// deadline comes from ctx. The returned connection is a *net.TCPConn.
func Dial(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     KeepAliveIdle,
			Interval: KeepAliveInterval,
			Count:    KeepAliveCount,
		},
		Control: userTimeoutControl,
	}
	return d.DialContext(ctx, "tcp", addr)
}

// ClientTLS returns a client configuration: TLS 1.2 or later, standard
// verification always on. caPEM nil selects the system roots.
func ClientTLS(caPEM []byte, serverName string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caPEM != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, ErrBadCA
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// ServerTLS returns a server configuration from certificate and key bytes.
// When clientCAPEM is non-nil, client certificates are required and verified.
func ServerTLS(certPEM, keyPEM, clientCAPEM []byte) (*tls.Config, error) {
	kp, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, ErrBadKeyPair
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{kp}}
	if clientCAPEM != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(clientCAPEM) {
			return nil, ErrBadCA
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// PeekState is the result of a non-consuming look at a socket.
type PeekState int

const (
	// PeekNone means nothing is pending and the peer has not closed.
	PeekNone PeekState = iota
	// PeekData means unread bytes are pending (on TLS: any record).
	PeekData
	// PeekClosed means the peer closed or reset the connection.
	PeekClosed
)

func (s PeekState) String() string {
	switch s {
	case PeekNone:
		return "none"
	case PeekData:
		return "data"
	case PeekClosed:
		return "closed"
	}
	return "unknown"
}

const pollInterval = 25 * time.Millisecond

// isTLS reports whether c is a TLS connection (it exposes its transport).
func isTLS(c net.Conn) bool {
	_, ok := c.(*tls.Conn)
	return ok
}

// WatchClose returns a channel that is closed when a peer close is seen on a
// plaintext connection. Pending data stops the watch without consuming it.
// stop ends the watcher. On TLS connections nothing is ever reported, since
// the transport cannot be told apart from a close_notify exchange.
func WatchClose(c net.Conn, stop <-chan struct{}) <-chan struct{} {
	ch := make(chan struct{})
	if isTLS(c) {
		return ch
	}
	go func() {
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			st, err := Peek(c)
			if err != nil || st == PeekData {
				return
			}
			if st == PeekClosed {
				close(ch)
				return
			}
		}
	}()
	return ch
}

// WatchReadable reports the first PeekData or PeekClosed seen on c, then
// closes the channel. stop ends the watcher without a report. Data is never
// consumed.
func WatchReadable(c net.Conn, stop <-chan struct{}) <-chan PeekState {
	ch := make(chan PeekState, 1)
	go func() {
		defer close(ch)
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			st, err := Peek(c)
			if err != nil {
				ch <- PeekClosed
				return
			}
			if st != PeekNone {
				ch <- st
				return
			}
		}
	}()
	return ch
}

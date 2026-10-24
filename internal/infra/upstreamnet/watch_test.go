package upstreamnet_test

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/upstreamnet"
)

func closedWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// tlsPair returns a connected TLS client and the server-side *tls.Conn.
func tlsPair(t *testing.T) (client, server *tls.Conn) {
	t.Helper()
	ca := newPKI(t, "ca")
	l := ca.issue(t, 2, "mongo.test")
	scfg, err := upstreamnet.ServerTLS(l.CertPEM, l.KeyPEM, nil)
	if err != nil {
		t.Fatal(err)
	}
	ccfg, err := upstreamnet.ClientTLS(ca.CAPEM, "mongo.test")
	if err != nil {
		t.Fatal(err)
	}
	rawC, rawS := tcpPair(t)
	srv := tls.Server(rawS, scfg)
	errc := make(chan error, 1)
	go func() { errc <- srv.Handshake() }()
	cli := tls.Client(rawC, ccfg)
	if err := cli.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	return cli, srv
}

// FR-45 (as implemented per the plan): the close watcher does nothing on a TLS
// connection, even when the peer closes the socket.
func TestWatchCloseTLSDetectsNothing_FR45(t *testing.T) {
	cli, srv := tlsPair(t)
	stop := make(chan struct{})
	defer close(stop)
	ch := upstreamnet.WatchClose(cli, stop)
	srv.NetConn().Close()
	if closedWithin(ch, 300*time.Millisecond) {
		t.Fatal("WatchClose reported a close on a TLS connection")
	}
}

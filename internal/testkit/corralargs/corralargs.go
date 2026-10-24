// Package corralargs builds command-line arguments for tests that start corral
// (ADR-0021 minimal arguments). Test infrastructure only.
package corralargs

import (
	"net"
	"strconv"
	"testing"
)

// FreePort returns a port that was free on 127.0.0.1 at the time of the call.
func FreePort(tb testing.TB) int {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("corralargs: no free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// Minimal returns the minimal arguments: the upstream host, no upstream
// authentication, and a loopback proxy listener on a free port.
func Minimal(tb testing.TB, upstream string) []string {
	tb.Helper()
	return []string{
		"--upstream.hosts=" + upstream,
		"--upstream.auth.mechanism=none",
		"--proxy.listen=127.0.0.1:" + strconv.Itoa(FreePort(tb)),
	}
}

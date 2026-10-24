package corralargs_test

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/corralargs"
)

// FreePort returns a port that can be bound on 127.0.0.1 right after the call.
func TestFreePort(t *testing.T) {
	for i := 0; i < 5; i++ {
		p := corralargs.FreePort(t)
		if p < 1024 || p > 65535 {
			t.Fatalf("port %d out of range", p)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			t.Fatalf("port %d not bindable: %v", p, err)
		}
		ln.Close()
	}
}

// ADR-0021: Minimal returns exactly the minimal arguments: upstream hosts,
// mechanism none, and a loopback proxy listener on a free port.
func TestMinimal_ADR0021(t *testing.T) {
	args := corralargs.Minimal(t, "127.0.0.1:27017")
	want := map[string]string{
		"--upstream.hosts":          "127.0.0.1:27017",
		"--upstream.auth.mechanism": "none",
	}
	seen := map[string]string{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			t.Fatalf("argument %q is not --name=value", a)
		}
		seen[k] = v
	}
	for k, v := range want {
		if seen[k] != v {
			t.Errorf("%s = %q, want %q", k, seen[k], v)
		}
	}
	listen, ok := seen["--proxy.listen"]
	if !ok {
		t.Fatal("no --proxy.listen")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("--proxy.listen = %q", listen)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1024 {
		t.Fatalf("bad port in %q", listen)
	}
	if len(args) != 3 {
		t.Fatalf("Minimal returned %d arguments, want exactly 3", len(args))
	}
}

// Two calls give independent listen ports in the common case; at minimum each
// is a valid loopback address.
func TestMinimalTwice(t *testing.T) {
	a := corralargs.Minimal(t, "127.0.0.1:1")
	b := corralargs.Minimal(t, "127.0.0.1:1")
	if strings.Join(a, " ") == strings.Join(b, " ") {
		t.Fatal("two Minimal calls returned identical listen addresses")
	}
}

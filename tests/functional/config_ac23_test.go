//go:build functional

package functional

import (
	"net"
	"strings"
	"testing"
)

// SPEC-003 AC-23 (FR-24, Amendment 3): an admin.listen address that is already
// in use is a startup failure, not a configuration error. Exit 1; exactly one
// "admin listener bind failed" record with setting=admin.listen, error=config
// and corral.error.detail=EADDRINUSE; no "invalid configuration" record; the
// address (here a marker host name, when this machine resolves *.localhost)
// appears nowhere in stdout or stderr in any encoding the AC-33 harness scans.

const msgBindFailedFn = "admin listener bind failed"

// bindTarget pre-binds an address and returns it as admin.listen text. When
// "<marker>.localhost" resolves here, the marker is the host name, so the
// marker scan can prove the address is not echoed. Otherwise a loopback IPv4
// address is used and the address text is checked directly.
func bindTarget(t *testing.T, marker string) (listen string, ln net.Listener, hostIsMarker bool) {
	t.Helper()
	name := marker + ".localhost"
	if l, err := net.Listen("tcp", name+":0"); err == nil {
		_, port, _ := net.SplitHostPort(l.Addr().String())
		return net.JoinHostPort(name, port), l, true
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l.Addr().String(), l, false
}

func TestFnAC23_AdminBindFailure(t *testing.T) {
	m := fmark(t)
	listen, ln, hostIsMarker := bindTarget(t, m)
	defer ln.Close()
	_, port, _ := net.SplitHostPort(listen)

	p := startProc(t, baseEnv(), "--admin.listen="+listen, "--log.level=debug")
	if code := p.waitExit(); code != 1 {
		t.Fatalf("exit code = %d, want 1 (FR-24)", code)
	}
	recs := p.records()
	var errs []map[string]any
	for _, r := range recs {
		if str(r, "msg") == "invalid configuration" {
			t.Errorf("an \"invalid configuration\" record on a bind failure")
		}
		if str(r, "level") == "ERROR" {
			errs = append(errs, r)
		}
	}
	if len(errs) != 1 {
		t.Fatalf("%d ERROR records, want exactly 1", len(errs))
	}
	e := errs[0]
	if str(e, "msg") != msgBindFailedFn {
		t.Errorf("msg = %q, want %q", str(e, "msg"), msgBindFailedFn)
	}
	if str(e, "setting") != "admin.listen" || str(e, "component") != "config" {
		t.Errorf("setting/component = %q/%q, want admin.listen/config", str(e, "setting"), str(e, "component"))
	}
	if str(e, "error") != "config" {
		t.Errorf("error = %q, want config", str(e, "error"))
	}
	if str(e, "corral.error.detail") != "EADDRINUSE" {
		t.Errorf("corral.error.detail = %q, want EADDRINUSE", str(e, "corral.error.detail"))
	}
	requireStderrEmpty(t, p)

	out := string(p.stdout.Bytes()) + string(p.stderr.Bytes())
	for _, bad := range []string{listen, ":" + port, "127.0.0.1", "localhost"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains address text (length %d)", len(bad))
		}
	}
	t.Logf("host name was the marker: %v", hostIsMarker)
	// Every encoding the AC-33 harness checks (raw, hex, base64, case-folded).
	scanClean(t, p, m)
}

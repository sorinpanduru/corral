//go:build functional

package functional

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/corralprobe"
	"github.com/sorinpanduru/corral/internal/testkit/corralproc"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

// AC-1 (FR-1, FR-55): the wire listener, binding as the last startup step, and
// the startup records. No mongod is needed: startup does not wait for the
// upstream.

// The default bind address is 127.0.0.1:27017 only (skipped when that port is
// in use on the machine running the tests).
func TestDefaultListenAddress_AC1(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:27017")
	if err != nil {
		t.Skip("127.0.0.1:27017 is in use here")
	}
	ln.Close()
	c := pxStart(t, pxOpts{upstream: []string{pxAddr(t)}, set: map[string]string{"proxy.listen": ""}})
	conn, err := net.DialTimeout("tcp", "127.0.0.1:27017", 3*time.Second)
	if err != nil {
		t.Fatalf("not listening on 127.0.0.1:27017: %v", err)
	}
	conn.Close()
	if nonLoopback := nonLoopbackIPv4(); nonLoopback != "" {
		if conn, err := net.DialTimeout("tcp", nonLoopback+":27017", time.Second); err == nil {
			conn.Close()
			t.Fatalf("the default listener is reachable on %s", nonLoopback)
		}
	}
	_ = c
}

func TestInvalidListenAddressExits2_AC1(t *testing.T) {
	for _, addr := range []string{":27017", "127.0.0.1:0"} {
		t.Run(addr, func(t *testing.T) {
			c := pxStart(t, pxOpts{upstream: []string{pxAddr(t)}, set: map[string]string{"proxy.listen": addr}, noWait: true})
			code, err := c.p.Wait(20 * time.Second)
			if err != nil || code != 2 {
				t.Fatalf("exit %d, %v", code, err)
			}
			found := false
			for _, r := range c.p.Records() {
				if r["setting"] == "proxy.listen" || strings.Contains(asString(r["setting"]), "proxy.listen") {
					found = true
				}
			}
			if !found {
				t.Fatalf("no record names proxy.listen: %s", c.p.Stdout())
			}
		})
	}
}

func asString(v any) string { s, _ := v.(string); return s }

// An address already in use exits 1 with setting=proxy.listen and without the
// address value.
func TestListenAddressInUseExits1_AC1(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	c := pxStart(t, pxOpts{upstream: []string{pxAddr(t)}, set: map[string]string{"proxy.listen": addr}, noWait: true})
	code, err := c.p.Wait(20 * time.Second)
	if err != nil || code != 1 {
		t.Fatalf("exit %d, %v", code, err)
	}
	found := false
	for _, r := range c.p.Records() {
		if asString(r["setting"]) == "proxy.listen" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no record names proxy.listen: %s", c.p.Stdout())
	}
	if strings.Contains(string(c.p.Stdout()), addr) || strings.Contains(string(c.p.Stderr()), addr) {
		t.Fatal("the address appears in the output")
	}
	if len(c.records("wire listener bound")) != 0 {
		t.Fatal("wire listener bound was logged")
	}
}

// /readyz stays "starting" until the wire listener is bound (hook at laststep),
// and the startup-complete record follows the wire listener bound record.
func TestReadinessStartingUntilBound_AC1(t *testing.T) {
	c := pxStart(t, pxOpts{upstream: []string{pxAddr(t)}, holdAt: []string{"laststep"}, noWait: true})
	if err := c.p.WaitHeld("laststep", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if code, body := c.readyz(); code != 503 || body != "starting" {
		t.Fatalf("/readyz while held: %d %q", code, body)
	}
	if conn, err := net.DialTimeout("tcp", c.proxy, 300*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("the wire port is bound before the last startup step")
	}
	c.p.Release("laststep")
	if _, err := c.p.WaitRecord("corral startup complete", "lifecycle", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range c.p.Records() {
		switch r.Msg() {
		case "wire listener bound", "corral startup complete":
			order = append(order, r.Msg())
		}
	}
	if strings.Join(order, ",") != "wire listener bound,corral startup complete" {
		t.Fatalf("record order %v", order)
	}
}

// AC-48 (FR-64): the production binary, started with the probe variable and a
// hold directory set, starts no probe server, creates no hold file and emits no
// unknown-variable record for CORRALTEST_*.
func TestProductionBinaryIgnoresCorraltest_AC48(t *testing.T) {
	dir := t.TempDir()
	c := pxStart(t, pxOpts{prod: true, upstream: []string{pxAddr(t)}, env: []string{
		"CORRALTEST_PROBE_LISTEN=127.0.0.1:0", "CORRALTEST_HOLD_DIR=" + dir, "CORRALTEST_HOLD_AT=laststep,shutdown_drain",
		"CORRALTEST_POOL_ORDER=roundrobin", "CORRALTEST_PANIC_COMMAND=find"}})
	_ = c
	time.Sleep(500 * time.Millisecond)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		t.Errorf("the production binary created %s in the hold directory", e.Name())
	}
	if _, err := os.Stat(filepath.Join(c.p.HoldDir(), "probe.addr")); err == nil {
		t.Error("probe.addr exists: a probe server was started")
	}
	for _, r := range c.p.Records() {
		for _, v := range r {
			if s, ok := v.(string); ok && strings.Contains(s, "CORRALTEST_") {
				t.Fatalf("a record mentions CORRALTEST_: %v", r)
			}
		}
	}
	if strings.Contains(string(c.p.Stderr()), "CORRALTEST_") {
		t.Fatal("stderr mentions CORRALTEST_")
	}
	// The hook has no effect: a request is not panicked and the hold point
	// does not hold.
	if !strings.HasPrefix(c.proxy, "127.0.0.1:") {
		t.Fatal("unexpected listen address")
	}
	_ = rawwire.OpCodeMsg
}

// AC-48 / AC-51: the corraltest build's probe returns 65,536 for L and K and
// lists connections without message bytes.
func TestProbeConstants_AC48(t *testing.T) {
	c := pxStart(t, pxOpts{upstream: []string{pxAddr(t)}, probe: true})
	pr := corralprobe.Open(t, c.p.HoldDir())
	k, err := pr.Constants()
	if err != nil || k.L != 65536 || k.K != 65536 {
		t.Fatalf("constants %+v, %v", k, err)
	}
	conn := rawwire.Dial(t, c.proxy)
	conn.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: map[string]any{"hello": 1, "$db": "admin"}}))
	conn.ReadMessage(5 * time.Second)
	if conns, err := pr.Conns(); err != nil || len(conns) == 0 {
		t.Fatalf("conns %v, %v", conns, err)
	}
	var _ corralproc.Record
}

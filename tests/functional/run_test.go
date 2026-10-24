//go:build functional

package functional

import (
	"net"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// AC-3 (FR-4, FR-5, FR-7, FR-28): default log settings plus tracing.enabled=true
// with an unreachable collector; admin requests; SIGTERM. Every stdout line is
// JSON, lifecycle records mark starting / startup complete / shutting down,
// nothing is written to stderr, exit 0. (admin.listen is set to a free
// loopback port so the test does not depend on 9187; AC-11 covers the default
// bind.)
func TestAC3_TracingUnreachableCollector(t *testing.T) {
	for _, proto := range []string{"http/protobuf", "grpc"} {
		t.Run(proto, func(t *testing.T) {
			addr := freeAddr(t)
			ep := otlpcollector.UnreachableEndpoint(t)
			p := startProc(t, baseEnv(
				"OTEL_EXPORTER_OTLP_ENDPOINT=http://"+ep,
				"OTEL_EXPORTER_OTLP_PROTOCOL="+proto,
			), "-tracing.enabled=true", "-admin.listen="+addr)
			p.waitReady(addr)
			for _, path := range []string{"/healthz", "/readyz", "/metrics", "/nope"} {
				if _, err := httpDo("GET", addr, path); err != nil {
					t.Fatalf("GET %s: %v", path, err)
				}
			}
			if code := p.sigterm(); code != 0 {
				t.Errorf("exit code = %d, want 0 after SIGTERM", code)
			}
			recs := p.records() // fails on any non-JSON stdout line
			if len(recs) == 0 {
				t.Fatal("no stdout records")
			}
			for _, r := range recs {
				for _, k := range []string{"time", "level", "msg", "component"} {
					if _, ok := r[k]; !ok {
						t.Errorf("record missing %q", k)
					}
				}
			}
			msgs := lifecycleMsgs(recs)
			if len(msgs) != 3 {
				t.Fatalf("lifecycle records = %q, want starting, startup complete, shutting down", msgs)
			}
			for i, frag := range []string{"starting", "startup complete", "shutting down"} {
				if !strings.Contains(strings.ToLower(msgs[i]), frag) {
					t.Errorf("lifecycle record %d = %q, want it to denote %q", i, msgs[i], frag)
				}
			}
			if b := p.stderr.Bytes(); len(b) != 0 {
				t.Errorf("stderr has %d bytes, want empty", len(b))
			}
		})
	}
}

// AC-7 (FR-9, FR-14, FR-15) on the binary: paths, methods, 404s.
func TestAC7_AdminSurface(t *testing.T) {
	addr := freeAddr(t)
	p := startProc(t, baseEnv(), "-admin.listen="+addr)
	p.waitReady(addr)

	m, err := httpDo("GET", addr, "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	if m.status != 200 {
		t.Fatalf("GET /metrics = %d", m.status)
	}
	if !strings.HasPrefix(m.ctype, "text/plain") || !strings.Contains(m.ctype, "version=0.0.4") {
		t.Errorf("/metrics Content-Type = %q, want Prometheus text format", m.ctype)
	}
	for _, want := range []string{"go_", "process_", "corral_tracing_spans_dropped_total"} {
		if !strings.Contains(m.body, "\n"+want) && !strings.HasPrefix(m.body, want) {
			t.Errorf("/metrics lacks %s metrics", want)
		}
	}

	for _, path := range []string{"/metrics", "/healthz", "/readyz"} {
		if r, err := httpDo("HEAD", addr, path); err != nil || r.status != 200 {
			t.Errorf("HEAD %s = %v %v, want 200", path, r.status, err)
		}
		for _, method := range []string{"POST", "PUT", "DELETE"} {
			r, err := httpDo(method, addr, path)
			if err != nil || r.status != 405 {
				t.Errorf("%s %s = %d %v, want 405", method, path, r.status, err)
			}
			if r.allow != "GET, HEAD" {
				t.Errorf("%s %s Allow = %q, want %q", method, path, r.allow, "GET, HEAD")
			}
		}
	}
	for _, path := range []string{"/metrics/", "/debug/pprof/", "/debug/pprof/heap", "/debug/vars", "/unknown", "/"} {
		if r, err := httpDo("GET", addr, path); err != nil || r.status != 404 {
			t.Errorf("GET %s = %d %v, want 404", path, r.status, err)
		}
	}
	q, err := httpDo("GET", addr, "/healthz?x=1")
	if err != nil || q.status != 200 || q.body != "ok\n" {
		t.Errorf("GET /healthz?x=1 = %d %q %v, want like /healthz", q.status, q.body, err)
	}
	if code := p.sigterm(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// AC-11 (FR-16) default bind: 127.0.0.1:9187 only. Skipped when the port is
// busy on this host.
func TestAC11_DefaultBind(t *testing.T) {
	if ln, err := net.Listen("tcp", "127.0.0.1:9187"); err != nil {
		t.Skipf("127.0.0.1:9187 is already in use on this host (%v); cannot test the default bind", err)
	} else {
		_ = ln.Close()
	}
	p := startProc(t, baseEnv())
	const addr = "127.0.0.1:9187"
	p.waitReady(addr)
	if other := nonLoopbackIPv4(); other != "" && dials(net.JoinHostPort(other, "9187")) {
		t.Errorf("default bind is reachable on a non-loopback address; want loopback only")
	}
	if code := p.sigterm(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	p.records()
	if b := p.stderr.Bytes(); len(b) != 0 {
		t.Errorf("stderr has %d bytes with default settings, want empty", len(b))
	}
}

// AC-11 configured bind: the configured address is used; 0.0.0.0:<port> binds
// all IPv4 interfaces.
func TestAC11_ConfiguredBind(t *testing.T) {
	t.Run("loopback", func(t *testing.T) {
		addr := freeAddr(t)
		p := startProc(t, baseEnv(), "-admin.listen="+addr)
		p.waitReady(addr)
		_, port, _ := net.SplitHostPort(addr)
		if other := nonLoopbackIPv4(); other != "" && dials(net.JoinHostPort(other, port)) {
			t.Errorf("127.0.0.1 bind is reachable on a non-loopback address")
		}
		if code := p.sigterm(); code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	})
	t.Run("all-ipv4", func(t *testing.T) {
		_, port, _ := net.SplitHostPort(freeAddr(t))
		p := startProc(t, baseEnv(), "-admin.listen=0.0.0.0:"+port)
		p.waitReady("127.0.0.1:" + port)
		if other := nonLoopbackIPv4(); other != "" && !dials(net.JoinHostPort(other, port)) {
			t.Errorf("0.0.0.0 bind is not reachable on %s", "a non-loopback local address")
		}
		if code := p.sigterm(); code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	})
}

// AC-12 (FR-17).
func TestAC12_Healthz(t *testing.T) {
	addr := freeAddr(t)
	p := startProc(t, baseEnv(), "-admin.listen="+addr)
	p.waitReady(addr)
	r, err := httpDo("GET", addr, "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if r.status != 200 || r.body != "ok\n" || r.ctype != plainUTF8 {
		t.Errorf("/healthz = %d %q %q, want 200 %q %q", r.status, r.body, r.ctype, "ok\n", plainUTF8)
	}
	if rz, err := httpDo("GET", addr, "/readyz"); err != nil || rz.status != 200 || rz.body != "ok\n" || rz.ctype != plainUTF8 {
		t.Errorf("/readyz after startup = %+v %v, want 200 ok", rz, err)
	}
	if code := p.sigterm(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

package adminhttp_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/health"
)

func readyz(t *testing.T, r *running) (int, string, string) {
	t.Helper()
	code, hdr, body := r.get(t, "/readyz")
	return code, body, hdr.Get("Content-Type")
}

func expectReadyz(t *testing.T, r *running, code int, body string) {
	t.Helper()
	c, b, ct := readyz(t, r)
	if c != code || b != body {
		t.Fatalf("/readyz = %d %q, want %d %q", c, b, code, body)
	}
	if ct != textPlainUTF {
		t.Fatalf("/readyz Content-Type = %q, want %q", ct, textPlainUTF)
	}
}

// AC-31, FR-18: 503 starting before startup completes; the port is not
// consulted; 200 afterwards.
func TestReadyzStartingDoesNotConsultPort(t *testing.T) {
	ready := &fakeReadiness{status: health.Status{Ready: true}}
	r := start(t, ready, nil)
	for i := 0; i < 3; i++ {
		expectReadyz(t, r, 503, "starting\n")
	}
	code, hdr, body := r.do(t, http.MethodHead, "/readyz", nil)
	if code != 503 || body != "" || hdr.Get("Content-Type") != textPlainUTF {
		t.Fatalf("HEAD /readyz while starting = %d %q %q", code, body, hdr.Get("Content-Type"))
	}
	if n := ready.calls.Load(); n != 0 {
		t.Fatalf("readiness port consulted %d times before MarkStarted", n)
	}
	r.srv.MarkStarted()
	expectReadyz(t, r, 200, "ok\n")
	if ready.calls.Load() == 0 {
		t.Fatal("readiness port not consulted after MarkStarted")
	}
}

// AC-31: starting also holds when no check is registered.
func TestReadyzStartingWithoutCheck(t *testing.T) {
	r := start(t, nil, nil)
	expectReadyz(t, r, 503, "starting\n")
	r.srv.MarkStarted()
	expectReadyz(t, r, 200, "ok\n")
}

// AC-13, FR-18: ready check -> 200 ok.
func TestReadyzReadyCheck(t *testing.T) {
	r := start(t, &fakeReadiness{status: health.Status{Ready: true}}, nil)
	r.srv.MarkStarted()
	expectReadyz(t, r, 200, "ok\n")
}

// AC-13, FR-18: not ready with an in-set reason -> 503 <reason>\n.
func TestReadyzInSetReasons(t *testing.T) {
	for _, reason := range health.Reasons() {
		t.Run(string(reason), func(t *testing.T) {
			ready := &fakeReadiness{status: health.Status{Ready: false, Reason: reason}}
			r := start(t, ready, nil)
			r.srv.MarkStarted()
			expectReadyz(t, r, 503, string(reason)+"\n")
		})
	}
}

// The port is consulted per request: readiness changes are visible.
func TestReadyzFollowsPort(t *testing.T) {
	ready := &fakeReadiness{status: health.Status{Ready: true}}
	r := start(t, ready, nil)
	r.srv.MarkStarted()
	expectReadyz(t, r, 200, "ok\n")
	ready.set(health.Status{Reason: health.ReasonStarting})
	expectReadyz(t, r, 503, "starting\n")
	ready.set(health.Status{Ready: true})
	expectReadyz(t, r, 200, "ok\n")
}

// AC-13, FR-18: a reason outside the set -> 503 unknown, never echoed; no
// header or body injection from control characters.
func TestReadyzOutOfSetReasonIsUnknown(t *testing.T) {
	m := marker(t)
	reasons := []health.Reason{
		health.Reason(m), "", "Starting", "starting ", "ok",
		health.Reason(m + "\r\nX-Injected: 1"), health.Reason("\x1b[31m" + m),
	}
	for _, reason := range reasons {
		ready := &fakeReadiness{status: health.Status{Ready: false, Reason: reason}}
		r := start(t, ready, nil)
		r.srv.MarkStarted()
		code, hdr, body := r.get(t, "/readyz")
		if code != 503 || body != "unknown\n" || hdr.Get("Content-Type") != textPlainUTF {
			t.Errorf("reason %q: /readyz = %d %q %q, want 503 unknown", reason, code, body, hdr.Get("Content-Type"))
		}
		if strings.Contains(body, m) || strings.Contains(strings.Join(flat(hdr), "|"), m) {
			t.Errorf("reason %q echoed", reason)
		}
		if hdr.Get("X-Injected") != "" {
			t.Errorf("header injection through reason %q", reason)
		}
		noControl(t, "body", body)
	}
}

// AC-31, FR-18: MarkStopping -> 503 shutting_down regardless of the check,
// without consulting it, synchronously.
func TestReadyzShuttingDown(t *testing.T) {
	ready := &fakeReadiness{status: health.Status{Ready: true}}
	r := start(t, ready, nil)
	r.srv.MarkStarted()
	expectReadyz(t, r, 200, "ok\n")
	before := ready.calls.Load()
	r.srv.MarkStopping()
	for i := 0; i < 20; i++ { // each request is served after MarkStopping returned
		expectReadyz(t, r, 503, "shutting_down\n")
	}
	if got := ready.calls.Load(); got != before {
		t.Fatalf("readiness port consulted %d times after MarkStopping", got-before)
	}
	r.srv.MarkStarted() // must not revive readiness
	expectReadyz(t, r, 503, "shutting_down\n")
}

func TestReadyzShuttingDownWithoutCheckAndBeforeStart(t *testing.T) {
	r := start(t, nil, nil)
	r.srv.MarkStarted()
	r.srv.MarkStopping()
	expectReadyz(t, r, 503, "shutting_down\n")

	r2 := start(t, &fakeReadiness{status: health.Status{Ready: true}}, nil)
	r2.srv.MarkStopping() // stopping before startup completed
	expectReadyz(t, r2, 503, "shutting_down\n")
}

// Race detector: transitions concurrent with requests.
func TestReadyzConcurrentTransitions(t *testing.T) {
	ready := &fakeReadiness{status: health.Status{Ready: true}}
	r := start(t, ready, nil)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				code, body, _ := readyzNoFatal(r)
				switch {
				case code == 200 && body == "ok\n":
				case code == 503 && (body == "starting\n" || body == "shutting_down\n"):
				default:
					t.Errorf("unexpected /readyz %d %q", code, body)
				}
			}
		}()
	}
	r.srv.MarkStarted()
	r.srv.MarkStopping()
	wg.Wait()
	expectReadyz(t, r, 503, "shutting_down\n")
}

func readyzNoFatal(r *running) (int, string, string) {
	resp, err := http.Get(r.base + "/readyz")
	if err != nil {
		return 0, err.Error(), ""
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 64)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String(), resp.Header.Get("Content-Type")
}

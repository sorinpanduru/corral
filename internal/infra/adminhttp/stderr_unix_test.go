//go:build unix

package adminhttp_test

import (
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/infra/adminhttp"
	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
)

// With a nil Options.ErrorLog a handler panic must not make net/http write to
// the process stderr (fd 2): the server installs a discarding logger. Not
// parallel: CaptureStdio swaps process-wide fds.
func TestNilErrorLogPanicWritesNothingToStderr(t *testing.T) {
	m := marker(t)
	stop := markerscan.CaptureStdio(t)
	ready := &fakeReadiness{status: health.Status{Ready: true}, panicV: m}
	srv := adminhttp.New(adminhttp.Options{Addr: loopback, Gatherer: newRegistry(t), Readiness: ready})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	srv.MarkStarted()
	r := &running{srv: srv, base: "http://" + srv.Addr().String()}
	for i := 0; i < 3; i++ {
		resp, err := newClient().Get(r.base + "/readyz")
		if err == nil {
			resp.Body.Close()
		}
	}
	if ready.calls.Load() == 0 {
		t.Fatal("handler never ran")
	}
	time.Sleep(100 * time.Millisecond)
	_ = srv.Shutdown(t.Context())
	<-done
	out, errb := stop()
	for name, b := range map[string][]byte{"stdout": out, "stderr": errb} {
		if strings.Contains(string(b), m) || strings.Contains(string(b), "panic serving") {
			t.Errorf("%s received net/http panic output: %q", name, b)
		}
	}
}

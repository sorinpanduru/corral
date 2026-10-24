package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	corralrt "github.com/sorinpanduru/corral/internal/app/runtime"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
)

// AC-31 (FR-18): /readyz is 503 "starting\n" while startup is held before its
// last step, 200 "ok\n" after startup completes (and the startup-complete
// record exists), and 503 "shutting_down\n" from the lifecycle "shutting down"
// record onward. Registered readiness checks cannot be injected through Run,
// so the "check not consulted" sub-bullets are covered at the adminhttp level.
func TestReadyzStartingOkShuttingDown(t *testing.T) { // AC-31
	addr := reserveAddr(t)

	var (
		mu         sync.Mutex
		held       resp
		heldErr    error
		heldHealth resp
		heldRecs   []map[string]any
		hookCalls  int
	)
	out := &syncBuf{}
	h := run(t, runOpts{
		out:  out,
		args: []string{"-admin.listen=" + addr},
		before: func(ctx context.Context) {
			r, err := get(addr, "/readyz")
			hz, _ := get(addr, "/healthz")
			recs := jsonRecordsNoFail(out.lines())
			mu.Lock()
			held, heldErr, heldHealth, heldRecs = r, err, hz, recs
			hookCalls++
			mu.Unlock()
		},
	})
	hd := h.waitStarted()

	mu.Lock()
	if hookCalls != 1 {
		t.Errorf("BeforeLastStep called %d times, want 1", hookCalls)
	}
	if heldErr != nil {
		t.Fatalf("GET /readyz during BeforeLastStep: %v", heldErr)
	}
	if held.status != 503 || held.body != "starting\n" {
		t.Errorf("/readyz while held: got %d %q, want 503 %q", held.status, held.body, "starting\n")
	}
	if held.ctype != plainUTF8 {
		t.Errorf("/readyz while held: Content-Type %q, want %q", held.ctype, plainUTF8)
	}
	if heldHealth.status != 200 || heldHealth.body != "ok\n" {
		t.Errorf("/healthz while held: got %d %q, want 200 ok", heldHealth.status, heldHealth.body)
	}
	if !hasLifecycle(heldRecs, "starting") {
		t.Errorf("no lifecycle starting record before BeforeLastStep (ADR-0006 step 4 precedes step 7)")
	}
	if hasLifecycle(heldRecs, "startup complete") {
		t.Errorf("startup-complete record emitted before the last startup step finished")
	}
	mu.Unlock()

	if hd.AdminAddr == nil || hd.AdminAddr.String() != addr {
		t.Errorf("Handles.AdminAddr = %v, want %s", hd.AdminAddr, addr)
	}

	// After startup completed: 200 ok, and the startup-complete record exists.
	eventually(t, "/readyz 200", func() bool {
		r, err := get(addr, "/readyz")
		return err == nil && r.status == 200
	})
	r, err := get(addr, "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	if r.status != 200 || r.body != "ok\n" || r.ctype != plainUTF8 {
		t.Errorf("/readyz after startup: got %d %q %q, want 200 %q %q", r.status, r.body, r.ctype, "ok\n", plainUTF8)
	}
	eventually(t, "startup complete record", func() bool {
		return hasLifecycle(h.out.records(t), "startup complete")
	})

	// Hold a request open across the start of shutdown: send the request head
	// minus the final CRLF, begin shutdown, wait for the shutting-down record,
	// then complete the request. The handler runs after the record exists.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := fmt.Fprintf(c, "GET /readyz HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n", addr); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let the server start reading it
	h.cancel()
	eventually(t, "lifecycle shutting down record", func() bool {
		return hasLifecycle(h.out.records(t), "shutting down")
	})
	if _, err := fmt.Fprint(c, "\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	sr := readResponse(t, c)
	if sr.status != 503 || sr.body != "shutting_down\n" {
		t.Errorf("/readyz after the shutting-down record: got %d %q, want 503 %q", sr.status, sr.body, "shutting_down\n")
	}
	if sr.ctype != plainUTF8 {
		t.Errorf("/readyz shutting_down Content-Type %q, want %q", sr.ctype, plainUTF8)
	}
	if code := h.wait(); code != 0 {
		t.Errorf("exit code after graceful shutdown = %d, want 0", code)
	}
}

func jsonRecordsNoFail(lines []string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// FR-7 lifecycle, FR-18, FR-34: lifecycle records exist, in order, once each,
// with component=lifecycle and constant messages (equal across runs, no
// settings echoed).
func TestLifecycleRecords(t *testing.T) { // AC-3 (lifecycle), FR-7
	var msgsPerRun [][]string
	for i := 0; i < 2; i++ {
		addr := reserveAddr(t)
		h := run(t, runOpts{args: []string{"-admin.listen=" + addr}})
		h.waitStarted()
		eventually(t, "startup complete record", func() bool {
			return hasLifecycle(h.out.records(t), "startup complete")
		})
		if code := h.stop(); code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		recs := h.out.records(t)
		for _, r := range recs {
			c := logging.Component(str(r, "component"))
			if !logging.ValidComponent(c) {
				t.Errorf("record with component outside the FR-7 set: %q", str(r, "component"))
			}
			for _, k := range []string{"time", "level", "msg"} {
				if _, ok := r[k]; !ok {
					t.Errorf("record without %q", k)
				}
			}
		}
		msgs := lifecycleMsgs(recs)
		if len(msgs) != 3 {
			t.Fatalf("lifecycle records = %d (%q), want exactly 3 (starting, startup complete, shutting down)", len(msgs), msgs)
		}
		for i, frag := range []string{"starting", "startup complete", "shutting down"} {
			if !strings.Contains(strings.ToLower(msgs[i]), frag) {
				t.Errorf("lifecycle record %d message %q does not denote %q (order: starting, startup complete, shutting down)", i, msgs[i], frag)
			}
		}
		for _, m := range msgs {
			if strings.Contains(m, addr) || strings.Contains(m, "127.0.0.1") {
				t.Errorf("lifecycle message embeds the listen address: %q", m)
			}
		}
		msgsPerRun = append(msgsPerRun, msgs)
	}
	for i := range msgsPerRun[0] {
		if msgsPerRun[0][i] != msgsPerRun[1][i] {
			t.Errorf("lifecycle message %d is not constant across runs: %q vs %q", i, msgsPerRun[0][i], msgsPerRun[1][i])
		}
	}
}

// AC-8 (FR-10, FR-11, FR-25, FR-31): the live registry of the running process
// has no corral_ family outside the catalog.
func TestLiveRegistryMatchesCatalog(t *testing.T) { // AC-8
	cat, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	inCatalog := map[string]bool{}
	for _, m := range cat.Metrics {
		inCatalog[m.Name] = true
	}
	h := run(t, runOpts{args: []string{"-admin.listen=" + reserveAddr(t)}})
	hd := h.waitStarted()
	if hd.Gatherer == nil {
		t.Fatal("Handles.Gatherer is nil")
	}
	fams, err := hd.Gatherer.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := false
	for _, f := range fams {
		n := f.GetName()
		if !strings.HasPrefix(n, "corral_") {
			continue
		}
		if !inCatalog[n] {
			t.Errorf("live registry has corral_ family %q outside the catalog", n)
		}
		if n == metrics.NameTracingSpansDropped {
			seen = true
		}
	}
	if !seen {
		t.Errorf("%s is not in the live registry", metrics.NameTracingSpansDropped)
	}
	if code := h.stop(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// FR-34 / ADR-0006: Started exposes a usable tracer provider even with
// tracing off (a no-op provider is acceptable, nil is not).
func TestStartedHandles(t *testing.T) { // FR-34 (hook contract)
	h := run(t, runOpts{args: []string{"-admin.listen=" + reserveAddr(t)}})
	hd := h.waitStarted()
	if hd.TracerProvider == nil {
		t.Error("Handles.TracerProvider is nil")
	}
	if hd.AdminAddr == nil {
		t.Error("Handles.AdminAddr is nil")
	}
	if n := h.startedCalls(); n != 1 {
		t.Errorf("Started called %d times, want 1", n)
	}
	if code := h.stop(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// Panics recovered in runtime goroutines are logged via errclass.FromPanic
// (class internal, optional stack). Run exposes no hook that makes a runtime
// goroutine panic, so this cannot be driven from outside the package.
func TestRecoveredPanicLoggedViaErrclass(t *testing.T) { // FR-24, FR-29
	t.Skip("no Hooks field lets a test trigger a panic in a runtime goroutine; covered by review and errclass unit tests")
}

var _ = corralrt.Run

package runtime_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	corralrt "github.com/sorinpanduru/corral/internal/app/runtime"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
)

// AC-31 (FR-18): /readyz is 503 "starting\n" while startup is held before its
// last step, 200 "ok\n" after startup completes (and the startup-complete
// record exists), and 503 "shutting_down\n" from the lifecycle "shutting down"
// record onward, observed from inside Hooks.Stopping (called after the record
// and before the admin server shuts down). Registered readiness checks cannot
// be injected through Run, so the "check not consulted" sub-bullets are covered
// at the adminhttp level.
func TestReadyzStartingOkShuttingDown(t *testing.T) { // AC-31
	addr := reserveAddr(t)
	out := &syncBuf{}

	type snap struct {
		r      resp
		err    error
		health resp
		recs   []map[string]any
		calls  int
	}
	var mu sync.Mutex
	var held, stopping snap
	take := func(s *snap) {
		r, err := get(addr, "/readyz")
		hz, _ := get(addr, "/healthz")
		recs := jsonRecordsNoFail(out.lines())
		mu.Lock()
		s.r, s.err, s.health, s.recs = r, err, hz, recs
		s.calls++
		mu.Unlock()
	}
	h := run(t, runOpts{
		out:      out,
		args:     []string{"-admin.listen=" + addr},
		before:   func(context.Context) { take(&held) },
		stopping: func(context.Context) { take(&stopping) },
	})
	hd := h.waitStarted()

	mu.Lock()
	if held.calls != 1 {
		t.Errorf("BeforeLastStep called %d times, want 1", held.calls)
	}
	if held.err != nil {
		t.Fatalf("GET /readyz during BeforeLastStep: %v", held.err)
	}
	if held.r.status != 503 || held.r.body != "starting\n" {
		t.Errorf("/readyz while held: got %d %q, want 503 %q", held.r.status, held.r.body, "starting\n")
	}
	if held.r.ctype != plainUTF8 {
		t.Errorf("/readyz while held: Content-Type %q, want %q", held.r.ctype, plainUTF8)
	}
	if held.health.status != 200 || held.health.body != "ok\n" {
		t.Errorf("/healthz while held: got %d %q, want 200 ok", held.health.status, held.health.body)
	}
	if !hasLifecycle(held.recs, "starting") {
		t.Errorf("no lifecycle starting record before BeforeLastStep")
	}
	if hasLifecycle(held.recs, "startup complete") {
		t.Errorf("startup-complete record emitted before the last startup step finished")
	}
	mu.Unlock()

	if hd.AdminAddr == nil || hd.AdminAddr.String() != addr {
		t.Errorf("Handles.AdminAddr = %v, want %s", hd.AdminAddr, addr)
	}

	// Started runs after startup completes: 200 ok, startup-complete record.
	r, err := get(addr, "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	if r.status != 200 || r.body != "ok\n" || r.ctype != plainUTF8 {
		t.Errorf("/readyz after startup: got %d %q %q, want 200 %q %q", r.status, r.body, r.ctype, "ok\n", plainUTF8)
	}
	if !hasLifecycle(out.records(t), "startup complete") {
		t.Errorf("no startup-complete record once Started ran")
	}

	if code := h.stop(); code != 0 {
		t.Errorf("exit code after graceful shutdown = %d, want 0", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if stopping.calls != 1 {
		t.Fatalf("Stopping called %d times, want 1", stopping.calls)
	}
	if stopping.err != nil {
		t.Fatalf("GET /readyz inside Stopping: %v", stopping.err)
	}
	if stopping.r.status != 503 || stopping.r.body != "shutting_down\n" {
		t.Errorf("/readyz inside Stopping: got %d %q, want 503 %q", stopping.r.status, stopping.r.body, "shutting_down\n")
	}
	if stopping.r.ctype != plainUTF8 {
		t.Errorf("/readyz inside Stopping: Content-Type %q, want %q", stopping.r.ctype, plainUTF8)
	}
	if !hasLifecycle(stopping.recs, "shutting down") {
		t.Errorf("Stopping ran before the lifecycle shutting-down record was emitted")
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

// Recovered panics are covered white-box in recoverpanic_test.go.

var _ = corralrt.Run

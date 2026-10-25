package proxyapp_test

// In-process tests of the application wiring (T21): the real settings, monitor,
// pool, connector, exchange engine, listener and observers, against the
// mongosim twin. Metrics go to a private registry and logs to logcheck.
//
// Interface assumptions (stated in the report): proxyapp.New(cfg, Deps) builds
// the service; Start(ctx) starts monitoring in the background; Bind binds the
// wire listener; Serve(ctx) runs the accept loop until Shutdown; Readiness()
// is the health.Readiness handed to the admin listener.

import (
	"context"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/sorinpanduru/corral/internal/app/proxyapp"
	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/testkit/corralargs"
	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
	"github.com/sorinpanduru/corral/internal/testkit/memfs"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

type app struct {
	t      *testing.T
	svc    *proxyapp.Service
	addr   string
	reg    *prometheus.Registry
	lh     *logcheck.Handler
	cancel context.CancelFunc
}

type cfgOpts struct {
	args  []string
	env   []string
	hooks proxyapp.Hooks
}

// load builds the typed settings through the real registry and loader.
func load(t *testing.T, upstreamAddrs []string, extra []string, env []string) (settings.Proxy, string) {
	t.Helper()
	port := corralargs.FreePort(t)
	proxyAddr := "127.0.0.1:" + strconv.Itoa(port)
	hosts := ""
	for i, a := range upstreamAddrs {
		if i > 0 {
			hosts += ","
		}
		hosts += a
	}
	args := []string{"--upstream.hosts=" + hosts, "--proxy.listen=" + proxyAddr, "--upstream.heartbeat_interval=500ms", "--pool.checkout_timeout=2s"}
	if !hasArg(extra, "--upstream.auth.mechanism") {
		args = append(args, "--upstream.auth.mechanism=none")
	}
	args = append(args, extra...)
	reg, err := settings.Registry()
	if err != nil {
		t.Fatal(err)
	}
	res := loader.Load(reg, loader.Inputs{Args: args, Env: source.NewEnv(env), ReadFile: memfs.New().Read})
	if len(res.Errors) != 0 {
		t.Fatalf("settings did not load: %v", res.Errors)
	}
	return settings.ProxyFrom(res), proxyAddr
}

func hasArg(args []string, prefix string) bool {
	for _, a := range args {
		if len(a) >= len(prefix) && a[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func start(t *testing.T, upstreamAddrs []string, o cfgOpts) *app {
	t.Helper()
	cfg, addr := load(t, upstreamAddrs, o.args, o.env)
	lh := logcheck.New(t)
	reg := metrics.NewRegistry()
	cat, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	mf, err := metrics.NewFactory(reg, cat)
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.RegisterAll(mf); err != nil {
		t.Fatal(err)
	}
	svc, err := proxyapp.New(cfg, proxyapp.Deps{Logs: logging.NewFactory(lh), Metrics: mf, Tracer: noop.NewTracerProvider().Tracer("x"),
		Version: "test", Clock: clock.System(), Hooks: o.hooks})
	if err != nil {
		t.Fatalf("proxyapp.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)
	if err := svc.Bind(); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	go svc.Serve(ctx)
	a := &app{t: t, svc: svc, addr: addr, reg: reg, lh: lh, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		sctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = svc.Shutdown(sctx)
	})
	return a
}

func (a *app) dial() *rawwire.Conn { return rawwire.Dial(a.t, a.addr) }

func (a *app) readiness() health.Status {
	return a.svc.Readiness().Readiness(context.Background())
}

func (a *app) waitReady(want bool, reason health.Reason) {
	a.t.Helper()
	eventually(a.t, "readiness", func() bool {
		s := a.readiness()
		return s.Ready == want && (want || s.Reason == reason)
	})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func findCmd(id int32, marker string) []byte {
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: id, Body: bson.D{{Key: "find", Value: "c"}, {Key: "$db", Value: "d"}, {Key: "corraltestMarker", Value: marker}}})
}

func insertCmd(id int32, marker string) []byte {
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: id, Body: bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "d"}, {Key: "corraltestMarker", Value: marker}}})
}

func send(t *testing.T, c *rawwire.Conn, raw []byte) *rawwire.Message {
	t.Helper()
	if err := c.Send(raw); err != nil {
		t.Fatal(err)
	}
	m, err := c.ReadMessage(15 * time.Second)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return m
}

func code(m *rawwire.Message) int64 { return m.Docs[0].Lookup("code").AsInt64() }

// ---- metrics and records ----

func (a *app) val(name string, labels ...string) float64 {
	want := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}
	fams, _ := a.reg.Gather()
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if match(m, want) {
				if m.GetCounter() != nil {
					return m.GetCounter().GetValue()
				}
				if m.GetGauge() != nil {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	return 0
}

func match(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, l := range m.GetLabel() {
		got[l.GetName()] = l.GetValue()
	}
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func (a *app) records(msg string) []logcheck.Record {
	var out []logcheck.Record
	for _, r := range a.lh.Records() {
		if r.Msg == msg {
			out = append(out, r)
		}
	}
	return out
}

func attrs(r logcheck.Record) map[string]slog.Value {
	m := map[string]slog.Value{}
	for _, a := range r.Attrs {
		m[a.Key] = a.Value.Resolve()
	}
	return m
}

func bsonD(kv ...any) bson.D {
	var d bson.D
	for i := 0; i+1 < len(kv); i += 2 {
		d = append(d, bson.E{Key: kv[i].(string), Value: kv[i+1]})
	}
	return d
}

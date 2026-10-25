//go:build twin

package twin

import (
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/corralproc"
	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// SPEC-002 AC-27 / FR-49 / FR-48 (T28, twin part): every event is emitted with
// its constant msg, its component, only its allowed keys and reason values only
// from its enumeration; every metric family carries exactly its labels; no
// record or span for a synthesized error carries db.response.status_code; none
// of the planted markers appears in any output.

type eventSpec struct {
	component string
	level     string
	keys      []string // attributes beyond time, level, msg, component
	reasons   []string // allowed values of "reason", when the event carries it
}

var clientClose = []string{"client_closed", "idle_timeout", "io_timeout", "protocol_error", "upstream_failure", "shutdown", "tls_failure", "limit_exceeded", "internal_error"}
var upstreamClose = []string{"idle", "error", "not_primary", "client_abandoned", "shutdown", "incompatible"}
var readiness = []string{"upstream_unavailable", "upstream_auth_failed", "upstream_incompatible"}
var incompat = []string{"wire_version", "standalone", "topology_mismatch", "replica_set_name_mismatch"}

var events = map[string]eventSpec{
	"wire listener bound":                   {"proxy", "INFO", nil, nil},
	"client connection opened":              {"proxy", "DEBUG", []string{"corral.connection.id", "client.address", "client.port"}, nil},
	"client connection closed":              {"proxy", "DEBUG", []string{"reason", "corral.connection.id"}, clientClose},
	"client connection rejected":            {"proxy", "WARN", []string{"reason"}, []string{"limit_exceeded"}},
	"client handshake answered":             {"proxy", "DEBUG", []string{"corral.connection.id", "corral.client.app_name", "corral.client.driver.name", "corral.client.driver.version"}, nil},
	"client authentication rejected":        {"proxy", "WARN", []string{"error", "corral.error.detail", "corral.connection.id"}, nil},
	"client command blocked":                {"proxy", "WARN", []string{"error", "corral.error.detail", "db.operation.name", "corral.connection.id"}, nil},
	"client protocol error":                 {"proxy", "WARN", []string{"error", "corral.error.detail", "corral.connection.id"}, nil},
	"client I/O timed out":                  {"proxy", "WARN", []string{"error", "corral.error.detail", "corral.connection.id"}, nil},
	"client TLS handshake failed":           {"proxy", "WARN", []string{"error", "corral.error.detail"}, nil},
	"request handler panic recovered":       {"proxy", "ERROR", []string{"error", "corral.error.detail", "stack"}, nil},
	"pool checkout timed out":               {"pool", "WARN", nil, nil},
	"pool wait queue full":                  {"pool", "WARN", nil, nil},
	"upstream connection opened":            {"upstream", "DEBUG", []string{"server.address", "server.port", "corral.upstream.connection.id"}, nil},
	"upstream monitoring connection opened": {"upstream", "DEBUG", []string{"server.address", "server.port", "corral.upstream.connection.id"}, nil},
	"upstream connection closed":            {"upstream", "DEBUG", []string{"reason", "server.address", "server.port"}, upstreamClose},
	"upstream monitoring connection closed": {"upstream", "DEBUG", []string{"reason", "server.address", "server.port"}, upstreamClose},
	"upstream connection failed":            {"upstream", "WARN", []string{"error", "corral.error.detail", "server.address", "server.port"}, nil},
	"upstream authentication failed":        {"upstream", "WARN", []string{"error", "corral.error.detail", "server.address", "server.port", "db.response.status_code", "corral.db.response.code_name"}, nil},
	"upstream unavailable":                  {"upstream", "WARN", []string{"reason"}, readiness},
	"upstream incompatible":                 {"upstream", "ERROR", []string{"reason", "server.address", "server.port"}, incompat},
	"upstream available":                    {"upstream", "INFO", nil, nil},
	"upstream primary changed":              {"upstream", "INFO", []string{"server.address", "server.port"}, nil},
	"upstream target marked not primary":    {"upstream", "WARN", []string{"error", "corral.error.detail", "server.address", "server.port", "db.response.status_code", "corral.db.response.code_name"}, nil},
	"upstream protocol error":               {"upstream", "WARN", []string{"error", "corral.error.detail", "server.address", "server.port"}, nil},
	"upstream barrier failed":               {"upstream", "WARN", []string{"error", "corral.error.detail", "server.address", "server.port"}, nil},
	"shutdown forced by second signal":      {"lifecycle", "WARN", nil, nil},
}

type harvest struct {
	t       *testing.T
	recs    map[string][]corralproc.Record
	scanner *markerscan.Scanner
	markers []string
	spans   []otlpcollector.RecordedSpan
}

func newHarvest(t *testing.T) *harvest {
	h := &harvest{t: t, recs: map[string][]corralproc.Record{}, scanner: markerscan.New()}
	for _, m := range []string{"mk-db-" + randHex(t, 8), "mk-coll-" + randHex(t, 8), "mk-filter-" + randHex(t, 8), "mk-comment-" + randHex(t, 8), "mk-user-" + randHex(t, 8)} {
		h.markers = append(h.markers, m)
		h.scanner.AddMarker(t, m, markerscan.SourceStdout, markerscan.SourceStderr, markerscan.SourceMetrics, markerscan.SourceReadyz)
	}
	return h
}

func (h *harvest) marker(i int) string { return h.markers[i] }

// take records the process's output (after it has finished or while running).
func (h *harvest) take(c *corral) {
	for _, r := range c.p.Records() {
		h.recs[r.Msg()] = append(h.recs[r.Msg()], r)
	}
	h.scanner.Add(markerscan.SourceStdout, c.p.Stdout())
	h.scanner.Add(markerscan.SourceStderr, c.p.Stderr())
	if txt, err := c.p.Metrics(); err == nil {
		h.scanner.Add(markerscan.SourceMetrics, []byte(txt))
		h.checkMetrics(txt)
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func (h *harvest) checkRecords() {
	h.t.Helper()
	base := []string{"time", "level", "msg", "component", "trace_id", "span_id"}
	for msg, spec := range events {
		rs := h.recs[msg]
		if len(rs) == 0 {
			h.t.Errorf("event %q was never emitted by any scenario", msg)
			continue
		}
		for _, r := range rs {
			if r.Component() != spec.component || r["level"] != spec.level {
				h.t.Errorf("event %q: component %q level %v, want %s/%s", msg, r.Component(), r["level"], spec.component, spec.level)
			}
			for k, v := range r {
				if contains(base, k) || contains(spec.keys, k) || k == "suppressed_count" {
					continue
				}
				h.t.Errorf("event %q carries the key %q", msg, k)
				_ = v
			}
			if len(spec.reasons) > 0 {
				if s, _ := r["reason"].(string); !contains(spec.reasons, s) {
					h.t.Errorf("event %q has reason %q, want one of %v", msg, r["reason"], spec.reasons)
				}
			}
			if _, has := r["db.response.status_code"]; has && (msg != "upstream authentication failed" && msg != "upstream target marked not primary") {
				h.t.Errorf("event %q carries db.response.status_code", msg)
			}
		}
	}
	// Synthesized-error events never carry the upstream code attributes.
	for _, msg := range []string{"client authentication rejected", "client command blocked", "client protocol error", "client I/O timed out"} {
		for _, r := range h.recs[msg] {
			if _, has := r["db.response.status_code"]; has {
				h.t.Errorf("%q carries db.response.status_code", msg)
			}
			if _, has := r["corral.db.response.code_name"]; has {
				h.t.Errorf("%q carries corral.db.response.code_name", msg)
			}
		}
	}
	var unknown []string
	for msg := range h.recs {
		if _, ok := events[msg]; !ok {
			unknown = append(unknown, msg)
		}
	}
	sort.Strings(unknown)
	// Other messages are SPEC-001 / SPEC-003 events; none may use a proxy, pool
	// or upstream component without being in the FR-49 table.
	for _, msg := range unknown {
		for _, r := range h.recs[msg] {
			switch r.Component() {
			case "proxy", "pool", "upstream":
				h.t.Errorf("record %q (component %s) is not an FR-49 event", msg, r.Component())
			}
		}
	}
}

var metricLabels = map[string]map[string][]string{
	"corral_client_connections_closed_total":   {"reason": clientClose},
	"corral_handshakes_total":                  {"opcode": {"op_msg", "op_query"}, "outcome": {"ok", "error"}},
	"corral_protocol_errors_total":             {"side": {"client", "upstream"}, "error_detail": {"unsupported_opcode", "message_too_large", "malformed_message", "checksum_mismatch", "unexpected_response"}},
	"corral_requests_total":                    {"command": nil, "outcome": {"ok", "server_error", "proxy_error"}},
	"corral_request_duration_seconds":          {"command": nil, "le": nil},
	"corral_request_errors_total":              {"error_class": {"network", "timeout", "protocol", "server", "config", "rejected", "internal"}},
	"corral_pool_connections":                  {"state": {"idle", "in_use", "connecting"}},
	"corral_pool_checkouts_total":              {"outcome": {"ok", "failed"}},
	"corral_pool_checkout_failures_total":      {"error_detail": {"pool_wait_timeout", "pool_queue_full", "upstream_unavailable", "upstream_auth_failed", "upstream_incompatible", "shutting_down"}},
	"corral_pool_checkout_wait_seconds":        {"le": nil},
	"corral_upstream_connections_opened_total": {"kind": {"pool", "monitor"}},
	"corral_upstream_connections_closed_total": {"kind": {"pool", "monitor"}, "reason": upstreamClose},
	"corral_upstream_connect_errors_total":     {"kind": {"pool", "monitor"}, "error_class": nil},
	"corral_upstream_auth_failures_total":      {"kind": {"pool", "monitor"}},
	"corral_client_connections":                {},
	"corral_client_connections_accepted_total": {},
	"corral_client_auth_rejected_total":        {},
	"corral_commands_blocked_total":            {},
	"corral_requests_dropped_total":            {},
	"corral_pool_max_connections":              {},
	"corral_pool_waiters":                      {},
	"corral_upstream_available":                {},
	"corral_upstream_primary_changes_total":    {},
}

func (h *harvest) checkMetrics(txt string) {
	for _, line := range strings.Split(txt, "\n") {
		m := sampleRE.FindStringSubmatch(line)
		if m == nil || !strings.HasPrefix(m[1], "corral_") {
			continue
		}
		name := m[1]
		for _, suf := range []string{"_bucket", "_sum", "_count"} {
			name = strings.TrimSuffix(name, suf)
		}
		allowed, ok := metricLabels[name]
		if !ok {
			if strings.HasPrefix(name, "corral_tracing") || strings.HasPrefix(name, "corral_build") || strings.HasPrefix(name, "corral_admin") || strings.HasPrefix(name, "corral_log") {
				continue
			}
			h.t.Errorf("unexpected corral metric family %s", name)
			continue
		}
		for _, l := range labelRE.FindAllStringSubmatch(m[2], -1) {
			vals, has := allowed[l[1]]
			if !has {
				h.t.Errorf("%s has the label %q, which is not declared", name, l[1])
				continue
			}
			if vals != nil && !contains(vals, l[2]) {
				h.t.Errorf("%s{%s=%q}: value outside its enumeration", name, l[1], l[2])
			}
		}
	}
}

func TestEveryEventAndMetricLabels_AC27(t *testing.T) {
	h := newHarvest(t)
	dbm, collm, filterm, commentm, userm := h.marker(0), h.marker(1), h.marker(2), h.marker(3), h.marker(4)
	traffic := func(c *corral, conn *rawwire.Conn) {
		rt(t, conn, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: d("hello", 1, "$db", "admin",
			"client", d("application", d("name", "events-app"), "driver", d("name", "events-driver", "version", "9.9"), "os", d("type", "Linux")))}))
		rt(t, conn, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 2, Body: d("find", collm, "$db", dbm, "filter", d("k", filterm), "comment", commentm)}))
	}

	// Scenario 1: refusals, limits, protocol errors, timeouts, panic, pool.
	o, col := traced(t, opts{panicCmd: "eventsPanic", set: map[string]string{
		"proxy.max_client_connections": "4", "proxy.client_io_timeout": "700ms", "pool.max_size": "1", "pool.max_waiters": "1", "pool.checkout_timeout": "700ms"}})
	c, tw := startMongos(t, o)
	c.waitReady()
	conn := c.dial()
	traffic(c, conn)
	rt(t, conn, cmd(3, "saslStart", 1, "mechanism", "SCRAM-SHA-256", "payload", userm, "$db", "admin"))
	rt(t, conn, cmd(4, "currentOp", 1, "$db", "admin", "comment", commentm))
	rt(t, conn, rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: 5, FullCollection: dbm + ".$cmd", NumberToReturn: 1, Query: d("find", collm)}))
	x := c.dial()
	x.Send(rawwire.HTTPGet())
	closedNoBytes(t, x)
	stall := c.dial()
	stall.Send([]byte{0x30, 0, 0, 0, 1, 0, 0, 0})
	closedNoBytes(t, stall)
	pan := c.dial()
	pan.Send(cmd(1, "eventsPanic", 1, "$db", dbm))
	closedNoBytes(t, pan)
	hold := tw.AddRule(mongosim.Match{Marker: "hold"}, mongosim.Hold())
	a := c.dial()
	a.Send(find(1, "hold"))
	hold.WaitHeld(1, 10*time.Second)
	b1 := c.dial()
	b1.Send(find(1, "q1"))
	time.Sleep(200 * time.Millisecond)
	rt(t, c.dial(), find(1, "q2"))  // queue full
	b1.ReadMessage(5 * time.Second) // queued request times out
	var over []*rawwire.Conn
	for i := 0; i < 4; i++ { // beyond max_client_connections
		o := c.dial()
		over = append(over, o)
	}
	time.Sleep(300 * time.Millisecond)
	for _, o := range over {
		o.Close()
	}
	conn.Close()
	b1.Close()
	hold.Release()
	a.ReadMessage(5 * time.Second)
	a.Close()
	time.Sleep(300 * time.Millisecond)
	tw.AddRule(mongosim.Match{Marker: "unk", Once: true}, mongosim.ReplyUnknownResponseTo())
	u := c.dial()
	u.Send(find(1, "unk"))
	closedNoBytes(t, u)
	tw.AddRule(mongosim.Match{Command: "ping", AppName: "corral", Once: true}, mongosim.ReplyError(91, "ShutdownInProgress"))
	m := c.dial()
	m.Send(mtcInsert(1, "evt-mtc"))
	time.Sleep(500 * time.Millisecond)
	_ = over
	c.waitReady()
	h.take(c)
	c.stop()
	h.take(c)
	col.Spans() // exported at shutdown
	h.spans = append(h.spans, col.Spans()...)
	h.scanner.AddCollector(col)

	// Scenario 2: upstream failure, recovery, incompatibility, auth failure.
	c2, tw2 := startMongos(t, opts{set: map[string]string{"pool.min_size": "1"}})
	c2.waitReady()
	rt(t, c2.dial(), find(1, "warm"))
	tw2.Stop()
	c2.waitReadyz(503, "upstream_unavailable")
	tw2.Restart()
	c2.waitReady()
	tw2.SetHello(mongosim.HelloSpec{Role: mongosim.Standalone})
	c2.waitReadyz(503, "upstream_incompatible")
	h.take(c2)

	tw3 := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}, Users: map[string]string{"u": "p"}, RequireAuth: true})
	tw3.SetAuthOutcome(mongosim.Fail18)
	a3 := newAuth(t)
	c3 := start(t, opts{upstream: []string{tw3.Addr()}, env: a3.env, set: map[string]string{"upstream.auth.mechanism": "SCRAM-SHA-256"}})
	c3.waitReadyz(503, "upstream_auth_failed")
	rt(t, c3.dial(), find(1, "x"))
	h.take(c3)

	// Scenario 3: replica set, not primary, connection failure.
	cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
	c4 := start(t, rsOpts(cl, map[string]string{"upstream.connect_timeout": "500ms"}))
	c4.waitReady()
	pri := cl.Member(cl.PrimaryIndex())
	pri.AddRule(mongosim.Match{Marker: "np", Once: true}, mongosim.ReplyError(10107, "NotWritablePrimary"))
	rt(t, c4.dial(), mk(1, "np", "insert", "c", "$db", "d"))
	c4.waitRecords("upstream target marked not primary", 1)
	pri.RefuseNew(true)
	pri.CloseConns()
	rt(t, c4.dial(), find(2, "refused"))
	pri.RefuseNew(false)
	h.take(c4)

	// Scenario 4: client TLS failure.
	cs := genCerts(t)
	c5, _ := startMongos(t, tlsOpts(cs, map[string]string{"proxy.client_io_timeout": "500ms"}))
	c5.waitReady()
	st := rawwire.DialStalledClientHello(t, c5.proxy)
	st.SetReadDeadline(time.Now().Add(5 * time.Second))
	st.Read(make([]byte, 8))
	c5.waitRecords("client TLS handshake failed", 1)
	h.take(c5)

	// Scenario 5: a second signal.
	c6, tw6 := startMongos(t, opts{holdAt: []string{"shutdown_drain"}})
	c6.waitReady()
	hold6 := tw6.AddRule(mongosim.Match{Marker: "stuck"}, mongosim.Hold())
	z := c6.dial()
	z.Send(find(1, "stuck"))
	hold6.WaitHeld(1, 10*time.Second)
	c6.p.Signal(syscall.SIGTERM)
	c6.p.WaitHeld("shutdown_drain", 15*time.Second)
	c6.p.Signal(syscall.SIGTERM)
	c6.p.Wait(30 * time.Second)
	h.take(c6)

	h.checkRecords()
	for _, s := range h.spans {
		if _, has := s.Attributes["db.response.status_code"]; has {
			switch s.Attributes["corral.error.detail"] {
			case "pool_wait_timeout", "pool_queue_full", "upstream_unavailable", "upstream_auth_failed", "upstream_incompatible", "shutting_down", "command_blocked", "client_auth_unsupported", "unsupported_opcode":
				t.Errorf("span %q for a synthesized error carries db.response.status_code", s.Name)
			}
		}
	}
	h.scanner.AssertClean(t)
	_ = bson.D{}
}

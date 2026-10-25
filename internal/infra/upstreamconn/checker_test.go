package upstreamconn_test

import (
	"context"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/upstreamconn"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

func check(c upstream.Checker, host string, auth, fresh bool) upstream.Report {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.Check(ctx, upstream.CheckRequest{Host: host, Authenticate: auth, Fresh: fresh})
}

// FR-37: the monitoring connection presents appName corral-monitor and the
// version, and one hello is sent per check.
func TestCheckerMetadata_FR37(t *testing.T) {
	s := sim(t, mongosim.Options{})
	c := upstreamconn.NewChecker(baseCfg(nil), upstream.Noop{})
	r := check(c, s.Addr(), false, false)
	if r.Err != nil || r.Host != s.Addr() {
		t.Fatalf("report: %+v", r)
	}
	ci := s.Conns()
	if len(ci) != 1 || ci[0].AppName != "corral-monitor" || ci[0].DriverName != "corral" || ci[0].DriverVersion != "9.8.7" {
		t.Fatalf("handshake metadata: %+v", ci)
	}
}

// FR-37, FR-9: the report carries the own-exchange fields from hello.
func TestCheckerReportFields_FR9(t *testing.T) {
	eid := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 7}
	cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{SetName: "rs0", Arbiters: 1})
	c := upstreamconn.NewChecker(baseCfg(nil), upstream.Noop{})
	cl.SetPrimary(1)
	var prim, sec upstream.Report
	prim = check(c, cl.Member(1).Addr(), false, false)
	sec = check(c, cl.Member(0).Addr(), false, false)
	if prim.Err != nil || !prim.IsWritablePrimary || !prim.HasSetName || prim.SetName != "rs0" || len(prim.Hosts) != 3 || !prim.HasElectionID || !prim.HasSetVersion {
		t.Fatalf("primary report: %+v", prim)
	}
	if prim.Params.MaxMessageSizeBytes != mongosim.DefaultMaxMessageSizeBytes || prim.Params.MaxBsonObjectSize != mongosim.DefaultMaxBsonObjectSize ||
		prim.Params.MaxWriteBatchSize != mongosim.DefaultMaxWriteBatchSize || !prim.Params.HasLSTM || prim.Params.MaxWire != mongosim.DefaultMaxWire {
		t.Fatalf("params: %+v", prim.Params)
	}
	if sec.Err != nil || !sec.Secondary || sec.IsWritablePrimary || sec.Primary != cl.Member(1).Addr() {
		t.Fatalf("secondary report: %+v", sec)
	}
	_ = eid

	m := sim(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	if r := check(c, m.Addr(), false, false); r.Err != nil || !r.IsMongos || r.HasSetName {
		t.Fatalf("mongos report: %+v", r)
	}
	st := sim(t, mongosim.Options{})
	if r := check(c, st.Addr(), false, false); r.Err != nil || r.IsMongos || r.HasSetName || !r.IsWritablePrimary {
		t.Fatalf("standalone report: %+v", r)
	}
	if r := check(c, cl.Arbiter(0).Addr(), false, false); r.Err != nil || !r.ArbiterOnly {
		t.Fatalf("arbiter report: %+v", r)
	}
}

// FR-37: one monitoring connection per host, reused across checks; Fresh opens
// a new one (and the old one goes away).
func TestCheckerReusesConnection_FR37(t *testing.T) {
	s := sim(t, mongosim.Options{})
	c := upstreamconn.NewChecker(baseCfg(nil), upstream.Noop{})
	for i := 0; i < 3; i++ {
		if r := check(c, s.Addr(), false, false); r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	if n := s.ConnAttemptsBy("corral-monitor"); n != 1 {
		t.Fatalf("connections = %d, want 1 (reused)", n)
	}
	if r := check(c, s.Addr(), false, true); r.Err != nil {
		t.Fatal(r.Err)
	}
	if n := s.ConnAttemptsBy("corral-monitor"); n != 2 {
		t.Fatalf("connections after Fresh = %d, want 2", n)
	}
	eventually(t, "the old connection closed", func() bool { return s.OpenConns("corral-monitor") == 1 })
	// A different host has its own connection.
	s2 := sim(t, mongosim.Options{})
	check(c, s2.Addr(), false, false)
	if s2.ConnAttemptsBy("corral-monitor") != 1 {
		t.Fatal("no connection to the second host")
	}
}

// FR-22 (AC-18 twin support): the checker authenticates only when asked.
func TestCheckerAuthenticatesOnlyWhenAsked_AC18(t *testing.T) {
	s := sim(t, mongosim.Options{RequireAuth: true})
	c := upstreamconn.NewChecker(baseCfg(creds()), upstream.Noop{})
	r := check(c, s.Addr(), false, false)
	if r.Err != nil || r.AuthErr != nil {
		t.Fatalf("report: %+v", r)
	}
	if len(s.ReceivedCommand("saslStart")) != 0 || s.Conns()[0].Authenticated {
		t.Fatal("authenticated although not asked")
	}
	// Asking on the same host: the connection authenticates (a fresh one is
	// allowed).
	r = check(c, s.Addr(), true, true)
	if r.Err != nil || r.AuthErr != nil {
		t.Fatalf("report: %+v", r)
	}
	if len(s.ReceivedCommand("saslStart")) != 1 {
		t.Fatalf("saslStart count = %d", len(s.ReceivedCommand("saslStart")))
	}
	authed := false
	for _, ci := range s.Conns() {
		if ci.Open && ci.AppName == "corral-monitor" && ci.Authenticated {
			authed = true
		}
	}
	if !authed {
		t.Fatal("no authenticated monitoring connection")
	}
	// With mechanism none, Authenticate:true sends no SASL.
	s2 := sim(t, mongosim.Options{})
	c2 := upstreamconn.NewChecker(baseCfg(nil), upstream.Noop{})
	check(c2, s2.Addr(), true, false)
	if len(s2.ReceivedCommand("saslStart")) != 0 {
		t.Fatal("SASL sent with no credentials")
	}
}

// FR-19 (G-8, AC-41 support): with hello answered and SCRAM rejected, the
// Report has a successful hello and a separate AuthErr.
func TestCheckerAuthFailureIsSeparate_AC41(t *testing.T) {
	for name, o := range map[string]mongosim.AuthOutcome{"18": mongosim.Fail18, "334": mongosim.Fail334, "signature": mongosim.BadServerSignature} {
		t.Run(name, func(t *testing.T) {
			s := sim(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
			s.SetAuthOutcome(o)
			rec := &recorder{}
			c := upstreamconn.NewChecker(baseCfg(creds()), rec)
			r := check(c, s.Addr(), true, false)
			if r.Err != nil {
				t.Fatalf("Err = %v: a successful hello must not be an error", r.Err)
			}
			if !r.IsMongos || r.Params.MaxMessageSizeBytes == 0 {
				t.Fatalf("hello fields missing: %+v", r)
			}
			if r.AuthErr == nil || !upstream.IsAuthFailed(r.AuthErr) {
				t.Fatalf("AuthErr = %v, want AuthFailed", r.AuthErr)
			}
			// Only the monitor reports authentication events; the checker
			// reports the outcome through the Report fields.
			rec.mu.Lock()
			if len(rec.authKO) != 0 {
				t.Errorf("the checker reported AuthFailed events: %v", rec.authKO)
			}
			rec.mu.Unlock()
		})
	}
}

// Transient authentication errors on a check are not AuthFailed.
func TestCheckerTransientAuthError_AC25(t *testing.T) {
	for name, o := range map[string]mongosim.AuthOutcome{"10107": mongosim.ErrorCode(10107, "NotWritablePrimary"), "reset": mongosim.AuthReset} {
		t.Run(name, func(t *testing.T) {
			s := sim(t, mongosim.Options{})
			s.SetAuthOutcome(o)
			c := upstreamconn.NewChecker(baseCfg(creds()), upstream.Noop{})
			r := check(c, s.Addr(), true, false)
			if upstream.IsAuthFailed(r.AuthErr) || upstream.IsAuthFailed(r.Err) {
				t.Fatalf("transient error counted as an authentication failure: %+v", r)
			}
		})
	}
}

// Successful authentication is reported to the observer.
func TestCheckerAuthSucceededEvent_AC25(t *testing.T) {
	s := sim(t, mongosim.Options{})
	rec := &recorder{}
	c := upstreamconn.NewChecker(baseCfg(creds()), rec)
	if r := check(c, s.Addr(), true, false); r.Err != nil || r.AuthErr != nil {
		t.Fatalf("report: %+v", r)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.authOK) != 0 {
		t.Fatalf("the checker reported AuthSucceeded events (only the monitor does): %v", rec.authOK)
	}
}

// FR-37: failures are Err (network, timeout), the next check opens a new
// connection, and observer events are reported.
func TestCheckerFailures_FR37(t *testing.T) {
	s := sim(t, mongosim.Options{})
	rec := &recorder{}
	cfg := baseCfg(nil)
	cfg.ConnectTimeout = 300 * time.Millisecond
	c := upstreamconn.NewChecker(cfg, rec)
	if r := check(c, s.Addr(), false, false); r.Err != nil {
		t.Fatal(r.Err)
	}
	// A reset on the next check.
	s.FailChecks(1, mongosim.CheckReset)
	r := check(c, s.Addr(), false, false)
	if r.Err == nil {
		t.Fatal("a reset check returned no error")
	}
	if cl, _ := errclass.Classify(r.Err); cl != errclass.ClassNetwork {
		t.Fatalf("class %q, want network", cl)
	}
	// No reply: the check times out within the connect timeout.
	s.FailChecks(1, mongosim.CheckNoReply)
	begin := time.Now()
	r = check(c, s.Addr(), false, false)
	if r.Err == nil {
		t.Fatal("a silent check returned no error")
	}
	if cl, _ := errclass.Classify(r.Err); cl != errclass.ClassTimeout {
		t.Fatalf("class %q, want timeout", cl)
	}
	if time.Since(begin) > 2*time.Second {
		t.Fatalf("the check took %v", time.Since(begin))
	}
	// Recovery on a new connection.
	s.ClearChecks()
	if r = check(c, s.Addr(), false, false); r.Err != nil {
		t.Fatalf("recovery: %v", r.Err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.opened) < 2 || len(rec.failed) < 1 {
		t.Fatalf("observer: opened=%d failed=%d closed=%v", len(rec.opened), len(rec.failed), rec.closed)
	}
}

// An unreachable host gives Err with the network class.
func TestCheckerUnreachable_FR37(t *testing.T) {
	s := sim(t, mongosim.Options{})
	addr := s.Addr()
	s.Stop()
	c := upstreamconn.NewChecker(baseCfg(nil), upstream.Noop{})
	r := check(c, addr, false, false)
	if r.Host != addr || r.Err == nil {
		t.Fatalf("report: %+v", r)
	}
	if cl, d := errclass.Classify(r.Err); cl != errclass.ClassNetwork || d != errclass.DetailECONNREFUSED {
		t.Fatalf("Classify = (%s, %s)", cl, d)
	}
}

// A hello that is not ok yields Err, never a half-filled report.
func TestCheckerHelloErrorReply_FR37(t *testing.T) {
	s := sim(t, mongosim.Options{})
	s.FailChecks(1, mongosim.CheckError(11600, "InterruptedAtShutdown"))
	c := upstreamconn.NewChecker(baseCfg(nil), upstream.Noop{})
	r := check(c, s.Addr(), false, false)
	if r.Err == nil {
		t.Fatal("an error reply to hello returned no error")
	}
	if cl, d := errclass.Classify(r.Err); cl != errclass.ClassServer || d != errclass.DetailOther {
		t.Fatalf("Classify = (%s, %s), want server/other", cl, d)
	}
}

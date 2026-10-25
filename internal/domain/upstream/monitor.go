package upstream

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/domain/health"
)

type host struct {
	addr string
	seed bool

	checking bool
	retrying bool
	checked  bool // has been checked at least once
	last     time.Time

	override    time.Time // immediate-check time (FR-37); zero when none
	hasOverride bool

	up        bool // the latest check succeeded
	hasReport bool
	rep       Report // latest successful report

	dropped  bool
	markedNP bool // marked not primary or unusable by a reply code (FR-35)
}

// Monitor tracks the upstream deployment. It is safe for concurrent use.
type Monitor struct {
	cfg Config
	chk Checker
	clk clock.Clock
	obs Observer

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	started bool
	stopped bool
	timer   clock.Timer
	wg      sync.WaitGroup

	hosts []*host
	byAdr map[string]*host

	reached     bool
	authFailing bool
	params      Params
	hasParams   bool
	hasMax      bool
	maxEID      [12]byte
	maxSV       int64

	cur      State
	onChange []func(Change)
}

// NewMonitor returns a monitor. Call Start to begin checking.
func NewMonitor(cfg Config, chk Checker, clk clock.Clock, obs Observer) *Monitor {
	if obs == nil {
		obs = Noop{}
	}
	return &Monitor{cfg: cfg, chk: chk, clk: clk, obs: obs, byAdr: map[string]*host{}}
}

// OnChange registers a callback for Change events. Callbacks run outside the
// monitor's lock, in order.
func (m *Monitor) OnChange(f func(Change)) {
	m.mu.Lock()
	m.onChange = append(m.onChange, f)
	m.mu.Unlock()
}

// events collects callbacks to run after the lock is released.
type events struct{ fns []func() }

func (e *events) add(f func()) { e.fns = append(e.fns, f) }

func (e *events) change(m *Monitor, c Change) {
	fns := append([]func(Change){}, m.onChange...)
	e.add(func() {
		for _, f := range fns {
			f(c)
		}
	})
}

func (m *Monitor) addHost(addr string, seed bool) *host {
	if h, ok := m.byAdr[addr]; ok {
		return h
	}
	h := &host{addr: addr, seed: seed}
	m.hosts = append(m.hosts, h)
	m.byAdr[addr] = h
	return h
}

// Start begins monitoring. ctx supplies the context passed to checks and to
// observer calls made by the monitor itself.
func (m *Monitor) Start(ctx context.Context) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.ctx, m.cancel = context.WithCancel(ctx)
	for _, s := range m.cfg.Seeds {
		m.addHost(s, true)
	}
	m.pump()
	m.mu.Unlock()
}

// Stop ends monitoring and waits for running checks (or ctx).
func (m *Monitor) Stop(ctx context.Context) {
	m.mu.Lock()
	m.stopped = true
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (m *Monitor) interval() time.Duration {
	if m.cur.HasTarget {
		d := m.cfg.HeartbeatInterval
		if d < MinHeartbeat {
			d = MinHeartbeat
		}
		return d
	}
	return MinHeartbeat
}

// dueAt is when h is next due, and whether it is eligible at all.
func (m *Monitor) dueAt(h *host, now time.Time) (time.Time, bool) {
	if h.dropped || h.checking {
		return time.Time{}, false
	}
	if !h.checked {
		return now, true
	}
	due := h.last.Add(m.interval())
	if h.hasOverride && h.override.Before(due) {
		due = h.override
	}
	return due, true
}

// pump starts every due check and re-arms the wake-up timer. Call with m.mu
// held.
func (m *Monitor) pump() {
	if m.stopped || !m.started {
		return
	}
	now := m.clk.Now()
	for _, h := range m.hosts {
		if due, ok := m.dueAt(h, now); ok && !due.After(now) {
			m.launch(h, false, false)
		}
	}
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	var next time.Time
	found := false
	for _, h := range m.hosts {
		if due, ok := m.dueAt(h, now); ok && (!found || due.Before(next)) {
			next, found = due, true
		}
	}
	if found {
		d := next.Sub(now)
		if d < 0 {
			d = 0
		}
		m.timer = m.clk.AfterFunc(d, m.onTimer)
	}
}

func (m *Monitor) onTimer() {
	m.mu.Lock()
	m.timer = nil
	m.pump()
	m.mu.Unlock()
}

// launch starts one check of h in its own goroutine. Call with m.mu held.
func (m *Monitor) launch(h *host, retry, fresh bool) {
	h.checking = true
	if !retry {
		h.last = m.clk.Now()
		h.checked = true
		h.hasOverride = false
	}
	isTarget := m.cur.HasTarget && m.cur.Target == h.addr
	req := CheckRequest{Host: h.addr, Authenticate: m.cfg.Authenticate && isTarget}
	req.Fresh = fresh || (isTarget && m.authFailing)
	ctx := m.ctx
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		rep := m.chk.Check(ctx, req)
		rep.Host = h.addr
		m.apply(h, req, rep)
	}()
}

// scheduleImmediate asks for a check of every known member, each at least
// MinHeartbeat after its previous one (FR-37). Call with m.mu held.
func (m *Monitor) scheduleImmediate() {
	now := m.clk.Now()
	for _, h := range m.hosts {
		if h.dropped {
			continue
		}
		at := now
		if h.checked {
			if floor := h.last.Add(MinHeartbeat); floor.After(at) {
				at = floor
			}
		}
		h.override, h.hasOverride = at, true
	}
	m.pump()
}

func (m *Monitor) matching(r Report) bool {
	return m.cfg.HasReplicaSet && r.HasSetName && r.SetName == m.cfg.ReplicaSet && !r.IsMongos
}

func pairLess(eidA [12]byte, svA int64, eidB [12]byte, svB int64) bool {
	if c := bytes.Compare(eidA[:], eidB[:]); c != 0 {
		return c < 0
	}
	return svA < svB
}

func (m *Monitor) stale(r Report) bool {
	return m.hasMax && pairLess(r.ElectionID, r.SetVersion, m.maxEID, m.maxSV)
}

// apply folds one check result into the state.
func (m *Monitor) apply(h *host, req CheckRequest, rep Report) {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	ctx := m.ctx
	ev := &events{}
	if rep.Err != nil {
		isTarget := m.cur.HasTarget && m.cur.Target == h.addr
		if isTarget && !h.retrying {
			// FR-35: retry once at once on a new connection; nothing changes
			// while it runs.
			h.retrying = true
			m.launch(h, true, true)
			m.mu.Unlock()
			return
		}
		h.retrying, h.checking, h.up = false, false, false
	} else {
		h.retrying, h.checking, h.up = false, false, true
		h.hasReport, h.rep, h.markedNP = true, rep, false
		if m.matching(rep) {
			m.discover(rep)
			if rep.IsWritablePrimary && !rep.ArbiterOnly {
				if !m.hasMax || pairLess(m.maxEID, m.maxSV, rep.ElectionID, rep.SetVersion) {
					m.hasMax, m.maxEID, m.maxSV = true, rep.ElectionID, rep.SetVersion
				}
			}
		}
	}
	ev2 := m.evaluate()
	if rep.Err == nil && ev2.hasTarget && ev2.target == h.addr {
		switch {
		case rep.AuthErr != nil && IsAuthFailed(rep.AuthErr):
			m.authFailing = true
			err := rep.AuthErr
			ev.add(func() { m.obs.AuthFailed(ctx, "monitor", h.addr, err) })
		case rep.AuthErr == nil && req.Authenticate:
			m.authFailing = false
			ev.add(func() { m.obs.AuthSucceeded(ctx, "monitor", h.addr) })
		}
	} else if rep.Err == nil && rep.AuthErr != nil && IsAuthFailed(rep.AuthErr) && !ev2.hasTarget && !ev2.incompat {
		m.authFailing = true
	}
	m.commit(ctx, ev, ev2)
	m.pump()
	m.mu.Unlock()
	m.run(ev)
}

func (m *Monitor) run(ev *events) {
	for _, f := range ev.fns {
		f()
	}
}

// discover adds the members a matching report lists (FR-37).
func (m *Monitor) discover(r Report) {
	add := func(a string) {
		if a != "" {
			m.addHost(a, false)
		}
	}
	for _, a := range r.Hosts {
		add(a)
	}
	for _, a := range r.Passives {
		add(a)
	}
	add(r.Primary)
	if r.ArbiterOnly {
		if h, ok := m.byAdr[r.Host]; ok {
			h.dropped = true
		}
	}
}

type evalResult struct {
	target    string
	hasTarget bool
	params    Params
	incompat  bool
	reason    Reason
	ihost     string
}

// evaluate derives the target and the compatibility from the hosts' latest
// reports (FR-18, FR-35). Call with m.mu held.
func (m *Monitor) evaluate() evalResult {
	if !m.cfg.HasReplicaSet {
		return m.evaluateMongos()
	}
	var r evalResult
	matchKnown := false
	for _, h := range m.hosts {
		if h.hasReport && m.matching(h.rep) {
			matchKnown = true
		}
	}
	for _, h := range m.hosts {
		if h.hasReport && !m.matching(h.rep) && (matchKnown || !h.seed) {
			h.dropped = true // sticky: no longer monitored (FR-37, FR-18)
		}
	}
	var chosen *host
	cand := func(h *host) bool {
		return h.up && h.hasReport && m.matching(h.rep) && h.rep.IsWritablePrimary &&
			!h.rep.ArbiterOnly && !h.markedNP && !m.stale(h.rep)
	}
	if m.cur.HasTarget {
		if h := m.byAdr[m.cur.Target]; h != nil && cand(h) {
			chosen = h
		}
	}
	if chosen == nil {
		for _, h := range m.hosts {
			if cand(h) {
				chosen = h
				break
			}
		}
	}
	if chosen != nil {
		if !WireAccepted(chosen.rep.Params.MinWire, chosen.rep.Params.MaxWire) {
			r.incompat, r.reason, r.ihost = true, ReasonWireVersion, chosen.addr
		} else {
			r.target, r.hasTarget, r.params = chosen.addr, true, chosen.rep.Params
		}
	}
	if !r.incompat && !matchKnown {
		for _, s := range m.cfg.Seeds {
			h := m.byAdr[s]
			if h == nil || !h.hasReport || m.matching(h.rep) {
				continue
			}
			r.incompat, r.ihost = true, h.addr
			switch {
			case h.rep.IsMongos:
				r.reason = ReasonTopologyMismatch
			case h.rep.HasSetName:
				r.reason = ReasonReplicaSetNameMismatch
			default:
				r.reason = ReasonStandalone
			}
			break
		}
	}
	return r
}

func (m *Monitor) evaluateMongos() evalResult {
	var r evalResult
	for _, s := range m.cfg.Seeds {
		h := m.byAdr[s]
		if h == nil || !h.hasReport {
			continue
		}
		rep := h.rep
		switch {
		case rep.IsMongos && !WireAccepted(rep.Params.MinWire, rep.Params.MaxWire):
			r.incompat, r.reason = true, ReasonWireVersion
		case rep.IsMongos:
			if !r.hasTarget && h.up && !h.markedNP {
				r.target, r.hasTarget, r.params = h.addr, true, rep.Params
			}
			continue
		case rep.HasSetName:
			r.incompat, r.reason = true, ReasonTopologyMismatch
		default:
			r.incompat, r.reason = true, ReasonStandalone
		}
		r.ihost = h.addr
		r.hasTarget = false
		r.target = ""
		return r
	}
	return r
}

// commit builds the new State from an evaluation, diffs it with the previous
// one and queues the events. Call with m.mu held.
func (m *Monitor) commit(ctx context.Context, ev *events, e evalResult) {
	old := m.cur
	if e.hasTarget && !e.incompat {
		m.reached = true
		m.params, m.hasParams = e.params, true
	}
	ns := State{
		Reached:      m.reached,
		Incompatible: e.incompat, IncompatReason: e.reason, IncompatHost: e.ihost,
		AuthFailing: m.authFailing,
		Target:      e.target, HasTarget: e.hasTarget && !e.incompat,
		Params: m.params, HasParams: m.hasParams,
	}
	m.cur = ns

	if old.HasTarget && (!ns.HasTarget || ns.Target != old.Target) {
		kind := HostNotPrimary
		if h := m.byAdr[old.Target]; h != nil && !h.up {
			kind = HostUnknown
		}
		ev.change(m, Change{Kind: kind, Host: old.Target})
	}
	if ns.HasTarget && (!old.HasTarget || ns.Target != old.Target) {
		host := ns.Target
		ev.change(m, Change{Kind: TargetChanged, Host: host})
		ev.add(func() { m.obs.PrimaryChanged(ctx, host) })
	}
	if !old.Incompatible && ns.Incompatible {
		reason, host := ns.IncompatReason, ns.IncompatHost
		ev.add(func() { m.obs.Incompatible(ctx, reason, host) })
		ev.change(m, Change{Kind: BecameIncompatible, Host: host, Reason: string(reason)})
	}
	or, nr := EvaluateReadiness(old), EvaluateReadiness(ns)
	if or != nr {
		ev.add(func() { m.obs.AvailabilityChanged(ctx, nr.Ready, nr.Reason) })
		if nr.Ready && !or.Ready {
			ev.add(func() { m.obs.Available(ctx) })
		}
	}
	if oa, na := evaluateAdmission(old), evaluateAdmission(ns); oa.Kind != na.Kind || oa.Detail != na.Detail {
		ev.change(m, Change{Kind: AvailabilityChanged, Reason: na.Detail})
	}
}

// State returns a snapshot.
func (m *Monitor) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

// Readiness implements health.Readiness (FR-46).
func (m *Monitor) Readiness(ctx context.Context) health.Status {
	return EvaluateReadiness(m.State())
}

// Admission is the FR-56 decision for a request.
func (m *Monitor) Admission() Admission {
	return evaluateAdmission(m.State())
}

// HandshakeAdmission is the FR-19 decision: the parameters to answer with, or
// the detail of the code-6 error ("upstream_incompatible" before
// "upstream_unavailable").
func (m *Monitor) HandshakeAdmission() (Params, string) {
	s := m.State()
	switch {
	case s.Incompatible:
		return Params{}, "upstream_incompatible"
	case !s.Reached:
		return Params{}, "upstream_unavailable"
	}
	return s.Params, ""
}

var notPrimaryCodes = []int32{10107, 13435, 13436, 11602, 11600, 189, 91}

func isNotPrimaryCode(c int32) bool {
	for _, x := range notPrimaryCodes {
		if x == c {
			return true
		}
	}
	return false
}

// ReplyCodes applies the FR-35 marking rule to a reply from host and reports
// whether the code qualifies (the engine then fails a barrier). Replica-set
// mode: the FR-35 list as a top-level ok:0 code, or as writeConcernError.code
// on a forwarded reply. Single-mongos mode: only top-level 91 or 11600 in an
// ok:0 reply. Barrier replies use the top-level code only.
func (m *Monitor) ReplyCodes(ctx context.Context, hostAddr string, src ReplySource, f ReplyCodeInfo) bool {
	var code int32
	var name string
	switch {
	case !m.cfg.HasReplicaSet:
		if f.HasTop && f.OKZero && (f.Top == 91 || f.Top == 11600) {
			code, name = f.Top, f.TopName
		} else {
			return false
		}
	case f.HasTop && f.OKZero && isNotPrimaryCode(f.Top):
		code, name = f.Top, f.TopName
	case src == Forwarded && f.HasWCE && isNotPrimaryCode(f.WCE):
		code = f.WCE
	default:
		return false
	}

	name = safeCodeName(name)
	m.mu.Lock()
	h := m.byAdr[hostAddr]
	if h == nil || m.stopped {
		m.mu.Unlock()
		return true
	}
	wasTarget := m.cur.HasTarget && m.cur.Target == hostAddr
	h.markedNP = true
	ev := &events{}
	m.commit(ctx, ev, m.evaluate())
	if wasTarget {
		ev.add(func() { m.obs.TargetMarkedNotPrimary(ctx, hostAddr, code, name) })
	} else {
		ev.change(m, Change{Kind: HostNotPrimary, Host: hostAddr})
	}
	m.scheduleImmediate()
	m.mu.Unlock()
	m.run(ev)
	return true
}

// PoolConnectFailed starts an immediate check; it never changes readiness
// itself (FR-46, Amendment 1 G-9).
func (m *Monitor) PoolConnectFailed(ctx context.Context, hostAddr string, err error) {
	m.mu.Lock()
	if !m.stopped {
		m.scheduleImmediate()
	}
	m.mu.Unlock()
}

// PoolAuthResult records the outcome of authenticating a pool connection
// (FR-22): an AuthFailed sets the failing state, a success clears it, anything
// else changes nothing.
func (m *Monitor) PoolAuthResult(ctx context.Context, hostAddr string, err error) {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	ev := &events{}
	switch {
	case IsAuthFailed(err):
		m.authFailing = true
		ev.add(func() { m.obs.AuthFailed(ctx, "pool", hostAddr, err) })
	case err == nil:
		m.authFailing = false
		ev.add(func() { m.obs.AuthSucceeded(ctx, "pool", hostAddr) })
	default:
		m.mu.Unlock()
		return
	}
	m.commit(ctx, ev, m.evaluate())
	m.pump()
	m.mu.Unlock()
	m.run(ev)
}

// safeCodeName keeps an upstream codeName only if it looks like one (letters,
// digits, underscore, at most 64 bytes); anything else would be upstream text
// on its way to logs and is dropped (SPEC-001 FR-24).
func safeCodeName(s string) string {
	if len(s) == 0 || len(s) > 64 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return ""
		}
	}
	return s
}

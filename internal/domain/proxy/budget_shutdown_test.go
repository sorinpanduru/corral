package proxy_test

import (
	"context"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
)

// FR-57, AC-34 (unit): the I/O budget counts only time spent reading. Waiting
// in the queue or for a lease between reads is not charged.
func TestIOBudgetChargesReadsOnly_AC34(t *testing.T) {
	clk := fakeclock.New(t0)
	b := proxy.NewIOBudget(30*time.Second, clk)
	if b.Remaining() != 30*time.Second {
		t.Fatalf("Remaining() = %v", b.Remaining())
	}
	deadline := b.BeginRead()
	if !deadline.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("deadline = %v, want %v", deadline, t0.Add(30*time.Second))
	}
	clk.Advance(10 * time.Second)
	b.EndRead()
	if b.Remaining() != 20*time.Second {
		t.Fatalf("Remaining() = %v after 10 s of reading", b.Remaining())
	}
	// A long pause (queue wait, lease wait, buffer limit reached): not charged.
	clk.Advance(time.Hour)
	if b.Remaining() != 20*time.Second {
		t.Fatalf("a pause was charged: %v", b.Remaining())
	}
	deadline = b.BeginRead()
	if !deadline.Equal(clk.Now().Add(20 * time.Second)) {
		t.Fatalf("deadline after the pause = %v", deadline)
	}
	clk.Advance(20 * time.Second)
	b.EndRead()
	if b.Remaining() != 0 {
		t.Fatalf("Remaining() = %v after the limit's worth of reading", b.Remaining())
	}
}

// While a read is active, Remaining counts the elapsed part live.
func TestIOBudgetLiveRemaining_AC34(t *testing.T) {
	clk := fakeclock.New(t0)
	b := proxy.NewIOBudget(10*time.Second, clk)
	b.BeginRead()
	clk.Advance(4 * time.Second)
	if got := b.Remaining(); got != 6*time.Second {
		t.Fatalf("Remaining() during a read = %v, want 6s", got)
	}
	clk.Advance(7 * time.Second)
	if got := b.Remaining(); got > 0 {
		t.Fatalf("Remaining() = %v after the budget was exceeded, want <= 0", got)
	}
}

// Reads in several pieces add up; Reset starts a new message with a full
// budget.
func TestIOBudgetPiecesAndReset_AC34(t *testing.T) {
	clk := fakeclock.New(t0)
	b := proxy.NewIOBudget(30*time.Second, clk)
	for i := 0; i < 3; i++ {
		b.BeginRead()
		clk.Advance(9 * time.Second)
		b.EndRead()
		clk.Advance(5 * time.Minute) // paused between pieces
	}
	if got := b.Remaining(); got != 3*time.Second {
		t.Fatalf("Remaining() = %v, want 3s", got)
	}
	b.Reset()
	if got := b.Remaining(); got != 30*time.Second {
		t.Fatalf("Remaining() after Reset = %v", got)
	}
}

// EndRead without BeginRead, and double EndRead, charge nothing.
func TestIOBudgetEndWithoutBegin(t *testing.T) {
	clk := fakeclock.New(t0)
	b := proxy.NewIOBudget(time.Second, clk)
	b.EndRead()
	clk.Advance(time.Hour)
	b.EndRead()
	if b.Remaining() != time.Second {
		t.Fatalf("Remaining() = %v", b.Remaining())
	}
}

// FR-47, AC-26 (unit): the shutdown state machine.
func TestShutdownPhases_AC26(t *testing.T) {
	clk := fakeclock.New(t0)
	s := proxy.NewShutdown(5*time.Second, 30*time.Second, clk)
	if s.Phase() != proxy.PhaseServing {
		t.Fatalf("initial phase %v", s.Phase())
	}
	s.Begin()
	if s.Phase() != proxy.PhaseServing {
		t.Fatalf("phase during the listener-close delay: %v, want Serving (normal service)", s.Phase())
	}
	select {
	case <-s.DelayExpired():
		t.Fatal("delay expired at once")
	default:
	}
	clk.WaitForTimers(1, time.Second)
	clk.Advance(5*time.Second - time.Millisecond)
	select {
	case <-s.DelayExpired():
		t.Fatal("delay expired early")
	default:
	}
	clk.Advance(time.Millisecond)
	select {
	case <-s.DelayExpired():
	case <-time.After(time.Second):
		t.Fatal("delay did not expire")
	}
	s.ListenerClosed()
	if s.Phase() != proxy.PhaseLate {
		t.Fatalf("phase after the listener closed: %v, want Late", s.Phase())
	}
}

// FR-47 step 4: the drain runs from step 2; DrainExpired closes when it ends.
func TestShutdownDrainTimer_AC26(t *testing.T) {
	clk := fakeclock.New(t0)
	s := proxy.NewShutdown(0, 30*time.Second, clk)
	s.Begin()
	s.ListenerClosed()
	select {
	case <-s.DrainExpired():
		t.Fatal("drain expired at once")
	default:
	}
	clk.WaitForTimers(1, time.Second)
	clk.Advance(30*time.Second - time.Millisecond)
	select {
	case <-s.DrainExpired():
		t.Fatal("drain expired early")
	default:
	}
	clk.Advance(time.Millisecond)
	select {
	case <-s.DrainExpired():
	case <-time.After(time.Second):
		t.Fatal("drain did not expire")
	}
}

// FR-47 step 3: what each open connection does.
func TestShutdownConnActions_AC26(t *testing.T) {
	clk := fakeclock.New(t0)
	s := proxy.NewShutdown(0, 30*time.Second, clk)
	s.Begin()
	s.ListenerClosed()
	idle := s.ConnAction(false, false)
	busy := s.ConnAction(true, false)
	late := s.ConnAction(false, true)
	if idle != proxy.ActionCloseNow {
		t.Errorf("no exchange in progress: %v, want close at once", idle)
	}
	if busy != proxy.ActionFinishThenClose {
		t.Errorf("exchange in progress: %v, want run to completion then close", busy)
	}
	if late != proxy.ActionRefuseThenClose {
		t.Errorf("first byte read after step 3: %v, want 91 then close", late)
	}
	if idle == busy || busy == late || idle == late {
		t.Fatal("actions are not distinct")
	}
	// An exchange in progress that also has a late first byte is still an
	// exchange in progress (it finishes first).
	if a := s.ConnAction(true, true); a != proxy.ActionFinishThenClose {
		t.Errorf("exchange in progress with a late byte: %v", a)
	}
}

// FR-63 (AC-47 unit): a second signal ends the drain at once, and further
// signals change nothing.
func TestShutdownForce_FR63(t *testing.T) {
	clk := fakeclock.New(t0)
	s := proxy.NewShutdown(0, time.Hour, clk)
	s.Begin()
	s.ListenerClosed()
	s.Force()
	select {
	case <-s.DrainExpired():
	case <-time.After(time.Second):
		t.Fatal("Force did not end the drain")
	}
	s.Force()
	s.Force() // idempotent, no panic
}

// FR-63, R-4: a second signal during the listener-close delay skips the rest
// of the delay, and the drain is over at once as well.
func TestShutdownForceDuringDelay_FR63(t *testing.T) {
	clk := fakeclock.New(t0)
	s := proxy.NewShutdown(time.Hour, time.Hour, clk)
	s.Begin()
	s.Force()
	select {
	case <-s.DelayExpired():
	case <-time.After(time.Second):
		t.Fatal("Force did not skip the delay")
	}
	select {
	case <-s.DrainExpired():
	case <-time.After(time.Second):
		t.Fatal("Force during the delay did not end the drain")
	}
}

// Without Begin, nothing expires and Force is harmless.
func TestShutdownNotBegun_AC26(t *testing.T) {
	clk := fakeclock.New(t0)
	s := proxy.NewShutdown(time.Second, time.Second, clk)
	clk.Advance(time.Hour)
	select {
	case <-s.DrainExpired():
		t.Fatal("drain expired before shutdown began")
	default:
	}
	if s.Phase() != proxy.PhaseServing {
		t.Fatalf("phase %v", s.Phase())
	}
}

// AC-2 Part B: the no-op observer accepts every call, and an observer that
// embeds it can override single methods. Contexts are passed through.
type recObs struct {
	proxy.Noop
	got []string
	ctx context.Context
}

func (r *recObs) ConnRejected(ctx context.Context, reason proxy.RejectReason) {
	r.got = append(r.got, string(reason))
	r.ctx = ctx
}
func (r *recObs) RequestCompleted(ctx context.Context, res proxy.RequestResult) {
	r.got = append(r.got, res.Command+":"+res.Outcome.String())
}

type ctxKey struct{}

func TestObserverNoopAndEmbedding_AC2(t *testing.T) {
	var o proxy.Observer = proxy.Noop{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "caller")
	o.ConnOpened(ctx, proxy.ConnInfo{})
	o.ConnClosed(ctx, 1, "")
	o.ConnRejected(ctx, "limit_exceeded")
	o.TLSFailed(ctx, nil)
	o.HandshakeAnswered(ctx, proxy.HandshakeInfo{})
	o.ClientAuthRejected(ctx, 1, proxy.ClientAuthSASL)
	o.CommandBlocked(ctx, 1, "currentOp")
	o.ClientProtocolError(ctx, 1, nil)
	o.ClientIOTimeout(ctx, 1)
	o.RequestReceived(ctx, "find")
	o.RequestCompleted(ctx, proxy.RequestResult{Command: "find", Outcome: proxy.OutcomeOK, Duration: time.Second})
	o.PanicRecovered(ctx, nil)

	r := &recObs{}
	o = r
	o.ConnRejected(ctx, "limit_exceeded")
	o.RequestCompleted(ctx, proxy.RequestResult{Command: "find", Outcome: proxy.OutcomeProxyError, Dropped: true})
	if len(r.got) != 2 || r.got[0] != "limit_exceeded" || r.got[1] != "find:proxy_error" {
		t.Fatalf("recorded %v", r.got)
	}
	if r.ctx.Value(ctxKey{}) != "caller" {
		t.Fatal("the caller's context was not passed through")
	}
}

package pool_test

import (
	"context"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/pool"
)

func TestBackoffConstants_FR56(t *testing.T) {
	if pool.BackoffInitial != 100*time.Millisecond || pool.BackoffMax != 5*time.Second || pool.BackoffJitter != 0.2 {
		t.Fatalf("constants: %v %v %v", pool.BackoffInitial, pool.BackoffMax, pool.BackoffJitter)
	}
}

// FR-56 (AC-32 unit): 100 ms, doubling, capped at 5 s, within +-20 %, reset on
// Reset. The zero Backoff is usable.
func TestBackoffSequence_AC32(t *testing.T) {
	var b pool.Backoff
	base := []time.Duration{100, 200, 400, 800, 1600, 3200, 5000, 5000, 5000}
	for round := 0; round < 3; round++ {
		for i, ms := range base {
			want := ms * time.Millisecond
			got := b.Next()
			lo, hi := time.Duration(float64(want)*0.8), time.Duration(float64(want)*1.2)
			if got < lo || got > hi {
				t.Fatalf("round %d step %d: %v outside [%v, %v]", round, i, got, lo, hi)
			}
		}
		b.Reset()
	}
}

// The jitter is not constant: over many draws the values spread across the
// +-20 % band.
func TestBackoffJitterSpreads_AC32(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		var b pool.Backoff
		seen[b.Next()] = true
	}
	if len(seen) < 10 {
		t.Fatalf("only %d distinct first delays in 200 draws", len(seen))
	}
}

// FR-56 (AC-32 unit): through the pool, failed opens are spaced by the
// backoff, within +-20 %, doubling up to 5 s, and reset after a success. Rand
// is deterministic at the band's edges and its middle.
func TestPoolBackoffSpacing_AC32(t *testing.T) {
	for name, rnd := range map[string]float64{"low": 0, "mid": 0.5, "high": 0.999999} {
		t.Run(name, func(t *testing.T) {
			c := pool.Config{MaxSize: 2, MaxWaiters: 10, CheckoutTimeout: 24 * time.Hour, Rand: func() float64 { return rnd }}
			x := newPool(t, c, ready(H1), false)
			x.op.setFailAll(true)
			ch := x.async(context.Background())

			wantBase := []time.Duration{100, 200, 400, 800, 1600, 3200, 5000, 5000}
			eventually(t, "first open", func() bool { return len(x.op.openCalls()) >= 1 })
			for i, ms := range wantBase {
				prev := x.op.openCalls()
				last := prev[len(prev)-1].At
				base := ms * time.Millisecond
				const step = 20 * time.Millisecond
				var gap time.Duration
				for {
					x.quiet2()
					x.clk.Advance(step)
					gap += step
					x.quiet2()
					if len(x.op.openCalls()) > len(prev) {
						break
					}
					if gap > 7*time.Second {
						t.Fatalf("attempt %d never retried", i+2)
					}
				}
				got := x.op.openCalls()[len(prev)].At.Sub(last)
				lo, hi := time.Duration(float64(base)*0.8)-step, time.Duration(float64(base)*1.2)+step
				if got < lo || got > hi {
					t.Fatalf("gap before attempt %d = %v, want about %v (+-20%%)", i+2, got, base)
				}
			}
			// A success resets the delay: the next failure waits ~100 ms again.
			x.op.setFailAll(false)
			for i := 0; i < 400; i++ {
				x.clk.Advance(50 * time.Millisecond)
				select {
				case r := <-ch:
					if r.err != nil {
						t.Fatal(r.err)
					}
					r.l.Discard("error")
					goto reset
				default:
				}
				x.quiet2()
			}
			t.Fatal("the waiter was never served after the opener recovered")
		reset:
			x.op.setFailAll(true)
			n0 := len(x.op.openCalls())
			ch2 := x.async(context.Background())
			_ = ch2
			eventually(t, "first attempt after the reset", func() bool { return len(x.op.openCalls()) > n0 })
			cur := len(x.op.openCalls())
			start := x.op.openCalls()[cur-1].At
			var gap time.Duration
			for len(x.op.openCalls()) == cur {
				x.clk.Advance(20 * time.Millisecond)
				gap += 20 * time.Millisecond
				x.quiet2()
				if gap > 2*time.Second {
					t.Fatal("no retry within 2 s after a reset")
				}
			}
			got := x.op.openCalls()[cur].At.Sub(start)
			if got > 150*time.Millisecond {
				t.Fatalf("delay after a reset = %v, want about 100 ms", got)
			}
		})
	}
}

func (x *h) quiet2() { time.Sleep(3 * time.Millisecond) }

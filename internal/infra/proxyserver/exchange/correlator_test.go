package exchange_test

import (
	"context"
	"testing"
	"time"
)

// FR-29: a fire-and-forget request gets no reply, so its upstream request ID
// must not stay outstanding; the barrier ping's own ID is matched.
func TestMoreToComeLeavesNoOutstandingIDs_FR29(t *testing.T) {
	r := newRig(t, opts{})
	c := r.dial()
	for i := int32(1); i <= 5; i++ {
		c.Send(insertMTC(i, "x"))
	}
	roundTrip(t, c, ping(9)) // ordered behind the five barriers
	eventually(t, "connection idle", func() bool { return r.p.Stats().InUse == 0 && r.p.Stats().Idle == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := r.p.Checkout(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Return()
	if n := l.Conn().Correlator().Outstanding(); n != 0 {
		t.Fatalf("Outstanding() = %d after fire-and-forget requests and their barriers, want 0", n)
	}
}

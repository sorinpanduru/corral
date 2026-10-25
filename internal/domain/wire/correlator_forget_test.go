package wire_test

import (
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/wire"
)

// Forget drops an ID that will never get a reply (moreToCome), so nothing
// stays outstanding and the ID is no longer matchable.
func TestCorrelatorForget_FR29(t *testing.T) {
	c := wire.NewCorrelator()
	a, b := c.Next(), c.Next()
	c.Forget(a)
	c.Forget(a) // harmless twice
	c.Forget(12345)
	if c.Outstanding() != 1 {
		t.Fatalf("Outstanding() = %d, want 1", c.Outstanding())
	}
	if detail(c.Match(a)) != "unexpected_response" {
		t.Fatal("a forgotten ID is still matchable")
	}
	if err := c.Match(b); err != nil || c.Outstanding() != 0 {
		t.Fatalf("Match(b)=%v outstanding=%d", err, c.Outstanding())
	}
}

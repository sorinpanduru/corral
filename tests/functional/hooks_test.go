//go:build functional

package functional

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/corralprobe"
)

// AC-51 (FR-64), functional, corraltest build: hold points create held-<point>
// and proceed after release-<point>; an unknown point holds nothing.
func TestHoldPointsAndUnknownPoint_AC51(t *testing.T) {
	t.Run("laststep", func(t *testing.T) {
		c := pxStart(t, pxOpts{upstream: []string{pxAddr(t)}, holdAt: []string{"laststep"}, noWait: true})
		if err := c.p.WaitHeld("laststep", 20*time.Second); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(c.p.HoldDir(), "held-laststep")); err != nil {
			t.Fatalf("held-laststep: %v", err)
		}
		c.p.Release("laststep")
		if _, err := c.p.WaitRecord("corral startup complete", "lifecycle", 20*time.Second); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unknown point holds nothing", func(t *testing.T) {
		c := pxStart(t, pxOpts{upstream: []string{pxAddr(t)}, holdAt: []string{"nosuchpoint"}, noWait: true})
		if _, err := c.p.WaitRecord("corral startup complete", "lifecycle", 20*time.Second); err != nil {
			t.Fatal(err)
		}
		ents, _ := os.ReadDir(c.p.HoldDir())
		for _, e := range ents {
			if e.Name() != "probe.addr" {
				t.Errorf("unexpected file %s in the hold directory", e.Name())
			}
		}
	})
	t.Run("probe lists connections without message bytes", func(t *testing.T) {
		c := pxStart(t, pxOpts{upstream: []string{pxAddr(t)}, probe: true})
		pr := corralprobe.Open(t, c.p.HoldDir())
		k, err := pr.Constants()
		if err != nil || k.L != 65536 || k.K != 65536 {
			t.Fatalf("%+v %v", k, err)
		}
	})
}

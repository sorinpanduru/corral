//go:build functional

package functional

import (
	"context"
	"syscall"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/mongofx"
	"github.com/sorinpanduru/corral/twins/faultproxy"
)

// AC-47 (FR-63), functional: with the drain held open by an exchange that the
// upstream path delays, a second SIGTERM ends the drain at once, one
// "shutdown forced by second signal" record is emitted and the process exits 0
// within the SPEC-001 FR-34 flush bound plus a tolerance; a third signal
// changes nothing.
func TestSecondSIGTERMEndsDrain_AC47(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: v, Members: 1})
		fp := faultproxy.Start(t, rs.Addrs()[0], faultproxy.Options{RewriteHelloSelf: true, DropCompression: true})
		c := pxStart(t, pxOpts{upstream: []string{fp.Addr()}, rs: "rs0", holdAt: []string{"shutdown_drain"},
			set: map[string]string{"proxy.shutdown.drain_timeout": "10m"}})
		c.waitReady()
		fp.AddFault(faultproxy.Match{Command: "find"}, faultproxy.DelayReply(10*time.Minute))
		cl := pxGoClient(t, "mongodb://"+c.proxy)
		go func() {
			cl.Database("corraltest").Collection("c").FindOne(context.Background(), bson.D{}).Err()
		}()
		if !fp.WaitCommands(30*time.Second, func(rs []faultproxy.Record) bool {
			for _, r := range rs {
				if r.Command == "find" && r.Direction == faultproxy.ToUpstream && r.AppName == "corral" {
					return true
				}
			}
			return false
		}) {
			t.Fatal("the find never reached mongod")
		}
		c.p.Signal(syscall.SIGTERM)
		if err := c.p.WaitHeld("shutdown_drain", 30*time.Second); err != nil {
			t.Fatal(err)
		}
		begin := time.Now()
		c.p.Signal(syscall.SIGTERM)
		time.Sleep(100 * time.Millisecond)
		c.p.Signal(syscall.SIGTERM) // a third signal changes nothing
		code, err := c.p.Wait(45 * time.Second)
		if err != nil || code != 0 {
			t.Fatalf("exit %d, %v", code, err)
		}
		if d := time.Since(begin); d > 30*time.Second {
			t.Fatalf("the process took %v to exit after the second signal", d)
		}
		if n := len(c.records("shutdown forced by second signal")); n != 1 {
			t.Fatalf("%d forced-shutdown records, want 1", n)
		}
	})
}

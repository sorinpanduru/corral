//go:build functional

package functional

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/mongofx"
)

// FR-34, FR-54 (T27): corral in single-mongos mode in front of a real mongos:
// CRUD through corral, readiness 200 and the handshake reply with msg isdbgrid.
func TestSingleMongosSmoke_FR34(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		sh := mongofx.StartShardedOneMongos(t, mongofx.Opts{Version: v})
		c := pxStart(t, pxOpts{upstream: []string{sh.MongosAddr()}})
		c.waitReady()
		if code, body := c.readyz(); code != 200 {
			t.Fatalf("readyz %d %s", code, body)
		}
		cl := pxGoClient(t, "mongodb://"+c.proxy)
		coll := cl.Database("corraltest").Collection("px27")
		ctx := pxCtx(t, 60e9)
		if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "a", Value: "x"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: "y"}}}}); err != nil {
			t.Fatal(err)
		}
		var got bson.M
		if err := coll.FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&got); err != nil || got["a"] != "y" {
			t.Fatalf("find: %v %v", got, err)
		}
		if _, err := coll.DeleteOne(ctx, bson.D{{Key: "_id", Value: 1}}); err != nil {
			t.Fatal(err)
		}
		r := pxHelloRaw(t, c.proxy, 1)
		if s, _ := r.Lookup("msg").StringValueOK(); s != "isdbgrid" {
			t.Fatalf("msg = %q", s)
		}
	})
}

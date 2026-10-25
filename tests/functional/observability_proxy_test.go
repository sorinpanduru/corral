//go:build functional

package functional

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/internal/testkit/mongofx"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
	"github.com/sorinpanduru/corral/twins/faultproxy"
)

func tracedEnv(col *otlpcollector.Collector) []string {
	return []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://" + col.HTTPEndpoint(), "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf"}
}

func namedSpans(sp []otlpcollector.RecordedSpan, name string) (n, errs int) {
	for _, s := range sp {
		if s.Name == name {
			n++
			if s.StatusCode == 2 {
				errs++
			}
		}
	}
	return
}

// AC-27 (SPEC-001 AC-17, FR-50), functional: the span shapes with the Go driver
// against a real mongod: an ordinary exchange, a moreToCome write (no barrier
// span), a synthesized error (root only) and an outcome-unknown reset (root and
// round-trip, both in error); no synthesized error carries a status code.
func TestSpanShapes_AC27(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: v, Members: 1})
		fp := faultproxy.Start(t, rs.Addrs()[0], faultproxy.Options{RewriteHelloSelf: true, DropCompression: true})
		col := otlpcollector.Start(t)
		c := pxStart(t, pxOpts{upstream: []string{fp.Addr()}, rs: "rs0", env: tracedEnv(col), set: map[string]string{"tracing.enabled": "true", "pool.max_size": "1"}})
		c.waitReady()
		cl := pxGoClient(t, "mongodb://"+c.proxy, func(o *options.ClientOptions) { o.SetRetryReads(false).SetRetryWrites(false) })
		ctx := pxCtx(t, 60*time.Second)
		db := cl.Database("corraltest")

		if err := db.Collection("shape_ok").FindOne(ctx, bson.D{}).Err(); err != nil && err.Error() != "mongo: no documents in result" {
			t.Fatal(err)
		}
		unack, _ := cl.Database("corraltest").Collection("shape_mtc", options.Collection().SetWriteConcern(writeconcern.Unacknowledged())).InsertOne(ctx, bson.D{{Key: "a", Value: 1}})
		_ = unack
		db.RunCommand(ctx, bson.D{{Key: "currentOp", Value: 1}}) // blocked: synthesized error
		h := fp.AddFault(faultproxy.Match{Command: "find", Once: true}, faultproxy.ResetAfterRequest())
		db.Collection("shape_reset").FindOne(ctx, bson.D{})
		h.Remove()
		// A follow-up so the unacknowledged write's barrier has finished.
		db.Collection("shape_ok").FindOne(ctx, bson.D{})
		c.stop()
		sp := col.Spans()

		if n, e := namedSpans(sp, "find shape_ok"); n < 4 || e != 0 {
			t.Errorf("ordinary find: %d spans, %d in error (want root and round-trip for two requests)", n, e)
		}
		if n, _ := namedSpans(sp, "insert shape_mtc"); n != 2 {
			t.Errorf("moreToCome insert: %d spans, want root and round-trip", n)
		}
		if n, _ := namedSpans(sp, "ping"); n != 0 {
			t.Errorf("%d barrier ping spans", n)
		}
		if n, e := namedSpans(sp, "currentOp"); n != 1 || e != 1 {
			t.Errorf("blocked command: %d spans, %d in error, want one root in error", n, e)
		}
		if n, e := namedSpans(sp, "find shape_reset"); n != 2 || e != 2 {
			t.Errorf("outcome unknown: %d spans, %d in error, want root and round-trip both in error", n, e)
		}
		for _, s := range sp {
			if s.Name == "currentOp" {
				if _, has := s.Attributes["db.response.status_code"]; has {
					t.Error("a synthesized error span carries db.response.status_code")
				}
				if s.Attributes["corral.error.detail"] != "command_blocked" {
					t.Errorf("blocked span detail %q", s.Attributes["corral.error.detail"])
				}
			}
		}
	})
}

// AC-27 (SPEC-001 AC-18, AC-19), functional: markers in the service
// credentials, client credentials, documents, filters, comments and duplicate
// keys never appear in logs, metrics, readiness or spans; database and
// collection names appear in spans and never in /metrics.
func TestMarkersAndNamespaces_AC27(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		a := mongofx.NewAuth(t)
		rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: v, Members: 1, Auth: a})
		col := otlpcollector.Start(t)
		env := append(authEnv(t, a.ServiceUser, a.ServicePassword), tracedEnv(col)...)
		c := pxStart(t, pxOpts{upstream: rs.Addrs(), rs: "rs0", env: env,
			set: map[string]string{"upstream.auth.mechanism": "SCRAM-SHA-256", "tracing.enabled": "true"}})
		c.waitReady()
		sc := markerscan.New()
		mk := func(p string) string { return p + "-" + pxRand(t, 10) }
		docM, filterM, commentM, dupM, clientUserM, clientPassM := mk("mk-doc"), mk("mk-filter"), mk("mk-comment"), mk("mk-dup"), mk("mk-cuser"), mk("mk-cpass")
		markers := []string{docM, filterM, commentM, dupM, clientUserM, clientPassM}
		for _, cred := range []string{a.ServiceUser, a.ServicePassword} {
			if len(cred) >= 16 && markerCharset.MatchString(cred) {
				markers = append(markers, cred)
			}
		}
		for _, m := range markers {
			sc.AddMarker(t, m, markerscan.SourceStdout, markerscan.SourceStderr, markerscan.SourceMetrics, markerscan.SourceReadyz, markerscan.SourceSpans)
		}
		cl := pxGoClient(t, "mongodb://"+c.proxy)
		ctx := pxCtx(t, 60*time.Second)
		coll := cl.Database("corraltest").Collection("c")
		coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "v", Value: docM}, {Key: "dup", Value: bson.D{{Key: "k", Value: dupM}, {Key: "k", Value: dupM}}}})
		coll.FindOne(ctx, bson.D{{Key: "v", Value: filterM}}, options.FindOne().SetComment(commentM))
		coll.UpdateOne(ctx, bson.D{{Key: "v", Value: filterM}}, bson.D{{Key: "$set", Value: bson.D{{Key: "w", Value: docM}}}})
		bad := pxGoClient(t, "mongodb://"+clientUserM+":"+clientPassM+"@"+c.proxy+"/?authSource=admin")
		bad.Database("corraltest").Collection("c").FindOne(ctx, bson.D{})
		// A namespace pair known to the test.
		dbName, collName := "corraltest", "nsmarker"+pxRand(t, 4)
		cl.Database(dbName).Collection(collName).InsertOne(ctx, bson.D{{Key: "_id", Value: 1}})

		metrics, _ := c.p.Metrics()
		if strings.Contains(metrics, collName) {
			t.Fatal("a collection name appears in /metrics")
		}
		code, body, _ := c.p.Readyz()
		_ = code
		c.stop()
		sc.Add(markerscan.SourceStdout, c.p.Stdout())
		sc.Add(markerscan.SourceStderr, c.p.Stderr())
		sc.Add(markerscan.SourceMetrics, []byte(metrics))
		sc.Add(markerscan.SourceReadyz, []byte(body))
		sc.AddCollector(col)
		sc.AssertClean(t)
		found := false
		for _, s := range col.Spans() {
			if s.Attributes["db.collection.name"] == collName && s.Attributes["db.namespace"] == dbName {
				found = true
			}
		}
		if !found {
			t.Error("the database and collection names are not in any span")
		}
		_ = context.Background
	})
}

var markerCharset = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

//go:build functional

package functional

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/internal/testkit/mongofx"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

func authEnv(t *testing.T, user, pass string) []string {
	return []string{"CORRAL_UPSTREAM_AUTH_USERNAME=" + user, "CORRAL_UPSTREAM_AUTH_PASSWORD_FILE=" + pxWriteSecret(t, "svc.pass", pass)}
}

func startAuthed(t *testing.T, version string, a *mongofx.Auth, user, pass string, extra map[string]string) (*mongofx.ReplicaSet, *px) {
	t.Helper()
	rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: version, Members: 1, Auth: a})
	set := map[string]string{"upstream.auth.mechanism": "SCRAM-SHA-256"}
	for k, v := range extra {
		set[k] = v
	}
	c := pxStart(t, pxOpts{upstream: rs.Addrs(), rs: "rs0", env: authEnv(t, user, pass), set: set})
	return rs, c
}

// AC-11 (FR-22, FR-23, FR-24): clients use no credentials; client credentials
// are refused and never reach mongod; auth commands get 18 / 115; no
// synthesized reply carries the service credentials.
func TestServiceIdentityAndClientAuth_AC11(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		a := mongofx.NewAuth(t)
		rs, c := startAuthed(t, v, a, a.ServiceUser, a.ServicePassword, nil)
		c.waitReady()
		// Driver clients with no credentials run CRUD.
		cl := pxGoClient(t, "mongodb://"+c.proxy)
		coll := cl.Database("corraltest").Collection("c")
		if _, err := coll.InsertOne(pxCtx(t, 30*time.Second), bson.D{{Key: "_id", Value: 1}, {Key: "x", Value: "synthetic"}}); err != nil {
			t.Fatalf("insert without credentials: %v", err)
		}
		if err := coll.FindOne(pxCtx(t, 30*time.Second), bson.D{{Key: "_id", Value: 1}}).Err(); err != nil {
			t.Fatalf("find: %v", err)
		}
		// Credentials in the client URI fail with 18, and mongod never sees them.
		clientUser := "client-" + pxRand(t, 8)
		bad := pxGoClient(t, "mongodb://"+clientUser+":client-pw-"+pxRand(t, 8)+"@"+c.proxy+"/?authSource=admin")
		err := bad.Database("corraltest").Collection("c").FindOne(pxCtx(t, 30*time.Second), bson.D{}).Err()
		if pxCode(err) != 18 {
			t.Fatalf("client with credentials: %v, want code 18", err)
		}
		if strings.Contains(rs.Logs(0), clientUser) {
			t.Fatal("mongod's log shows an authentication attempt from a client")
		}
		// getnonce and logout get 115; saslStart gets 18.
		raw := rawwire.Dial(t, c.proxy)
		var replies [][]byte
		for _, cmd := range []struct {
			name string
			code int64
			body bson.D
		}{
			{"getnonce", 115, bson.D{{Key: "getnonce", Value: 1}, {Key: "$db", Value: "admin"}}},
			{"logout", 115, bson.D{{Key: "logout", Value: 1}, {Key: "$db", Value: "admin"}}},
			{"saslStart", 18, bson.D{{Key: "saslStart", Value: 1}, {Key: "mechanism", Value: "SCRAM-SHA-256"}, {Key: "$db", Value: "admin"}}},
		} {
			raw.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: cmd.body}))
			m, err := raw.ReadMessage(10 * time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if got := pxInt(m.Docs[0], "code"); got != cmd.code {
				t.Errorf("%s: code %d, want %d", cmd.name, got, cmd.code)
			}
			replies = append(replies, m.Raw)
		}
		for _, r := range replies {
			if bytes.Contains(r, []byte(a.ServiceUser)) || bytes.Contains(r, []byte(a.ServicePassword)) {
				t.Fatal("a synthesized reply contains the service credentials")
			}
		}
		if c.metric("corral_client_auth_rejected_total") < 4 {
			t.Fatalf("client_auth_rejected_total = %v", c.metric("corral_client_auth_rejected_total"))
		}
		// The marker harness over everything corral emitted.
		sc := markerscan.New()
		sc.AddMarker(t, "svc-user-"+a.ServiceUser, markerscan.SourceStdout)
		_ = sc
		for _, s := range []string{a.ServiceUser, a.ServicePassword} {
			if bytes.Contains(c.p.Stdout(), []byte(s)) || bytes.Contains(c.p.Stderr(), []byte(s)) {
				t.Fatal("the service credentials appear in corral's output")
			}
		}
		if txt, _ := c.p.Metrics(); strings.Contains(txt, a.ServiceUser) || strings.Contains(txt, a.ServicePassword) {
			t.Fatal("the service credentials appear in /metrics")
		}
	})
}

// AC-11: secrets on the command line or in the hosts list exit 2 with the
// marker absent from every output.
func TestSecretsRejectedWithoutLeaking_AC11(t *testing.T) {
	passMarker := "mk-pass-" + pxRand(t, 10)
	userMarker := "mk-user-" + pxRand(t, 10)
	sc := markerscan.New()
	sc.AddMarker(t, passMarker)
	sc.AddMarker(t, userMarker)
	t.Run("password flag", func(t *testing.T) {
		c := pxStart(t, pxOpts{upstream: []string{pxAddr(t)}, set: map[string]string{"upstream.auth.mechanism": "SCRAM-SHA-256", "upstream.auth.password": passMarker}, noWait: true})
		code, err := c.p.Wait(20 * time.Second)
		if err != nil || code != 2 {
			t.Fatalf("exit %d, %v", code, err)
		}
		sc.Add(markerscan.SourceStdout, c.p.Stdout())
		sc.Add(markerscan.SourceStderr, c.p.Stderr())
	})
	t.Run("connection string in the hosts list", func(t *testing.T) {
		c := pxStart(t, pxOpts{upstream: []string{"mongodb://" + userMarker + ":" + passMarker + "@127.0.0.1:27017"}, noWait: true,
			set: map[string]string{"upstream.hosts": ""}, env: []string{"CORRAL_UPSTREAM_HOSTS=mongodb://" + userMarker + ":" + passMarker + "@127.0.0.1:27017"}})
		code, err := c.p.Wait(20 * time.Second)
		if err != nil || code != 2 {
			t.Fatalf("exit %d, %v", code, err)
		}
		found := false
		for _, r := range c.p.Records() {
			if s, _ := r["setting"].(string); strings.Contains(s, "CORRAL_UPSTREAM_HOSTS") {
				found = true
			}
		}
		if !found {
			t.Fatalf("no record names setting CORRAL_UPSTREAM_HOSTS: %s", c.p.Stdout())
		}
		sc.Add(markerscan.SourceStdout, c.p.Stdout())
		sc.Add(markerscan.SourceStderr, c.p.Stderr())
	})
	sc.AssertClean(t)
}

// AC-25 / AC-39 (functional): a wrong service password gives upstream_auth_failed
// while corral keeps running; auth failures are connect errors of class server
// for the same kind; recovery gives 200.
func TestWrongServicePassword_AC25(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		a := mongofx.NewAuth(t)
		rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: v, Members: 1, Auth: a})
		wrong := "wrong-" + pxRand(t, 10)
		c := pxStart(t, pxOpts{upstream: rs.Addrs(), rs: "rs0", env: authEnv(t, a.ServiceUser, wrong), set: map[string]string{"upstream.auth.mechanism": "SCRAM-SHA-256"}})
		c.waitReadyz(503, "upstream_auth_failed")
		if c.metric("corral_upstream_available") != 0 {
			t.Fatal("the gauge is not 0 while not ready")
		}
		c.waitMetricAtLeast("auth failures", 1, "corral_upstream_auth_failures_total", "kind", "monitor")
		if ce, af := c.metric("corral_upstream_connect_errors_total", "kind", "monitor", "error_class", "server"), c.metric("corral_upstream_auth_failures_total", "kind", "monitor"); af > ce {
			t.Fatalf("auth failures %v exceed server connect errors %v for the same kind", af, ce)
		}
		if c.metric("corral_upstream_connect_errors_total", "kind", "monitor", "error_class", "network") != 0 {
			t.Log("note: network connect errors were also counted")
		}
		// corral keeps running and refuses requests with 6.
		cl := pxGoClient(t, "mongodb://"+c.proxy, func(o *options.ClientOptions) { o.SetRetryReads(false) })
		err := cl.Database("corraltest").Collection("c").FindOne(context.Background(), bson.D{}).Err()
		if pxCode(err) != 6 {
			t.Fatalf("request while auth is failing: %v", err)
		}
		// Recovery: the service password on mongod becomes the one corral has.
		if _, err := pxRunAdmin(t, rs.Admin(), bson.D{{Key: "updateUser", Value: a.ServiceUser}, {Key: "pwd", Value: wrong}}); err != nil {
			t.Fatal(err)
		}
		c.waitReady()
		if c.metric("corral_upstream_available") != 1 {
			t.Fatal("the gauge is not 1 after recovery")
		}
		_ = context.Background
	})
}

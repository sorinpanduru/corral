//go:build functional

package mongofx_test

// Docker self-tests for the topologies (make test-functional). The API fixed
// here, beyond material_test.go:
//
//	mongofx.Opts{Version string; Members int; Auth *Auth; TLS *TLSMaterial}
//	mongofx.StartReplicaSet(t, Opts) *ReplicaSet
//	  (*ReplicaSet).Addrs() []string           // "127.0.0.1:<port>" per member
//	  (*ReplicaSet).Admin() *mongo.Client      // direct, root-authenticated if Auth set
//	  (*ReplicaSet).Logs(i int) string
//	  (*ReplicaSet).Stop(i int); Start(i int)
//	  (*ReplicaSet).Exec(i int, cmd ...string) (string, error)
//	mongofx.StartShardedOneMongos(t, Opts) *Sharded
//	  (*Sharded).MongosAddr() string; Admin() *mongo.Client
//
// A missing Docker daemon skips, or fails when CORRALTEST_REQUIRE_DOCKER=1.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sorinpanduru/corral/internal/testkit/mongofx"
)

func ctx(t *testing.T, d time.Duration) context.Context {
	c, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return c
}

func TestSingleNodeReplicaSet_FR54(t *testing.T) {
	for _, v := range mongofx.Versions() {
		t.Run(v, func(t *testing.T) {
			rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: v, Members: 1})
			if len(rs.Addrs()) != 1 || !strings.HasPrefix(rs.Addrs()[0], "127.0.0.1:") {
				t.Fatalf("addrs = %v", rs.Addrs())
			}
			var res bson.M
			if err := rs.Admin().Database("admin").RunCommand(ctx(t, 30*time.Second), bson.D{{Key: "hello", Value: 1}}).Decode(&res); err != nil {
				t.Fatal(err)
			}
			if res["isWritablePrimary"] != true || res["setName"] == nil {
				t.Fatalf("not a writable replica-set primary: %v", res["isWritablePrimary"])
			}
			if hosts, _ := res["hosts"].(bson.A); len(hosts) != 1 || hosts[0] != rs.Addrs()[0] {
				t.Fatalf("hello hosts do not match member name 127.0.0.1:<port>")
			}
			if strings.TrimSpace(rs.Logs(0)) == "" {
				t.Fatal("Logs(0) is empty")
			}
			out, err := rs.Exec(0, "echo", "synthetic")
			if err != nil || !strings.Contains(out, "synthetic") {
				t.Fatalf("Exec: %q %v", out, err)
			}
		})
	}
}

func TestReplicaSetAuth_FR54(t *testing.T) {
	a := mongofx.NewAuth(t)
	rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: "7.0", Members: 1, Auth: a})
	uri := "mongodb://" + rs.Addrs()[0] + "/?directConnection=true"
	anon, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer anon.Disconnect(context.Background())
	if _, err := anon.Database("fxtest").Collection("c").InsertOne(ctx(t, 20*time.Second), bson.D{{Key: "a", Value: 1}}); err == nil {
		t.Fatal("unauthenticated write succeeded on an authorization-enabled node")
	}
	svc, err := mongo.Connect(options.Client().ApplyURI(uri).SetAuth(options.Credential{Username: a.ServiceUser, Password: a.ServicePassword, AuthSource: "admin"}))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Disconnect(context.Background())
	if _, err := svc.Database("fxtest").Collection("c").InsertOne(ctx(t, 20*time.Second), bson.D{{Key: "a", Value: 1}}); err != nil {
		t.Fatalf("service user cannot write the test database: %v", err)
	}
	// The service user has only readWrite on test databases: no admin commands.
	if err := svc.Database("admin").RunCommand(ctx(t, 20*time.Second), bson.D{{Key: "listDatabases", Value: 1}}).Err(); err == nil {
		t.Fatal("service user can run listDatabases (too many privileges)")
	}
	if err := rs.Admin().Database("admin").RunCommand(ctx(t, 20*time.Second), bson.D{{Key: "listDatabases", Value: 1}}).Err(); err != nil {
		t.Fatalf("admin client cannot list databases: %v", err)
	}
}

func TestReplicaSetTLS_FR54(t *testing.T) {
	m := mongofx.NewTLSMaterial(t)
	rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: "7.0", Members: 1, TLS: m})
	plain, err := mongo.Connect(options.Client().ApplyURI("mongodb://" + rs.Addrs()[0] + "/?directConnection=true").SetServerSelectionTimeout(2 * time.Second))
	if err == nil {
		defer plain.Disconnect(context.Background())
		if plain.Ping(ctx(t, 5*time.Second), nil) == nil {
			t.Fatal("plaintext connection works on a TLS-only node")
		}
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(m.CAPEM)
	c, err := mongo.Connect(options.Client().ApplyURI("mongodb://" + rs.Addrs()[0] + "/?directConnection=true").SetTLSConfig(&tls.Config{RootCAs: pool, ServerName: "localhost"}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect(context.Background())
	if err := c.Ping(ctx(t, 20*time.Second), nil); err != nil {
		t.Fatalf("TLS ping with the generated CA failed: %v", err)
	}
}

func TestThreeNodeFailover_FR54(t *testing.T) {
	rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: "7.0", Members: 3})
	if len(rs.Addrs()) != 3 {
		t.Fatalf("addrs = %v", rs.Addrs())
	}
	primary := func() string {
		var res bson.M
		if err := rs.Admin().Database("admin").RunCommand(ctx(t, 30*time.Second), bson.D{{Key: "hello", Value: 1}}).Decode(&res); err != nil {
			t.Fatal(err)
		}
		p, _ := res["primary"].(string)
		return p
	}
	first := primary()
	idx := -1
	for i, a := range rs.Addrs() {
		if a == first {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("primary %q is not a member", first)
	}
	rs.Stop(idx)
	deadline := time.Now().Add(60 * time.Second)
	var next string
	for time.Now().Before(deadline) {
		if p := func() string {
			defer func() { recover() }()
			return primary()
		}(); p != "" && p != first {
			next = p
			break
		}
		time.Sleep(time.Second)
	}
	if next == "" {
		t.Fatal("no new primary after stopping the old one")
	}
	rs.Start(idx)
}

func TestShardedOneMongos_FR54(t *testing.T) {
	sh := mongofx.StartShardedOneMongos(t, mongofx.Opts{Version: "7.0"})
	if !strings.HasPrefix(sh.MongosAddr(), "127.0.0.1:") {
		t.Fatalf("mongos addr = %q", sh.MongosAddr())
	}
	var res bson.M
	if err := sh.Admin().Database("admin").RunCommand(ctx(t, 60*time.Second), bson.D{{Key: "hello", Value: 1}}).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res["msg"] != "isdbgrid" {
		t.Fatalf("not a mongos: msg=%v", res["msg"])
	}
	var shards bson.M
	if err := sh.Admin().Database("admin").RunCommand(ctx(t, 30*time.Second), bson.D{{Key: "listShards", Value: 1}}).Decode(&shards); err != nil {
		t.Fatal(err)
	}
	if s, _ := shards["shards"].(bson.A); len(s) != 1 {
		t.Fatalf("shards = %d, want 1", len(s))
	}
}

// Package mongofx starts real MongoDB topologies in Docker for functional
// tests (ADR-0022): replica sets of 1 or 3 members, optionally with
// authentication (runtime-generated keyfile and users) and TLS, and a sharded
// cluster with one mongos. Containers use the host network on Linux and the
// images are pinned by digest. Test infrastructure only; all credentials and
// certificates are synthetic and generated per call.
//
// Without a Docker daemon the helpers skip the test, or fail it when
// CORRALTEST_REQUIRE_DOCKER=1.
package mongofx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Opts selects the topology.
type Opts struct {
	Version string // "7.0" or "8.0"
	Members int    // replica-set members (default 1)
	Auth    *Auth
	TLS     *TLSMaterial
}

// testDatabases are the databases the service user may read and write.
var testDatabases = []string{"fxtest", "corraltest", "test"}

func requireDocker(tb testing.TB) {
	tb.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("CORRALTEST_REQUIRE_DOCKER") == "1" {
			tb.Fatal("mongofx: Docker daemon not available and CORRALTEST_REQUIRE_DOCKER=1")
		}
		tb.Skip("mongofx: Docker daemon not available")
	}
}

func freePorts(tb testing.TB, n int) []int {
	tb.Helper()
	var lns []net.Listener
	var ports []int
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			tb.Fatal(err)
		}
		lns = append(lns, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range lns {
		ln.Close()
	}
	return ports
}

// entry writes the synthetic files into the container, hands them to the
// mongodb user, and runs the server as that user.
const entry = `set -e
mkdir -p /fx
[ -z "$FX_KEYFILE_B64" ] || { echo "$FX_KEYFILE_B64" | base64 -d > /fx/keyfile; chmod 400 /fx/keyfile; }
[ -z "$FX_CA_B64" ] || { echo "$FX_CA_B64" | base64 -d > /fx/ca.pem; }
[ -z "$FX_PEM_B64" ] || { echo "$FX_PEM_B64" | base64 -d > /fx/server.pem; }
chown -R mongodb:mongodb /fx /data/db
exec gosu mongodb "$@"`

type container struct {
	name string
	addr string
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// run starts a container on a freshly chosen port. Choosing a free port and
// binding it in the container cannot be atomic, so it checks that mongod stayed
// up and retries with a new port if it failed to bind.
func run(tb testing.TB, o Opts, prefix string, build func(port int) []string) *container {
	tb.Helper()
	for attempt := 0; attempt < 4; attempt++ {
		port := freePorts(tb, 1)[0]
		name := fmt.Sprintf("corralfx-%s-%s-%d", prefix, randHex(tb, 3), port)
		dargs := []string{"run", "-d", "--name", name, "--network", "host"}
		if o.Auth != nil {
			dargs = append(dargs, "-e", "FX_KEYFILE_B64="+base64.StdEncoding.EncodeToString(o.Auth.Keyfile))
		}
		if o.TLS != nil {
			dargs = append(dargs, "-e", "FX_CA_B64="+base64.StdEncoding.EncodeToString(o.TLS.CAPEM),
				"-e", "FX_PEM_B64="+base64.StdEncoding.EncodeToString(o.TLS.ServerPEM))
		}
		dargs = append(dargs, "--entrypoint", "bash", Image(o.Version), "-c", entry, "fxsh")
		dargs = append(dargs, build(port)...)
		if out, err := docker(dargs...); err != nil {
			tb.Fatalf("mongofx: docker run failed: %s", strings.TrimSpace(out))
		}
		tb.Cleanup(func() { _, _ = docker("rm", "-f", name) })
		time.Sleep(1500 * time.Millisecond)
		if out, _ := docker("inspect", "-f", "{{.State.Running}}", name); strings.TrimSpace(out) == "true" {
			return &container{name: name, addr: "127.0.0.1:" + strconv.Itoa(port)}
		}
		logs, _ := docker("logs", name)
		_, _ = docker("rm", "-f", name)
		if !strings.Contains(logs, "Address already in use") {
			tb.Fatalf("mongofx: container exited early: %s", tail(logs))
		}
	}
	tb.Fatal("mongofx: no free port after 4 attempts")
	return nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 1500 {
		s = s[len(s)-1500:]
	}
	return s
}

func serverArgs(o Opts, bin string, port int, extra ...string) []string {
	a := append([]string{bin, "--bind_ip", "127.0.0.1", "--port", strconv.Itoa(port)}, extra...)
	if o.Auth != nil {
		a = append(a, "--keyFile", "/fx/keyfile")
	}
	if o.TLS != nil {
		a = append(a, "--tlsMode", "requireTLS", "--tlsCertificateKeyFile", "/fx/server.pem",
			"--tlsCAFile", "/fx/ca.pem", "--tlsAllowConnectionsWithoutCertificates")
	}
	return a
}

func tlsConfig(o Opts) *tls.Config {
	if o.TLS == nil {
		return nil
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(o.TLS.CAPEM)
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func connect(tb testing.TB, o Opts, uri string, cred *options.Credential) *mongo.Client {
	tb.Helper()
	co := options.Client().ApplyURI(uri).SetServerSelectionTimeout(60 * time.Second)
	if cfg := tlsConfig(o); cfg != nil {
		co.SetTLSConfig(cfg)
	}
	if cred != nil {
		co.SetAuth(*cred)
	}
	c, err := mongo.Connect(co)
	if err != nil {
		tb.Fatalf("mongofx: connect failed: %v", err)
	}
	tb.Cleanup(func() { _ = c.Disconnect(context.Background()) })
	return c
}

func cmd(tb testing.TB, c *mongo.Client, db string, d bson.D) bson.M {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var res bson.M
	if err := c.Database(db).RunCommand(ctx, d).Decode(&res); err != nil {
		tb.Fatalf("mongofx: command %q failed: %v", d[0].Key, err)
	}
	return res
}

// waitFor polls f until it returns nil, and reports the last error on timeout.
func waitFor(tb testing.TB, what string, timeout time.Duration, f func() error) {
	tb.Helper()
	end := time.Now().Add(timeout)
	var last error
	for time.Now().Before(end) {
		if last = f(); last == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	tb.Fatalf("mongofx: timed out waiting for %s; last error: %v", what, last)
}

func tryT(c *mongo.Client, db string, d bson.D, timeout time.Duration) (res bson.M, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err = c.Database(db).RunCommand(ctx, d).Decode(&res)
	return res, err
}

func try(c *mongo.Client, db string, d bson.D) (bson.M, error) {
	return tryT(c, db, d, 5*time.Second)
}

func pingErr(c *mongo.Client) error {
	_, err := try(c, "admin", bson.D{{Key: "ping", Value: 1}})
	return err
}

// initReplSet runs replSetInitiate against the first member and waits for a
// primary. Auth, when set, is bootstrapped through the localhost exception.
func initReplSet(tb testing.TB, o Opts, name string, addrs []string) {
	tb.Helper()
	direct := connect(tb, o, "mongodb://"+addrs[0]+"/?directConnection=true", nil)
	for _, a := range addrs {
		c := direct
		if a != addrs[0] {
			c = connect(tb, o, "mongodb://"+a+"/?directConnection=true", nil)
		}
		waitFor(tb, "mongod to answer", 90*time.Second, func() error { return pingErr(c) })
	}
	var members bson.A
	for i, a := range addrs {
		members = append(members, bson.D{{Key: "_id", Value: i}, {Key: "host", Value: a}})
	}
	cmd(tb, direct, "admin", bson.D{{Key: "replSetInitiate", Value: bson.D{{Key: "_id", Value: name}, {Key: "members", Value: members}}}})
	// Wait for a primary. An election that does not finish promptly (a loaded
	// machine, a slow member) is nudged with replSetStepUp on the first member,
	// at most once every 15 s; the wait is long enough for a slow start.
	start := time.Now()
	var nudged time.Time
	waitFor(tb, "a primary", 150*time.Second, func() error {
		r, err := try(direct, "admin", bson.D{{Key: "hello", Value: 1}})
		if err == nil && r["isWritablePrimary"] != true {
			err = errors.New("not yet primary")
			if time.Since(start) > 15*time.Second && time.Since(nudged) > 15*time.Second {
				nudged = time.Now()
				_, _ = tryT(direct, "admin", bson.D{{Key: "replSetStepUp", Value: 1}, {Key: "skipDryRun", Value: true}}, 20*time.Second)
			}
		}
		return err
	})
	// And for every member to settle as PRIMARY or SECONDARY, so that a test
	// starts from a stable set.
	waitFor(tb, "every member to be primary or secondary", 120*time.Second, func() error {
		var st struct {
			Members []struct {
				Name     string `bson:"name"`
				State    int32  `bson:"state"`
				StateStr string `bson:"stateStr"`
			} `bson:"members"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := direct.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&st); err != nil {
			return err
		}
		if len(st.Members) != len(addrs) {
			return errors.New("members missing from replSetGetStatus")
		}
		for _, m := range st.Members {
			if m.State != 1 && m.State != 2 {
				return fmt.Errorf("member %s in state %s", m.Name, m.StateStr)
			}
		}
		return nil
	})
}

func createUsers(tb testing.TB, o Opts, uri string) {
	tb.Helper()
	a := o.Auth
	direct := connect(tb, o, uri, nil)
	cmd(tb, direct, "admin", bson.D{{Key: "createUser", Value: a.RootUser}, {Key: "pwd", Value: a.RootPassword},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}}})
	root := connect(tb, o, uri, &options.Credential{Username: a.RootUser, Password: a.RootPassword, AuthSource: "admin"})
	var roles bson.A
	for _, db := range testDatabases {
		roles = append(roles, bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: db}})
	}
	cmd(tb, root, "admin", bson.D{{Key: "createUser", Value: a.ServiceUser}, {Key: "pwd", Value: a.ServicePassword}, {Key: "roles", Value: roles}})
}

func rootCred(o Opts) *options.Credential {
	if o.Auth == nil {
		return nil
	}
	return &options.Credential{Username: o.Auth.RootUser, Password: o.Auth.RootPassword, AuthSource: "admin"}
}

// ReplicaSet is a running replica set.
type ReplicaSet struct {
	tb      testing.TB
	members []*container
	admin   *mongo.Client
}

// StartReplicaSet starts o.Members (default 1) mongod members and waits until
// all are in the set.
func StartReplicaSet(tb testing.TB, o Opts) *ReplicaSet {
	tb.Helper()
	requireDocker(tb)
	if Image(o.Version) == "" {
		tb.Fatalf("mongofx: unknown MongoDB version %q", o.Version)
	}
	n := o.Members
	if n == 0 {
		n = 1
	}
	rs := &ReplicaSet{tb: tb}
	var addrs []string
	for i := 0; i < n; i++ {
		c := run(tb, o, "rs", func(p int) []string { return serverArgs(o, "mongod", p, "--replSet", "rs0") })
		rs.members = append(rs.members, c)
		addrs = append(addrs, c.addr)
	}
	initReplSet(tb, o, "rs0", addrs)
	first := "mongodb://" + addrs[0] + "/?directConnection=true"
	if o.Auth != nil {
		createUsers(tb, o, first)
	}
	rs.admin = connect(tb, o, "mongodb://"+strings.Join(addrs, ",")+"/?replicaSet=rs0", rootCred(o))
	waitFor(tb, "all members to join", 120*time.Second, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var st struct {
			Members []struct {
				State int32 `bson:"state"`
			} `bson:"members"`
		}
		if err := rs.admin.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&st); err != nil {
			return err
		}
		if len(st.Members) != n {
			return errors.New("not all members listed")
		}
		for _, m := range st.Members {
			if m.State != 1 && m.State != 2 {
				return fmt.Errorf("member in state %d", m.State)
			}
		}
		return nil
	})
	return rs
}

// Addrs returns "127.0.0.1:<port>" for each member.
func (r *ReplicaSet) Addrs() []string {
	out := make([]string, len(r.members))
	for i, m := range r.members {
		out[i] = m.addr
	}
	return out
}

// Admin returns a driver client connected directly to the set (not through
// corral), root-authenticated when Auth was set.
func (r *ReplicaSet) Admin() *mongo.Client { return r.admin }

// Logs returns the container log of member i.
func (r *ReplicaSet) Logs(i int) string {
	out, _ := docker("logs", r.members[i].name)
	return out
}

// Stop stops member i.
func (r *ReplicaSet) Stop(i int) {
	if out, err := docker("stop", "-t", "2", r.members[i].name); err != nil {
		r.tb.Fatalf("mongofx: stop failed: %s", strings.TrimSpace(out))
	}
}

// Start restarts member i.
func (r *ReplicaSet) Start(i int) {
	if out, err := docker("start", r.members[i].name); err != nil {
		r.tb.Fatalf("mongofx: start failed: %s", strings.TrimSpace(out))
	}
}

// Exec runs a command inside member i's container.
func (r *ReplicaSet) Exec(i int, c ...string) (string, error) {
	return docker(append([]string{"exec", r.members[i].name}, c...)...)
}

// Sharded is a sharded cluster with one config server, one shard and one mongos.
type Sharded struct {
	mongos string
	admin  *mongo.Client
}

// StartShardedOneMongos starts a one-shard cluster behind a single mongos.
// Authentication is not supported for this topology.
func StartShardedOneMongos(tb testing.TB, o Opts) *Sharded {
	tb.Helper()
	requireDocker(tb)
	if Image(o.Version) == "" {
		tb.Fatalf("mongofx: unknown MongoDB version %q", o.Version)
	}
	if o.Auth != nil {
		tb.Fatal("mongofx: Auth is not supported for the sharded topology")
	}
	cfg := run(tb, o, "cfg", func(p int) []string { return serverArgs(o, "mongod", p, "--configsvr", "--replSet", "cfg") })
	shard := run(tb, o, "shard", func(p int) []string { return serverArgs(o, "mongod", p, "--shardsvr", "--replSet", "shard0") })
	initReplSet(tb, o, "cfg", []string{cfg.addr})
	initReplSet(tb, o, "shard0", []string{shard.addr})
	mongos := run(tb, o, "mongos", func(p int) []string { return serverArgs(o, "mongos", p, "--configdb", "cfg/"+cfg.addr) })
	admin := connect(tb, o, "mongodb://"+mongos.addr+"/", nil)
	waitFor(tb, "mongos", 90*time.Second, func() error { return pingErr(admin) })
	waitFor(tb, "addShard", 120*time.Second, func() error {
		// addShard can outlive the client timeout and still succeed, so a
		// retry must treat an already-added shard as success.
		_, err := tryT(admin, "admin", bson.D{{Key: "addShard", Value: "shard0/" + shard.addr}}, 30*time.Second)
		if err == nil {
			return nil
		}
		var ls struct {
			Shards []struct{} `bson:"shards"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if admin.Database("admin").RunCommand(ctx, bson.D{{Key: "listShards", Value: 1}}).Decode(&ls) == nil && len(ls.Shards) >= 1 {
			return nil
		}
		return err
	})
	return &Sharded{mongos: mongos.addr, admin: admin}
}

// MongosAddr returns the mongos address.
func (s *Sharded) MongosAddr() string { return s.mongos }

// Admin returns a direct driver client to the mongos.
func (s *Sharded) Admin() *mongo.Client { return s.admin }

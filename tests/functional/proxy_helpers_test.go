//go:build functional

package functional

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sorinpanduru/corral/internal/testkit/corralargs"
	"github.com/sorinpanduru/corral/internal/testkit/corralproc"
	"github.com/sorinpanduru/corral/internal/testkit/mongofx"
)

// SPEC-002 functional suites (T26 to T28): the corral binary against a
// containerised mongod with official drivers (FR-54). Test data and credentials
// are synthetic and generated at run time. A missing Docker daemon skips the
// test (or fails it when CORRALTEST_REQUIRE_DOCKER=1), through mongofx.

// pxVersions are the server versions of the matrix; CORRALTEST_VERSIONS (a
// comma-separated list) narrows it for a quick local run.
func pxVersions() []string {
	if v := os.Getenv("CORRALTEST_VERSIONS"); v != "" {
		return strings.Split(v, ",")
	}
	return mongofx.Versions()
}

func forVersions(t *testing.T, f func(t *testing.T, version string)) {
	t.Helper()
	for _, v := range pxVersions() {
		t.Run("mongo-"+v, func(t *testing.T) { f(t, v) })
	}
}

type pxOpts struct {
	upstream   []string
	rs         string
	set        map[string]string
	env        []string
	prod       bool
	holdAt     []string
	probe      bool
	roundRob   bool
	noWait     bool
	noDefaults bool
}

type px struct {
	t     *testing.T
	p     *corralproc.Proc
	proxy string
	admin string
}

func pxAddr(t testing.TB) string { return fmt.Sprintf("127.0.0.1:%d", corralargs.FreePort(t)) }

func pxRand(t testing.TB, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func pxStart(t *testing.T, o pxOpts) *px {
	t.Helper()
	set := map[string]string{
		"upstream.hosts":              strings.Join(o.upstream, ","),
		"upstream.auth.mechanism":     "none",
		"proxy.listen":                pxAddr(t),
		"admin.listen":                pxAddr(t),
		"upstream.heartbeat_interval": "500ms",
		"log.level":                   "debug",
	}
	if o.rs != "" {
		set["upstream.replica_set"] = o.rs
	}
	for k, v := range o.set {
		if v == "" {
			delete(set, k)
			continue
		}
		set[k] = v
	}
	var keys []string
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var args []string
	for _, k := range keys {
		args = append(args, "--"+k+"="+set[k])
	}
	env := append([]string(nil), o.env...)
	if len(o.holdAt) > 0 {
		env = append(env, "CORRALTEST_HOLD_AT="+strings.Join(o.holdAt, ","))
	}
	if o.probe {
		env = append(env, "CORRALTEST_PROBE_LISTEN=127.0.0.1:0")
	}
	if o.roundRob {
		env = append(env, "CORRALTEST_POOL_ORDER=roundrobin")
	}
	bin := testBinPath
	if o.prod {
		bin = binPath
	}
	p := corralproc.Start(t, corralproc.Opts{Binary: bin, Args: args, Env: env, AdminAddr: set["admin.listen"]})
	if !o.noWait {
		if _, err := p.WaitRecord("wire listener bound", "proxy", 30*time.Second); err != nil {
			t.Fatalf("corral did not bind: %v\nstderr: %s", err, p.Stderr())
		}
	}
	return &px{t: t, p: p, proxy: set["proxy.listen"], admin: set["admin.listen"]}
}

func pxEventually(t testing.TB, d time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (c *px) readyz() (int, string) {
	code, body, err := c.p.Readyz()
	if err != nil {
		return 0, err.Error()
	}
	return code, strings.TrimSpace(body)
}

func (c *px) waitReady() {
	c.t.Helper()
	pxEventually(c.t, 60*time.Second, "readiness 200", func() bool { code, _ := c.readyz(); return code == 200 })
}

func (c *px) waitReadyz(code int, body string) {
	c.t.Helper()
	pxEventually(c.t, 60*time.Second, fmt.Sprintf("readyz %d %s", code, body), func() bool {
		got, b := c.readyz()
		return got == code && b == body
	})
}

var (
	pxSampleRE = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})?\s+(\S+)$`)
	pxLabelRE  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)
)

// metric sums the samples of name whose labels include every key, value pair.
func (c *px) metric(name string, kv ...string) float64 {
	c.t.Helper()
	txt, err := c.p.Metrics()
	if err != nil {
		c.t.Fatalf("metrics: %v", err)
	}
	var sum float64
	for _, line := range strings.Split(txt, "\n") {
		m := pxSampleRE.FindStringSubmatch(line)
		if m == nil || m[1] != name {
			continue
		}
		labels := map[string]string{}
		for _, l := range pxLabelRE.FindAllStringSubmatch(m[2], -1) {
			labels[l[1]] = l[2]
		}
		ok := true
		for i := 0; i+1 < len(kv); i += 2 {
			if labels[kv[i]] != kv[i+1] {
				ok = false
			}
		}
		if ok {
			v, _ := strconv.ParseFloat(m[3], 64)
			sum += v
		}
	}
	return sum
}

func (c *px) waitMetricAtLeast(what string, want float64, name string, kv ...string) {
	c.t.Helper()
	pxEventually(c.t, 30*time.Second, what, func() bool { return c.metric(name, kv...) >= want })
}

func (c *px) records(msg string) []corralproc.Record {
	var out []corralproc.Record
	for _, r := range c.p.Records() {
		if r.Msg() == msg {
			out = append(out, r)
		}
	}
	return out
}

func (c *px) stop() int {
	c.t.Helper()
	c.p.Signal(syscall.SIGTERM)
	code, err := c.p.Wait(60 * time.Second)
	if err != nil {
		c.t.Fatalf("corral did not exit: %v", err)
	}
	return code
}

// ---- Go driver ----

func pxGoClient(t testing.TB, uri string, mod ...func(*options.ClientOptions)) *mongo.Client {
	t.Helper()
	o := options.Client().ApplyURI(uri).SetServerSelectionTimeout(30 * time.Second)
	for _, f := range mod {
		f(o)
	}
	cl, err := mongo.Connect(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Disconnect(context.Background()) })
	return cl
}

func pxCtx(t testing.TB, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func pxCode(err error) int32 {
	if err == nil {
		return 0
	}
	if ce, ok := err.(mongo.CommandError); ok {
		return ce.Code
	}
	if we, ok := err.(mongo.WriteException); ok {
		if we.WriteConcernError != nil {
			return int32(we.WriteConcernError.Code)
		}
		if len(we.WriteErrors) > 0 {
			return int32(we.WriteErrors[0].Code)
		}
	}
	return -1
}

// pxWriteSecret writes content to a private file and returns its path.
func pxWriteSecret(t testing.TB, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// pxDirect connects the driver directly to one member (not through corral).
func pxDirect(t testing.TB, addr string) *mongo.Client {
	t.Helper()
	return pxGoClient(t, "mongodb://"+addr+"/?directConnection=true")
}

func pxRunAdmin(t testing.TB, cl *mongo.Client, cmd bson.D) (bson.M, error) {
	var res bson.M
	err := cl.Database("admin").RunCommand(pxCtx(t, 60*time.Second), cmd).Decode(&res)
	return res, err
}

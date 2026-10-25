//go:build twin

package twin

import (
	"encoding/binary"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/corralargs"
	"github.com/sorinpanduru/corral/internal/testkit/corralproc"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// opts configures one corral process. set holds setting overrides by logical
// name (--name=value); defaults are applied first so a test never repeats a
// flag.
type opts struct {
	upstream []string // host:port seeds; empty: a closed port
	rs       string   // upstream.replica_set
	set      map[string]string
	env      []string
	prod     bool
	holdAt   []string // CORRALTEST_HOLD_AT points
	probe    bool     // CORRALTEST_PROBE_LISTEN=127.0.0.1:0
	roundRob bool
	panicCmd string
	noWait   bool // do not wait for the wire listener
}

type corral struct {
	t     *testing.T
	p     *corralproc.Proc
	proxy string
	admin string
}

func reserveAddr(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func start(t *testing.T, o opts) *corral {
	t.Helper()
	up := o.upstream
	if len(up) == 0 {
		up = []string{reserveAddr(t)}
	}
	set := map[string]string{
		"upstream.hosts":              strings.Join(up, ","),
		"upstream.auth.mechanism":     "none",
		"proxy.listen":                "127.0.0.1:" + strconv.Itoa(corralargs.FreePort(t)),
		"admin.listen":                reserveAddr(t),
		"upstream.heartbeat_interval": "500ms",
		"upstream.connect_timeout":    "3s",
		"pool.checkout_timeout":       "3s",
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
	if o.panicCmd != "" {
		env = append(env, "CORRALTEST_PANIC_COMMAND="+o.panicCmd)
	}
	bin := testBin
	if o.prod {
		bin = prodBin
	}
	p := corralproc.Start(t, corralproc.Opts{Binary: bin, Args: args, Env: env, AdminAddr: set["admin.listen"]})
	if !o.noWait {
		if _, err := p.WaitRecord("wire listener bound", "proxy", 20*time.Second); err != nil {
			t.Fatalf("corral did not bind its wire listener: %v\nstderr: %s", err, p.Stderr())
		}
	}
	return &corral{t: t, p: p, proxy: set["proxy.listen"], admin: set["admin.listen"]}
}

func eventually(t testing.TB, what string, f func() bool) {
	t.Helper()
	eventuallyWithin(t, 15*time.Second, what, f)
}

func eventuallyWithin(t testing.TB, d time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *corral) readyz() (int, string) {
	code, body, err := c.p.Readyz()
	if err != nil {
		return 0, err.Error()
	}
	return code, strings.TrimSpace(body)
}

func (c *corral) waitReady() {
	c.t.Helper()
	eventually(c.t, "readiness 200", func() bool { code, _ := c.readyz(); return code == 200 })
}

func (c *corral) waitReadyz(code int, body string) {
	c.t.Helper()
	eventually(c.t, fmt.Sprintf("readyz %d %s", code, body), func() bool {
		got, b := c.readyz()
		return got == code && b == body
	})
}

func (c *corral) dial() *rawwire.Conn { return rawwire.Dial(c.t, c.proxy) }

// ---- metrics ----

var sampleRE = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})?\s+(\S+)$`)
var labelRE = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

// metric returns the sum of the samples of name whose labels include every
// given key, value pair (0 when there is none).
func (c *corral) metric(name string, kv ...string) float64 {
	c.t.Helper()
	txt, err := c.p.Metrics()
	if err != nil {
		c.t.Fatalf("metrics: %v", err)
	}
	var sum float64
	for _, line := range strings.Split(txt, "\n") {
		m := sampleRE.FindStringSubmatch(line)
		if m == nil || m[1] != name {
			continue
		}
		labels := map[string]string{}
		for _, l := range labelRE.FindAllStringSubmatch(m[2], -1) {
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

// waitDelta waits until the metric has grown by want since base.
func (c *corral) waitDelta(base, want float64, name string, kv ...string) {
	c.t.Helper()
	eventually(c.t, fmt.Sprintf("%s%v to grow by %v", name, kv, want), func() bool {
		return c.metric(name, kv...) >= base+want
	})
	if got := c.metric(name, kv...) - base; got != want {
		c.t.Fatalf("%s%v grew by %v, want %v", name, kv, got, want)
	}
}

// steady asserts the metric does not change over a short settle time.
func (c *corral) steady(base float64, name string, kv ...string) {
	c.t.Helper()
	time.Sleep(300 * time.Millisecond)
	if got := c.metric(name, kv...); got != base {
		c.t.Fatalf("%s%v changed from %v to %v", name, kv, base, got)
	}
}

func (c *corral) records(msg string) []corralproc.Record {
	var out []corralproc.Record
	for _, r := range c.p.Records() {
		if r.Msg() == msg {
			out = append(out, r)
		}
	}
	return out
}

func (c *corral) waitRecords(msg string, n int) []corralproc.Record {
	c.t.Helper()
	eventually(c.t, fmt.Sprintf("%d %q records", n, msg), func() bool { return len(c.records(msg)) >= n })
	return c.records(msg)
}

// ---- messages ----

func d(kv ...any) bson.D {
	var out bson.D
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, bson.E{Key: kv[i].(string), Value: kv[i+1]})
	}
	return out
}

// cmd builds an OP_MSG with the given command document (key, value pairs).
func cmd(id int32, kv ...any) []byte {
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: id, Body: d(kv...)})
}

func mk(id int32, marker string, kv ...any) []byte {
	return cmd(id, append(kv, "corraltestMarker", marker)...)
}

// find builds a find on db d, with a marker.
func find(id int32, marker string) []byte {
	return mk(id, marker, "find", "c", "$db", "d")
}

func ping(id int32) []byte { return cmd(id, "ping", 1, "$db", "admin") }

func rt(t testing.TB, c *rawwire.Conn, raw []byte) *rawwire.Message {
	t.Helper()
	if err := c.Send(raw); err != nil {
		t.Fatalf("send: %v", err)
	}
	m, err := c.ReadMessage(15 * time.Second)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return m
}

func doc(m *rawwire.Message) bson.Raw {
	if len(m.Docs) == 0 {
		return nil
	}
	return m.Docs[0]
}

func intField(r bson.Raw, key string) (int64, bool) {
	v, err := r.LookupErr(key)
	if err != nil {
		return 0, false
	}
	switch v.Type {
	case bson.TypeInt32:
		return int64(v.Int32()), true
	case bson.TypeInt64:
		return v.Int64(), true
	case bson.TypeDouble:
		return int64(v.Double()), true
	}
	return 0, false
}

func code(m *rawwire.Message) int64 {
	c, _ := intField(doc(m), "code")
	return c
}

func isOK(m *rawwire.Message) bool {
	v, ok := intField(doc(m), "ok")
	return ok && v == 1
}

func strField(r bson.Raw, key string) string {
	v, err := r.LookupErr(key)
	if err != nil {
		return ""
	}
	s, _ := v.StringValueOK()
	return s
}

func hasLabel(m *rawwire.Message, name string) bool {
	arr, err := doc(m).LookupErr("errorLabels")
	if err != nil {
		return false
	}
	vals, _ := arr.Array().Values()
	for _, v := range vals {
		if s, ok := v.StringValueOK(); ok && s == name {
			return true
		}
	}
	return false
}

func closedNoBytes(t testing.TB, c *rawwire.Conn) {
	t.Helper()
	if err := c.ExpectClosedNoBytes(10 * time.Second); err != nil {
		t.Fatalf("expected the connection to close with no bytes: %v", err)
	}
}

// rawOpMsg builds an OP_MSG from raw section bytes.
func rawOpMsg(id int32, flags uint32, sections ...[]byte) []byte {
	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, flags)
	for _, s := range sections {
		body = append(body, s...)
	}
	return rawwire.RawHeader(int32(16+len(body)), id, 0, rawwire.OpCodeMsg, body)
}

func kind0(doc []byte) []byte { return append([]byte{0}, doc...) }

// kind1 builds a document-sequence section holding the given raw documents.
func kind1(ident string, docs ...[]byte) []byte {
	var payload []byte
	payload = append(payload, ident...)
	payload = append(payload, 0)
	for _, dd := range docs {
		payload = append(payload, dd...)
	}
	out := []byte{1, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(out[1:], uint32(4+len(payload)))
	return append(out, payload...)
}

func bsonOf(v any) []byte {
	b, err := bson.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// mongosTwin starts a single-mongos twin; mod adjusts its hello spec.
func mongosTwin(t *testing.T, mod ...func(*mongosim.HelloSpec)) *mongosim.Server {
	t.Helper()
	h := mongosim.HelloSpec{Role: mongosim.Mongos}
	for _, f := range mod {
		f(&h)
	}
	return mongosim.Start(t, mongosim.Options{Hello: h})
}

func startMongos(t *testing.T, o opts, mod ...func(*mongosim.HelloSpec)) (*corral, *mongosim.Server) {
	t.Helper()
	tw := mongosTwin(t, mod...)
	o.upstream = []string{tw.Addr()}
	return start(t, o), tw
}

// userMessages are the twin's received messages that are not corral's own
// handshake, monitoring or barrier traffic.
func userMessages(tw *mongosim.Server) []mongosim.Message {
	var out []mongosim.Message
	for _, m := range tw.Received() {
		switch strings.ToLower(m.Command) {
		case "hello", "ismaster", "saslstart", "saslcontinue":
			if m.Marker == "" {
				continue
			}
		}
		if m.AppName == mongosim.MonitorAppName {
			continue
		}
		if strings.EqualFold(m.Command, "ping") && m.Marker == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

func countCommand(tw *mongosim.Server, name string) int { return len(tw.ReceivedCommand(name)) }

func sections(raw []byte, flags uint32) []byte {
	end := len(raw)
	if flags&1 != 0 {
		end -= 4
	}
	return raw[20:end]
}

var sigterm = syscall.SIGTERM

func mongosimMatch(marker string) mongosim.Match { return mongosim.Match{Marker: marker} }
func holdAction() mongosim.Action                { return mongosim.Hold() }

// with returns a view of c that reports failures through t (for subtests).
func (c *corral) with(t *testing.T) *corral { cc := *c; cc.t = t; return &cc }

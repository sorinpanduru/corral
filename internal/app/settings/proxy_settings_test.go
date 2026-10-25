package settings_test

// SPEC-002 T17 / AC-31: the SPEC-002 settings group through the real SPEC-003
// registry and loader, with an injected environment and an in-memory file
// reader. The oracle tables below are copied from the spec, independently of
// the implementation.

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
	"github.com/sorinpanduru/corral/internal/infra/config/usage"
)

const spec002 = "SPEC-002"

func hostport() types.Spec { return types.Spec{Kind: types.KindHostPort} }
func dur() types.Spec      { return types.Spec{Kind: types.KindDuration} }
func durZ() types.Spec     { return types.Spec{Kind: types.KindDuration, AllowZero: true} }
func intT() types.Spec     { return types.Spec{Kind: types.KindInt} }
func strT() types.Spec     { return types.Spec{Kind: types.KindString} }

type row struct {
	name   string
	typ    types.Spec
	pres   registry.Presence
	def    string
	secret bool
	desc   string
}

// spec002Rows is the SPEC-002 settings table, verbatim.
func spec002Rows() []row {
	return []row{
		{"proxy.listen", hostport(), registry.PresenceDefault, "127.0.0.1:27017", false, "MongoDB wire listener address."},
		{"proxy.max_client_connections", intT(), registry.PresenceDefault, "10000", false, "Maximum open client connections."},
		{"proxy.client_idle_timeout", durZ(), registry.PresenceDefault, "0s", false, "Close client connections idle this long."},
		{"proxy.client_io_timeout", dur(), registry.PresenceDefault, "30s", false, "Maximum time to receive a started client message, to write a reply to a client, or to complete a client TLS handshake."},
		{"proxy.tls.enabled", types.Spec{Kind: types.KindBool}, registry.PresenceDefault, "false", false, "Require TLS on the wire listener."},
		{"proxy.tls.cert_path", strT(), registry.PresenceOptional, "", false, "Path to the wire listener certificate chain (PEM)."},
		{"proxy.tls.key", types.Spec{Kind: types.KindText}, registry.PresenceOptional, "", true, "Wire listener TLS private key (PEM); supply via `CORRAL_PROXY_TLS_KEY_FILE` or `--proxy.tls.key_file`."},
		{"proxy.tls.client_ca_path", strT(), registry.PresenceOptional, "", false, "CA bundle for verifying client certificates; when set, mutual TLS is required."},
		{"proxy.shutdown.listener_close_delay", durZ(), registry.PresenceDefault, "0s", false, "Keep serving this long after shutdown starts before closing the listener."},
		{"proxy.shutdown.drain_timeout", dur(), registry.PresenceDefault, "20s", false, "Maximum time to drain in-flight exchanges at shutdown."},
		{"pool.max_size", intT(), registry.PresenceDefault, "10", false, "Maximum upstream connections in the pool."},
		{"pool.min_size", intT(), registry.PresenceDefault, "0", false, "Upstream connections kept open while the upstream is available."},
		{"pool.max_waiters", intT(), registry.PresenceOptional, "", false, "Maximum requests waiting for an upstream connection."},
		{"pool.checkout_timeout", dur(), registry.PresenceDefault, "5s", false, "Maximum wait for an upstream connection."},
		{"upstream.hosts", types.Spec{Kind: types.KindList, Elem: types.KindHostPort}, registry.PresenceRequired, "", false, "Upstream seed addresses (`host:port`); never a connection string."},
		{"upstream.replica_set", strT(), registry.PresenceOptional, "", false, "Replica set name; when set, corral targets that set's current primary. When unset, the single host must be a `mongos`."},
		{"upstream.auth.mechanism", types.Spec{Kind: types.KindEnum, EnumValues: []string{"SCRAM-SHA-256", "none"}}, registry.PresenceDefault, "SCRAM-SHA-256", false, "Upstream authentication mechanism."},
		{"upstream.auth.source", strT(), registry.PresenceDefault, "admin", false, "Authentication database for the service identity."},
		{"upstream.auth.username", strT(), registry.PresenceOptional, "", true, "Service identity username."},
		{"upstream.auth.password", strT(), registry.PresenceOptional, "", true, "Service identity password."},
		{"upstream.connect_timeout", dur(), registry.PresenceDefault, "10s", false, "Time limit to connect, handshake and authenticate an upstream connection, and for one monitoring check."},
		{"upstream.heartbeat_interval", dur(), registry.PresenceDefault, "10s", false, "Interval between upstream health and topology checks."},
		{"upstream.max_idle_time", durZ(), registry.PresenceDefault, "5m", false, "Close idle pool connections after this long, keeping `pool.min_size`."},
		{"upstream.tls.enabled", types.Spec{Kind: types.KindBool}, registry.PresenceDefault, "false", false, "Use verified TLS to the upstream."},
		{"upstream.tls.ca_path", strT(), registry.PresenceOptional, "", false, "CA bundle for verifying the upstream; unset uses system roots."},
	}
}

type ruleRow struct {
	id       string
	involves []string
	primary  string
	unset    *bool // nil: n/a
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

func spec002Rules() []ruleRow {
	return []ruleRow{
		{"proxy-tls-material", []string{"proxy.tls.enabled", "proxy.tls.cert_path", "proxy.tls.key"}, "proxy.tls.enabled", yes()},
		{"proxy-tls-keypair", []string{"proxy.tls.cert_path", "proxy.tls.key"}, "proxy.tls.key", no()},
		{"proxy-tls-mtls", []string{"proxy.tls.client_ca_path", "proxy.tls.enabled"}, "proxy.tls.client_ca_path", no()},
		{"pool-min-le-max", []string{"pool.min_size", "pool.max_size"}, "pool.min_size", nil},
		{"upstream-single-host", []string{"upstream.replica_set", "upstream.hosts"}, "upstream.hosts", yes()},
		{"upstream-auth-credentials", []string{"upstream.auth.mechanism", "upstream.auth.username", "upstream.auth.password"}, "upstream.auth.mechanism", yes()},
		{"upstream-tls-ca", []string{"upstream.tls.ca_path", "upstream.tls.enabled"}, "upstream.tls.ca_path", no()},
	}
}

func group002(tb testing.TB) registry.Group {
	tb.Helper()
	for _, g := range settings.Groups() {
		if g.Spec == spec002 {
			return g
		}
	}
	tb.Fatalf("settings.Groups() has no group %q", spec002)
	return registry.Group{}
}

// AC-31: registration. Every row, with type, presence, secrecy, default and
// description, in namespace proxy, pool or upstream.
func TestAC31_Registration(t *testing.T) {
	g := group002(t)
	if !reflect.DeepEqual(g.Namespaces, []string{"proxy", "pool", "upstream"}) && !sameSet(g.Namespaces, []string{"proxy", "pool", "upstream"}) {
		t.Fatalf("namespaces = %v", g.Namespaces)
	}
	rows := spec002Rows()
	if len(rows) != 25 || len(g.Decls) != 25 {
		t.Fatalf("oracle %d rows, group %d decls, want 25", len(rows), len(g.Decls))
	}
	r := mustRegistry(t)
	for _, w := range rows {
		t.Run(w.name, func(t *testing.T) {
			s, ok := r.Lookup(w.name)
			if !ok {
				t.Fatal("not registered")
			}
			if s.Type.Describe() != w.typ.Describe() || s.Type.Kind != w.typ.Kind || s.Type.Elem != w.typ.Elem {
				t.Errorf("type %q, want %q", s.Type.Describe(), w.typ.Describe())
			}
			if w.typ.Kind == types.KindEnum && s.Type.EnumCaseSensitive {
				t.Error("upstream.auth.mechanism must be case-insensitive")
			}
			if s.Presence != w.pres {
				t.Errorf("presence %v, want %v", s.Presence, w.pres)
			}
			if s.Secret != w.secret {
				t.Errorf("secret %v, want %v", s.Secret, w.secret)
			}
			if s.Description != w.desc {
				t.Errorf("description %q, want %q", s.Description, w.desc)
			}
			if w.pres == registry.PresenceDefault {
				want, err := w.typ.Parse(w.def)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(s.DefaultValue, want) {
					t.Errorf("default %#v, want %#v", s.DefaultValue, want)
				}
			}
			ns := strings.SplitN(w.name, ".", 2)[0]
			if ns != "proxy" && ns != "pool" && ns != "upstream" {
				t.Fatalf("oracle namespace %q", ns)
			}
		})
	}
	// Names do not end in file or _file (SPEC-003 FR-2).
	for _, w := range rows {
		if strings.HasSuffix(w.name, "file") {
			t.Errorf("%s ends in file", w.name)
		}
	}
}

// AC-31: the seven cross rules with their primaries and applies-to-unset flags.
func TestAC31_CrossRulesDeclared(t *testing.T) {
	g := group002(t)
	want := spec002Rules()
	if len(g.Rules) != len(want) {
		t.Fatalf("%d cross rules, want %d", len(g.Rules), len(want))
	}
	byID := map[string]registry.CrossRule{}
	for _, r := range g.Rules {
		byID[r.ID] = r
	}
	for _, w := range want {
		r, ok := byID[w.id]
		if !ok {
			t.Errorf("rule %s missing", w.id)
			continue
		}
		if r.Primary != w.primary {
			t.Errorf("%s: primary %q, want %q", w.id, r.Primary, w.primary)
		}
		if !sameSet(r.Involves, w.involves) {
			t.Errorf("%s: involves %v, want %v", w.id, r.Involves, w.involves)
		}
		if w.unset != nil && r.AppliesToUnset != *w.unset {
			t.Errorf("%s: AppliesToUnset %v, want %v", w.id, r.AppliesToUnset, *w.unset)
		}
	}
}

// The PEM settings carry a File rule (4 MiB) and nothing else does.
func TestAC31_FileRulesOnPEMSettingsOnly(t *testing.T) {
	r := mustRegistry(t)
	pem := map[string]bool{"proxy.tls.cert_path": true, "proxy.tls.client_ca_path": true, "upstream.tls.ca_path": true}
	for _, w := range spec002Rows() {
		s, _ := r.Lookup(w.name)
		if pem[w.name] {
			if s.File == nil || s.File.MaxBytes != 4<<20 || s.File.Check == nil {
				t.Errorf("%s: File rule %+v", w.name, s.File)
			}
		} else if s.File != nil {
			t.Errorf("%s has a File rule", w.name)
		}
	}
}

// AC-31: each default applies when no source is given.
func TestAC31_Defaults(t *testing.T) {
	res := newEnv(t).minimal().load()
	requireClean(t, res)
	for _, w := range spec002Rows() {
		if w.pres != registry.PresenceDefault || w.name == "upstream.auth.mechanism" {
			continue
		}
		v := res.Values[w.name]
		want, _ := w.typ.Parse(w.def)
		if !v.Set || v.Source != loader.SourceDefault || !reflect.DeepEqual(v.V, want) {
			t.Errorf("%s: default not applied (set=%v source=%v value=%#v)", w.name, v.Set, v.Source, v.V)
		}
	}
	// Optional settings are unset.
	for _, n := range []string{"pool.max_waiters", "upstream.replica_set", "proxy.tls.cert_path", "proxy.tls.client_ca_path", "upstream.tls.ca_path", "proxy.tls.key", "upstream.auth.username", "upstream.auth.password"} {
		if res.Values[n].Set {
			t.Errorf("%s is set by default", n)
		}
	}
}

// AC-31: the default mechanism is SCRAM-SHA-256, which needs credentials.
func TestAC31_DefaultMechanismNeedsCredentials(t *testing.T) {
	res := newEnv(t).arg("--upstream.hosts=a.test:27017").load()
	requireRecordsFor(t, res, "upstream.auth.mechanism")
	if v := res.Values["upstream.auth.mechanism"]; v.Set && !strings.EqualFold(v.V.(string), "SCRAM-SHA-256") {
		t.Errorf("default mechanism %v", v.V)
	}
}

// AC-31: each per-setting rule rejects a boundary violation with exactly one
// record under the setting, and the boundary values just inside are accepted.
func TestAC31_BoundaryViolations(t *testing.T) {
	bad := []struct {
		name string
		args []string
		want string
	}{
		{"pool.max_size 0", []string{"--pool.max_size=0"}, "pool.max_size"},
		{"pool.max_size 10001", []string{"--pool.max_size=10001"}, "pool.max_size"},
		{"pool.min_size -1", []string{"--pool.min_size=-1"}, "pool.min_size"},
		{"pool.min_size 10001", []string{"--pool.max_size=10000", "--pool.min_size=10001"}, "pool.min_size"},
		{"pool.max_waiters 0", []string{"--pool.max_waiters=0"}, "pool.max_waiters"},
		{"pool.max_waiters 1000001", []string{"--pool.max_waiters=1000001"}, "pool.max_waiters"},
		{"pool.checkout_timeout 0s", []string{"--pool.checkout_timeout=0s"}, "pool.checkout_timeout"},
		{"pool.checkout_timeout negative", []string{"--pool.checkout_timeout=-1s"}, "pool.checkout_timeout"},
		{"heartbeat 100ms", []string{"--upstream.heartbeat_interval=100ms"}, "upstream.heartbeat_interval"},
		{"heartbeat 499ms", []string{"--upstream.heartbeat_interval=499ms"}, "upstream.heartbeat_interval"},
		{"heartbeat above 1h", []string{"--upstream.heartbeat_interval=3601s"}, "upstream.heartbeat_interval"},
		{"proxy.listen port 0", []string{"--proxy.listen=127.0.0.1:0"}, "proxy.listen"},
		{"proxy.listen empty host", []string{"--proxy.listen=:27017"}, "proxy.listen"},
		{"proxy.listen port 65536", []string{"--proxy.listen=127.0.0.1:65536"}, "proxy.listen"},
		{"max_client_connections 0", []string{"--proxy.max_client_connections=0"}, "proxy.max_client_connections"},
		{"max_client_connections 1000001", []string{"--proxy.max_client_connections=1000001"}, "proxy.max_client_connections"},
		{"client_io_timeout 0s", []string{"--proxy.client_io_timeout=0s"}, "proxy.client_io_timeout"},
		{"client_io_timeout negative", []string{"--proxy.client_io_timeout=-5s"}, "proxy.client_io_timeout"},
		{"client_idle_timeout negative", []string{"--proxy.client_idle_timeout=-1s"}, "proxy.client_idle_timeout"},
		{"listener_close_delay negative", []string{"--proxy.shutdown.listener_close_delay=-1s"}, "proxy.shutdown.listener_close_delay"},
		{"drain_timeout 0s", []string{"--proxy.shutdown.drain_timeout=0s"}, "proxy.shutdown.drain_timeout"},
		{"connect_timeout 0s", []string{"--upstream.connect_timeout=0s"}, "upstream.connect_timeout"},
		{"max_idle_time negative", []string{"--upstream.max_idle_time=-1s"}, "upstream.max_idle_time"},
		{"hosts port 0", []string{"--upstream.hosts=a.test:0"}, "upstream.hosts"},
		{"hosts port 65536", []string{"--upstream.hosts=a.test:65536"}, "upstream.hosts"},
		{"hosts without port", []string{"--upstream.hosts=a.test"}, "upstream.hosts"},
		{"hosts duplicate", []string{"--upstream.replica_set=rs0", "--upstream.hosts=a.test:27017,a.test:27017"}, "upstream.hosts"},
		{"auth.source with $", []string{"--upstream.auth.source=a$b"}, "upstream.auth.source"},
		{"auth.source with dot", []string{"--upstream.auth.source=a.b"}, "upstream.auth.source"},
		{"auth.source with slash", []string{"--upstream.auth.source=a/b"}, "upstream.auth.source"},
		{"auth.source with backslash", []string{`--upstream.auth.source=a\b`}, "upstream.auth.source"},
		{"auth.source with quote", []string{`--upstream.auth.source=a"b`}, "upstream.auth.source"},
		{"auth.source with space", []string{"--upstream.auth.source=a b"}, "upstream.auth.source"},
		{"auth.mechanism unknown", []string{"--upstream.auth.mechanism=PLAIN"}, "upstream.auth.mechanism"},
		{"tls.enabled not a bool", []string{"--upstream.tls.enabled=maybe"}, "upstream.tls.enabled"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			joined := strings.Join(tc.args, " ")
			e := newEnv(t)
			if !strings.Contains(joined, "--upstream.hosts=") {
				e.arg("--upstream.hosts=a.test:27017")
			}
			if !strings.Contains(joined, "--upstream.auth.mechanism=") {
				e.arg("--upstream.auth.mechanism=none")
			}
			e.arg(tc.args...)
			res := e.load()
			requireOneOf(t, res, tc.want)
			requireNoMarkers(t, res, e.m)
		})
	}
	good := [][]string{
		{"--pool.max_size=10000"}, {"--pool.max_size=1"}, {"--pool.min_size=0"}, {"--pool.max_waiters=1"}, {"--pool.max_waiters=1000000"},
		{"--upstream.heartbeat_interval=500ms"}, {"--upstream.heartbeat_interval=1h"}, {"--upstream.max_idle_time=0s"},
		{"--proxy.client_idle_timeout=0s"}, {"--proxy.shutdown.listener_close_delay=0s"}, {"--proxy.max_client_connections=1"},
		{"--proxy.max_client_connections=1000000"}, {"--proxy.listen=0.0.0.0:27017"}, {"--proxy.listen=[::]:27017"}, {"--proxy.listen=127.0.0.1:65535"},
		{"--upstream.auth.source=my_db-1"},
	}
	for _, a := range good {
		res := newEnv(t).minimal().arg(a...).load()
		if got := records(res); len(got) != 0 {
			t.Errorf("%v rejected: %v", a, got)
		}
	}
}

// AC-31, AC-1 (unit): proxy.listen violations also through the environment,
// reported under the environment name.
func TestAC1_ProxyListenViaEnvironment(t *testing.T) {
	for _, v := range []string{":27017", "127.0.0.1:0", "127.0.0.1:99999"} {
		res := newEnv(t).minimal().setenv("CORRAL_PROXY_LISTEN=" + v).load()
		requireOneOf(t, res, "CORRAL_PROXY_LISTEN")
	}
}

// AC-11 (unit): a connection string in upstream.hosts is rejected without
// echoing it.
func TestAC11_ConnectionStringHostRejected(t *testing.T) {
	m := marker(t)
	res := newEnv(t).setenv("CORRAL_UPSTREAM_HOSTS=mongodb://"+m+":"+m+"@h.test:27017", "CORRAL_UPSTREAM_AUTH_MECHANISM=none").load()
	requireOneOf(t, res, "CORRAL_UPSTREAM_HOSTS")
	requireNoMarkers(t, res, m)
	e := newEnv(t).arg("--upstream.auth.mechanism=none", "--upstream.hosts=mongodb://"+m+":"+m+"@h.test:27017")
	res = e.load()
	requireOneOf(t, res, "upstream.hosts")
	requireNoMarkers(t, res, m)
	// SRV style and userinfo forms too.
	for _, h := range []string{"mongodb+srv://h.test", "user@h.test:27017", "h.test:27017/db", "h.test:27017?x=1"} {
		res := newEnv(t).arg("--upstream.auth.mechanism=none", "--upstream.hosts="+h).load()
		requireOneOf(t, res, "upstream.hosts")
	}
}

// AC-31: upstream.hosts is required.
func TestAC31_HostsRequired(t *testing.T) {
	res := newEnv(t).arg("--upstream.auth.mechanism=none").load()
	if got := records(res); len(got) == 0 || got[0] != "upstream.hosts" {
		t.Fatalf("records = %v, want a record for upstream.hosts", got)
	}
	if v := res.Values["upstream.hosts"]; v.Set {
		t.Fatal("upstream.hosts set without input")
	}
}

// AC-31: the seven cross rules: each fails once with setting = its primary,
// and passes in its valid configuration.
func TestAC31_CrossRules(t *testing.T) {
	ec := newEC(t, 1)
	other := newEC(t, 2)

	t.Run("proxy-tls-material", func(t *testing.T) {
		requireRecordsFor(t, newEnv(t).minimal().arg("--proxy.tls.enabled=true").load(), "proxy.tls.enabled")
		// A certificate without a key.
		e := newEnv(t).minimal().arg("--proxy.tls.enabled=true")
		e.arg("--proxy.tls.cert_path=" + e.put("c.pem", ec.CertPEM))
		requireRecordsFor(t, e.load(), "proxy.tls.enabled")
		// A key without a certificate.
		e = newEnv(t).minimal().arg("--proxy.tls.enabled=true")
		e.arg("--proxy.tls.key_file=" + e.put("k.pem", ec.PKCS8))
		requireRecordsFor(t, e.load(), "proxy.tls.enabled")
		requireClean(t, newEnv(t).minimal().tls(ec).load())
	})
	t.Run("proxy-tls-keypair", func(t *testing.T) {
		for _, enabled := range []string{"true", "false"} {
			e := newEnv(t).minimal().arg("--proxy.tls.enabled=" + enabled)
			e.arg("--proxy.tls.cert_path="+e.put("c.pem", ec.CertPEM), "--proxy.tls.key_file="+e.put("k.pem", other.PKCS8))
			res := e.load()
			requireOneOf(t, res, "proxy.tls.key", "proxy.tls.key_file", "CORRAL_PROXY_TLS_KEY_FILE")
			requireNoMarkers(t, res, e.m)
		}
		// TLS off with a matching pair is fine.
		e := newEnv(t).minimal().arg("--proxy.tls.enabled=false")
		e.arg("--proxy.tls.cert_path="+e.put("c.pem", ec.CertPEM), "--proxy.tls.key_file="+e.put("k.pem", ec.PKCS8))
		requireClean(t, e.load())
	})
	t.Run("proxy-tls-mtls", func(t *testing.T) {
		e := newEnv(t).minimal()
		e.arg("--proxy.tls.client_ca_path=" + e.put("ca.pem", ec.CertPEM))
		requireRecordsFor(t, e.load(), "proxy.tls.client_ca_path")
		e = newEnv(t).minimal().tls(ec)
		e.arg("--proxy.tls.client_ca_path=" + e.put("ca.pem", other.CertPEM))
		requireClean(t, e.load())
	})
	t.Run("pool-min-le-max", func(t *testing.T) {
		requireRecordsFor(t, newEnv(t).minimal().arg("--pool.min_size=11").load(), "pool.min_size")
		requireRecordsFor(t, newEnv(t).minimal().arg("--pool.max_size=3", "--pool.min_size=4").load(), "pool.min_size")
		requireClean(t, newEnv(t).minimal().arg("--pool.max_size=3", "--pool.min_size=3").load())
		requireClean(t, newEnv(t).minimal().arg("--pool.max_size=10", "--pool.min_size=0").load())
	})
	t.Run("upstream-single-host", func(t *testing.T) {
		e := newEnv(t).arg("--upstream.auth.mechanism=none", "--upstream.hosts=a.test:1,b.test:2")
		requireRecordsFor(t, e.load(), "upstream.hosts")
		requireClean(t, newEnv(t).arg("--upstream.auth.mechanism=none", "--upstream.hosts=a.test:1,b.test:2", "--upstream.replica_set=rs0").load())
		requireClean(t, newEnv(t).arg("--upstream.auth.mechanism=none", "--upstream.hosts=a.test:1", "--upstream.replica_set=rs0").load())
		requireClean(t, newEnv(t).minimal().load())
	})
	t.Run("upstream-auth-credentials", func(t *testing.T) {
		base := func() *env { return newEnv(t).arg("--upstream.hosts=a.test:27017") }
		// SCRAM without a password, and without anything.
		requireRecordsFor(t, base().setenv("CORRAL_UPSTREAM_AUTH_USERNAME=svc").load(), "upstream.auth.mechanism")
		requireRecordsFor(t, base().load(), "upstream.auth.mechanism")
		requireRecordsFor(t, base().arg("--upstream.auth.mechanism=SCRAM-SHA-256").setenv("CORRAL_UPSTREAM_AUTH_PASSWORD=pw").load(), "upstream.auth.mechanism")
		// none with a username (or a password).
		requireRecordsFor(t, base().arg("--upstream.auth.mechanism=none").setenv("CORRAL_UPSTREAM_AUTH_USERNAME=svc").load(), "upstream.auth.mechanism")
		requireRecordsFor(t, base().arg("--upstream.auth.mechanism=none").setenv("CORRAL_UPSTREAM_AUTH_PASSWORD=pw").load(), "upstream.auth.mechanism")
		// Valid combinations.
		requireClean(t, base().setenv("CORRAL_UPSTREAM_AUTH_USERNAME=svc", "CORRAL_UPSTREAM_AUTH_PASSWORD=pw").load())
		requireClean(t, base().arg("--upstream.auth.mechanism=none").load())
		requireClean(t, base().arg("--upstream.auth.mechanism=scram-sha-256").setenv("CORRAL_UPSTREAM_AUTH_USERNAME=svc", "CORRAL_UPSTREAM_AUTH_PASSWORD=pw").load())
		requireClean(t, base().arg("--upstream.auth.mechanism=NONE").load())
	})
	t.Run("upstream-tls-ca", func(t *testing.T) {
		e := newEnv(t).minimal()
		e.arg("--upstream.tls.ca_path=" + e.put("ca.pem", ec.CertPEM))
		requireRecordsFor(t, e.load(), "upstream.tls.ca_path")
		e = newEnv(t).minimal().arg("--upstream.tls.enabled=true")
		e.arg("--upstream.tls.ca_path=" + e.put("ca.pem", ec.CertPEM))
		requireClean(t, e.load())
		requireClean(t, newEnv(t).minimal().arg("--upstream.tls.enabled=true").load()) // system roots
	})
}

// AC-31: the PEM rule rejects a missing file, a non-PEM file, a file over
// 4 MiB and a PEM without a parsable certificate, naming the setting without
// the path or content.
func TestAC31_PEMRule(t *testing.T) {
	ec := newEC(t, 1)
	type pemSetting struct {
		name string
		prep func(*env, string) // adds the argument for the setting plus its companions
	}
	pems := []pemSetting{
		{"proxy.tls.cert_path", func(e *env, p string) {
			e.arg("--proxy.tls.enabled=true", "--proxy.tls.cert_path="+p, "--proxy.tls.key_file="+e.put("k.pem", ec.PKCS8))
		}},
		{"proxy.tls.client_ca_path", func(e *env, p string) {
			e.tls(ec)
			e.arg("--proxy.tls.client_ca_path=" + p)
		}},
		{"upstream.tls.ca_path", func(e *env, p string) {
			e.arg("--upstream.tls.enabled=true", "--upstream.tls.ca_path="+p)
		}},
	}
	for _, ps := range pems {
		for name, content := range map[string][]byte{
			"not PEM":                  []byte("this is not a PEM file"),
			"empty":                    nil,
			"key block only":           ec.PKCS8,
			"garbage certificate body": []byte("-----BEGIN CERTIFICATE-----\nQUJDREVGRw==\n-----END CERTIFICATE-----\n"),
			"over 4 MiB":               append(append([]byte(nil), ec.CertPEM...), bytes.Repeat([]byte("#"), 4<<20)...),
		} {
			t.Run(ps.name+"/"+name, func(t *testing.T) {
				e := newEnv(t).minimal()
				p := e.put("f.pem", content)
				ps.prep(e, p)
				res := e.load()
				requireRecordsFor(t, res, ps.name)
				requireNoMarkers(t, res, e.m, p)
				if !res.Failed(ps.name) {
					t.Error("setting not marked failed")
				}
			})
		}
		t.Run(ps.name+"/missing file", func(t *testing.T) {
			e := newEnv(t).minimal()
			p := e.path("absent.pem")
			ps.prep(e, p)
			res := e.load()
			requireRecordsFor(t, res, ps.name)
			requireNoMarkers(t, res, e.m, p)
		})
		t.Run(ps.name+"/valid", func(t *testing.T) {
			e := newEnv(t).minimal()
			ps.prep(e, e.put("f.pem", ec.CertPEM))
			requireClean(t, e.load())
		})
		t.Run(ps.name+"/chain with one good certificate", func(t *testing.T) {
			e := newEnv(t).minimal()
			chain := append([]byte("# synthetic comment\n"), ec.CertPEM...)
			chain = append(chain, []byte("-----BEGIN CERTIFICATE-----\nQUJD\n-----END CERTIFICATE-----\n")...)
			ps.prep(e, e.put("f.pem", chain))
			requireClean(t, e.load())
		})
	}
	// The limit is exact: a PEM of exactly 4 MiB is read.
	e := newEnv(t).minimal()
	pad := 4<<20 - len(ec.CertPEM)
	content := append(append([]byte(nil), ec.CertPEM...), bytes.Repeat([]byte("#"), pad)...)
	e.arg("--upstream.tls.enabled=true", "--upstream.tls.ca_path="+e.put("big.pem", content))
	requireClean(t, e.load())
}

// AC-31: the key rule: PKCS#1, PKCS#8 and SEC 1 parse; everything else is
// rejected without echoing content.
func TestAC31_KeyRule(t *testing.T) {
	good := map[string]func() keyMaterial{
		"PKCS#8 EC":  func() keyMaterial { return newEC(t, 3) },
		"PKCS#8 RSA": func() keyMaterial { return newRSA(t, 4) },
	}
	for name, mk := range good {
		km := mk()
		e := newEnv(t).minimal().tls(km)
		requireClean(t, e.load())
		_ = name
	}
	ec := newEC(t, 5)
	rs := newRSA(t, 6)
	for name, c := range map[string]struct {
		cert keyMaterial
		key  []byte
	}{"SEC 1": {ec, ec.SEC1}, "PKCS#1": {rs, rs.PKCS1}} {
		e := newEnv(t).minimal().arg("--proxy.tls.enabled=true")
		e.arg("--proxy.tls.cert_path="+e.put("c.pem", c.cert.CertPEM), "--proxy.tls.key_file="+e.put("k.pem", c.key))
		if got := records(e.load()); len(got) != 0 {
			t.Errorf("%s key rejected: %v", name, got)
		}
	}
	for name, content := range map[string][]byte{
		"not PEM":          []byte("not a key " + marker(t)),
		"certificate":      ec.CertPEM,
		"garbage key body": []byte("-----BEGIN PRIVATE KEY-----\nQUJD\n-----END PRIVATE KEY-----\n"),
		"public key":       []byte("-----BEGIN PUBLIC KEY-----\nQUJD\n-----END PUBLIC KEY-----\n"),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t).minimal().arg("--proxy.tls.enabled=true")
			p := e.put("k.pem", content)
			e.arg("--proxy.tls.cert_path="+e.put("c.pem", ec.CertPEM), "--proxy.tls.key_file="+p)
			res := e.load()
			requireOneOf(t, res, "proxy.tls.key", "proxy.tls.key_file")
			requireNoMarkers(t, res, e.m, p, string(content))
		})
	}
	// Through the environment: CORRAL_PROXY_TLS_KEY_FILE, with the same rule.
	e := newEnv(t).minimal()
	e.arg("--proxy.tls.enabled=true", "--proxy.tls.cert_path="+e.put("c.pem", ec.CertPEM))
	e.setenv("CORRAL_PROXY_TLS_KEY_FILE=" + e.put("k.pem", []byte("not a key")))
	requireOneOf(t, e.load(), "CORRAL_PROXY_TLS_KEY_FILE", "proxy.tls.key")
}

// AC-11, AC-31: secrets are rejected as command-line literals and accepted
// through the environment and _FILE references.
func TestAC31_SecretsAndFileReferences(t *testing.T) {
	m := marker(t)
	for _, name := range []string{"upstream.auth.username", "upstream.auth.password", "proxy.tls.key"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t).minimal().arg("--" + name + "=" + m)
			res := e.load()
			if got := records(res); len(got) == 0 || got[0] != name {
				t.Fatalf("records = %v, want a record for %s first", got, name)
			}
			if !res.Failed(name) {
				t.Error("not failed")
			}
			requireNoMarkers(t, res, m)
		})
	}
	// Accepted: environment, environment _FILE, and --x_file.
	cred := func(e *env) *env {
		return e.arg("--upstream.hosts=a.test:27017")
	}
	requireClean(t, cred(newEnv(t)).setenv("CORRAL_UPSTREAM_AUTH_USERNAME=svc-"+m, "CORRAL_UPSTREAM_AUTH_PASSWORD=pw-"+m).load())
	e := cred(newEnv(t))
	e.setenv("CORRAL_UPSTREAM_AUTH_USERNAME_FILE="+e.put("u", []byte("svc\n")), "CORRAL_UPSTREAM_AUTH_PASSWORD_FILE="+e.put("p", []byte("pw\r\n")))
	res := e.load()
	requireClean(t, res)
	if v, _ := res.Get("upstream.auth.password"); v != "pw" {
		t.Errorf("password from file = %q (trailing newline must be stripped)", v)
	}
	e = cred(newEnv(t))
	e.arg("--upstream.auth.username_file="+e.put("u", []byte("svc")), "--upstream.auth.password_file="+e.put("p", []byte("pw")))
	requireClean(t, e.load())
	// An empty secret is not a valid non-empty value.
	e = cred(newEnv(t))
	e.setenv("CORRAL_UPSTREAM_AUTH_USERNAME_FILE="+e.put("u", []byte("\n")), "CORRAL_UPSTREAM_AUTH_PASSWORD=pw")
	res = e.load()
	if got := records(res); len(got) == 0 {
		t.Fatal("an empty username file was accepted")
	}
	requireNoMarkers(t, res, e.m)
	// A secret never appears in the loader's Supplied list values.
	e = cred(newEnv(t)).setenv("CORRAL_UPSTREAM_AUTH_USERNAME=svc-"+m, "CORRAL_UPSTREAM_AUTH_PASSWORD=pw-"+m)
	for _, s := range e.load().Supplied {
		if strings.Contains(s, m) {
			t.Fatal("a secret value is in Supplied")
		}
	}
}

// The effective queue bound: pool.max_waiters unset means
// proxy.max_client_connections.
func TestAC31_EffectiveMaxWaiters(t *testing.T) {
	res := newEnv(t).minimal().load()
	requireClean(t, res)
	if p := settings.ProxyFrom(res); p.Pool.MaxWaiters != 10000 || p.MaxClientConnections != 10000 {
		t.Fatalf("defaults: MaxWaiters=%d MaxClientConnections=%d", p.Pool.MaxWaiters, p.MaxClientConnections)
	}
	res = newEnv(t).minimal().arg("--proxy.max_client_connections=50").load()
	if p := settings.ProxyFrom(res); p.Pool.MaxWaiters != 50 {
		t.Fatalf("MaxWaiters = %d, want 50", p.Pool.MaxWaiters)
	}
	res = newEnv(t).minimal().arg("--proxy.max_client_connections=50", "--pool.max_waiters=7").load()
	if p := settings.ProxyFrom(res); p.Pool.MaxWaiters != 7 {
		t.Fatalf("MaxWaiters = %d, want 7", p.Pool.MaxWaiters)
	}
}

// The typed view maps every setting.
func TestProxyFromMapping(t *testing.T) {
	e := newEnv(t).arg("--upstream.hosts=a.test:27017,b.test:27018", "--upstream.replica_set=rs0",
		"--proxy.listen=0.0.0.0:28000", "--proxy.max_client_connections=77", "--proxy.client_idle_timeout=3m", "--proxy.client_io_timeout=11s",
		"--proxy.shutdown.listener_close_delay=2s", "--proxy.shutdown.drain_timeout=9s",
		"--pool.max_size=12", "--pool.min_size=3", "--pool.max_waiters=40", "--pool.checkout_timeout=6s",
		"--upstream.auth.source=authdb", "--upstream.connect_timeout=4s", "--upstream.heartbeat_interval=2s", "--upstream.max_idle_time=1m")
	e.setenv("CORRAL_UPSTREAM_AUTH_USERNAME=svc-user", "CORRAL_UPSTREAM_AUTH_PASSWORD=svc-pass")
	res := e.load()
	requireClean(t, res)
	p := settings.ProxyFrom(res)
	if p.Listen != "0.0.0.0:28000" || p.MaxClientConnections != 77 || p.ClientIdleTimeout != 3*time.Minute || p.ClientIOTimeout != 11*time.Second ||
		p.ListenerCloseDelay != 2*time.Second || p.DrainTimeout != 9*time.Second {
		t.Fatalf("proxy fields: %+v", p)
	}
	if p.Pool.MaxSize != 12 || p.Pool.MinSize != 3 || p.Pool.MaxWaiters != 40 || p.Pool.CheckoutTimeout != 6*time.Second {
		t.Fatalf("pool fields: %+v", p.Pool)
	}
	u := p.Upstream
	if !reflect.DeepEqual(u.Hosts, []string{"a.test:27017", "b.test:27018"}) || u.ReplicaSet != "rs0" || !u.HasReplicaSet ||
		!strings.EqualFold(u.Mechanism, "SCRAM-SHA-256") || u.Source != "authdb" ||
		u.ConnectTimeout != 4*time.Second || u.HeartbeatInterval != 2*time.Second || u.MaxIdleTime != time.Minute {
		t.Fatalf("upstream fields: %+v", u)
	}
	if string(u.Username.Bytes()) != "svc-user" || string(u.Password.Bytes()) != "svc-pass" {
		t.Fatal("secrets not carried")
	}
	if p.TLS != nil || u.TLS != nil {
		t.Fatalf("TLS pointers set with TLS off: %v %v", p.TLS, u.TLS)
	}
	// Replica set unset.
	res = newEnv(t).minimal().load()
	if p := settings.ProxyFrom(res); p.Upstream.HasReplicaSet || p.Upstream.ReplicaSet != "" || !strings.EqualFold(p.Upstream.Mechanism, "none") {
		t.Fatalf("upstream: %+v", p.Upstream)
	}
}

// AC-24 (unit): the TLS bytes in settings.Proxy equal the file content, and the
// reader is called once per file.
func TestAC24_TLSBytesCarriedReaderOnce(t *testing.T) {
	ec := newEC(t, 1)
	ca := newEC(t, 2)
	upCA := newEC(t, 3)
	e := newEnv(t).minimal().tls(ec)
	capath := e.put("clientca.pem", ca.CertPEM)
	upath := e.put("upca.pem", upCA.CertPEM)
	e.arg("--proxy.tls.client_ca_path="+capath, "--upstream.tls.enabled=true", "--upstream.tls.ca_path="+upath)
	res := e.load()
	requireClean(t, res)
	p := settings.ProxyFrom(res)
	if p.TLS == nil || !bytes.Equal(p.TLS.CertPEM, ec.CertPEM) || !bytes.Equal(p.TLS.Key.Bytes(), ec.PKCS8) || !bytes.Equal(p.TLS.ClientCAPEM, ca.CertPEM) {
		t.Fatal("proxy TLS bytes differ from the files")
	}
	if p.Upstream.TLS == nil || !bytes.Equal(p.Upstream.TLS.CAPEM, upCA.CertPEM) {
		t.Fatal("upstream CA bytes differ from the file")
	}
	for _, n := range []string{"cert.pem", "key.pem", "clientca.pem", "upca.pem"} {
		if c := e.fs.Calls(e.path(n)); c != 1 {
			t.Errorf("%s read %d times, want 1", n, c)
		}
	}
	// Upstream TLS without a CA: system roots (nil CAPEM), TLS pointer set.
	res = newEnv(t).minimal().arg("--upstream.tls.enabled=true").load()
	p = settings.ProxyFrom(res)
	if p.Upstream.TLS == nil || p.Upstream.TLS.CAPEM != nil {
		t.Fatalf("upstream TLS without a CA: %+v", p.Upstream.TLS)
	}
	// Proxy TLS without client CA: no client verification bytes.
	e = newEnv(t).minimal().tls(ec)
	p = settings.ProxyFrom(e.load())
	if p.TLS == nil || p.TLS.ClientCAPEM != nil {
		t.Fatalf("ClientCAPEM = %v", p.TLS)
	}
}

// FR-24: Secret redacts in every formatting path; Bytes returns the value.
func TestSecretRedaction_FR24(t *testing.T) {
	m := marker(t)
	e := newEnv(t).minimal().setenv("CORRAL_UPSTREAM_AUTH_MECHANISM=scram-sha-256")
	e.args = []string{"--upstream.hosts=a.test:27017"}
	e.setenv("CORRAL_UPSTREAM_AUTH_USERNAME=user-"+m, "CORRAL_UPSTREAM_AUTH_PASSWORD=pass-"+m)
	res := e.load()
	requireClean(t, res)
	p := settings.ProxyFrom(res)
	sec := p.Upstream.Password
	if string(sec.Bytes()) != "pass-"+m {
		t.Fatal("Bytes() does not return the secret")
	}
	for name, got := range map[string]string{
		"String":    sec.String(),
		"%v":        fmtS("%v", sec),
		"%+v":       fmtS("%+v", sec),
		"%#v":       fmtS("%#v", sec),
		"%s":        fmtS("%s", sec),
		"%q":        fmtS("%q", sec),
		"%x":        fmtS("%x", sec),
		"%d":        fmtS("%d", sec),
		"%T-ish":    fmtS("%v", &sec),
		"Proxy %v":  fmtS("%v", p),
		"Proxy %+v": fmtS("%+v", p),
		"Proxy %#v": fmtS("%#v", p),
		"Upstream":  fmtS("%+v", p.Upstream),
		"slog":      slogS(sec),
		"slog Any":  slogS(p),
		"slog Up":   slogS(p.Upstream),
	} {
		if strings.Contains(got, m) {
			t.Errorf("%s leaks the secret", name)
		}
	}
	if sec.String() != "[redacted]" || fmtS("%v", sec) != "[redacted]" {
		t.Errorf("String/%%v = %q / %q, want [redacted]", sec.String(), fmtS("%v", sec))
	}
	// The TLS key secret as well.
	ec := newEC(t, 9)
	e2 := newEnv(t).minimal().tls(ec)
	p2 := settings.ProxyFrom(e2.load())
	if got := fmtS("%+v %#v", p2, p2); strings.Contains(got, "PRIVATE KEY") || strings.Contains(got, string(ec.PKCS8[:40])) {
		t.Error("the TLS key leaks through Proxy formatting")
	}
	if p2.TLS.Key.String() != "[redacted]" {
		t.Errorf("Key.String() = %q", p2.TLS.Key.String())
	}
}

// HasProxyGroup and RegistryOf (ADR-0021).
func TestHasProxyGroup_ADR0021(t *testing.T) {
	if !settings.HasProxyGroup(mustRegistry(t)) {
		t.Fatal("the full registry has no proxy group")
	}
	obs, err := settings.RegistryOf(settings.ObservabilityGroup())
	if err != nil {
		t.Fatal(err)
	}
	if settings.HasProxyGroup(obs) {
		t.Fatal("an observability-only registry claims a proxy group")
	}
	if _, ok := obs.Lookup("upstream.hosts"); ok {
		t.Fatal("upstream.hosts registered without the proxy group")
	}
	for _, n := range spec001Names {
		if _, ok := obs.Lookup(n); !ok {
			t.Errorf("%s missing from the observability-only registry", n)
		}
	}
	g := settings.ObservabilityGroup()
	if g.Spec != spec001 {
		t.Fatalf("ObservabilityGroup().Spec = %q", g.Spec)
	}
	both, err := settings.RegistryOf(settings.ObservabilityGroup(), settings.ProxyGroup())
	if err != nil || !settings.HasProxyGroup(both) {
		t.Fatalf("RegistryOf(both): %v", err)
	}
	// An observability-only registry loads cleanly with no input (existing
	// SPEC-001 and SPEC-003 behaviour is unchanged).
	res := loader.Load(obs, loader.Inputs{ReadFile: newEnv(t).fs.Read})
	requireClean(t, res)
}

// Help for the SPEC-002 group renders exactly the golden text, which is also
// what the spec table renders to.
func TestHelp_Spec002Golden(t *testing.T) {
	golden := readGolden(t, "testdata/help_spec002.golden")
	render := func(g registry.Group) string {
		r, err := registry.New(g)
		if err != nil {
			t.Fatalf("registry: %v", err)
		}
		var b bytes.Buffer
		if err := usage.Write(&b, r); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if got := render(spec002Table()); got != golden {
		t.Fatal("golden does not match the usage rendering of the spec table (regenerate)")
	}
	if got := render(group002(t)); got != golden {
		t.Fatalf("help for the SPEC-002 group differs from the golden text:\n%s", firstDiff(got, golden))
	}
	// Secrets show their _file spelling and never a value.
	for _, s := range []string{"proxy.tls.key_file", "upstream.auth.username_file", "upstream.auth.password_file"} {
		if !strings.Contains(golden, s) {
			t.Errorf("golden lacks %s", s)
		}
	}
}

func spec002Table() registry.Group {
	var ds []registry.Decl
	for _, w := range spec002Rows() {
		ds = append(ds, registry.Decl{Name: w.name, Type: w.typ, Presence: w.pres, Default: w.def, Secret: w.secret, Description: w.desc})
	}
	return registry.Group{Spec: spec002, Namespaces: []string{"proxy", "pool", "upstream"}, Decls: ds}
}

// upstream.replica_set: non-empty; its empty flag value is rejected (AC-4) and
// an empty environment value counts as unset.
func TestAC31_ReplicaSetRule(t *testing.T) {
	res := newEnv(t).minimal().arg("--upstream.replica_set=rs0").load()
	requireClean(t, res)
	res = newEnv(t).minimal().arg("--upstream.replica_set=").load()
	requireRecordsFor(t, res, "upstream.replica_set")
	res = newEnv(t).minimal().setenv("CORRAL_UPSTREAM_REPLICA_SET=").load()
	requireClean(t, res)
}

// The contextual settings excluded from the generic AC-2 loop each get their
// own flag/env/default checks here with valid companions.
func TestAC2_ContextualSettings(t *testing.T) {
	ec := newEC(t, 1)
	// proxy.tls.enabled: default false, flag and env true with material.
	res := newEnv(t).minimal().load()
	if v := res.Values["proxy.tls.enabled"]; !v.Set || v.V != false || v.Source != loader.SourceDefault {
		t.Fatalf("proxy.tls.enabled default: %+v", v)
	}
	e := newEnv(t).minimal().tls(ec)
	if v := e.load().Values["proxy.tls.enabled"]; v.V != true || v.Source != loader.SourceFlag {
		t.Fatalf("flag: %+v", v)
	}
	e = newEnv(t).minimal()
	e.setenv("CORRAL_PROXY_TLS_ENABLED=true", "CORRAL_PROXY_TLS_CERT_PATH="+e.put("c.pem", ec.CertPEM), "CORRAL_PROXY_TLS_KEY_FILE="+e.put("k.pem", ec.PKCS8))
	res = e.load()
	requireClean(t, res)
	if v := res.Values["proxy.tls.enabled"]; v.V != true || v.Source != loader.SourceEnv {
		t.Fatalf("env: %+v", v)
	}
	if v := res.Values["proxy.tls.cert_path"]; !v.Set || v.Source != loader.SourceEnv {
		t.Fatalf("cert_path from env: %+v", v)
	}
	// An invalid environment value is reported under the environment name.
	requireOneOf(t, newEnv(t).minimal().setenv("CORRAL_PROXY_TLS_ENABLED=maybe").load(), "CORRAL_PROXY_TLS_ENABLED")
	// The mechanism: case-insensitive, flag beats environment, invalid rejected.
	res = newEnv(t).arg("--upstream.hosts=a.test:27017", "--upstream.auth.mechanism=NONE").setenv("CORRAL_UPSTREAM_AUTH_MECHANISM=SCRAM-SHA-256").load()
	requireClean(t, res)
	if v := res.Values["upstream.auth.mechanism"]; v.Source != loader.SourceFlag || !strings.EqualFold(v.V.(string), "none") {
		t.Fatalf("mechanism: %+v", v)
	}
	requireOneOf(t, newEnv(t).arg("--upstream.hosts=a.test:27017").setenv("CORRAL_UPSTREAM_AUTH_MECHANISM=plain").load(), "CORRAL_UPSTREAM_AUTH_MECHANISM")
}

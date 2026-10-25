package settings

import (
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

const proxySpec = "SPEC-002"

// ProxyGroup is the SPEC-002 settings group (namespaces proxy, pool and
// upstream).
func ProxyGroup() registry.Group {
	str := types.Spec{Kind: types.KindString}
	hostport := types.Spec{Kind: types.KindHostPort}
	dur := types.Spec{Kind: types.KindDuration}
	durZ := types.Spec{Kind: types.KindDuration, AllowZero: true}
	num := types.Spec{Kind: types.KindInt}
	boolean := types.Spec{Kind: types.KindBool}
	def := func(name string, t types.Spec, d, desc string, rule func(any) error) registry.Decl {
		return registry.Decl{Name: name, Type: t, Presence: registry.PresenceDefault, Default: d, Description: desc, Rule: rule}
	}
	opt := func(name string, t types.Spec, desc string, rule func(any) error) registry.Decl {
		return registry.Decl{Name: name, Type: t, Presence: registry.PresenceOptional, Description: desc, Rule: rule}
	}
	pemFile := &registry.FileRule{MaxBytes: source.MaxPEMFileBytes, Check: pemCertificates}
	certPath := opt("proxy.tls.cert_path", str, "Path to the wire listener certificate chain (PEM).", nil)
	certPath.File = pemFile
	clientCA := opt("proxy.tls.client_ca_path", str, "CA bundle for verifying client certificates; when set, mutual TLS is required.", nil)
	clientCA.File = pemFile
	upCA := opt("upstream.tls.ca_path", str, "CA bundle for verifying the upstream; unset uses system roots.", nil)
	upCA.File = pemFile
	key := opt("proxy.tls.key", types.Spec{Kind: types.KindText},
		"Wire listener TLS private key (PEM); supply via `CORRAL_PROXY_TLS_KEY_FILE` or `--proxy.tls.key_file`.", pemPrivateKey)
	key.Secret = true
	user := opt("upstream.auth.username", str, "Service identity username.", nil)
	user.Secret = true
	pass := opt("upstream.auth.password", str, "Service identity password.", nil)
	pass.Secret = true
	hosts := registry.Decl{Name: "upstream.hosts", Type: types.Spec{Kind: types.KindList, Elem: types.KindHostPort},
		Presence: registry.PresenceRequired, Rule: hostsRule,
		Description: "Upstream seed addresses (`host:port`); never a connection string."}
	mech := def("upstream.auth.mechanism", types.Spec{Kind: types.KindEnum, EnumValues: []string{"SCRAM-SHA-256", "none"}},
		"SCRAM-SHA-256", "Upstream authentication mechanism.", nil)

	return registry.Group{
		Spec:       proxySpec,
		Namespaces: []string{"proxy", "pool", "upstream"},
		Decls: []registry.Decl{
			def("proxy.listen", hostport, "127.0.0.1:27017", "MongoDB wire listener address.", listenRule),
			def("proxy.max_client_connections", num, "10000", "Maximum open client connections.", intIn(1, 1000000)),
			def("proxy.client_idle_timeout", durZ, "0s", "Close client connections idle this long.", nil),
			def("proxy.client_io_timeout", dur, "30s", "Maximum time to receive a started client message, to write a reply to a client, or to complete a client TLS handshake.", nil),
			def("proxy.tls.enabled", boolean, "false", "Require TLS on the wire listener.", nil),
			certPath, key, clientCA,
			def("proxy.shutdown.listener_close_delay", durZ, "0s", "Keep serving this long after shutdown starts before closing the listener.", nil),
			def("proxy.shutdown.drain_timeout", dur, "20s", "Maximum time to drain in-flight exchanges at shutdown.", nil),
			def("pool.max_size", num, "10", "Maximum upstream connections in the pool.", intIn(1, 10000)),
			def("pool.min_size", num, "0", "Upstream connections kept open while the upstream is available.", intIn(0, 10000)),
			opt("pool.max_waiters", num, "Maximum requests waiting for an upstream connection.", intIn(1, 1000000)),
			def("pool.checkout_timeout", dur, "5s", "Maximum wait for an upstream connection.", nil),
			hosts,
			opt("upstream.replica_set", str, "Replica set name; when set, corral targets that set's current primary. When unset, the single host must be a `mongos`.", nil),
			mech,
			def("upstream.auth.source", str, "admin", "Authentication database for the service identity.", sourceRule),
			user, pass,
			def("upstream.connect_timeout", dur, "10s", "Time limit to connect, handshake and authenticate an upstream connection, and for one monitoring check.", nil),
			def("upstream.heartbeat_interval", dur, "10s", "Interval between upstream health and topology checks.", heartbeatRule),
			def("upstream.max_idle_time", durZ, "5m", "Close idle pool connections after this long, keeping `pool.min_size`.", nil),
			def("upstream.tls.enabled", boolean, "false", "Use verified TLS to the upstream.", nil),
			upCA,
		},
		Rules: proxyCrossRules(),
	}
}

func heartbeatRule(v any) error {
	d, ok := v.(time.Duration)
	if !ok {
		return errRuleType
	}
	if d < 500*time.Millisecond || d > time.Hour {
		return errRange
	}
	return nil
}

// HasProxyGroup reports whether r carries the SPEC-002 group (ADR-0021).
func HasProxyGroup(r *registry.Registry) bool {
	_, ok := r.Lookup("upstream.hosts")
	return ok
}

// RegistryOf builds a registry from the given groups (ADR-0021).
func RegistryOf(groups ...registry.Group) (*registry.Registry, error) {
	return registry.New(groups...)
}

// Proxy is the typed view of the SPEC-002 settings.
type Proxy struct {
	Listen               string
	MaxClientConnections int
	ClientIdleTimeout    time.Duration
	ClientIOTimeout      time.Duration
	TLS                  *ProxyTLS
	ListenerCloseDelay   time.Duration
	DrainTimeout         time.Duration
	Pool                 PoolSettings
	Upstream             UpstreamSettings
}

// ProxyTLS carries the wire listener TLS material as read once during loading.
type ProxyTLS struct {
	CertPEM     []byte
	Key         Secret
	ClientCAPEM []byte
}

// PoolSettings are the pool bounds. MaxWaiters is the effective value.
type PoolSettings struct {
	MaxSize, MinSize, MaxWaiters int
	CheckoutTimeout              time.Duration
}

// UpstreamSettings are the upstream settings.
type UpstreamSettings struct {
	Hosts                                          []string
	ReplicaSet                                     string
	HasReplicaSet                                  bool
	Mechanism, Source                              string
	Username, Password                             Secret
	ConnectTimeout, HeartbeatInterval, MaxIdleTime time.Duration
	TLS                                            *UpstreamTLS
}

// UpstreamTLS says to use verified TLS; a nil CAPEM means the system roots.
type UpstreamTLS struct{ CAPEM []byte }

// ProxyFrom maps a load result to the typed view. Callers must check
// res.Errors first.
func ProxyFrom(res loader.Result) Proxy {
	str := func(name string) string { v, _ := res.Get(name); s, _ := v.(string); return s }
	num := func(name string) int { v, _ := res.Get(name); n, _ := v.(int64); return int(n) }
	dur := func(name string) time.Duration { v, _ := res.Get(name); d, _ := v.(time.Duration); return d }
	flag := func(name string) bool { v, _ := res.Get(name); b, _ := v.(bool); return b }
	file := func(name string) []byte {
		v, ok := res.Get(name)
		if !ok {
			return nil
		}
		fv, _ := v.(registry.FileValue)
		return fv.Content
	}
	secret := func(name string) Secret { return NewSecret([]byte(str(name))) }

	p := Proxy{
		Listen:               str("proxy.listen"),
		MaxClientConnections: num("proxy.max_client_connections"),
		ClientIdleTimeout:    dur("proxy.client_idle_timeout"),
		ClientIOTimeout:      dur("proxy.client_io_timeout"),
		ListenerCloseDelay:   dur("proxy.shutdown.listener_close_delay"),
		DrainTimeout:         dur("proxy.shutdown.drain_timeout"),
		Pool: PoolSettings{MaxSize: num("pool.max_size"), MinSize: num("pool.min_size"),
			MaxWaiters: num("pool.max_waiters"), CheckoutTimeout: dur("pool.checkout_timeout")},
	}
	if _, ok := res.Get("pool.max_waiters"); !ok {
		p.Pool.MaxWaiters = p.MaxClientConnections
	}
	if flag("proxy.tls.enabled") {
		p.TLS = &ProxyTLS{CertPEM: file("proxy.tls.cert_path"), Key: NewSecret(pemWithNewline(str("proxy.tls.key"))), ClientCAPEM: file("proxy.tls.client_ca_path")}
	}
	u := UpstreamSettings{
		ReplicaSet: str("upstream.replica_set"), Source: str("upstream.auth.source"),
		Username: secret("upstream.auth.username"), Password: secret("upstream.auth.password"),
		ConnectTimeout: dur("upstream.connect_timeout"), HeartbeatInterval: dur("upstream.heartbeat_interval"),
		MaxIdleTime: dur("upstream.max_idle_time"),
	}
	if v, ok := res.Get("upstream.hosts"); ok {
		u.Hosts, _ = v.([]string)
	}
	_, u.HasReplicaSet = res.Get("upstream.replica_set")
	u.Mechanism = "SCRAM-SHA-256"
	if m := str("upstream.auth.mechanism"); equalFold(m, "none") {
		u.Mechanism = "none"
	}
	if flag("upstream.tls.enabled") {
		u.TLS = &UpstreamTLS{CAPEM: file("upstream.tls.ca_path")}
	}
	p.Upstream = u
	return p
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// pemWithNewline restores the final newline the loader strips from secret
// file content (SPEC-003 FR-15), so the TLS key bytes equal the file's.
func pemWithNewline(s string) []byte {
	if s == "" || s[len(s)-1] == '\n' {
		return []byte(s)
	}
	return []byte(s + "\n")
}

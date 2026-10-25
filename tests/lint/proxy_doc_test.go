package lint_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/infra/proxyserver/exchange"
	"github.com/sorinpanduru/corral/internal/infra/upstreamnet"
)

// SPEC-002 AC-28 (FR-51, FR-65): docs/operations/proxy.md. The content is
// judged by review; these tests guard that every FR-51 topic is present and
// that the numeric constants named in the document equal the code constants.
// Keyword presence cannot judge the content of a statement.

func proxyDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "operations", "proxy.md"))
	if err != nil {
		t.Fatalf("docs/operations/proxy.md: %v", err)
	}
	return string(b)
}

func commas(n int) string {
	s := fmt.Sprint(n)
	if len(s) <= 3 {
		return s
	}
	return commas(n/1000) + "," + s[len(s)-3:]
}

// FR-65: L and K are stated as normative values, 65,536 bytes each, and equal
// the code constants. L includes the 16-byte header.
func TestProxyDocBufferConstants_FR65(t *testing.T) {
	doc := proxyDoc(t)
	if proxy.RequestBufferLimit != 65536 || exchange.ReplyChunk != 65536 {
		t.Fatalf("L = %d, K = %d: the spec fixes both at 65,536 (FR-65)", proxy.RequestBufferLimit, exchange.ReplyChunk)
	}
	l, k := commas(proxy.RequestBufferLimit), commas(exchange.ReplyChunk)
	if !regexp.MustCompile(`(?m)\bL\b[^\n]*` + l).MatchString(doc) {
		t.Errorf("proxy.md does not state L as %s bytes on one line", l)
	}
	if !regexp.MustCompile(`(?m)\bK\b[^\n]*` + k).MatchString(doc) {
		t.Errorf("proxy.md does not state K as %s bytes on one line", k)
	}
	low := strings.ToLower(doc)
	if !strings.Contains(low, "request buffer limit") || !strings.Contains(low, "reply chunk") {
		t.Error("proxy.md does not name the request buffer limit and the reply chunk")
	}
	if !strings.Contains(low, "header") {
		t.Error("proxy.md does not say that L includes the 16-byte header")
	}
	if !strings.Contains(low, "worst-case memory") && !strings.Contains(low, "worst case memory") {
		t.Error("proxy.md does not state the worst-case memory")
	}
}

func numRE(d time.Duration) string {
	switch {
	case d%time.Second == 0:
		return fmt.Sprintf(`%d\s?(s|sec|seconds?)\b`, int(d/time.Second))
	default:
		return fmt.Sprintf(`%d\s?(ms|milliseconds?)\b`, int(d/time.Millisecond))
	}
}

// FR-51: the backoff constants (FR-56) and the keepalive values (FR-28).
func TestProxyDocBackoffAndKeepalive_FR51(t *testing.T) {
	doc := proxyDoc(t)
	for _, c := range []struct {
		what string
		re   string
	}{
		{"backoff initial", numRE(pool.BackoffInitial)},
		{"backoff cap", numRE(pool.BackoffMax)},
		{"backoff jitter", fmt.Sprintf(`(±|\+/-|plus or minus)\s?%d\s?%%`, int(pool.BackoffJitter*100))},
		{"keepalive idle", numRE(upstreamnet.KeepAliveIdle)},
		{"keepalive interval", numRE(upstreamnet.KeepAliveInterval)},
		{"keepalive probes", fmt.Sprintf(`\b%d\s+probes?\b`, upstreamnet.KeepAliveCount)},
		{"TCP_USER_TIMEOUT", numRE(upstreamnet.UserTimeout)},
	} {
		if !regexp.MustCompile(`(?i)` + c.re).MatchString(doc) {
			t.Errorf("proxy.md does not state the %s value (pattern %s)", c.what, c.re)
		}
	}
	for _, k := range []string{"TCP_USER_TIMEOUT", "keepalive", "backoff"} {
		if !strings.Contains(strings.ToLower(doc), strings.ToLower(k)) {
			t.Errorf("proxy.md does not mention %s", k)
		}
	}
}

// FR-51 and its amendments: every statement has at least a keyword.
func TestProxyDocTopics_FR51(t *testing.T) {
	doc := strings.ToLower(proxyDoc(t))
	topics := []struct {
		topic string
		any   []string
	}{
		{"shared identity", []string{"shared identity", "single upstream identity", "share corral's"}},
		{"blocked currentOp", []string{"currentop"}},
		{"top-level only $currentOp", []string{"top-level"}},
		{"killOp", []string{"killop"}},
		{"killAllSessions", []string{"killallsessions"}},
		{"killAllSessionsByPattern", []string{"killallsessionsbypattern"}},
		{"killSessions empty list (Amendment 3)", []string{"killsessions"}},
		{"forwarded identity-revealing commands", []string{"connectionstatus"}},
		{"whatsmyuri", []string{"whatsmyuri"}},
		{"handshake metadata", []string{"appname=corral", "appname: corral", "`corral-monitor`"}},
		{"corral-monitor", []string{"corral-monitor"}},
		{"read preference", []string{"read preference", "readpreference"}},
		{"no standalone", []string{"standalone"}},
		{"pool sizing", []string{"maxincomingconnections"}},
		{"pool.max_size", []string{"pool.max_size"}},
		{"leases for long operations", []string{"lease"}},
		{"client io timeout", []string{"proxy.client_io_timeout"}},
		{"no client authentication", []string{"does not authenticate", "no client authentication", "not authenticate clients"}},
		{"network placement", []string{"network placement"}},
		{"mutual tls", []string{"mutual tls"}},
		{"authSource", []string{"authsource"}},
		{"authMechanism", []string{"authmechanism"}},
		{"replicaSet", []string{"replicaset"}},
		{"compressors", []string{"compressors"}},
		{"loadBalanced", []string{"loadbalanced"}},
		{"restart for rotation", []string{"restart"}},
		{"least privilege", []string{"least privilege", "least-privilege"}},
		{"no clusterMonitor", []string{"clustermonitor"}},
		{"system.profile", []string{"system.profile"}},
		{"queryStatsRead", []string{"querystatsread"}},
		{"residual exposure", []string{"residual"}},
		{"$listSessions", []string{"$listsessions"}},
		{"$listLocalSessions", []string{"$listlocalsessions"}},
		{"lsid killSessions", []string{"lsid", "session id"}},
		{"explain-wrapped $currentOp", []string{"explain"}},
		{"$unionWith", []string{"$unionwith"}},
		{"$lookup", []string{"$lookup"}},
		{"case sensitivity (FR-13)", []string{"case-sensitive", "case sensitive", "case-sensitively"}},
		{"duplicated inspected fields (FR-60)", []string{"duplicate"}},
		{"pipeline as a document sequence", []string{"document sequence", "kind-1", "kind 1"}},
		{"second signal (FR-63)", []string{"second sigterm", "second signal"}},
		{"listener close delay (Amendment 2)", []string{"listener_close_delay"}},
		{"ghost members (Amendment 3)", []string{"ghost"}},
		{"rolling restarts", []string{"rolling restart"}},
		{"foreign set name", []string{"different replica set name", "foreign"}},
		{"unreachable members stay monitored", []string{"unreachable"}},
		{"readiness reasons", []string{"upstream_unavailable"}},
		{"readiness auth", []string{"upstream_auth_failed"}},
		{"readiness incompatible", []string{"upstream_incompatible"}},
	}
	for _, c := range topics {
		found := false
		for _, k := range c.any {
			if strings.Contains(doc, strings.ToLower(k)) {
				found = true
			}
		}
		if !found {
			t.Errorf("proxy.md lacks the topic %q (any of %v)", c.topic, c.any)
		}
	}
}

// FR-51: "It also lists metrics, log events and readiness reasons."
func TestProxyDocListsMetricsAndEvents_FR51(t *testing.T) {
	doc := proxyDoc(t)
	for _, m := range []string{
		"corral_client_connections", "corral_client_connections_accepted_total", "corral_client_connections_closed_total",
		"corral_client_auth_rejected_total", "corral_commands_blocked_total", "corral_handshakes_total",
		"corral_protocol_errors_total", "corral_requests_total", "corral_request_duration_seconds",
		"corral_request_errors_total", "corral_requests_dropped_total", "corral_pool_connections",
		"corral_pool_max_connections", "corral_pool_waiters", "corral_pool_checkouts_total",
		"corral_pool_checkout_failures_total", "corral_pool_checkout_wait_seconds",
		"corral_upstream_connections_opened_total", "corral_upstream_connections_closed_total",
		"corral_upstream_connect_errors_total", "corral_upstream_auth_failures_total",
		"corral_upstream_available", "corral_upstream_primary_changes_total",
	} {
		if !regexp.MustCompile(regexp.QuoteMeta(m) + `\b`).MatchString(doc) {
			t.Errorf("proxy.md does not list the metric %s", m)
		}
	}
	for _, msg := range []string{
		"wire listener bound", "client connection rejected", "client authentication rejected", "client command blocked",
		"client protocol error", "client I/O timed out", "client TLS handshake failed", "request handler panic recovered",
		"pool checkout timed out", "pool wait queue full", "upstream connection failed", "upstream authentication failed",
		"upstream unavailable", "upstream incompatible", "upstream available", "upstream primary changed",
		"upstream target marked not primary", "upstream protocol error", "upstream barrier failed",
		"shutdown forced by second signal",
	} {
		if !strings.Contains(doc, msg) {
			t.Errorf("proxy.md does not list the log event %q", msg)
		}
	}
}

// Documentation compliance: examples use placeholders only.
func TestProxyDocPlaceholdersOnly(t *testing.T) {
	doc := proxyDoc(t)
	for _, re := range []string{
		`AKIA[0-9A-Z]{16}`, `sk-[A-Za-z0-9]{20,}`, `sk-ant-`, `AIza[0-9A-Za-z_-]{35}`, `ya29\.`,
		`-----BEGIN [A-Z ]*PRIVATE KEY-----`, `(?i)bearer\s+[A-Za-z0-9._-]{20,}`, `AccountKey=`,
		`mongodb(\+srv)?://[^\s<]+:[^\s<@]+@`,
	} {
		if regexp.MustCompile(re).MatchString(doc) {
			t.Errorf("proxy.md matches the credential pattern %s", re)
		}
	}
	if !strings.Contains(doc, "<YOUR_DB_PASSWORD>") && !strings.Contains(doc, "/run/secrets/<") {
		t.Error("proxy.md shows no credential placeholder (<YOUR_DB_PASSWORD> or /run/secrets/<name>)")
	}
}

// SPEC-003 FR-26 / AC-16: configuration.md has an entry (logical name and
// environment variable) for every SPEC-002 setting, and observability.md
// points to proxy.md.
func TestConfigurationDocHasEverySpec002Setting_AC16(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "operations", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for _, n := range []string{
		"proxy.listen", "proxy.max_client_connections", "proxy.client_idle_timeout", "proxy.client_io_timeout",
		"proxy.tls.enabled", "proxy.tls.cert_path", "proxy.tls.key", "proxy.tls.client_ca_path",
		"proxy.shutdown.listener_close_delay", "proxy.shutdown.drain_timeout",
		"pool.max_size", "pool.min_size", "pool.max_waiters", "pool.checkout_timeout",
		"upstream.hosts", "upstream.replica_set", "upstream.auth.mechanism", "upstream.auth.source",
		"upstream.auth.username", "upstream.auth.password", "upstream.connect_timeout",
		"upstream.heartbeat_interval", "upstream.max_idle_time", "upstream.tls.enabled", "upstream.tls.ca_path",
	} {
		env := "CORRAL_" + strings.ToUpper(strings.ReplaceAll(n, ".", "_"))
		if !strings.Contains(doc, n) {
			t.Errorf("configuration.md does not mention %s", n)
		}
		if !strings.Contains(doc, env) {
			t.Errorf("configuration.md does not mention %s (for %s)", env, n)
		}
	}
	ob, err := os.ReadFile(filepath.Join("..", "..", "docs", "operations", "observability.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ob), "proxy.md") {
		t.Error("observability.md has no pointer to proxy.md")
	}
}

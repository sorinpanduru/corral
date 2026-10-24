package types_test

import (
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

var hp = spec(types.KindHostPort)

func label(n int, c byte) string { return strings.Repeat(string(c), n) }

// host253 builds a hostname of exactly 253 bytes ending in a letter label.
func host253() string {
	return label(63, 'a') + "." + label(63, 'b') + "." + label(63, 'c') + "." + label(61, 'd')
}

func TestHostPort_Accepted(t *testing.T) {
	in := []string{
		"127.0.0.1:9187", "0.0.0.0:0", "127.0.0.1:0", "localhost:80", "LOCALHOST:80",
		"[::1]:9187", "[::]:0", "[::ffff:1.2.3.4]:80", "[2001:db8::1]:65535",
		"example.com:65535", "a-b.example.com:1", "host1:80", "1host:80", "a.b.c.d:8080",
		"xn--bcher-kva.example:80", "host.0xg:80", "x:1",
		label(63, 'a') + ".com:80", // label boundary 63
		host253() + ":80",          // total boundary 253
	}
	for i, s := range in {
		got, err := hp.Parse(s)
		if err != nil {
			t.Errorf("%s: rejected (class %s), want accepted", desc(i, s), classOf(err))
			continue
		}
		// hostport is returned as validated text (ADR-0011).
		if g, ok := got.(string); !ok || g != s {
			t.Errorf("%s: value %T differs from input text", desc(i, s), got)
		}
	}
}

func TestHostPort_Rejected(t *testing.T) {
	requireRejected(t, hp, []string{
		// AC-4 examples
		"127.0.0.1:09187", "127.0.0.1:+80", ":9187", "0:80", "0x0:80", "127.1:80",
		// port syntax
		"127.0.0.1:00", "127.0.0.1:010", "127.0.0.1:-1", "127.0.0.1:", "127.0.0.1:8 0",
		"127.0.0.1:0x50", "127.0.0.1:1_0", "127.0.0.1:８０", "127.0.0.1:80\n", "127.0.0.1:1e2",
		"127.0.0.1:65536", "127.0.0.1:99999999999999999999", "127.0.0.1:-0", "127.0.0.1:+0",
		// shape
		"", ":", "127.0.0.1", "localhost", "::1:80", "[::1]", "[::1]80", "host:80:90",
		"[]:80", "[::1:80", "::1]:80", "[::1]:", "[host]:80", " 127.0.0.1:80", "127.0.0.1:80 ",
		"127.0.0.1 :80", "http://127.0.0.1:80", "127.0.0.1:80/",
		// hostname rules
		"-a.com:80", "a-.com:80", "a..com:80", ".com:80", ".:80", "exa_mple.com:80",
		"exämple.com:80", "host name:80", "ho$t:80", "a.-b.com:80", "a.b-.com:80",
		label(64, 'a') + ".com:80", // label 64
		host253() + "a" + ":80",    // total 254 (last label 62)
		label(254, 'a') + ":80",
		// numeric and hex-like last labels
		"0x0:80", "0xff:80", "0XFF:80", "a.0x1f:80", "0x:80", "1.2.3.4.5:80", "256.1.1.1:80",
		"999.999.999.999:80", "127.1:80", "1.2.3:80", "12345:80", "a.123:80", "0:0",
	}, types.ErrSyntax, types.ErrRange, types.ErrEmpty, types.ErrControlChar)
}

func TestHostPort_PortBoundaries(t *testing.T) {
	requireAccepted(t, hp, []accept{
		{"127.0.0.1:0", "127.0.0.1:0"},
		{"127.0.0.1:1", "127.0.0.1:1"},
		{"127.0.0.1:65535", "127.0.0.1:65535"},
		{"127.0.0.1:9187", "127.0.0.1:9187"},
	})
	requireRejected(t, hp, []string{"127.0.0.1:65536", "127.0.0.1:70000", "127.0.0.1:100000"},
		types.ErrRange, types.ErrSyntax)
}

func TestHostPort_ControlCharsAndEmptyHost(t *testing.T) {
	for _, in := range []string{"a\x00b:80", "host\x7f:80", "host\u0085:80", "host:\x0080", "\t127.0.0.1:80"} {
		_, err := hp.Parse(in)
		if err == nil {
			t.Errorf("input of length %d accepted", len(in))
		}
	}
}

// SPEC-003 Amendment 1 (P2, AC-4): a trailing dot leaves an empty last label
// and an IPv6 zone is not a plain IP literal; both are rejected.
func TestHostPort_TrailingDotRejected(t *testing.T) {
	requireIs(t, hp, "localhost.:9187", types.ErrSyntax)
	requireRejected(t, hp, []string{"example.com.:80", "a.b.:1", ".:80", "[::1].:80"},
		types.ErrSyntax)
}

func TestHostPort_IPv6ZoneRejected(t *testing.T) {
	requireIs(t, hp, "[fe80::1%eth0]:9187", types.ErrSyntax)
	requireRejected(t, hp, []string{"[fe80::1%25eth0]:80", "[::1%lo]:80", "[fe80::1%]:80", "[::1%1]:0"},
		types.ErrSyntax)
}

// AC-4 accepted examples after Amendment 1.
func TestHostPort_AC4Accepted(t *testing.T) {
	requireAccepted(t, hp, []accept{
		{"localhost:9187", "localhost:9187"},
		{"[::1]:9187", "[::1]:9187"},
	})
}

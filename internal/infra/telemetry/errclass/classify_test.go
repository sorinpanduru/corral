package errclass_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
)

func strs[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	sort.Strings(out)
	return out
}

func equalSets(a, b []string) bool {
	sort.Strings(a)
	sort.Strings(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// FR-29 (Amendment 3, AC-41): the class enum is exactly the seven values.
func TestClassesEnum_FR29(t *testing.T) {
	want := []string{"timeout", "network", "protocol", "server", "config", "internal", "rejected"}
	if got := strs(errclass.Classes()); !equalSets(got, want) {
		t.Fatalf("Classes() = %v, want %v", got, want)
	}
}

// FR-29 (Amendment 3, AC-41): the detail enum is exactly the 28 values.
func TestDetailsEnum_FR29(t *testing.T) {
	want := []string{
		"ECONNREFUSED", "ECONNRESET", "ECONNABORTED", "ETIMEDOUT", "EPIPE",
		"EHOSTUNREACH", "ENETUNREACH", "EADDRINUSE", "deadline_exceeded",
		"canceled", "eof", "tls_handshake", "dns_resolution", "other",
		"pool_wait_timeout", "pool_queue_full", "upstream_unavailable",
		"upstream_auth_failed", "upstream_incompatible", "shutting_down",
		"client_auth_unsupported", "command_blocked", "handshake_rejected",
		"unsupported_opcode", "message_too_large", "malformed_message",
		"checksum_mismatch", "unexpected_response",
	}
	if got := strs(errclass.Details()); !equalSets(got, want) {
		t.Fatalf("Details() = %v, want %v", got, want)
	}
}

type classCase struct {
	name   string
	err    error
	class  string
	detail string
}

func inEnums(t *testing.T, err error) (string, string) {
	t.Helper()
	c, d := errclass.Classify(err)
	foundC, foundD := false, false
	for _, x := range errclass.Classes() {
		foundC = foundC || x == c
	}
	for _, x := range errclass.Details() {
		foundD = foundD || x == d
	}
	if !foundC || !foundD {
		t.Fatalf("Classify returned values outside the enums: %q %q", c, d)
	}
	return string(c), string(d)
}

func runCases(t *testing.T, cases []classCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, d := inEnums(t, tc.err)
			if c != tc.class || d != tc.detail {
				t.Fatalf("Classify = (%s, %s), want (%s, %s)", c, d, tc.class, tc.detail)
			}
			// Wrapping in the chain must not change the result.
			wrapped := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", tc.err))
			c2, d2 := inEnums(t, wrapped)
			if c2 != tc.class || d2 != tc.detail {
				t.Fatalf("wrapped Classify = (%s, %s), want (%s, %s)", c2, d2, tc.class, tc.detail)
			}
		})
	}
}

// AC-35 / FR-29 / D3: errno table, each wrapped like a real net error.
func TestClassify_ErrnoTable_AC35(t *testing.T) {
	op := func(e error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", e)}
	}
	runCases(t, []classCase{
		{"ECONNREFUSED", op(syscall.ECONNREFUSED), "network", "ECONNREFUSED"},
		{"ECONNRESET", op(syscall.ECONNRESET), "network", "ECONNRESET"},
		{"ECONNABORTED", op(syscall.ECONNABORTED), "network", "ECONNABORTED"},
		{"EPIPE", op(syscall.EPIPE), "network", "EPIPE"},
		{"EHOSTUNREACH", op(syscall.EHOSTUNREACH), "network", "EHOSTUNREACH"},
		{"ENETUNREACH", op(syscall.ENETUNREACH), "network", "ENETUNREACH"},
		{"ETIMEDOUT", op(syscall.ETIMEDOUT), "timeout", "ETIMEDOUT"},
	})
}

// AC-35: context, EOF, tls and dns sentinels.
func TestClassify_Sentinels_AC35(t *testing.T) {
	runCases(t, []classCase{
		{"deadline", context.DeadlineExceeded, "timeout", "deadline_exceeded"},
		{"os deadline", os.ErrDeadlineExceeded, "timeout", "deadline_exceeded"},
		{"canceled", context.Canceled, "internal", "canceled"},
		{"eof", io.EOF, "network", "eof"},
		{"unexpected eof", io.ErrUnexpectedEOF, "network", "eof"},
		{"dns not found", &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}, "network", "dns_resolution"},
		{"tls record header", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, "network", "tls_handshake"},
		{"tls cert verify", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "network", "tls_handshake"},
		{"x509 hostname", x509.HostnameError{Host: "x.invalid"}, "network", "tls_handshake"},
		{"unmapped", errors.New("something odd"), "internal", "other"},
	})
}

// FR-29 / D3: a net.Error timeout with no errno or sentinel is exactly
// timeout/ETIMEDOUT.
func TestClassify_NetErrorTimeout(t *testing.T) {
	c, d := inEnums(t, fmt.Errorf("wrap: %w", timeoutErr{msg: "synthetic timeout"}))
	if c != "timeout" || d != "ETIMEDOUT" {
		t.Fatalf("Classify = (%s, %s), want (timeout, ETIMEDOUT)", c, d)
	}
}

// D3: config errors (including the admin bind failure) are class config; the
// detail still comes from the wrapped cause.
func TestClassify_Config_D3(t *testing.T) {
	bind := &fakeConfigErr{setting: "admin.listen", cause: &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}}
	plain := &fakeConfigErr{setting: "log.level", cause: errors.New("bad value")}
	runCases(t, []classCase{
		{"admin bind", bind, "config", "EADDRINUSE"},
		{"plain config", plain, "config", "other"},
	})
}

// D3 / AC-35: protocol errors (wire-parse failures, OTLP collector
// rejections) are class protocol, detail other.
func TestClassify_Protocol_D3(t *testing.T) {
	m := newMarker(t)
	runCases(t, []classCase{
		{"protocol", &fakeProtocolErr{msg: "bad frame " + m}, "protocol", "other"},
	})
	// %w-wrapped inside another error with marker text.
	w := fmt.Errorf("export to endpoint %s: %w", m, &fakeProtocolErr{msg: "rejected"})
	if c, d := inEnums(t, w); c != "protocol" || d != "other" {
		t.Fatalf("wrapped Classify = (%s, %s)", c, d)
	}
	// A protocol error wrapping a network errno keeps class protocol; the
	// detail only has to be an enum value (checked by inEnums).
	perr := &fakeProtocolErr{msg: "reset mid-frame", cause: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}
	if c, _ := inEnums(t, perr); c != "protocol" {
		t.Fatalf("protocol wrapping errno: class = %s, want protocol", c)
	}
}

// D3 / AC-35: a MongoDB server reply is class server, detail other.
func TestClassify_Server_AC35(t *testing.T) {
	m := newMarker(t)
	runCases(t, []classCase{
		{"server", &fakeServerErr{code: 11000, name: "DuplicateKey", msg: "E11000 dup key: " + m}, "server", "other"},
	})
}

// D3: a recovered panic is internal/other, whatever the panic value was.
func TestClassify_Panic_D3(t *testing.T) {
	m := newMarker(t)
	runCases(t, []classCase{
		{"string value", errclass.FromPanic(m, nil), "internal", "other"},
		{"error value is not classified by its content", errclass.FromPanic(syscall.ECONNREFUSED, nil), "internal", "other"},
		{"with stack", errclass.FromPanic(m, []byte("goroutine 1 [running]:\nsynthetic")), "internal", "other"},
	})
}

// AC-35: exporter-style *url.Error whose text carries a userinfo marker.
func TestClassify_URLError_AC35(t *testing.T) {
	m := newMarker(t)
	u := "http://user:" + m + "@collector.invalid:4318/v1/traces"
	runCases(t, []classCase{
		{"refused", &url.Error{Op: "Post", URL: u, Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}, "network", "ECONNREFUSED"},
		{"opaque", &url.Error{Op: "Post", URL: u, Err: errors.New("opaque failure")}, "internal", "other"},
		{"deadline", &url.Error{Op: "Post", URL: u, Err: context.DeadlineExceeded}, "timeout", "deadline_exceeded"},
	})
}

// Real loopback: refused dial (AC-35 ECONNREFUSED).
func TestClassify_RealRefusedDial_AC35(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	_, err = net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		t.Skip("port unexpectedly reopened")
	}
	c, d := inEnums(t, err)
	if c != "network" || d != "ECONNREFUSED" {
		t.Fatalf("Classify = (%s, %s)", c, d)
	}
}

// Real loopback: peer reset (AC-35 ECONNRESET).
func TestClassify_RealReset_AC35(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.(*net.TCPConn).SetLinger(0) // close sends RST
			c.Close()
		}
	}()
	// The RST can race the client's connect, so the reset may surface from
	// Dial or from the first Read. Classify whichever error surfaced.
	var got error
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		got = err
	} else {
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, got = conn.Read(make([]byte, 1))
	}
	if !errors.Is(got, syscall.ECONNRESET) {
		t.Skipf("platform did not surface ECONNRESET (%v)", got)
	}
	c, d := inEnums(t, got)
	if c != "network" || d != "ECONNRESET" {
		t.Fatalf("Classify = (%s, %s)", c, d)
	}
}

// Real loopback: write to a closed connection (AC-35 EPIPE). The peer's
// close can race the connect (Dial may fail with a reset) or a write may see
// ECONNRESET before EPIPE, so each attempt is retried.
func TestClassify_RealBrokenPipe_AC35(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	var werr error
	for attempt := 0; attempt < 20; attempt++ {
		conn, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			continue // reset raced the connect; try again
		}
		werr = nil
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && werr == nil {
			_, werr = conn.Write([]byte("synthetic"))
			time.Sleep(20 * time.Millisecond)
		}
		conn.Close()
		if errors.Is(werr, syscall.EPIPE) {
			break
		}
	}
	if !errors.Is(werr, syscall.EPIPE) {
		t.Skipf("platform did not surface EPIPE (%v)", werr)
	}
	c, d := inEnums(t, werr)
	if c != "network" || d != "EPIPE" {
		t.Fatalf("Classify = (%s, %s)", c, d)
	}
}

// Real loopback: TLS client against a plaintext server (AC-35 tls_handshake).
func TestClassify_RealTLSHandshakeFailure_AC35(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\nplaintext\r\n"))
		time.Sleep(200 * time.Millisecond)
	}()
	raw, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
	err = tls.Client(raw, &tls.Config{ServerName: "x.invalid", MinVersion: tls.VersionTLS12}).Handshake()
	if err == nil {
		t.Fatal("handshake unexpectedly succeeded")
	}
	c, d := inEnums(t, err)
	if c != "network" || d != "tls_handshake" {
		t.Fatalf("Classify = (%s, %s)", c, d)
	}
}

// Real resolver failure with a stub dialer, reserved .invalid name (AC-35).
func TestClassify_RealDNSFailure_AC35(t *testing.T) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, errors.New("stub resolver: no upstream")
		},
	}
	_, err := r.LookupHost(context.Background(), "corral-test.invalid")
	if err == nil {
		t.Fatal("lookup unexpectedly succeeded")
	}
	var de *net.DNSError
	if !errors.As(err, &de) {
		t.Skipf("lookup did not return *net.DNSError: %v", err)
	}
	c, d := inEnums(t, err)
	if c != "network" || d != "dns_resolution" {
		t.Fatalf("Classify = (%s, %s)", c, d)
	}
}

// Real loopback: double listen (AC-35 EADDRINUSE). Raw bind error is
// network/EADDRINUSE; wrapped as a config error it becomes config (D3).
func TestClassify_RealAddrInUse_AC35(t *testing.T) {
	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Close()
	_, err = net.Listen("tcp", l1.Addr().String())
	if err == nil {
		t.Fatal("second listen unexpectedly succeeded")
	}
	if c, d := inEnums(t, err); c != "network" || d != "EADDRINUSE" {
		t.Fatalf("raw Classify = (%s, %s), want (network, EADDRINUSE)", c, d)
	}
	cfg := &fakeConfigErr{setting: "admin.listen", cause: err}
	if c, d := inEnums(t, cfg); c != "config" || d != "EADDRINUSE" {
		t.Fatalf("config-wrapped Classify = (%s, %s)", c, d)
	}
}

// Real context errors surfaced through net (AC-35 canceled, deadline_exceeded).
func TestClassify_RealContextDial_AC35(t *testing.T) {
	var d net.Dialer
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.DialContext(cctx, "tcp", "127.0.0.1:1")
	if err == nil {
		t.Fatal("dial with canceled context succeeded")
	}
	if c, det := inEnums(t, err); c != "internal" || det != "canceled" {
		t.Fatalf("canceled: (%s, %s)", c, det)
	}

	dctx, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	_, err = d.DialContext(dctx, "tcp", "127.0.0.1:1")
	if err == nil {
		t.Fatal("dial with expired context succeeded")
	}
	if c, det := inEnums(t, err); c != "timeout" || det != "deadline_exceeded" {
		t.Fatalf("deadline: (%s, %s)", c, det)
	}
}

// Real conn deadline (os.ErrDeadlineExceeded through net.OpError).
func TestClassify_RealConnDeadline(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(500 * time.Millisecond)
		}
	}()
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	_, err = conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("read unexpectedly succeeded")
	}
	if c, d := inEnums(t, err); c != "timeout" || d != "deadline_exceeded" {
		t.Fatalf("Classify = (%s, %s)", c, d)
	}
}

// FR-45 (TLS handshake errors): an error wrapped by errclass.TLSHandshake is
// network/tls_handshake even when the underlying cause is a reset or an EOF (a
// peer that drops or garbles the ClientHello), while a deadline stays
// timeout/deadline_exceeded.
func TestClassify_TLSHandshakeWrapper_FR45(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		class  errclass.Class
		detail errclass.Detail
	}{
		{"ECONNRESET", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, errclass.ClassNetwork, errclass.DetailTLSHandshake},
		{"EPIPE", &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}, errclass.ClassNetwork, errclass.DetailTLSHandshake},
		{"EOF", io.EOF, errclass.ClassNetwork, errclass.DetailTLSHandshake},
		{"unexpected EOF", io.ErrUnexpectedEOF, errclass.ClassNetwork, errclass.DetailTLSHandshake},
		{"an alert", tls.AlertError(40), errclass.ClassNetwork, errclass.DetailTLSHandshake},
		{"a certificate error", x509.UnknownAuthorityError{}, errclass.ClassNetwork, errclass.DetailTLSHandshake},
		{"context deadline", context.DeadlineExceeded, errclass.ClassTimeout, errclass.DetailDeadlineExceeded},
		{"os deadline", os.ErrDeadlineExceeded, errclass.ClassTimeout, errclass.DetailDeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := errclass.TLSHandshake(tc.err)
			if wrapped == nil {
				t.Fatal("TLSHandshake returned nil for a non-nil error")
			}
			class, detail := errclass.Classify(wrapped)
			if class != tc.class || detail != tc.detail {
				t.Fatalf("Classify = %s/%s, want %s/%s", class, detail, tc.class, tc.detail)
			}
			// The cause is still reachable.
			if !errors.Is(wrapped, tc.err) && tc.name != "a certificate error" {
				t.Fatalf("the wrapper hides its cause")
			}
		})
	}
	if errclass.TLSHandshake(nil) != nil {
		t.Fatal("TLSHandshake(nil) is not nil")
	}
}

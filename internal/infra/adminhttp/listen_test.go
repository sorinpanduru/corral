package adminhttp_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/infra/adminhttp"
	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// AC-11 (FR-16): a configured loopback address is bound there.
func TestListenConfiguredAddress(t *testing.T) {
	r := start(t, nil, nil)
	ta, ok := r.srv.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("Addr() = %T, want *net.TCPAddr", r.srv.Addr())
	}
	if !ta.IP.Equal(net.IPv4(127, 0, 0, 1)) || ta.Port == 0 {
		t.Fatalf("Addr() = %v, want 127.0.0.1:<port>", ta)
	}
}

// AC-11 (FR-16): 0.0.0.0:<port> binds all IPv4 interfaces.
func TestListenAllIPv4(t *testing.T) {
	srv := adminhttp.New(adminhttp.Options{Addr: "0.0.0.0:0", Gatherer: newRegistry(t), ErrorLog: newLogs(t).l})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(t.Context())
	ta := srv.Addr().(*net.TCPAddr)
	// Go may report the IPv4 wildcard as 0.0.0.0 or as the dual-stack [::].
	if !ta.IP.IsUnspecified() {
		t.Fatalf("Addr() = %v, want the wildcard address", ta)
	}
	c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(ta.Port)), 2*time.Second)
	if err != nil {
		t.Fatalf("not reachable over IPv4: %v", err)
	}
	c.Close()
}

// AC-11 (FR-16): no admin.listen configured -> 127.0.0.1:9187 only. The
// default itself is owned by config (T9); here an empty Options.Addr must bind
// it. Skipped when the port is busy on the machine running the tests.
func TestListenDefaultAddress(t *testing.T) {
	if config.Defaults().AdminListen != "127.0.0.1:9187" {
		t.Fatalf("config default = %q", config.Defaults().AdminListen)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:9187")
	if err != nil {
		t.Skip("127.0.0.1:9187 is busy on this machine")
	}
	probe.Close()
	srv := adminhttp.New(adminhttp.Options{Gatherer: newRegistry(t), ErrorLog: newLogs(t).l})
	if err := srv.Listen(); err != nil {
		t.Skipf("default address not bindable here: %v", err)
	}
	defer srv.Shutdown(t.Context())
	if got := srv.Addr().String(); got != "127.0.0.1:9187" {
		t.Fatalf("Addr() = %q, want 127.0.0.1:9187", got)
	}
}

// AC-30 (FR-35, unit): EADDRINUSE is a config.Error for admin.listen, classified
// config/EADDRINUSE, with no address text anywhere in the error or its record.
func TestListenAddrInUse(t *testing.T) {
	busy, err := net.Listen("tcp", loopback)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	addr := busy.Addr().String()
	port := strconv.Itoa(busy.Addr().(*net.TCPAddr).Port)

	srv := adminhttp.New(adminhttp.Options{Addr: addr, Gatherer: newRegistry(t), ErrorLog: newLogs(t).l})
	err = srv.Listen()
	if err == nil {
		t.Fatal("Listen on a busy address succeeded")
	}
	var ce *config.Error
	if !errors.As(err, &ce) || ce.Setting != config.SettingAdminListen {
		t.Fatalf("err = %T %v, want *config.Error for admin.listen", err, err)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("cause lost: errors.Is(err, EADDRINUSE) = false")
	}
	class, detail := errclass.Classify(err)
	if class != errclass.ClassConfig || detail != errclass.DetailEADDRINUSE {
		t.Fatalf("Classify = %s/%s, want config/EADDRINUSE", class, detail)
	}
	for _, s := range []string{err.Error(), ce.Error()} {
		if strings.Contains(s, addr) || strings.Contains(s, port) || strings.Contains(s, "127.0.0.1") {
			t.Fatalf("error text contains the address: %q", s)
		}
	}

	// FR-35: the startup error record has setting=admin.listen and no address.
	lg := newLogs(t)
	cl, e := lg.f.Logger(logging.Config)
	if e != nil {
		t.Fatal(e)
	}
	config.LogError(cl, err)
	recs := lg.lc.Records()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	out := lg.buf.String()
	for _, want := range []string{`"setting":"admin.listen"`, `"component":"config"`, `EADDRINUSE`} {
		if !strings.Contains(out, want) {
			t.Errorf("record lacks %s: %s", want, out)
		}
	}
	if strings.Contains(out, port) || strings.Contains(out, "127.0.0.1") {
		t.Errorf("record contains the address: %s", out)
	}
}

// FR-35: other bind failures are also config errors for admin.listen, never
// echoing the supplied address.
func TestListenOtherFailuresAreConfigErrors(t *testing.T) {
	m := marker(t)
	for _, addr := range []string{m + ":9187", "127.0.0.1:" + m, "127.0.0.1:99999"} {
		srv := adminhttp.New(adminhttp.Options{Addr: addr, Gatherer: newRegistry(t), ErrorLog: newLogs(t).l})
		err := srv.Listen()
		if err == nil {
			t.Errorf("Listen(%q) succeeded", addr)
			continue
		}
		var ce *config.Error
		if !errors.As(err, &ce) || ce.Setting != "admin.listen" {
			t.Errorf("Listen(%q) err = %T, want *config.Error admin.listen", addr, err)
		}
		if c, _ := errclass.Classify(err); c != errclass.ClassConfig {
			t.Errorf("Listen(%q) class = %s, want config", addr, c)
		}
		if strings.Contains(err.Error(), m) || strings.Contains(err.Error(), "99999") {
			t.Errorf("Listen(%q) error echoes the address: %q", addr, err.Error())
		}
	}
}

// FR-28: the http.Server ErrorLog is routed through logbridge. A panic in a
// handler (here the readiness port) makes net/http log through Server.ErrorLog;
// the record must be the bridge's constant one with component=admin_http and
// nothing of the panic text, value or stack.
func TestErrorLogRoutedViaLogbridge(t *testing.T) {
	m := marker(t)
	ready := &fakeReadiness{status: health.Status{Ready: true}, panicV: m}
	r := start(t, ready, nil)
	r.srv.MarkStarted()

	// The connection is dropped by net/http after the panic; the client error
	// is expected and irrelevant. Retry a few times in case the limiter or
	// timing hides the first record.
	deadline := time.Now().Add(5 * time.Second)
	for len(r.logs.lc.Records()) == 0 && time.Now().Before(deadline) {
		resp, err := newClient().Get(r.base + "/readyz")
		if err == nil {
			resp.Body.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	recs := r.logs.lc.Records()
	if len(recs) == 0 {
		t.Fatal("no record reached the logbridge ErrorLog (is Options.ErrorLog set on http.Server?)")
	}
	rec := recs[0]
	if rec.Msg != logbridge.MsgAdminHTTPError || rec.Level != slog.LevelWarn {
		t.Fatalf("record = %v %q, want warn %q", rec.Level, rec.Msg, logbridge.MsgAdminHTTPError)
	}
	out := r.logs.buf.String()
	if !strings.Contains(out, `"component":"admin_http"`) {
		t.Errorf("component != admin_http: %s", out)
	}
	if strings.Contains(out, m) || strings.Contains(out, "goroutine") || strings.Contains(out, ".go:") {
		t.Errorf("third-party text leaked into the record: %s", out)
	}
}

// Defensive: the server keeps running (and stays race-free) when ErrorLog is nil.
func TestNilErrorLogAllowed(t *testing.T) {
	srv := adminhttp.New(adminhttp.Options{Addr: loopback, Gatherer: newRegistry(t)})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	r := &running{srv: srv, base: "http://" + srv.Addr().String()}
	if code, _, body := r.get(t, "/healthz"); code != 200 || body != "ok\n" {
		t.Fatalf("healthz = %d %q", code, body)
	}
	_ = srv.Shutdown(t.Context())
	if err := <-done; !isClosed(err) {
		t.Fatal(err)
	}
}

// FR-15 (slowloris, plan T17): ReadHeaderTimeout is set. A client that sends
// an incomplete header block and then stalls is disconnected within 15 s.
// (The exact value is the implementer's; anything above 15 s is not "sensible"
// for an unauthenticated listener.)
func TestReadHeaderTimeoutSet(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the server's header timeout")
	}
	t.Parallel()
	r := start(t, nil, nil)
	conn, err := net.Dial("tcp", r.srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /healthz HTTP/1.1\r\nHost: x\r\nX-Slow: 1\r\n")); err != nil { // no blank line
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var got bytes.Buffer
	buf := make([]byte, 256)
	for {
		n, err := conn.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("server kept a stalled header connection open for 15 s: ReadHeaderTimeout not set or too large")
			}
			break // EOF or reset: the server closed the connection
		}
	}
}

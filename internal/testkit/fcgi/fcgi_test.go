package fcgi_test

// Unit test for the FastCGI test client. The API below is fixed by this test:
//
//	fcgi.Do(addr string, req fcgi.Request, timeout time.Duration) (*fcgi.Response, error)
//	fcgi.Request{Params map[string]string; Stdin []byte}
//	fcgi.Response{Stdout, Stderr []byte; AppStatus uint32; ProtocolStatus byte}
//	(*fcgi.Response).Body() []byte        // stdout after the CGI header block
//	(*fcgi.Response).StatusCode() int     // from a "Status:" header, default 200
//
// It runs against Go's net/http/fcgi server and a small raw server that emits
// STDERR records and a non-zero application status.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/fcgi"
	"strings"
	"testing"
	"time"

	client "github.com/sorinpanduru/corral/internal/testkit/fcgi"
)

func startGoFCGI(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go fcgi.Serve(ln, h)
	return ln.Addr().String()
}

func baseParams() map[string]string {
	return map[string]string{
		"REQUEST_METHOD":  "GET",
		"SERVER_PROTOCOL": "HTTP/1.1",
		"REQUEST_URI":     "/index.php?a=1",
		"SCRIPT_NAME":     "/index.php",
		"QUERY_STRING":    "a=1",
		"SERVER_NAME":     "localhost",
		"SERVER_PORT":     "80",
	}
}

func TestGetRequest(t *testing.T) {
	addr := startGoFCGI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Reply", "yes")
		fmt.Fprintf(w, "method=%s query=%s xtest=%s", r.Method, r.URL.RawQuery, r.Header.Get("X-Test"))
	}))
	p := baseParams()
	p["HTTP_X_TEST"] = "synthetic"
	resp, err := client.Do(addr, client.Request{Params: p}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Body()); got != "method=GET query=a=1 xtest=synthetic" {
		t.Fatalf("body = %q", got)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("status = %d", resp.StatusCode())
	}
	if !bytes.Contains(resp.Stdout, []byte("X-Reply: yes")) {
		t.Fatal("Stdout lost the response headers")
	}
	if resp.AppStatus != 0 || resp.ProtocolStatus != 0 {
		t.Fatalf("statuses = %d/%d", resp.AppStatus, resp.ProtocolStatus)
	}
}

// STDIN larger than one record (65535 bytes) is split and reassembled; the
// body is echoed back intact.
func TestLargeStdin(t *testing.T) {
	addr := startGoFCGI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%d:%x", len(b), b[len(b)-4:])
	}))
	p := baseParams()
	p["REQUEST_METHOD"] = "POST"
	p["CONTENT_TYPE"] = "application/octet-stream"
	body := bytes.Repeat([]byte{0xAB}, 200_000)
	body = append(body, 1, 2, 3, 4)
	p["CONTENT_LENGTH"] = fmt.Sprint(len(body))
	resp, err := client.Do(addr, client.Request{Params: p, Stdin: body}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Body()); got != fmt.Sprintf("%d:01020304", len(body)) {
		t.Fatalf("body = %q", got)
	}
}

// Parameters larger than one record, and values longer than 127 bytes (the
// 4-byte length encoding), are encoded correctly.
func TestLargeParams(t *testing.T) {
	addr := startGoFCGI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%d|%d", len(r.Header.Get("X-Long")), len(r.Header.Get("X-Short")))
	}))
	p := baseParams()
	p["HTTP_X_LONG"] = strings.Repeat("v", 300)
	p["HTTP_X_SHORT"] = "s"
	for i := 0; i < 2500; i++ {
		p[fmt.Sprintf("HTTP_X_PAD_%04d", i)] = strings.Repeat("p", 40)
	}
	resp, err := client.Do(addr, client.Request{Params: p}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Body()); got != "300|1" {
		t.Fatalf("body = %q", got)
	}
}

// A non-200 status from a CGI header is exposed.
func TestStatusCode(t *testing.T) {
	addr := startGoFCGI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(418)
		w.Write([]byte("teapot"))
	}))
	resp, err := client.Do(addr, client.Request{Params: baseParams()}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 418 || string(resp.Body()) != "teapot" {
		t.Fatalf("status=%d body=%q", resp.StatusCode(), resp.Body())
	}
}

// Raw server: STDOUT, STDERR and END_REQUEST with a non-zero application status.
func rawServer(t *testing.T, handle func(c net.Conn, reqID uint16)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var id uint16
		var sawBegin, sawParamsEnd bool
		for {
			var h [8]byte
			if _, err := io.ReadFull(c, h[:]); err != nil {
				return
			}
			typ := h[1]
			id = binary.BigEndian.Uint16(h[2:])
			n := int(binary.BigEndian.Uint16(h[4:]))
			pad := int(h[6])
			buf := make([]byte, n+pad)
			io.ReadFull(c, buf)
			switch typ {
			case 1:
				sawBegin = true
			case 4:
				if n == 0 {
					sawParamsEnd = true
				}
			case 5:
				if n == 0 && sawBegin && sawParamsEnd {
					handle(c, id)
					return
				}
			}
		}
	}()
	return ln.Addr().String()
}

func rec(c net.Conn, typ byte, id uint16, payload []byte) {
	h := []byte{1, typ, byte(id >> 8), byte(id), byte(len(payload) >> 8), byte(len(payload)), 0, 0}
	c.Write(append(h, payload...))
}

func TestStderrAndAppStatus(t *testing.T) {
	addr := rawServer(t, func(c net.Conn, id uint16) {
		rec(c, 6, id, []byte("Status: 500 Internal Server Error\r\n\r\npart1-"))
		rec(c, 7, id, []byte("warn: synthetic"))
		rec(c, 6, id, []byte("part2"))
		rec(c, 6, id, nil)
		rec(c, 7, id, nil)
		end := make([]byte, 8)
		binary.BigEndian.PutUint32(end, 3)
		rec(c, 3, id, end)
	})
	resp, err := client.Do(addr, client.Request{Params: baseParams()}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body()) != "part1-part2" {
		t.Fatalf("body = %q", resp.Body())
	}
	if string(resp.Stderr) != "warn: synthetic" {
		t.Fatalf("stderr = %q", resp.Stderr)
	}
	if resp.AppStatus != 3 || resp.ProtocolStatus != 0 {
		t.Fatalf("statuses = %d/%d, want 3/0", resp.AppStatus, resp.ProtocolStatus)
	}
	if resp.StatusCode() != 500 {
		t.Fatalf("StatusCode = %d", resp.StatusCode())
	}
}

// A server that never answers: Do returns an error at the timeout. One that
// closes mid-response: Do returns an error, not a partial success.
func TestTimeoutAndTruncation(t *testing.T) {
	quiet := rawServer(t, func(c net.Conn, id uint16) { time.Sleep(2 * time.Second) })
	begin := time.Now()
	if _, err := client.Do(quiet, client.Request{Params: baseParams()}, 300*time.Millisecond); err == nil {
		t.Fatal("no error from a server that never answers")
	}
	if time.Since(begin) > 1500*time.Millisecond {
		t.Fatal("timeout not honoured")
	}
	cut := rawServer(t, func(c net.Conn, id uint16) { rec(c, 6, id, []byte("partial")) })
	if _, err := client.Do(cut, client.Request{Params: baseParams()}, 2*time.Second); err == nil {
		t.Fatal("a response without END_REQUEST was reported as success")
	}
	if _, err := client.Do("127.0.0.1:1", client.Request{Params: baseParams()}, time.Second); err == nil {
		t.Fatal("connect failure not reported")
	}
}

// Several sequential requests on separate connections work (each Do dials).
func TestSequentialRequests(t *testing.T) {
	var n int
	addr := startGoFCGI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		fmt.Fprint(w, n)
	}))
	for i := 1; i <= 3; i++ {
		resp, err := client.Do(addr, client.Request{Params: baseParams()}, 5*time.Second)
		if err != nil || string(resp.Body()) != fmt.Sprint(i) {
			t.Fatalf("request %d: %v %q", i, err, resp.Body())
		}
	}
}

package tracing_test

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing"
)

// Expected API (T15 review round 1, decision D):
//
//	tracing.ClassifyExportError(err error) error
//
// returns err wrapped as an errclass.ProtocolError when it is a collector
// rejection, and otherwise returns err unchanged (nil stays nil). An HTTP
// error is a rejection only if its chain holds neither *url.Error nor
// net.Error; gRPC InvalidArgument and PermissionDenied are rejections, while
// Unavailable, DeadlineExceeded and Canceled are not.

func classOf(err error) errclass.Class {
	c, _ := errclass.Classify(tracing.ClassifyExportError(err))
	return c
}

func opErr(errno syscall.Errno) error {
	return &url.Error{Op: "Post", URL: "http://127.0.0.1:1/v1/traces", Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno),
	}}
}

// ADR-0007 e / D3: a plain non-transport error (what otlptracehttp returns for
// a non-2xx response) becomes protocol/other, and its text (which may hold the
// URL or body) is not retained.
func TestClassifyExportError_PlainHTTPStatusErrorIsProtocol(t *testing.T) {
	m := "MK0123456789abcdef"
	in := errors.New("failed to send to http://u:" + m + "@host/v1/traces: 400 Bad Request")
	out := tracing.ClassifyExportError(in)
	c, d := errclass.Classify(out)
	if c != errclass.ClassProtocol || d != errclass.DetailOther {
		t.Fatalf("classified as %s/%s, want protocol/other", c, d)
	}
	var pe errclass.ProtocolError
	if !errors.As(out, &pe) {
		t.Error("result does not implement errclass.ProtocolError")
	}
	if strings.Contains(out.Error(), m) {
		t.Error("marker from the original error text survives in the wrapper")
	}
}

// D: transport failures stay non-protocol.
func TestClassifyExportError_TransportErrorsStayNonProtocol(t *testing.T) {
	cases := map[string]error{
		"url.Error EADDRNOTAVAIL": opErr(syscall.EADDRNOTAVAIL),
		"url.Error ENETDOWN":      opErr(syscall.ENETDOWN),
		"url.Error EPERM":         opErr(syscall.EPERM),
		"url.Error ECONNREFUSED":  opErr(syscall.ECONNREFUSED),
		"url.Error idle conn":     &url.Error{Op: "Post", URL: "http://x/v1/traces", Err: errors.New("http: server closed idle connection")},
		"net.AddrError":           &net.AddrError{Err: "missing port in address", Addr: "host"},
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			if c := classOf(err); c == errclass.ClassProtocol {
				t.Errorf("classified as protocol, want non-protocol")
			}
		})
	}
}

// D: gRPC statuses.
func TestClassifyExportError_GRPCStatuses(t *testing.T) {
	for _, code := range []codes.Code{codes.InvalidArgument, codes.PermissionDenied} {
		if c := classOf(status.Error(code, "synthetic")); c != errclass.ClassProtocol {
			t.Errorf("%v classified as %s, want protocol", code, c)
		}
	}
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Canceled} {
		if c := classOf(status.Error(code, "synthetic")); c == errclass.ClassProtocol {
			t.Errorf("%v classified as protocol, want non-protocol", code)
		}
	}
}

func TestClassifyExportError_NilStaysNil(t *testing.T) {
	if got := tracing.ClassifyExportError(nil); got != nil {
		t.Errorf("ClassifyExportError(nil) = %v, want nil", got)
	}
}

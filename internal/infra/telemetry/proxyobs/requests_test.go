package proxyobs_test

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/wire"
)

var (
	connReset = &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	timeoutE  = os.ErrDeadlineExceeded
)

func complete(r *rig, cmd string, o proxy.Outcome, err error, d time.Duration, dropped bool) {
	r.ad.Proxy().RequestCompleted(r.ctx, proxy.RequestResult{Command: cmd, Outcome: o, Err: err, Duration: d, Dropped: dropped})
}

// FR-48: an ok request.
func TestRequestOK_FR48(t *testing.T) {
	r := newRig(t)
	r.ad.Proxy().RequestReceived(r.ctx, "find")
	complete(r, "find", proxy.OutcomeOK, nil, 1500*time.Millisecond, false)
	if r.val("corral_requests_total", "command", "find", "outcome", "ok") != 1 {
		t.Fatal("requests_total{find, ok} not incremented")
	}
	n, sum := r.hist("corral_request_duration_seconds", "command", "find")
	if n != 1 || sum < 1.49 || sum > 1.51 {
		t.Fatalf("duration histogram: count=%d sum=%v", n, sum)
	}
	if r.sum("corral_request_errors_total") != 0 || r.val("corral_requests_dropped_total") != 0 {
		t.Fatal("error or dropped counters moved on an ok request")
	}
}

// SPEC-001 FR-12: labels come from the allowlist or other; aliases map.
func TestRequestCommandLabels_FR12(t *testing.T) {
	r := newRig(t)
	for cmd, label := range map[string]string{"insert": "insert", "isMaster": "hello", "ismaster": "hello", "hello": "hello", "GetMore": "other", "weirdCommand": "other", "": "other", "a\xffb": "other"} {
		complete(r, cmd, proxy.OutcomeOK, nil, time.Millisecond, false)
		if r.val("corral_requests_total", "command", label, "outcome", "ok") < 1 {
			t.Errorf("command %q not counted under %q", cmd, label)
		}
	}
}

// FR-48 (G-4): server_error requests are not counted in request_errors_total.
func TestServerErrorNotInRequestErrors_FR48(t *testing.T) {
	r := newRig(t)
	complete(r, "insert", proxy.OutcomeServerError, nil, time.Millisecond, false)
	if r.val("corral_requests_total", "command", "insert", "outcome", "server_error") != 1 {
		t.Fatal("not counted as server_error")
	}
	if r.sum("corral_request_errors_total") != 0 {
		t.Fatal("a server_error request reached request_errors_total")
	}
}

// FR-48: every synthesized refusal counts once with its class.
func TestSynthesizedRefusalClasses_FR48(t *testing.T) {
	for _, ref := range []proxy.Refusal{proxy.PoolWaitTimeout, proxy.PoolQueueFull, proxy.UpstreamUnavailable, proxy.UpstreamAuthFailed, proxy.UpstreamIncompatible,
		proxy.ShuttingDown, proxy.ClientAuthSASL, proxy.ClientAuthOther, proxy.CommandBlocked, proxy.UnsupportedOpQuery, proxy.MetadataRepeated, proxy.LoadBalanced} {
		r := newRig(t)
		complete(r, "find", proxy.OutcomeProxyError, ref, time.Millisecond, false)
		if r.val("corral_requests_total", "command", "find", "outcome", "proxy_error") != 1 {
			t.Fatalf("%s: not a proxy_error request", ref.Detail())
		}
		if r.val("corral_request_errors_total", "error_class", ref.Class()) != 1 || r.sum("corral_request_errors_total") != 1 {
			t.Fatalf("%s: request_errors_total{%s} = %v (sum %v)", ref.Detail(), ref.Class(), r.val("corral_request_errors_total", "error_class", ref.Class()), r.sum("corral_request_errors_total"))
		}
	}
}

// AC-38 (metric part): the 352 refusal is a proxy_error request of class
// protocol and is not counted in protocol_errors_total.
func TestOpQueryRefusalNotAProtocolError_AC38(t *testing.T) {
	r := newRig(t)
	complete(r, "saslStart", proxy.OutcomeProxyError, proxy.UnsupportedOpQuery, time.Millisecond, false)
	if r.val("corral_requests_total", "command", "other", "outcome", "proxy_error")+r.val("corral_requests_total", "command", "saslStart", "outcome", "proxy_error") < 1 {
		t.Fatal("352 refusal not counted as a proxy_error request")
	}
	if r.val("corral_request_errors_total", "error_class", "protocol") != 1 {
		t.Fatal("352 refusal not counted with class protocol")
	}
	if r.sum("corral_protocol_errors_total") != 0 {
		t.Fatal("the 352 refusal reached protocol_errors_total")
	}
	// The label for a wrapped query comes from the first key (mapped by FR-12).
	r2 := newRig(t)
	complete(r2, "find", proxy.OutcomeProxyError, proxy.UnsupportedOpQuery, time.Millisecond, false)
	if r2.val("corral_requests_total", "command", "find", "outcome", "proxy_error") != 1 {
		t.Fatal("352 refusal of find mislabelled")
	}
}

// AC-49 (metric part): an HTTP GET close has command other; a late aggregate
// failure has command aggregate. Both count once with class protocol.
func TestProtocolCloseCommandLabels_AC49(t *testing.T) {
	r := newRig(t)
	// HTTP GET: no command read.
	r.ad.Proxy().ClientProtocolError(r.ctx, 1, wire.ErrMessageTooLarge())
	complete(r, "", proxy.OutcomeProxyError, wire.ErrMessageTooLarge(), 3*time.Millisecond, false)
	// Late aggregate failure.
	r.ad.Proxy().ClientProtocolError(r.ctx, 2, wire.ErrMalformed())
	complete(r, "aggregate", proxy.OutcomeProxyError, wire.ErrMalformed(), 9*time.Millisecond, false)
	if r.val("corral_requests_total", "command", "other", "outcome", "proxy_error") != 1 || r.val("corral_requests_total", "command", "aggregate", "outcome", "proxy_error") != 1 {
		t.Fatal("labels wrong for the two protocol closes")
	}
	if r.val("corral_request_errors_total", "error_class", "protocol") != 2 {
		t.Fatalf("request_errors_total{protocol} = %v, want 2", r.val("corral_request_errors_total", "error_class", "protocol"))
	}
	// Each close is counted once in protocol_errors_total, from the
	// ClientProtocolError call only (RequestCompleted must not add to it).
	if r.val("corral_protocol_errors_total", "side", "client", "error_detail", "message_too_large") != 1 ||
		r.val("corral_protocol_errors_total", "side", "client", "error_detail", "malformed_message") != 1 || r.sum("corral_protocol_errors_total") != 2 {
		t.Fatalf("protocol_errors_total = %v", r.sum("corral_protocol_errors_total"))
	}
	// Durations are observed once each.
	if r.histCountAll("corral_request_duration_seconds") != 2 {
		t.Fatalf("duration observations = %d, want 2", r.histCountAll("corral_request_duration_seconds"))
	}
}

// I/O timeout closes: class timeout, one request.
func TestIOTimeoutRequest_FR57(t *testing.T) {
	r := newRig(t)
	r.ad.Proxy().ClientIOTimeout(r.ctx, 1)
	complete(r, "find", proxy.OutcomeProxyError, timeoutE, 30*time.Second, false)
	if r.val("corral_request_errors_total", "error_class", "timeout") != 1 || r.val("corral_requests_total", "command", "find", "outcome", "proxy_error") != 1 {
		t.Fatal("I/O timeout request not counted")
	}
	if r.sum("corral_protocol_errors_total") != 0 {
		t.Fatal("an I/O timeout is not a protocol error")
	}
}

// R-3: a client closing mid-message is a proxy_error request of class network,
// not a protocol error, observed once.
func TestClientClosesMidMessage_R3(t *testing.T) {
	r := newRig(t)
	complete(r, "insert", proxy.OutcomeProxyError, connReset, 12*time.Millisecond, false)
	if r.val("corral_request_errors_total", "error_class", "network") != 1 || r.sum("corral_protocol_errors_total") != 0 {
		t.Fatal("mid-message close accounting wrong")
	}
	if n, _ := r.hist("corral_request_duration_seconds", "command", "insert"); n != 1 {
		t.Fatalf("duration observations = %d", n)
	}
}

// FR-59: dropped moreToCome messages.
func TestDroppedMoreToCome_FR59(t *testing.T) {
	r := newRig(t)
	complete(r, "insert", proxy.OutcomeProxyError, proxy.UpstreamUnavailable, time.Millisecond, true)
	complete(r, "insert", proxy.OutcomeOK, nil, time.Millisecond, false)
	if r.val("corral_requests_dropped_total") != 1 {
		t.Fatalf("requests_dropped_total = %v", r.val("corral_requests_dropped_total"))
	}
	if r.sum("corral_requests_total") != 2 {
		t.Fatalf("requests_total = %v", r.sum("corral_requests_total"))
	}
}

// AC-39 (unit): over a mixed sequence, the sum of request_errors_total equals
// the sum of requests_total{outcome=proxy_error}; server_error is excluded;
// every request is observed once in the duration histogram.
func TestRequestErrorsEqualProxyErrors_AC39(t *testing.T) {
	r := newRig(t)
	type step struct {
		cmd   string
		o     proxy.Outcome
		err   error
		close error // a ClientProtocolError call preceding the completion
	}
	var seq []step
	for _, ref := range []proxy.Refusal{proxy.PoolWaitTimeout, proxy.PoolQueueFull, proxy.UpstreamUnavailable, proxy.ShuttingDown, proxy.CommandBlocked, proxy.ClientAuthSASL} {
		seq = append(seq, step{"find", proxy.OutcomeProxyError, ref, nil}, step{"insert", proxy.OutcomeProxyError, ref, nil})
	}
	seq = append(seq,
		step{"", proxy.OutcomeProxyError, wire.ErrMessageTooLarge(), wire.ErrMessageTooLarge()},
		step{"aggregate", proxy.OutcomeProxyError, wire.ErrMalformed(), wire.ErrMalformed()},
		step{"find", proxy.OutcomeProxyError, timeoutE, nil},
		step{"insert", proxy.OutcomeProxyError, connReset, nil},
		step{"find", proxy.OutcomeProxyError, proxy.UnsupportedOpQuery, nil},
		step{"saslStart", proxy.OutcomeProxyError, proxy.UnsupportedOpQuery, nil},
		step{"find", proxy.OutcomeServerError, nil, nil},
		step{"insert", proxy.OutcomeServerError, nil, nil},
		step{"find", proxy.OutcomeOK, nil, nil},
		step{"update", proxy.OutcomeOK, nil, nil},
	)
	protocolCloses := 0
	for i, s := range seq {
		if s.close != nil {
			r.ad.Proxy().ClientProtocolError(r.ctx, int32(i+1), s.close)
			protocolCloses++
		}
		complete(r, s.cmd, s.o, s.err, time.Duration(i+1)*time.Millisecond, false)
	}
	perr := r.sum("corral_requests_total", "outcome", "proxy_error")
	if got := r.sum("corral_request_errors_total"); got != perr || perr == 0 {
		t.Fatalf("sum(request_errors_total)=%v, sum(requests_total{proxy_error})=%v", got, perr)
	}
	if got := r.sum("corral_requests_total"); got != float64(len(seq)) {
		t.Fatalf("requests_total = %v, want %d", got, len(seq))
	}
	if got := r.histCountAll("corral_request_duration_seconds"); got != uint64(len(seq)) {
		t.Fatalf("duration observations = %d, want %d", got, len(seq))
	}
	if got := r.sum("corral_protocol_errors_total", "side", "client"); got != float64(protocolCloses) {
		t.Fatalf("protocol_errors_total{client} = %v, want %d (352 refusals and others excluded)", got, protocolCloses)
	}
	if r.val("corral_request_errors_total", "error_class", "protocol") < 4 {
		t.Fatal("protocol class undercounted")
	}
	if got := r.sum("corral_requests_total", "outcome", "server_error"); got != 2 {
		t.Fatalf("server_error requests = %v", got)
	}
	_ = fmt.Sprint
}

// FR-48: observations land in the histogram of the command's label.
func TestDurationLabelFollowsCommand_FR48(t *testing.T) {
	r := newRig(t)
	complete(r, "isMaster", proxy.OutcomeOK, nil, 2*time.Second, false)
	complete(r, "mystery", proxy.OutcomeOK, nil, 4*time.Second, false)
	if n, s := r.hist("corral_request_duration_seconds", "command", "hello"); n != 1 || s < 1.99 || s > 2.01 {
		t.Fatalf("hello: %d %v", n, s)
	}
	if n, s := r.hist("corral_request_duration_seconds", "command", "other"); n != 1 || s < 3.99 || s > 4.01 {
		t.Fatalf("other: %d %v", n, s)
	}
}

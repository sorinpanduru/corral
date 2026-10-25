package proxy_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/wire"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/spans"
)

type row struct {
	r      proxy.Refusal
	code   int32
	name   string
	detail string
	class  string
	labels bool
}

// The synthesized-error table of the spec, row by row.
var table = []row{
	{proxy.PoolWaitTimeout, 262, "ExceededTimeLimit", "pool_wait_timeout", "timeout", true},
	{proxy.PoolQueueFull, 262, "ExceededTimeLimit", "pool_queue_full", "rejected", true},
	{proxy.UpstreamUnavailable, 6, "HostUnreachable", "upstream_unavailable", "network", true},
	{proxy.UpstreamAuthFailed, 6, "HostUnreachable", "upstream_auth_failed", "server", true},
	{proxy.UpstreamIncompatible, 6, "HostUnreachable", "upstream_incompatible", "config", true},
	{proxy.ShuttingDown, 91, "ShutdownInProgress", "shutting_down", "rejected", true},
	{proxy.ClientAuthSASL, 18, "AuthenticationFailed", "client_auth_unsupported", "rejected", false},
	{proxy.ClientAuthOther, 115, "CommandNotSupported", "client_auth_unsupported", "rejected", false},
	{proxy.CommandBlocked, 13, "Unauthorized", "command_blocked", "rejected", false},
	{proxy.UnsupportedOpQuery, 352, "UnsupportedOpQueryCommand", "unsupported_opcode", "protocol", false},
	{proxy.MetadataRepeated, 186, "ClientMetadataCannotBeMutated", "handshake_rejected", "rejected", false},
	{proxy.LoadBalanced, 72, "InvalidOptions", "handshake_rejected", "rejected", false},
}

func TestRefusalTable_FR39(t *testing.T) {
	if len(table) != 12 {
		t.Fatalf("oracle has %d rows", len(table))
	}
	for _, w := range table {
		t.Run(w.detail+"/"+fmt.Sprint(w.code), func(t *testing.T) {
			r := w.r
			if r.Code() != w.code || r.CodeName() != w.name || r.Detail() != w.detail || r.Class() != w.class {
				t.Fatalf("code=%d name=%q detail=%q class=%q, want %d %q %q %q", r.Code(), r.CodeName(), r.Detail(), r.Class(), w.code, w.name, w.detail, w.class)
			}
			if r.CorralErrorDetail() != w.detail {
				t.Errorf("CorralErrorDetail = %q", r.CorralErrorDetail())
			}
			msg := r.Errmsg()
			if strings.TrimSpace(msg) == "" {
				t.Error("empty errmsg")
			}
			// Constant text: fixed across calls (no runtime data).
			if r.Errmsg() != msg || r.Error() == "" || r.Error() != r.Error() {
				t.Error("errmsg or error text not constant")
			}
		})
	}
}

// Error() text is the same constant for equal refusals and never contains
// credentials or hosts.
func TestRefusalErrorsAreConstant_FR24(t *testing.T) {
	for _, w := range table {
		if strings.Contains(strings.ToLower(w.r.Error()+w.r.Errmsg()), "password") {
			t.Errorf("%s mentions a password", w.detail)
		}
	}
	var e error = proxy.CommandBlocked
	if e.Error() != proxy.CommandBlocked.Error() || detailOf(e) != "command_blocked" {
		t.Fatal("Refusal does not behave as an error with a detail")
	}
}

// RefusalForDetail maps the pool and upstream details, and nothing else.
func TestRefusalForDetail_FR56(t *testing.T) {
	for _, d := range []string{"pool_wait_timeout", "pool_queue_full", "upstream_unavailable", "upstream_auth_failed", "upstream_incompatible", "shutting_down"} {
		r, ok := proxy.RefusalForDetail(d)
		if !ok || r.Detail() != d {
			t.Errorf("RefusalForDetail(%q) = %v, %v", d, r.Detail(), ok)
		}
	}
	for _, d := range []string{"", "other", "canceled", "client_auth_unsupported", "command_blocked", "malformed_message", "POOL_WAIT_TIMEOUT", "pool_wait_timeout "} {
		if _, ok := proxy.RefusalForDetail(d); ok {
			t.Errorf("RefusalForDetail(%q) ok", d)
		}
	}
}

func req(command string, mutate func(*wire.Request)) wire.Request {
	r := msg(command, 0)
	if mutate != nil {
		mutate(&r)
	}
	return r
}

// FR-39, AC-13 (unit): error labels for codes 262, 6 and 91 only.
func TestLabels_AC13(t *testing.T) {
	retryable := req("insert", func(r *wire.Request) { r.HasTxnNumber = true })
	commit := req("commitTransaction", func(r *wire.Request) { r.HasTxnNumber = true; r.AutocommitFalse = true })
	abort := req("abortTransaction", func(r *wire.Request) { r.HasTxnNumber = true; r.AutocommitFalse = true })
	commitNoTxn := req("commitTransaction", nil)
	inTxn := req("find", func(r *wire.Request) { r.AutocommitFalse = true })
	inTxnWrite := req("insert", func(r *wire.Request) { r.HasTxnNumber = true; r.AutocommitFalse = true })
	plain := req("find", nil)
	plainWrite := req("insert", nil)

	cases := []struct {
		name string
		req  wire.Request
		want []string
	}{
		{"retryable write", retryable, []string{"RetryableWriteError"}},
		{"commitTransaction", commit, []string{"RetryableWriteError"}},
		{"abortTransaction", abort, []string{"RetryableWriteError"}},
		{"commitTransaction without txnNumber", commitNoTxn, []string{"RetryableWriteError"}},
		{"read inside a transaction", inTxn, []string{"TransientTransactionError"}},
		{"write inside a transaction", inTxnWrite, []string{"TransientTransactionError"}},
		{"plain read", plain, nil},
		{"write without txnNumber", plainWrite, nil},
	}
	for _, r := range []proxy.Refusal{proxy.PoolWaitTimeout, proxy.PoolQueueFull, proxy.UpstreamUnavailable, proxy.UpstreamAuthFailed, proxy.UpstreamIncompatible, proxy.ShuttingDown} {
		for _, tc := range cases {
			got := proxy.Labels(r, tc.req)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("%s / %s: labels %v, want %v", r.Detail(), tc.name, got, tc.want)
			}
		}
	}
	// Any other code never gets labels, whatever the request looks like.
	for _, r := range []proxy.Refusal{proxy.ClientAuthSASL, proxy.ClientAuthOther, proxy.CommandBlocked, proxy.UnsupportedOpQuery, proxy.MetadataRepeated, proxy.LoadBalanced} {
		for _, tc := range cases {
			if got := proxy.Labels(r, tc.req); len(got) != 0 {
				t.Errorf("%s / %s: labels %v, want none", r.Detail(), tc.name, got)
			}
		}
	}
}

// The reply body: ok 0, code, codeName, errmsg, and errorLabels only when
// there are labels.
func TestErrorBody_FR39(t *testing.T) {
	for _, w := range table {
		t.Run(w.detail, func(t *testing.T) {
			var labels []string
			if w.labels {
				labels = []string{"RetryableWriteError"}
			}
			d := bson.Raw(proxy.ErrorBody(w.r, labels))
			if err := d.Validate(); err != nil {
				t.Fatal(err)
			}
			if d.Lookup("ok").AsFloat64() != 0 || d.Lookup("code").AsInt64() != int64(w.code) || d.Lookup("codeName").StringValue() != w.name {
				t.Fatal("ok/code/codeName wrong")
			}
			if d.Lookup("errmsg").StringValue() != w.r.Errmsg() {
				t.Fatal("errmsg differs from Refusal.Errmsg()")
			}
			want := []string{"code", "codeName", "errmsg", "ok"}
			if w.labels {
				want = append(want, "errorLabels")
				arr, ok := d.Lookup("errorLabels").ArrayOK()
				if !ok {
					t.Fatal("errorLabels is not an array")
				}
				vals, _ := arr.Values()
				if len(vals) != 1 || vals[0].StringValue() != "RetryableWriteError" {
					t.Fatalf("errorLabels = %v", vals)
				}
			}
			if got := keys(t, d); !equalKeys(got, want) {
				t.Fatalf("keys = %v, want %v", got, want)
			}
			for _, k := range []string{"db.response.status_code", "codeName2", "$clusterTime"} {
				if _, err := d.LookupErr(k); err == nil {
					t.Errorf("unexpected field %q", k)
				}
			}
		})
	}
	// No labels: no errorLabels field, also when the slice is empty.
	for _, l := range [][]string{nil, {}} {
		d := bson.Raw(proxy.ErrorBody(proxy.PoolWaitTimeout, l))
		if _, err := d.LookupErr("errorLabels"); err == nil {
			t.Error("errorLabels present with no labels")
		}
	}
	// Both labels can be carried.
	d := bson.Raw(proxy.ErrorBody(proxy.PoolWaitTimeout, []string{"TransientTransactionError", "RetryableWriteError"}))
	if vals, _ := d.Lookup("errorLabels").Array().Values(); len(vals) != 2 {
		t.Fatalf("labels = %d", len(vals))
	}
}

// FR-8: the OP_QUERY failure document.
func TestQueryFailureDoc_FR8(t *testing.T) {
	d := bson.Raw(proxy.QueryFailureDoc())
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if d.Lookup("code").AsInt64() != 352 || d.Lookup("codeName").StringValue() != "UnsupportedOpQueryCommand" || d.Lookup("ok").AsFloat64() != 0 {
		t.Fatal("code/codeName/ok wrong")
	}
	if s := d.Lookup("$err").StringValue(); strings.TrimSpace(s) == "" {
		t.Fatal("empty $err")
	}
	if got := keys(t, d); !equalKeys(got, []string{"$err", "code", "codeName", "ok"}) {
		t.Fatalf("keys = %v", got)
	}
}

// FR-48: request outcome from reply fields.
func TestOutcomeForReply_FR48(t *testing.T) {
	cases := []struct {
		name string
		f    wire.ReplyFields
		want proxy.Outcome
		str  string
	}{
		{"ok 1", wire.ReplyFields{HasOK: true, OK: 1}, proxy.OutcomeOK, "ok"},
		{"ok 0", wire.ReplyFields{HasOK: true, OK: 0, HasCode: true, Code: 11000}, proxy.OutcomeServerError, "server_error"},
		{"ok 1 with writeConcernError", wire.ReplyFields{HasOK: true, OK: 1, HasWCECode: true, WCECode: 64}, proxy.OutcomeOK, "ok"},
		{"ok 1 with cursor", wire.ReplyFields{HasOK: true, OK: 1, HasCursorID: true, CursorID: 5}, proxy.OutcomeOK, "ok"},
	}
	for _, tc := range cases {
		got := proxy.OutcomeForReply(tc.f)
		if got != tc.want || got.String() != tc.str {
			t.Errorf("%s: %v (%q), want %q", tc.name, got, got.String(), tc.str)
		}
	}
	if proxy.OutcomeProxyError.String() != "proxy_error" {
		t.Errorf("proxy_error text = %q", proxy.OutcomeProxyError.String())
	}
}

// Review: proxy.OutcomeForReply and spans-side error detection use one ok:0
// predicate. For every reply shape, a reply is a server_error exactly when it is
// an upstream error, and the two agree.
func TestOutcomeAndReplyErrorAgree_FR48(t *testing.T) {
	nan := math.NaN()
	cases := []wire.ReplyFields{
		{}, {HasOK: true, OK: 1}, {HasOK: true, OK: 0}, {HasOK: true, OK: 0, HasCode: true, Code: 11000},
		{HasOK: true, OK: 0.5}, {HasOK: true, OK: 2}, {HasOK: true, OK: -1}, {HasOK: true, OK: nan},
		{HasOK: true, OK: math.Inf(1)}, {HasOK: true, OK: math.Copysign(0, -1)},
		{HasOK: true, OK: 1, HasCode: true, Code: 10107}, {HasOK: true, OK: 1, HasWCECode: true, WCECode: 91},
		{HasOK: false, OK: 0, HasCode: true, Code: 13}, {HasOK: false, OK: 1},
	}
	for i, f := range cases {
		isErr := proxy.OutcomeForReply(f) == proxy.OutcomeServerError
		if got := spans.ReplyError(f) != nil; got != isErr {
			t.Errorf("case %d (%+v): OutcomeForReply server_error=%v, spans.ReplyError non-nil=%v", i, f, isErr, got)
		}
	}
}

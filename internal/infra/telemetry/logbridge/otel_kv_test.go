package logbridge_test

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"testing"

	"github.com/go-logr/logr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
)

// T24.4 (FR-28, FR-24, FR-29): the OTel SDK's envconfig calls
// global.Error(err, "read tls client cert", "file", <path>) when a cert file
// becomes unreadable after validation. The sink must forward neither the
// message nor the key/values, and the error only as FR-29 class and detail
// (the *os.PathError text contains the path). Synthetic markers only.

func pathErr(m string) error {
	return &os.PathError{Op: "open", Path: "/synthetic/" + m + "/client.pem", Err: fs.ErrPermission}
}

type kvCall struct {
	name string
	call func(lg logr.Logger, m string)
}

func kvCalls() []kvCall {
	return []kvCall{
		{"error-with-kv", func(lg logr.Logger, m string) {
			lg.Error(pathErr(m), "read tls client cert "+m, "file", "/synthetic/"+m+"/client.pem", "other", m)
		}},
		{"error-ca", func(lg logr.Logger, m string) {
			lg.Error(pathErr(m), "read tls ca cert "+m, "file", "/synthetic/"+m+"/ca.pem")
		}},
		{"error-key", func(lg logr.Logger, m string) {
			lg.Error(pathErr(m), "read tls client key "+m, "file", "/synthetic/"+m+"/key.pem")
		}},
		{"error-chain", func(lg logr.Logger, m string) {
			lg.WithName("otel-"+m).WithValues("file", m).WithName(m).WithValues("k", pathErr(m)).
				Error(pathErr(m), m, "file", m)
		}},
		{"error-nonstring-kv", func(lg logr.Logger, m string) {
			lg.Error(pathErr(m), m, 42, pathErr(m), "stringer", fmt.Sprint(m), "odd")
		}},
		{"error-nil", func(lg logr.Logger, m string) {
			lg.Error(nil, m, "file", m)
		}},
	}
}

func TestT24_OTelLogSinkErrorNeverEmitsKV(t *testing.T) {
	for _, c := range kvCalls() {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, slog.LevelDebug)
			m := marker(t)
			c.call(logr.New(logbridge.NewOTelLogSink(e.f, e.clk.Now)), m)
			r := e.only()
			expect(t, r, slog.LevelWarn, "otel", 0)
			if r.Msg != logbridge.MsgOTelLogWarn {
				t.Errorf("msg = %q, want fixed %q", r.Msg, logbridge.MsgOTelLogWarn)
			}
			e.noMarker(m)
			sc := markerscan.New()
			sc.AddMarker(t, m)
			sc.Add(markerscan.SourceStdout, []byte(e.buf.String()))
			sc.AssertClean(t)
		})
	}
}

// The error is carried only as the FR-29 class and detail.
func TestT24_OTelLogSinkErrorCarriesClassAndDetailOnly(t *testing.T) {
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	err := pathErr(m)
	logr.New(logbridge.NewOTelLogSink(e.f, e.clk.Now)).Error(err, m, "file", m)
	r := e.only()
	want := map[string]string{}
	for _, a := range errclass.LogAttrs(err) {
		want[a.Key] = a.Value.String()
	}
	if len(want) < 2 {
		t.Fatalf("test premise: errclass attrs = %v", want)
	}
	for k, v := range want {
		if got, ok := r.Attrs[k]; !ok || got.String() != v {
			t.Errorf("attr %q = %v (present=%v), want %q", k, got, ok, v)
		}
	}
	allowed := map[string]bool{"component": true, "suppressed_count": true}
	for k := range want {
		allowed[k] = true
	}
	for k := range r.Attrs {
		if !allowed[k] {
			t.Errorf("unexpected attribute %q: only class/detail, component and suppressed_count allowed", k)
		}
	}
	e.noMarker(m)
}

func TestT24_OTelLogSinkInfoNeverEmitsKV(t *testing.T) {
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	lg := logr.New(logbridge.NewOTelLogSink(e.f, e.clk.Now))
	lg.WithName(m).WithValues("file", m).V(2).Info("read tls client cert "+m, "file", "/synthetic/"+m, "err", pathErr(m))
	r := e.only()
	expect(t, r, slog.LevelDebug, "otel", 0)
	if r.Msg != logbridge.MsgOTelLogDebug {
		t.Errorf("msg = %q, want fixed %q", r.Msg, logbridge.MsgOTelLogDebug)
	}
	for k := range r.Attrs {
		if k != "component" && k != "suppressed_count" {
			t.Errorf("unexpected attribute %q on info record", k)
		}
	}
	e.noMarker(m)
}

// The message is one constant per level whatever the arguments.
func TestT24_OTelLogSinkMessagesConstantAcrossCalls(t *testing.T) {
	e := newEnv(t, slog.LevelDebug)
	lg := logr.New(logbridge.NewOTelLogSink(e.f, e.clk.Now))
	lg.Error(errors.New("a"), "first "+marker(t), "k", marker(t))
	lg.Info("second "+marker(t), "k", marker(t))
	rs := e.records()
	if len(rs) != 2 || rs[0].Msg != logbridge.MsgOTelLogWarn || rs[1].Msg != logbridge.MsgOTelLogDebug {
		t.Fatalf("records = %+v", rs)
	}
}

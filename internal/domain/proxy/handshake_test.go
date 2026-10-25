package proxy_test

import (
	"sort"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/domain/wire"
)

func keys(t *testing.T, doc []byte) []string {
	t.Helper()
	els, err := bson.Raw(doc).Elements()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range els {
		out = append(out, e.Key())
	}
	sort.Strings(out)
	return out
}

func reply(t *testing.T, h wire.Handshake, p upstream.Params, id int32) bson.Raw {
	t.Helper()
	d := proxy.HandshakeReply(h, p, id, t0)
	if err := bson.Raw(d).Validate(); err != nil {
		t.Fatalf("reply is not valid BSON: %v", err)
	}
	return d
}

func equalKeys(a, b []string) bool {
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

// FR-14, AC-9 (unit): the handshake reply has exactly these fields.
func TestHandshakeReplyFields_AC9(t *testing.T) {
	common := []string{"msg", "maxBsonObjectSize", "maxMessageSizeBytes", "maxWriteBatchSize", "logicalSessionTimeoutMinutes",
		"minWireVersion", "maxWireVersion", "localTime", "connectionId", "readOnly", "ok"}
	cases := []struct {
		name  string
		h     wire.Handshake
		extra []string
	}{
		{"hello", wire.Handshake{Name: "hello"}, []string{"isWritablePrimary"}},
		{"hello with helloOk", wire.Handshake{Name: "hello", HelloOK: true}, []string{"isWritablePrimary", "helloOk"}},
		{"isMaster", wire.Handshake{Name: "isMaster"}, []string{"ismaster"}},
		{"ismaster", wire.Handshake{Name: "ismaster"}, []string{"ismaster"}},
		{"isMaster with helloOk", wire.Handshake{Name: "isMaster", HelloOK: true}, []string{"ismaster", "helloOk"}},
		{"hello over OP_QUERY", wire.Handshake{Name: "hello", ViaOpQuery: true}, []string{"isWritablePrimary"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := reply(t, tc.h, params(), 77)
			want := append(append([]string(nil), common...), tc.extra...)
			if got := keys(t, d); !equalKeys(got, want) {
				t.Fatalf("keys = %v, want %v", got, want)
			}
			r := bson.Raw(d)
			if v := r.Lookup("msg"); v.StringValue() != "isdbgrid" {
				t.Error("msg is not isdbgrid")
			}
			for _, k := range []string{"isWritablePrimary", "ismaster"} {
				if v, err := r.LookupErr(k); err == nil && !v.Boolean() {
					t.Errorf("%s is not true", k)
				}
			}
			if v, err := r.LookupErr("helloOk"); err == nil && !v.Boolean() {
				t.Error("helloOk is not true")
			}
			if v := r.Lookup("readOnly"); v.Boolean() {
				t.Error("readOnly is true")
			}
			if v := r.Lookup("ok"); v.AsFloat64() != 1 {
				t.Error("ok is not 1")
			}
			if v := r.Lookup("connectionId"); v.AsInt64() != 77 {
				t.Errorf("connectionId = %d", v.AsInt64())
			}
			if v := r.Lookup("localTime"); v.Time().UnixMilli() != t0.UnixMilli() {
				t.Errorf("localTime = %v, want %v", v.Time(), t0)
			}
			p := params()
			for k, want := range map[string]int64{"maxBsonObjectSize": int64(p.MaxBsonObjectSize), "maxMessageSizeBytes": int64(p.MaxMessageSizeBytes),
				"maxWriteBatchSize": int64(p.MaxWriteBatchSize), "logicalSessionTimeoutMinutes": int64(p.LogicalSessionTimeoutMinutes)} {
				if got := r.Lookup(k).AsInt64(); got != want {
					t.Errorf("%s = %d, want %d", k, got, want)
				}
			}
		})
	}
}

// FR-14: none of the forbidden fields, whatever the client asked for.
func TestHandshakeReplyForbiddenFields_AC9(t *testing.T) {
	h := wire.Handshake{Name: "hello", HelloOK: true, HasTopologyVersion: true, HasMaxAwaitTimeMS: true, HasClient: true,
		HasCompression: true, HasSpeculativeAuth: true, HasSaslSupportedMechs: true, AppName: "a", DriverName: "d", DriverVersion: "v"}
	d := reply(t, h, params(), 1)
	for _, k := range []string{"topologyVersion", "compression", "saslSupportedMechs", "speculativeAuthenticate", "setName",
		"hosts", "serviceId", "$clusterTime", "primary", "me", "electionId", "setVersion", "arbiters", "passives", "secondary"} {
		if _, err := bson.Raw(d).LookupErr(k); err == nil {
			t.Errorf("forbidden field %q present", k)
		}
	}
}

// FR-14, AC-10 (unit): minWireVersion is forwarded unchanged, maxWireVersion is
// capped at 25.
func TestHandshakeWireVersions_AC10(t *testing.T) {
	cases := []struct{ minIn, maxIn, minOut, maxOut int32 }{
		{6, 27, 6, 25}, {21, 21, 21, 21}, {25, 30, 25, 25}, {0, 21, 0, 21}, {0, 25, 0, 25}, {0, 26, 0, 25}, {20, 22, 20, 22},
	}
	for _, c := range cases {
		p := params()
		p.MinWire, p.MaxWire = c.minIn, c.maxIn
		r := bson.Raw(reply(t, wire.Handshake{Name: "hello"}, p, 1))
		if r.Lookup("minWireVersion").AsInt64() != int64(c.minOut) || r.Lookup("maxWireVersion").AsInt64() != int64(c.maxOut) {
			t.Errorf("(%d,%d): got (%d,%d), want (%d,%d)", c.minIn, c.maxIn,
				r.Lookup("minWireVersion").AsInt64(), r.Lookup("maxWireVersion").AsInt64(), c.minOut, c.maxOut)
		}
	}
}

// The reply's localTime follows the supplied clock value.
func TestHandshakeLocalTime_FR14(t *testing.T) {
	for _, now := range []time.Time{t0, t0.Add(36 * time.Hour), time.Unix(0, 0)} {
		r := bson.Raw(proxy.HandshakeReply(wire.Handshake{Name: "hello"}, params(), 1, now))
		if got := r.Lookup("localTime").Time().UnixMilli(); got != now.UnixMilli() {
			t.Errorf("localTime = %d, want %d", got, now.UnixMilli())
		}
	}
}

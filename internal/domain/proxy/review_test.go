package proxy_test

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/wire"
)

// FR-14: the reply carries logicalSessionTimeoutMinutes only when the upstream
// reported it (an absent value is not invented).
func TestHandshakeReplyOmitsAbsentLSTM_FR14(t *testing.T) {
	p := params()
	p.HasLSTM = false
	p.LogicalSessionTimeoutMinutes = 0
	r := bson.Raw(proxy.HandshakeReply(wire.Handshake{Name: "hello"}, p, 1, t0))
	if _, err := r.LookupErr("logicalSessionTimeoutMinutes"); err == nil {
		t.Fatal("logicalSessionTimeoutMinutes present although the upstream did not report it")
	}
	// The rest of the reply is unchanged.
	for _, k := range []string{"isWritablePrimary", "msg", "maxBsonObjectSize", "maxMessageSizeBytes", "maxWriteBatchSize", "minWireVersion", "maxWireVersion", "localTime", "connectionId", "readOnly", "ok"} {
		if _, err := r.LookupErr(k); err != nil {
			t.Errorf("field %s missing: %v", k, err)
		}
	}
	// And present when reported.
	r = bson.Raw(proxy.HandshakeReply(wire.Handshake{Name: "hello"}, params(), 1, t0))
	if v, err := r.LookupErr("logicalSessionTimeoutMinutes"); err != nil || v.AsInt64() != 30 {
		t.Fatalf("reported value missing: %v", err)
	}
}

func opMsgBytes(body bson.D) []byte {
	b, err := bson.Marshal(body)
	if err != nil {
		panic(err)
	}
	return cat(hdrBytes(int32(16+4+1+len(b)), 1, 0, 2013), le32(0), []byte{0}, b)
}

func hdrBytes(length, reqID, respTo, op int32) []byte {
	return cat(le32(length), le32(reqID), le32(respTo), le32(op))
}
func le32(v int32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func decideBytes(t *testing.T, msg []byte) proxy.Decision {
	t.Helper()
	req, err := wire.ClassifyPrefix(msg, int32(len(msg)))
	if err != nil {
		t.Fatalf("ClassifyPrefix: %v", err)
	}
	return proxy.Decide(req, &proxy.ConnState{}, okView, proxy.PhaseServing)
}

// Owner decision: killSessions with an empty array (or no usable array) is
// blocked like killAllSessions (13, command_blocked); with explicit lsids it is
// forwarded. Over OP_MSG only: OP_QUERY gets 352.
func TestKillSessionsBlockedWhenEmpty_FR33(t *testing.T) {
	lsid := bson.D{{Key: "id", Value: bson.Binary{Subtype: 4, Data: make([]byte, 16)}}}
	blocked := map[string]bson.D{
		"empty array":    {{Key: "killSessions", Value: bson.A{}}, {Key: "$db", Value: "admin"}},
		"null":           {{Key: "killSessions", Value: nil}, {Key: "$db", Value: "admin"}},
		"scalar value":   {{Key: "killSessions", Value: 1}, {Key: "$db", Value: "admin"}},
		"empty document": {{Key: "killSessions", Value: bson.D{}}, {Key: "$db", Value: "admin"}},
		"string value":   {{Key: "killSessions", Value: "all"}, {Key: "$db", Value: "admin"}},
	}
	for name, body := range blocked {
		t.Run("blocked/"+name, func(t *testing.T) {
			d := decideBytes(t, opMsgBytes(body))
			if d.Kind != proxy.Refuse || d.Refusal != proxy.CommandBlocked || !d.Blocked {
				t.Fatalf("decision %+v, want a command_blocked refusal", d)
			}
		})
	}
	forwarded := map[string]bson.D{
		"one lsid":                     {{Key: "killSessions", Value: bson.A{lsid}}, {Key: "$db", Value: "admin"}},
		"two lsids":                    {{Key: "killSessions", Value: bson.A{lsid, lsid}}, {Key: "$db", Value: "admin"}},
		"array with an empty document": {{Key: "killSessions", Value: bson.A{bson.D{}}}, {Key: "$db", Value: "admin"}},
		"array of scalars":             {{Key: "killSessions", Value: bson.A{1, 2}}, {Key: "$db", Value: "admin"}},
		"lsid with extra":              {{Key: "killSessions", Value: bson.A{bson.D{{Key: "id", Value: bson.Binary{Subtype: 4, Data: make([]byte, 16)}}, {Key: "uid", Value: bson.Binary{Subtype: 0, Data: make([]byte, 32)}}}}}, {Key: "$db", Value: "admin"}},
	}
	for name, body := range forwarded {
		t.Run("forward/"+name, func(t *testing.T) {
			d := decideBytes(t, opMsgBytes(body))
			if d.Kind != proxy.Forward {
				t.Fatalf("decision %+v, want Forward", d)
			}
		})
	}
	// Owner decision (wave 4 review): any non-empty array is forwarded; the proxy
	// does not judge its elements.
	// Case-sensitive: other spellings are ordinary commands.
	for _, c := range []string{"KillSessions", "killsessions"} {
		d := decideBytes(t, opMsgBytes(bson.D{{Key: c, Value: bson.A{}}, {Key: "$db", Value: "admin"}}))
		if d.Kind != proxy.Forward {
			t.Errorf("%s: %+v, want Forward", c, d)
		}
	}
	// OP_QUERY: 352, never forwarded.
	for _, q := range []bson.D{{{Key: "killSessions", Value: bson.A{}}}, {{Key: "killSessions", Value: bson.A{lsid}}}} {
		qb, _ := bson.Marshal(q)
		body := cat(le32(0), []byte("admin.$cmd\x00"), le32(0), le32(-1), qb)
		msg := cat(hdrBytes(int32(16+len(body)), 1, 0, 2004), body)
		d := decideBytes(t, msg)
		if d.Kind != proxy.QueryFailure {
			t.Fatalf("OP_QUERY killSessions: %+v, want QueryFailure", d)
		}
	}
	// killAllSessions stays blocked (regression).
	if d := decideBytes(t, opMsgBytes(bson.D{{Key: "killAllSessions", Value: bson.A{}}, {Key: "$db", Value: "admin"}})); d.Refusal != proxy.CommandBlocked {
		t.Fatal("killAllSessions no longer blocked")
	}
}

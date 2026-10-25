package wire_test

import (
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/wire"
)

// Amendment 3 (FR-18): ParseHelloReply reads `isreplicaset` (the marker of a
// ghost: a member that is a replica-set member but reports no setName).
// Interface assumption: the field is HelloReply.IsReplicaSet.
func TestParseHelloReplyIsReplicaSet_AC56(t *testing.T) {
	r, err := wire.ParseHelloReply(doc(eBool("isreplicaset", true), eBool("isWritablePrimary", false), eInt32("minWireVersion", 0), eInt32("maxWireVersion", 21), eDouble("ok", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsReplicaSet || r.SetName != "" {
		t.Fatalf("ghost reply: %+v", r)
	}
	r, err = wire.ParseHelloReply(doc(eBool("isWritablePrimary", true), eDouble("ok", 1)))
	if err != nil || r.IsReplicaSet {
		t.Fatalf("standalone reply: %+v %v", r, err)
	}
	r, err = wire.ParseHelloReply(doc(eBool("isreplicaset", false), eDouble("ok", 1)))
	if err != nil || r.IsReplicaSet {
		t.Fatalf("isreplicaset:false: %+v %v", r, err)
	}
	// Only the exact (case-sensitive) key counts, and only a boolean.
	for _, d := range [][]byte{doc(eBool("isReplicaSet", true), eDouble("ok", 1)), doc(eString("isreplicaset", "true"), eDouble("ok", 1)), doc(eInt32("isreplicaset", 1), eDouble("ok", 1))} {
		if r, err := wire.ParseHelloReply(d); err != nil || r.IsReplicaSet {
			t.Fatalf("a lookalike set IsReplicaSet: %+v %v", r, err)
		}
	}
}

// FR-5 order (review): an unknown required flag bit on a large message that
// would be message_too_large (a local command over L) is malformed_message,
// because the flag check (check 5) comes before the FR-38 size rule.
func TestFlagBitBeforeLocalSizeRule_FR5(t *testing.T) {
	big := strings.Repeat("p", 100_000)
	m := opMsg(1, 1<<2, sec0(doc(eInt32("hello", 1), eString("pad", big), eString("$db", "admin"))))
	if _, err := classify(t, m); detail(err) != "malformed_message" {
		t.Fatalf("detail %q, want malformed_message", detail(err))
	}
	// Without the flag bit the same message is message_too_large.
	m = opMsg(1, 0, sec0(doc(eInt32("hello", 1), eString("pad", big), eString("$db", "admin"))))
	if _, err := classify(t, m); detail(err) != "message_too_large" {
		t.Fatalf("detail %q, want message_too_large", detail(err))
	}
	// Other optional bits do not change it.
	m = opMsg(1, 1<<17, sec0(doc(eInt32("hello", 1), eString("pad", big), eString("$db", "admin"))))
	if _, err := classify(t, m); detail(err) != "message_too_large" {
		t.Fatalf("detail %q with an optional bit", detail(err))
	}
}

// FR-38 (review): a deciding key present in the prefix without its value bytes
// is malformed_message (the value is outside the prefix too). The collection
// name is not a deciding field: when its value is cut at the prefix edge,
// classification succeeds and the collection is left unset.
func TestKeyWithoutValueBytesInPrefix_FR38(t *testing.T) {
	pipe := eArray("pipeline", eDoc("0", doc(eDoc("$match", doc()))))
	cases := map[string][]byte{
		"command value cut":      cmdMsg(eString("find", strings.Repeat("c", 200)), eString("$db", "d")),
		"pipeline value cut":     cmdMsg(eString("aggregate", "c"), pipe, eString("$db", "d")),
		"getMore collection cut": cmdMsg(eInt64("getMore", 5), eString("collection", strings.Repeat("k", 200)), eString("$db", "d")),
		"killSessions arg cut":   cmdMsg(eArray("killSessions", eDoc("0", doc(eString("id", strings.Repeat("i", 200))))), eString("$db", "d")),
	}
	keys := map[string]string{"command value cut": "find", "pipeline value cut": "pipeline", "getMore collection cut": "collection", "killSessions arg cut": "killSessions"}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			idx := strings.Index(string(m), keys[name]+"\x00")
			if idx < 0 {
				t.Fatal("setup: key not found")
			}
			cut := idx + len(keys[name]) + 1 // right after the key's NUL
			req, err := wire.ClassifyPrefix(m[:cut], int32(len(m)))
			if name == "command value cut" || name == "getMore collection cut" {
				if err != nil || req.HasCollection {
					t.Fatalf("a cut collection value: err %v HasCollection %v, want success with the collection unset", err, req.HasCollection)
				}
				return
			}
			if detail(err) != "malformed_message" {
				t.Fatalf("cut after the key: detail %q, want malformed_message", detail(err))
			}
		})
	}
}

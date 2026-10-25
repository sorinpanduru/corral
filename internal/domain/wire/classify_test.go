package wire_test

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/wire"
)

const prefixLimit = 65536 // L, FR-38

func classify(t *testing.T, msg []byte) (wire.Request, error) {
	t.Helper()
	p := msg
	if len(p) > prefixLimit {
		p = p[:prefixLimit]
	}
	return wire.ClassifyPrefix(p, int32(len(msg)))
}

func mustClassify(t *testing.T, msg []byte) wire.Request {
	t.Helper()
	r, err := classify(t, msg)
	if err != nil {
		t.Fatalf("ClassifyPrefix: %v (detail %q)", err, detail(err))
	}
	return r
}

// FR-9: a request exposes the command name and the inspected fields.
func TestClassifyFields_FR9(t *testing.T) {
	lsid := make([]byte, 16)
	for i := range lsid {
		lsid[i] = byte(i + 1)
	}
	m := opMsg(42, flagMore|flagExhaust, sec0(doc(
		eString("find", "orders"),
		eString("$db", "shop"),
		eDoc("lsid", doc(eBinary("id", 4, lsid))),
		eInt64("txnNumber", 7),
		eBool("autocommit", false),
		eBool("startTransaction", true),
		eDoc("filter", doc(eString("secret", "synthetic-filter-marker"))),
	)))
	r := mustClassify(t, m)
	if r.Kind != wire.KindOpMsg || r.RequestID != 42 || r.Flags != flagMore|flagExhaust {
		t.Fatalf("kind/id/flags = %v %d %#x", r.Kind, r.RequestID, r.Flags)
	}
	if r.Command != "find" || r.DB != "shop" {
		t.Fatalf("command/db = %q %q", r.Command, r.DB)
	}
	if !r.HasLSID || string(r.LSID[:]) != string(lsid) {
		t.Fatal("lsid.id not read")
	}
	if !r.HasTxnNumber || !r.AutocommitFalse || !r.StartTransaction {
		t.Fatalf("txn fields: %v %v %v", r.HasTxnNumber, r.AutocommitFalse, r.StartTransaction)
	}
	if !r.HasCollection || r.Collection != "orders" {
		t.Fatalf("collection = %q (%v)", r.Collection, r.HasCollection)
	}
	if !r.FitsPrefix {
		t.Fatal("a small message must fit the prefix")
	}
	if strings.Contains(fmt.Sprintf("%+v", r), "synthetic-filter-marker") {
		t.Fatal("a field outside the FR-9 list leaked into the Request")
	}
}

// autocommit true, absent fields and a non-UUID lsid.id are not reported as set.
func TestClassifyAbsentFields_FR9(t *testing.T) {
	r := mustClassify(t, cmdMsg(eString("ping", ""), eString("$db", "admin"), eBool("autocommit", true)))
	if r.HasLSID || r.HasTxnNumber || r.AutocommitFalse || r.StartTransaction || r.HasCollection || r.HasCursorID {
		t.Fatalf("unexpected fields set: %+v", r)
	}
	if r.Command != "ping" || r.DB != "admin" {
		t.Fatalf("command/db = %q %q", r.Command, r.DB)
	}
	// lsid.id of the wrong type or length is ignored (no HasLSID).
	r = mustClassify(t, cmdMsg(eString("find", "c"), eDoc("lsid", doc(eBinary("id", 4, []byte{1, 2, 3})))))
	if r.HasLSID {
		t.Fatal("a 3-byte lsid.id was accepted as a UUID")
	}
	r = mustClassify(t, cmdMsg(eString("find", "c"), eDoc("lsid", doc(eString("id", "nope")))))
	if r.HasLSID {
		t.Fatal("a string lsid.id was accepted")
	}
}

// FR-9 / FR-40: getMore takes the cursor ID from its first element and the
// collection from `collection`.
func TestClassifyGetMore_FR40(t *testing.T) {
	r := mustClassify(t, cmdMsg(eInt64("getMore", 123456789012), eString("collection", "events"), eString("$db", "d")))
	if r.Command != "getMore" || !r.HasCursorID || r.CursorID != 123456789012 {
		t.Fatalf("cursor: %+v", r)
	}
	if !r.HasCollection || r.Collection != "events" {
		t.Fatalf("collection = %q", r.Collection)
	}
	// The cursor ID is read only for getMore.
	r = mustClassify(t, cmdMsg(eInt64("find", 5), eString("$db", "d")))
	if r.HasCursorID {
		t.Fatal("cursor ID read for a non-getMore command")
	}
	// getMore without a string collection has none.
	r = mustClassify(t, cmdMsg(eInt64("getMore", 5), eInt32("collection", 3)))
	if r.HasCollection {
		t.Fatal("non-string collection accepted")
	}
}

// FR-9 / FR-33: the first pipeline stage name of aggregate, never its
// arguments.
func TestClassifyAggregateFirstStage_FR33(t *testing.T) {
	stage := func(name string) []byte {
		return eArray("pipeline", eDoc("0", doc(eDoc(name, doc(eString("arg", "synthetic-arg-marker"))))), eDoc("1", doc(eDoc("$limit", doc()))))
	}
	for _, name := range []string{"$match", "$currentOp", "$unionWith", "$collStats"} {
		r := mustClassify(t, cmdMsg(eString("aggregate", "c"), stage(name), eString("$db", "d")))
		if r.FirstStage != name {
			t.Errorf("FirstStage = %q, want %q", r.FirstStage, name)
		}
		if strings.Contains(fmt.Sprintf("%+v", r), "synthetic-arg-marker") {
			t.Error("stage arguments leaked")
		}
	}
	// aggregate: 1 has no collection; an empty pipeline has no first stage.
	r := mustClassify(t, cmdMsg(eInt32("aggregate", 1), eArray("pipeline"), eString("$db", "admin")))
	if r.HasCollection || r.FirstStage != "" {
		t.Fatalf("aggregate:1 / empty pipeline: %+v", r)
	}
	// The pipeline is read for aggregate only.
	r = mustClassify(t, cmdMsg(eString("find", "c"), stage("$currentOp")))
	if r.FirstStage != "" {
		t.Fatal("first stage read for a non-aggregate command")
	}
	// The pipeline may come before the command's other fields.
	r = mustClassify(t, cmdMsg(eString("aggregate", "c"), eString("$db", "d"), stage("$match")))
	if r.FirstStage != "$match" {
		t.Fatalf("FirstStage = %q", r.FirstStage)
	}
}

// SPEC-001 FR-40 (AC-39): the closed collection command list.
func TestCollectionCommandList_FR40(t *testing.T) {
	want := []string{"find", "insert", "update", "delete", "aggregate", "count", "distinct",
		"findAndModify", "create", "drop", "createIndexes", "dropIndexes", "listIndexes", "collMod", "killCursors"}
	got := wire.CollectionCommands()
	have := map[string]bool{}
	for _, c := range got {
		have[c] = true
	}
	for _, c := range want {
		if !have[c] {
			t.Errorf("CollectionCommands() lacks %q", c)
		}
		delete(have, c)
	}
	delete(have, "getMore") // getMore may be listed; its source differs
	if len(have) != 0 {
		var extra []string
		for c := range have {
			extra = append(extra, c)
		}
		sort.Strings(extra)
		t.Fatalf("CollectionCommands() has commands outside the closed list: %v", extra)
	}
	got[0] = "tampered"
	if wire.CollectionCommands()[0] == "tampered" {
		t.Fatal("CollectionCommands() exposes internal state")
	}
}

func TestCollectionExtraction_AC39(t *testing.T) {
	for _, c := range []string{"find", "insert", "update", "delete", "count", "distinct", "findAndModify",
		"create", "drop", "createIndexes", "dropIndexes", "listIndexes", "collMod", "killCursors", "aggregate"} {
		r := mustClassify(t, cmdMsg(eString(c, "coll-"+c), eString("$db", "d")))
		if !r.HasCollection || r.Collection != "coll-"+c {
			t.Errorf("%s: collection = %q (%v)", c, r.Collection, r.HasCollection)
		}
	}
	// Only a string value counts.
	for _, c := range []string{"find", "insert", "aggregate", "drop"} {
		if r := mustClassify(t, cmdMsg(eInt32(c, 1), eString("$db", "d"))); r.HasCollection {
			t.Errorf("%s with a numeric value has a collection", c)
		}
	}
	// User, role and auth commands never yield a collection, even with a
	// string first value.
	for _, c := range []string{"createUser", "updateUser", "dropUser", "dropAllUsersFromDatabase",
		"grantRolesToUser", "revokeRolesFromUser", "usersInfo", "createRole", "updateRole", "dropRole",
		"dropAllRolesFromDatabase", "grantPrivilegesToRole", "revokePrivilegesFromRole", "grantRolesToRole",
		"revokeRolesFromRole", "rolesInfo", "authenticate", "saslStart", "saslContinue", "getnonce", "logout",
		"hello", "ping", "currentOp", "listCollections", "whatsmyuri"} {
		r := mustClassify(t, cmdMsg(eString(c, "SYNTHETIC-USER-MARKER"), eString("$db", "admin")))
		if r.HasCollection || r.Collection != "" {
			t.Errorf("%s yielded a collection", c)
		}
		if strings.Contains(fmt.Sprintf("%+v", r), "SYNTHETIC-USER-MARKER") {
			t.Errorf("%s: first value leaked into the Request", c)
		}
	}
	// Invalid UTF-8 is passed through raw here; telemetry sanitises it later.
	r := mustClassify(t, cmdMsg(eString("find", "a\xffb"), eString("$db", "d")))
	if !r.HasCollection {
		t.Fatal("collection with invalid UTF-8 not extracted")
	}
}

// FR-38: the command name and the first pipeline stage must lie inside the
// prefix; a local message must fit it entirely.
func TestClassifyPrefixLimits_FR38(t *testing.T) {
	big := strings.Repeat("p", 100_000)
	pipe := eArray("pipeline", eDoc("0", doc(eDoc("$match", doc()))))

	// Command name beyond a short prefix.
	m := cmdMsg(eString("find", "c"), eString("$db", "d"))
	if _, err := wire.ClassifyPrefix(m[:22], int32(len(m))); detail(err) != "malformed_message" {
		t.Fatalf("command name outside the prefix: detail %q", detail(err))
	}
	// aggregate whose pipeline lies after a 100 KB field: first stage outside
	// the prefix.
	agg := cmdMsg(eString("aggregate", "c"), eString("pad", big), pipe, eString("$db", "d"))
	if _, err := classify(t, agg); detail(err) != "malformed_message" {
		t.Fatalf("first stage outside the prefix: detail %q", detail(err))
	}
	// A non-aggregate message with a far-away tail is fine and forwarded.
	find := cmdMsg(eString("find", "c"), eString("pad", big), eString("$db", "d"))
	r, err := classify(t, find)
	if err != nil {
		t.Fatalf("large find rejected: %v", err)
	}
	if r.FitsPrefix {
		t.Fatal("a 100 KB message cannot fit the prefix")
	}
	if r.Command != "find" || !r.HasCollection {
		t.Fatalf("large find not classified: %+v", r)
	}
	// Local messages larger than the prefix: message_too_large.
	for _, c := range []string{"hello", "isMaster", "ismaster", "saslStart", "saslContinue", "authenticate", "getnonce", "logout", "currentOp", "killOp", "killAllSessions", "killAllSessionsByPattern"} {
		m := cmdMsg(eInt32(c, 1), eString("pad", big), eString("$db", "admin"))
		if _, err := classify(t, m); detail(err) != "message_too_large" {
			t.Errorf("%s over the limit: detail %q, want message_too_large", c, detail(err))
		}
	}
	// A blocked aggregate (first stage $currentOp) over the limit too.
	blocked := cmdMsg(eString("aggregate", "c"), eArray("pipeline", eDoc("0", doc(eDoc("$currentOp", doc())))), eString("pad", big), eString("$db", "admin"))
	if _, err := classify(t, blocked); detail(err) != "message_too_large" {
		t.Errorf("blocked aggregate over the limit: detail %q", detail(err))
	}
	// Under the limit the same local messages classify normally.
	if r := mustClassify(t, cmdMsg(eInt32("hello", 1), eString("$db", "admin"))); !r.FitsPrefix || r.Command != "hello" {
		t.Fatalf("small hello: %+v", r)
	}
}

// Handshake over OP_MSG: classified with its fields.
func TestClassifyHandshakeOpMsg_FR9(t *testing.T) {
	m := cmdMsg(eInt32("hello", 1), eBool("helloOk", true), eString("$db", "admin"),
		eDoc("topologyVersion", doc(eInt64("counter", 1))), eInt64("maxAwaitTimeMS", 10000),
		eDoc("client", doc(
			eDoc("application", doc(eString("name", "synthetic-app"))),
			eDoc("driver", doc(eString("name", "synthetic-driver"), eString("version", "9.9.9"))),
			eDoc("os", doc(eString("type", "SYNTHETIC-OS-MARKER"))),
		)),
		eArray("compression", eString("0", "zlib")),
		eDoc("speculativeAuthenticate", doc(eString("saslStart", "x"))),
		eString("saslSupportedMechs", "admin.SYNTHETIC-USER-MARKER"),
	)
	r := mustClassify(t, m)
	if r.Kind != wire.KindOpMsg || r.Command != "hello" || r.Handshake == nil {
		t.Fatalf("not a handshake: %+v", r)
	}
	h := r.Handshake
	if h.Name != "hello" || !h.HelloOK || h.LoadBalanced || h.ViaOpQuery {
		t.Fatalf("handshake fields: %+v", h)
	}
	if !h.HasTopologyVersion || !h.HasMaxAwaitTimeMS || !h.HasClient || !h.HasCompression || !h.HasSpeculativeAuth || !h.HasSaslSupportedMechs {
		t.Fatalf("presence flags: %+v", h)
	}
	if h.AppName != "synthetic-app" || h.DriverName != "synthetic-driver" || h.DriverVersion != "9.9.9" {
		t.Fatalf("client metadata: %q %q %q", h.AppName, h.DriverName, h.DriverVersion)
	}
	// FR-9, SPEC-001 FR-24: no value from sasl mechs or client.os appears.
	out := fmt.Sprintf("%+v %#v %v", r, h, *h)
	for _, marker := range []string{"SYNTHETIC-USER-MARKER", "SYNTHETIC-OS-MARKER", "zlib"} {
		if strings.Contains(out, marker) {
			t.Errorf("%q leaked into the classified request", marker)
		}
	}
	// loadBalanced is read as a value.
	r = mustClassify(t, cmdMsg(eInt32("isMaster", 1), eBool("loadBalanced", true), eString("$db", "admin")))
	if r.Handshake == nil || !r.Handshake.LoadBalanced || r.Handshake.Name != "isMaster" {
		t.Fatalf("loadBalanced: %+v", r.Handshake)
	}
	// Client metadata fields are bounded to 256 bytes.
	long := strings.Repeat("a", 1000)
	r = mustClassify(t, cmdMsg(eInt32("hello", 1), eDoc("client", doc(eDoc("application", doc(eString("name", long)))))))
	if r.Handshake == nil || len(r.Handshake.AppName) > 256 {
		t.Fatalf("AppName not bounded: %d bytes", len(r.Handshake.AppName))
	}
	// Handshake spelling is exact and case-sensitive (FR-13).
	for _, c := range []string{"HELLO", "Hello", "IsMaster", "ISMASTER", "ismaster "} {
		r := mustClassify(t, cmdMsg(eInt32(c, 1), eString("$db", "admin")))
		if r.Handshake != nil {
			t.Errorf("%q treated as a handshake", c)
		}
	}
	// ismaster is a handshake.
	if r := mustClassify(t, cmdMsg(eInt32("ismaster", 1))); r.Handshake == nil || r.Handshake.Name != "ismaster" {
		t.Fatal("ismaster not a handshake")
	}
}

func query(ns string, d ...[]byte) []byte { return opQuery(5, 0, ns, 0, -1, d...) }

// AC-5 (unit), FR-8: OP_QUERY handshake detection.
func TestClassifyOpQueryHandshake_AC5(t *testing.T) {
	for _, name := range []string{"isMaster", "hello", "ismaster"} {
		r := mustClassify(t, query("admin.$cmd", doc(eInt32(name, 1), eBool("helloOk", true))))
		if r.Kind != wire.KindOpQueryHandshake || r.Handshake == nil || r.Handshake.Name != name || !r.Handshake.ViaOpQuery || !r.Handshake.HelloOK {
			t.Errorf("%s: %+v", name, r)
		}
		if r.RequestID != 5 {
			t.Errorf("request ID %d", r.RequestID)
		}
		// Any database before .$cmd works.
		if r := mustClassify(t, query("other.$cmd", doc(eInt32(name, 1)))); r.Kind != wire.KindOpQueryHandshake {
			t.Errorf("%s on other.$cmd not a handshake", name)
		}
		// $query wrapper with $readPreference.
		w := doc(eDoc("$query", doc(eInt32(name, 1), eDoc("client", doc(eDoc("driver", doc(eString("name", "legacy"), eString("version", "1"))))))), eDoc("$readPreference", doc(eString("mode", "primaryPreferred"))))
		r = mustClassify(t, query("admin.$cmd", w))
		if r.Kind != wire.KindOpQueryHandshake || r.Handshake == nil || r.Handshake.Name != name || !r.Handshake.HasClient || r.Handshake.DriverName != "legacy" {
			t.Errorf("%s wrapped: %+v", name, r)
		}
	}
	other := map[string][]byte{
		"find":                       query("admin.$cmd", doc(eString("find", "c"))),
		"wrong case HELLO":           query("admin.$cmd", doc(eInt32("HELLO", 1))),
		"wrong case IsMaster":        query("admin.$cmd", doc(eInt32("IsMaster", 1))),
		"not .$cmd":                  query("admin.coll", doc(eInt32("hello", 1))),
		"ns ending $cmdx":            query("admin.$cmdx", doc(eInt32("hello", 1))),
		"ns without dot":             query("$cmd", doc(eInt32("hello", 1))),
		"hello not first":            query("admin.$cmd", doc(eString("x", "y"), eInt32("hello", 1))),
		"$query first key not hello": query("admin.$cmd", doc(eDoc("$query", doc(eString("find", "c"))))),
		"$query not a document":      query("admin.$cmd", doc(eString("$query", "hello"))),
		"hello only in $query but not first in wrapper": query("admin.$cmd", doc(eDoc("$query", doc(eString("x", "y"), eInt32("hello", 1))))),
	}
	for name, m := range other {
		r := mustClassify(t, m)
		if r.Kind != wire.KindOpQueryOther || r.Handshake != nil {
			t.Errorf("%s: kind %v handshake %v, want OpQueryOther", name, r.Kind, r.Handshake)
		}
	}
}

// Amendment 2, R-1: OP_QUERY auth and blocked commands are KindOpQueryOther
// (the 352 refusal), labelled with the first key (or the first key inside
// $query).
func TestClassifyOpQueryAuthAndBlocked_R1(t *testing.T) {
	for _, c := range []string{"saslStart", "saslContinue", "authenticate", "getnonce", "logout", "currentOp", "killOp", "killAllSessions", "find"} {
		r := mustClassify(t, query("admin.$cmd", doc(eInt32(c, 1))))
		if r.Kind != wire.KindOpQueryOther || r.Command != c {
			t.Errorf("%s: kind %v command %q", c, r.Kind, r.Command)
		}
		r = mustClassify(t, query("admin.$cmd", doc(eDoc("$query", doc(eInt32(c, 1))))))
		if r.Kind != wire.KindOpQueryOther || r.Command != c {
			t.Errorf("%s wrapped: kind %v command %q", c, r.Kind, r.Command)
		}
	}
}

// AC-38 (unit part): OP_QUERY structure.
func TestOpQueryStructure_AC38(t *testing.T) {
	q := doc(eInt32("hello", 1))
	sel := doc(eInt32("a", 1))
	good := map[string][]byte{
		"query only":                query("admin.$cmd", q),
		"with returnFieldsSelector": query("admin.$cmd", q, sel),
		"flags ignored":             opQuery(5, 0xFFFFFFFF, "admin.$cmd", 0, -1, q),
		"skip and return arbitrary": opQuery(5, 0, "admin.$cmd", 7, 1<<30, q),
		"unicode namespace":         query("dé.$cmd", q),
		"1024 levels each":          query("admin.$cmd", doc(eInt32("find", 1), eDoc("n", nested(1023, false))), nested(1024, true)),
	}
	for name, m := range good {
		if _, err := classify(t, m); err != nil {
			t.Errorf("good/%s: %v (%q)", name, err, detail(err))
		}
	}
	head := func(ns []byte, rest ...[]byte) []byte {
		body := cat(le32(0), ns, cat(rest...))
		return cat(hdrBytes(int32(16+len(body)), 1, 0, 2004), body)
	}
	bad := map[string][]byte{
		"unterminated namespace":            head([]byte("admin.$cmd")),
		"unterminated namespace with data":  head([]byte("admin.$cmd"), le32(0), le32(-1), q),
		"missing numberToReturn":            head(cstr("admin.$cmd"), le32(0)),
		"missing numberToSkip":              head(cstr("admin.$cmd")),
		"half numberToReturn":               head(cstr("admin.$cmd"), le32(0), []byte{1, 2}),
		"missing query document":            head(cstr("admin.$cmd"), le32(0), le32(-1)),
		"invalid query document":            head(cstr("admin.$cmd"), le32(0), le32(-1), doc(el(0x14, "a", nil))),
		"query document overrun":            head(cstr("admin.$cmd"), le32(0), le32(-1), []byte{99, 0, 0, 0, 0}),
		"trailing bytes after the query":    head(cstr("admin.$cmd"), le32(0), le32(-1), q, []byte{1, 2, 3}),
		"trailing bytes after the selector": head(cstr("admin.$cmd"), le32(0), le32(-1), q, sel, []byte{1}),
		"two selectors":                     head(cstr("admin.$cmd"), le32(0), le32(-1), q, sel, sel),
		"invalid selector":                  head(cstr("admin.$cmd"), le32(0), le32(-1), q, doc(el(0x14, "a", nil))),
		"1025 levels in the query":          query("admin.$cmd", doc(eInt32("find", 1), eDoc("n", nested(1024, false)))),
		"1025 levels in the selector":       query("admin.$cmd", q, nested(1025, false)),
	}
	for name, m := range bad {
		if _, err := classify(t, m); detail(err) != "malformed_message" {
			t.Errorf("bad/%s: detail %q, want malformed_message", name, detail(err))
		}
	}
	// HELLO and IsMaster over OP_QUERY are valid but not handshakes (above);
	// a valid non-handshake OP_QUERY is KindOpQueryOther.
	if r := mustClassify(t, query("db.coll", doc(eString("find", "c")))); r.Kind != wire.KindOpQueryOther {
		t.Fatalf("valid non-handshake OP_QUERY: %v", r.Kind)
	}
}

// FR-9 minimisation: Request, Handshake and ReplyFields have only the listed
// fields.
func TestFieldSetsAreExact_FR9(t *testing.T) {
	names := func(v any) []string {
		rt := reflect.TypeOf(v)
		var out []string
		for i := 0; i < rt.NumField(); i++ {
			out = append(out, rt.Field(i).Name)
		}
		sort.Strings(out)
		return out
	}
	check := func(label string, got, want []string) {
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s fields = %v, want %v", label, got, want)
		}
	}
	// Request may additionally carry one derived killSessions flag (Owner
	// decision: an empty killSessions is blocked); nothing else may be added.
	got := names(wire.Request{})
	var kept []string
	for _, n := range got {
		if !strings.Contains(n, "KillSessions") {
			kept = append(kept, n)
		}
	}
	if len(got)-len(kept) > 1 {
		t.Errorf("Request has several killSessions fields: %v", got)
	}
	check("Request", kept, []string{"Kind", "RequestID", "Flags", "Command", "DB", "HasLSID", "LSID",
		"HasTxnNumber", "AutocommitFalse", "StartTransaction", "Collection", "HasCollection", "CursorID", "HasCursorID",
		"FirstStage", "Handshake", "FitsPrefix"})
	check("Handshake", names(wire.Handshake{}), []string{"Name", "HelloOK", "LoadBalanced", "HasTopologyVersion",
		"HasMaxAwaitTimeMS", "HasClient", "HasCompression", "HasSpeculativeAuth", "HasSaslSupportedMechs",
		"AppName", "DriverName", "DriverVersion", "ViaOpQuery"})
	check("ReplyFields", names(wire.ReplyFields{}), []string{"HasOK", "OK", "HasCode", "Code", "CodeName",
		"HasWCECode", "WCECode", "HasCursorID", "CursorID"})
}

// Kind constants are distinct.
func TestKindsDistinct(t *testing.T) {
	if wire.KindOpMsg == wire.KindOpQueryHandshake || wire.KindOpMsg == wire.KindOpQueryOther || wire.KindOpQueryHandshake == wire.KindOpQueryOther {
		t.Fatal("request kinds are not distinct")
	}
}

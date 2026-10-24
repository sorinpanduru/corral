package wire_test

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/wire"
)

func scan(msg []byte, chunk int) (*wire.ReplyScanner, error) {
	s := wire.NewReplyScanner(hdr(msg))
	body := msg[16:]
	for off := 0; off < len(body); off += chunk {
		end := off + chunk
		if end > len(body) {
			end = len(body)
		}
		if err := s.Write(body[off:end]); err != nil {
			return s, err
		}
	}
	return s, s.Close()
}

func reply(elems ...[]byte) []byte { return opMsg(7, 0, sec0(doc(elems...))) }

// FR-58 / AC-43: fields are read as the reply streams. ok after a 5 MB cursor
// document is captured under 4 KiB chunks.
func TestReplyScannerOkAfterLargeCursor_FR58(t *testing.T) {
	big := strings.Repeat("c", 5<<20)
	m := reply(eDoc("cursor", doc(eInt64("id", 4242), eString("ns", "d.c"), eArray("firstBatch", eDoc("0", doc(eString("v", big)))))), eDouble("ok", 1))
	for _, chunk := range []int{4096, 1 << 16, len(m)} {
		s, err := scan(m, chunk)
		if err != nil {
			t.Fatalf("chunk %d: %v", chunk, err)
		}
		f := s.Fields()
		if !f.HasOK || f.OK != 1 {
			t.Fatalf("chunk %d: ok = %v (%v)", chunk, f.OK, f.HasOK)
		}
		if !f.HasCursorID || f.CursorID != 4242 {
			t.Fatalf("chunk %d: cursor.id = %d (%v)", chunk, f.CursorID, f.HasCursorID)
		}
		if f.HasCode || f.HasWCECode {
			t.Fatalf("chunk %d: unexpected code fields: %+v", chunk, f)
		}
	}
}

func TestReplyScannerErrorFields_FR9(t *testing.T) {
	s, err := scan(reply(eDouble("ok", 0), eString("errmsg", "SYNTHETIC-ERRMSG-MARKER"), eInt32("code", 10107), eString("codeName", "NotWritablePrimary")), 5)
	if err != nil {
		t.Fatal(err)
	}
	f := s.Fields()
	if !f.HasOK || f.OK != 0 || !f.HasCode || f.Code != 10107 || f.CodeName != "NotWritablePrimary" {
		t.Fatalf("fields: %+v", f)
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", f, f), "SYNTHETIC-ERRMSG-MARKER") {
		t.Fatal("errmsg leaked into ReplyFields")
	}
	// ok may be int32 or int64 on the wire.
	for _, ok := range [][]byte{eInt32("ok", 1), eInt64("ok", 1)} {
		s, err := scan(reply(ok), 4)
		if err != nil || !s.Fields().HasOK || s.Fields().OK != 1 {
			t.Fatalf("integer ok: %+v %v", s.Fields(), err)
		}
	}
}

// FR-35: writeConcernError.code in an ok:1 reply.
func TestReplyScannerWriteConcernError_FR35(t *testing.T) {
	s, err := scan(reply(eDouble("ok", 1), eInt32("n", 1), eDoc("writeConcernError", doc(eInt32("code", 91), eString("codeName", "ShutdownInProgress"), eString("errmsg", "x")))), 6)
	if err != nil {
		t.Fatal(err)
	}
	f := s.Fields()
	if !f.HasWCECode || f.WCECode != 91 || !f.HasOK || f.OK != 1 || f.HasCode {
		t.Fatalf("fields: %+v", f)
	}
}

// AC-43 (unit): writeErrors are never read, so a code inside them is not
// reported, even next to an ok:1 reply.
func TestReplyScannerIgnoresWriteErrors_AC43(t *testing.T) {
	m := reply(eDouble("ok", 1), eInt32("n", 0), eArray("writeErrors",
		eDoc("0", doc(eInt32("index", 0), eInt32("code", 10107), eString("errmsg", "not primary"))),
		eDoc("1", doc(eInt32("code", 91)))))
	for _, chunk := range []int{3, 4096, len(m)} {
		s, err := scan(m, chunk)
		if err != nil {
			t.Fatal(err)
		}
		f := s.Fields()
		if f.HasCode || f.HasWCECode {
			t.Fatalf("chunk %d: a code was read from writeErrors: %+v", chunk, f)
		}
		if !f.HasOK || f.OK != 1 {
			t.Fatalf("chunk %d: ok lost: %+v", chunk, f)
		}
	}
}

// Only top-level fields count: a nested `code` or `ok` is not the reply's.
func TestReplyScannerTopLevelOnly_FR9(t *testing.T) {
	s, err := scan(reply(eDoc("cursor", doc(eInt32("code", 5), eDouble("ok", 0), eInt64("id", 3))), eDouble("ok", 1)), 5)
	if err != nil {
		t.Fatal(err)
	}
	f := s.Fields()
	if f.HasCode || f.OK != 1 {
		t.Fatalf("nested fields leaked into the top level: %+v", f)
	}
}

// FR-58: a reply with moreToCome is unexpected_response, reported from the
// flags (the first bytes), before any later check.
func TestReplyMoreToCome_FR58(t *testing.T) {
	m := opMsg(7, flagMore, sec0(doc(eDouble("ok", 1))))
	s := wire.NewReplyScanner(hdr(m))
	err := s.Write(m[16:])
	if err == nil {
		err = s.Close()
	}
	if detail(err) != "unexpected_response" {
		t.Fatalf("detail %q, want unexpected_response", detail(err))
	}
	// Detected on the flags alone: even a 4-byte first chunk fails.
	s = wire.NewReplyScanner(hdr(m))
	if err := s.Write(m[16:20]); detail(err) != "unexpected_response" {
		t.Fatalf("flags chunk: detail %q", detail(err))
	}
}

// FR-58: reply checksums are verified, and structure errors are malformed.
func TestReplyValidation_FR58(t *testing.T) {
	good := withCRC(reply(eDouble("ok", 1)), false)
	if _, err := scan(good, 3); err != nil {
		t.Fatalf("checksummed reply rejected: %v", err)
	}
	if _, err := scan(withCRC(reply(eDouble("ok", 1)), true), 3); detail(err) != "checksum_mismatch" {
		t.Fatalf("bad checksum: %q", detail(err))
	}
	bad := map[string][]byte{
		"invalid BSON":      opMsg(7, 0, sec0(doc(el(0x14, "ok", nil)))),
		"no body section":   opMsg(7, 0, sec1("x", doc())),
		"two body sections": opMsg(7, 0, sec0(doc(eDouble("ok", 1))), sec0(doc(eDouble("ok", 1)))),
		"unknown flag bit":  opMsg(7, 1<<4, sec0(doc(eDouble("ok", 1)))),
	}
	for name, m := range bad {
		if _, err := scan(m, 7); detail(err) != "malformed_message" {
			t.Errorf("%s: detail %q, want malformed_message", name, detail(err))
		}
	}
	// A reply is not subject to the client-side inspected-field rules.
	if _, err := scan(reply(eDouble("ok", 1), eString("$db", "a"), eString("$db", "b")), 9); err != nil {
		t.Fatalf("duplicate $db in a reply was refused: %v", err)
	}
}

// FR-5: a reply validator via NewMsgValidator(h, true) behaves like a
// scanner for flags.
func TestReplyValidatorMode_FR58(t *testing.T) {
	m := opMsg(7, flagMore, sec0(doc(eDouble("ok", 1))))
	v := wire.NewMsgValidator(hdr(m), true)
	if err := v.Write(m[16:]); detail(err) != "unexpected_response" {
		t.Fatalf("detail %q", detail(err))
	}
}

// Builders.

func parseMsg(t *testing.T, b []byte) (h wire.Header, flags uint32, body bson.Raw) {
	t.Helper()
	h = wire.ParseHeader(b)
	if int(h.Length) != len(b) || h.OpCode != wire.OpMsg {
		t.Fatalf("header %+v for %d bytes", h, len(b))
	}
	flags = binary.LittleEndian.Uint32(b[16:])
	if b[20] != 0 {
		t.Fatalf("section kind %d, want 0", b[20])
	}
	body = bson.Raw(b[21:])
	if err := body.Validate(); err != nil {
		t.Fatalf("body invalid: %v", err)
	}
	return
}

func str(t *testing.T, r bson.Raw, path ...string) string {
	t.Helper()
	v, err := r.LookupErr(path...)
	if err != nil {
		t.Fatalf("lookup %v: %v", path, err)
	}
	s, ok := v.StringValueOK()
	if !ok {
		t.Fatalf("%v is not a string", path)
	}
	return s
}

func TestBuildMsg_FR10(t *testing.T) {
	body := doc(eString("ping", ""), eString("$db", "admin"))
	b := wire.BuildMsg(11, 22, flagMore, body)
	h, flags, got := parseMsg(t, b)
	if h.RequestID != 11 || h.ResponseTo != 22 || flags != flagMore {
		t.Fatalf("header/flags: %+v %#x", h, flags)
	}
	if string(got) != string(body) {
		t.Fatal("body not carried byte for byte")
	}
}

// FR-8: OP_REPLY layout: responseFlags, cursorID 0, startingFrom 0,
// numberReturned 1, then the document.
func TestBuildReply_FR8(t *testing.T) {
	d := doc(eDouble("ok", 0), eInt32("code", 352))
	b := wire.BuildReply(100, 9, 2, d)
	h := wire.ParseHeader(b)
	if h.OpCode != wire.OpReply || h.RequestID != 100 || h.ResponseTo != 9 || int(h.Length) != len(b) {
		t.Fatalf("header %+v", h)
	}
	if int32(binary.LittleEndian.Uint32(b[16:])) != 2 {
		t.Fatal("responseFlags not carried")
	}
	if binary.LittleEndian.Uint64(b[20:]) != 0 {
		t.Fatal("cursorID not 0")
	}
	if binary.LittleEndian.Uint32(b[28:]) != 0 || binary.LittleEndian.Uint32(b[32:]) != 1 {
		t.Fatal("startingFrom/numberReturned wrong")
	}
	if string(b[36:]) != string(d) {
		t.Fatal("document not carried byte for byte")
	}
}

func TestOwnMessageBuilders_FR9(t *testing.T) {
	ping := bson.Raw(wire.PingBody())
	if err := ping.Validate(); err != nil {
		t.Fatal(err)
	}
	els, _ := ping.Elements()
	if len(els) != 2 || els[0].Key() != "ping" || els[1].Key() != "$db" || str(t, ping, "$db") != "admin" {
		t.Fatalf("PingBody keys wrong")
	}
	if _, err := ping.LookupErr("lsid"); err == nil {
		t.Fatal("PingBody carries an lsid")
	}

	hello := bson.Raw(wire.HelloBody("corral", "1.2.3", "linux"))
	if err := hello.Validate(); err != nil {
		t.Fatal(err)
	}
	if els, _ := hello.Elements(); els[0].Key() != "hello" {
		t.Fatalf("first key = %q, want hello", els[0].Key())
	}
	if str(t, hello, "client", "application", "name") != "corral" ||
		str(t, hello, "client", "driver", "name") != "corral" ||
		str(t, hello, "client", "driver", "version") != "1.2.3" ||
		str(t, hello, "client", "os", "type") != "linux" ||
		str(t, hello, "$db") != "admin" {
		t.Fatal("HelloBody metadata wrong")
	}

	start := bson.Raw(wire.SaslStartBody("admin", []byte("n,,n=u,r=nonce")))
	if err := start.Validate(); err != nil {
		t.Fatal(err)
	}
	if els, _ := start.Elements(); els[0].Key() != "saslStart" {
		t.Fatal("saslStart not first")
	}
	if str(t, start, "mechanism") != "SCRAM-SHA-256" || str(t, start, "$db") != "admin" {
		t.Fatal("saslStart mechanism/db wrong")
	}
	if v, err := start.LookupErr("payload"); err != nil || len(binData(v)) == 0 {
		t.Fatal("saslStart payload missing")
	}
	if v, err := start.LookupErr("options", "skipEmptyExchange"); err != nil || !v.Boolean() {
		t.Fatal("skipEmptyExchange not true")
	}

	cont := bson.Raw(wire.SaslContinueBody("admin", 7, []byte("c=biws,r=x,p=y")))
	if err := cont.Validate(); err != nil {
		t.Fatal(err)
	}
	if els, _ := cont.Elements(); els[0].Key() != "saslContinue" {
		t.Fatal("saslContinue not first")
	}
	if v, err := cont.LookupErr("conversationId"); err != nil || v.AsInt64() != 7 {
		t.Fatal("conversationId wrong")
	}
	if str(t, cont, "$db") != "admin" {
		t.Fatal("saslContinue db wrong")
	}
	// The builders' output is accepted by the validator when framed.
	for name, b := range map[string][]byte{"ping": wire.PingBody(), "hello": wire.HelloBody("a", "b", "c"), "start": wire.SaslStartBody("admin", []byte("p"))} {
		verdict(t, wire.BuildMsg(1, 0, 0, b), "")
		_ = name
	}
}

// Reply parsers read only the FR-9 own-exchange fields. The field names of
// HelloReply are fixed by this test (see the test-writer report).
func TestParseHelloReply_FR9(t *testing.T) {
	oid := [12]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	d := doc(
		eBool("isWritablePrimary", true), eBool("secondary", false), eString("setName", "rs0"),
		eArray("hosts", eString("0", "a:1"), eString("1", "b:2")),
		eArray("passives", eString("0", "c:3")),
		eString("primary", "a:1"),
		el(0x07, "electionId", oid[:]), eInt32("setVersion", 4),
		eInt32("maxBsonObjectSize", 16777216), eInt32("maxMessageSizeBytes", 48000000), eInt32("maxWriteBatchSize", 100000),
		eInt32("logicalSessionTimeoutMinutes", 30), eInt32("minWireVersion", 0), eInt32("maxWireVersion", 21),
		eString("me", "SYNTHETIC-ME-MARKER"), eDouble("ok", 1),
	)
	r, err := wire.ParseHelloReply(d)
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsWritablePrimary || r.Secondary || r.ArbiterOnly || r.IsMongos {
		t.Fatalf("role flags: %+v", r)
	}
	if r.SetName != "rs0" || !reflectStrings(r.Hosts, "a:1", "b:2") || !reflectStrings(r.Passives, "c:3") || r.Primary != "a:1" {
		t.Fatalf("topology fields: %+v", r)
	}
	if !r.HasElectionID || r.ElectionID != oid || !r.HasSetVersion || r.SetVersion != 4 {
		t.Fatalf("election: %+v", r)
	}
	if r.MaxBsonObjectSize != 16777216 || r.MaxMessageSizeBytes != 48000000 || r.MaxWriteBatchSize != 100000 || !r.HasLSTM || r.LogicalSessionTimeoutMinutes != 30 || r.MinWire != 0 || r.MaxWire != 21 {
		t.Fatalf("parameters: %+v", r)
	}
	if strings.Contains(fmt.Sprintf("%+v", r), "SYNTHETIC-ME-MARKER") {
		t.Fatal("a field outside the FR-9 list was kept")
	}
	// mongos and arbiter markers.
	r, err = wire.ParseHelloReply(doc(eBool("isWritablePrimary", true), eString("msg", "isdbgrid"), eInt32("minWireVersion", 0), eInt32("maxWireVersion", 25), eDouble("ok", 1)))
	if err != nil || !r.IsMongos || r.HasLSTM {
		t.Fatalf("mongos: %+v %v", r, err)
	}
	r, err = wire.ParseHelloReply(doc(eBool("arbiterOnly", true), eString("setName", "rs0"), eDouble("ok", 1)))
	if err != nil || !r.ArbiterOnly {
		t.Fatalf("arbiter: %+v %v", r, err)
	}
	// An ok:0 reply, or a reply with no ok, is an error.
	if _, err := wire.ParseHelloReply(doc(eDouble("ok", 0), eInt32("code", 13), eString("errmsg", "x"))); err == nil {
		t.Fatal("ok:0 hello reply accepted")
	}
	if _, err := wire.ParseHelloReply([]byte{1, 2, 3}); err == nil {
		t.Fatal("garbage accepted")
	}
}

func reflectStrings(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestParseSaslReply_FR22(t *testing.T) {
	r, err := wire.ParseSaslReply(doc(eInt32("conversationId", 3), eBinary("payload", 0, []byte("v=abc")), eBool("done", true), eDouble("ok", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if r.ConversationID != 3 || string(r.Payload) != "v=abc" || !r.Done {
		t.Fatalf("reply: %+v", r)
	}
	r, err = wire.ParseSaslReply(doc(eInt32("conversationId", 1), eBinary("payload", 0, []byte("r=x")), eBool("done", false), eDouble("ok", 1)))
	if err != nil || r.Done {
		t.Fatalf("not-done reply: %+v %v", r, err)
	}
	// An error reply carries code and codeName (authentication failure, FR-22).
	r, err = wire.ParseSaslReply(doc(eDouble("ok", 0), eInt32("code", 18), eString("codeName", "AuthenticationFailed"), eString("errmsg", "SYNTHETIC-ERRMSG")))
	if err != nil {
		t.Fatalf("an error reply must parse so the caller can read the code: %v", err)
	}
	if r.Code != 18 || r.CodeName != "AuthenticationFailed" {
		t.Fatalf("code/name: %+v", r)
	}
	if strings.Contains(fmt.Sprintf("%+v", r), "SYNTHETIC-ERRMSG") {
		t.Fatal("errmsg kept in SaslReply")
	}
	if _, err := wire.ParseSaslReply([]byte{9}); err == nil {
		t.Fatal("garbage accepted")
	}
}

func binData(v bson.RawValue) []byte { _, d := v.Binary(); return d }

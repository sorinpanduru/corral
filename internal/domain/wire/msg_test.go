package wire_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/wire"
)

// AC-4 (unit): OP_MSG validation table with exact details.
func TestMsgValidation_AC4(t *testing.T) {
	body := doc(eString("find", "c"), eString("$db", "d"))
	ins := doc(eString("insert", "c"), eString("$db", "d"))
	docs := sec1("documents", doc(eInt32("_id", 1)), doc(eInt32("_id", 2)))

	good := map[string][]byte{
		"plain":                       opMsg(1, 0, sec0(body)),
		"moreToCome":                  opMsg(1, flagMore, sec0(body)),
		"exhaustAllowed":              opMsg(1, flagExhaust, sec0(body)),
		"optional bit 17":             opMsg(1, 1<<17, sec0(body)),
		"optional bit 31":             opMsg(1, 1<<31, sec0(body)),
		"kind-1 after body":           opMsg(1, 0, sec0(ins), docs),
		"kind-1 before body":          opMsg(1, 0, docs, sec0(ins)),
		"two kind-1 sections":         opMsg(1, 0, sec0(ins), docs, sec1("other", doc())),
		"empty kind-1 section":        opMsg(1, 0, sec0(ins), sec1("documents")),
		"checksummed":                 withCRC(opMsg(1, 0, sec0(body)), false),
		"checksummed with kind-1":     withCRC(opMsg(1, 0, sec0(ins), docs), false),
		"checksummed with more":       withCRC(opMsg(1, flagMore, sec0(body)), false),
		"unicode and invalid UTF-8":   opMsg(1, 0, sec0(doc(eString("find", "c"), eString("note", "\xff\xfe\xc3(")))),
		"duplicate non-inspected":     opMsg(1, 0, sec0(doc(eString("find", "c"), eString("comment", "a"), eString("comment", "b")))),
		"bodies with every BSON kind": opMsg(1, 0, sec0(doc(eString("find", "c"), eDouble("d", 1), eArray("a", eInt32("0", 1)), eBinary("b", 0, []byte{1}), eBool("t", true), eNull("n"), eInt64("l", 2)))),
	}
	for name, m := range good {
		t.Run("good/"+name, func(t *testing.T) { verdict(t, m, "") })
	}

	bad := map[string]struct {
		msg  []byte
		want string
	}{
		"flag bit 2":                    {opMsg(1, 1<<2, sec0(body)), "malformed_message"},
		"flag bit 3":                    {opMsg(1, 1<<3, sec0(body)), "malformed_message"},
		"flag bit 8":                    {opMsg(1, 1<<8, sec0(body)), "malformed_message"},
		"flag bit 15":                   {opMsg(1, 1<<15, sec0(body)), "malformed_message"},
		"all required bits":             {opMsg(1, 0xFFFC, sec0(body)), "malformed_message"},
		"bad CRC":                       {withCRC(opMsg(1, 0, sec0(body)), true), "checksum_mismatch"},
		"bad CRC with kind-1":           {withCRC(opMsg(1, 0, sec0(ins), docs), true), "checksum_mismatch"},
		"unknown flag plus bad CRC":     {withCRC(opMsg(1, 1<<5, sec0(body)), true), "malformed_message"},
		"two kind-0 sections":           {opMsg(1, 0, sec0(body), sec0(body)), "malformed_message"},
		"no kind-0 section":             {opMsg(1, 0, docs), "malformed_message"},
		"no sections":                   {opMsg(1, 0), "malformed_message"},
		"unknown section kind 2":        {opMsg(1, 0, sec0(body), []byte{2, 5, 0, 0, 0}), "malformed_message"},
		"unknown section kind 255":      {opMsg(1, 0, []byte{0xFF}, sec0(body)), "malformed_message"},
		"truncated kind-0":              {opMsg(1, 0, sec0(body)[:len(sec0(body))-3]), "malformed_message"},
		"kind-0 length overruns":        {opMsg(1, 0, cat([]byte{0}, le32(500), []byte{0x10, 'a', 0, 1, 0, 0, 0, 0})), "malformed_message"},
		"truncated kind-1 header":       {opMsg(1, 0, sec0(ins), []byte{1, 9, 0}), "malformed_message"},
		"kind-1 size below 4":           {opMsg(1, 0, sec0(ins), cat([]byte{1}, le32(3))), "malformed_message"},
		"kind-1 size overruns":          {opMsg(1, 0, sec0(ins), cat([]byte{1}, le32(900), cstr("documents"))), "malformed_message"},
		"kind-1 id unterminated":        {opMsg(1, 0, sec0(ins), cat([]byte{1}, le32(8), []byte("abcd"))), "malformed_message"},
		"kind-1 inner doc overruns":     {opMsg(1, 0, sec0(ins), cat([]byte{1}, le32(4+10+4), cstr("documents"), le32(100))), "malformed_message"},
		"kind-1 invalid doc":            {opMsg(1, 0, sec0(ins), sec1("documents", doc(el(0x14, "a", nil)))), "malformed_message"},
		"kind-1 trailing bytes in size": {opMsg(1, 0, sec0(ins), sec1("documents", doc(), []byte{7})), "malformed_message"},
		"invalid BSON body":             {opMsg(1, 0, sec0(doc(el(0x14, "find", nil)))), "malformed_message"},
		"bool byte 2 in body":           {opMsg(1, 0, sec0(doc(eString("find", "c"), el(0x08, "b", []byte{2})))), "malformed_message"},
		"nested invalid in body":        {opMsg(1, 0, sec0(doc(eString("find", "c"), eDoc("f", doc(el(0x14, "x", nil)))))), "malformed_message"},
		"empty body (no command)":       {opMsg(1, 0, sec0(doc())), "malformed_message"},
		"body shorter than 5":           {opMsg(1, 0, cat([]byte{0}, []byte{4, 0, 0, 0})), "malformed_message"},
		"nothing after the flags":       {opMsg(1, 0, []byte{0}), "malformed_message"},
	}
	for name, tc := range bad {
		t.Run("bad/"+name, func(t *testing.T) { verdict(t, tc.msg, tc.want) })
	}
}

// Bytes beyond the header's declared length are an error; a short message is
// malformed at Close.
func TestMsgLengthConsistency_FR5(t *testing.T) {
	msg := cmdMsg(eString("find", "c"), eString("$db", "d"))
	v := wire.NewMsgValidator(hdr(msg), false)
	extra := append(append([]byte(nil), msg[16:]...), 0)
	if err := v.Write(extra); err == nil {
		if err := v.Close(); err == nil {
			t.Fatal("bytes beyond the declared length were accepted")
		}
	}
	short := wire.NewMsgValidator(hdr(msg), false)
	if err := short.Write(msg[16 : len(msg)-2]); err != nil {
		t.Fatalf("a valid prefix was rejected: %v", err)
	}
	if err := short.Close(); detail(err) != "malformed_message" {
		t.Fatalf("Close on a short message: detail %q", detail(err))
	}
}

// Done() is false until Close succeeds and every check has passed.
func TestMsgDone_FR38(t *testing.T) {
	msg := withCRC(cmdMsg(eString("find", "c"), eString("$db", "d")), false)
	v := wire.NewMsgValidator(hdr(msg), false)
	if v.Done() {
		t.Fatal("Done() before any byte")
	}
	body := msg[16:]
	if err := v.Write(body[:len(body)-1]); err != nil {
		t.Fatal(err)
	}
	if v.Done() {
		t.Fatal("Done() with the last byte missing")
	}
	if err := v.Write(body[len(body)-1:]); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if !v.Done() {
		t.Fatal("Done() = false after a clean Close")
	}
}

// Errors are sticky: after a failing Write every later Write fails too.
func TestMsgErrorSticky(t *testing.T) {
	msg := opMsg(1, 1<<2, sec0(doc(eString("find", "c"))))
	v := wire.NewMsgValidator(hdr(msg), false)
	if err := v.Write(msg[16:]); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if err := v.Write([]byte{0}); err == nil {
		t.Fatal("Write after failure succeeded")
	}
	if err := v.Close(); err == nil {
		t.Fatal("Close after failure succeeded")
	}
}

// FR-60 / AC-44 (unit part): inspected-field integrity and structure.
func TestInspectedFieldIntegrity_AC44(t *testing.T) {
	pipe := eArray("pipeline", eDoc("0", doc(eDoc("$match", doc()))))
	cur := eArray("pipeline", eDoc("0", doc(eDoc("$currentOp", doc()))))
	agg := func(extra ...[]byte) []byte {
		return cat(eString("aggregate", "c"), cat(extra...))
	}
	pipeDoc := doc(eDoc("$match", doc()))

	bad := map[string][]byte{
		"aggregate with kind-1 pipeline, section after body":  opMsg(1, 0, sec0(doc(agg(eString("$db", "d")))), sec1("pipeline", pipeDoc)),
		"aggregate with kind-1 pipeline, section before body": opMsg(1, 0, sec1("pipeline", pipeDoc), sec0(doc(agg(eString("$db", "d"))))),
		"aggregate with both body and kind-1 pipeline":        opMsg(1, 0, sec0(doc(agg(pipe, eString("$db", "d")))), sec1("pipeline", pipeDoc)),
		"two top-level pipeline fields":                       opMsg(1, 0, sec0(doc(agg(pipe, cur, eString("$db", "d"))))),
		"two pipeline fields, currentOp second":               opMsg(1, 0, sec0(doc(agg(pipe, eString("$db", "d"), cur)))),
		"two $db fields":                                      opMsg(1, 0, sec0(doc(eString("find", "c"), eString("$db", "a"), eString("$db", "b")))),
		"two $db, different types":                            opMsg(1, 0, sec0(doc(eString("find", "c"), eString("$db", "a"), eInt32("$db", 1)))),
		"repeated command name":                               opMsg(1, 0, sec0(doc(eString("find", "a"), eString("$db", "d"), eString("find", "b")))),
		"repeated command name, adjacent":                     opMsg(1, 0, sec0(doc(eString("find", "a"), eString("find", "b")))),
		"repeated command name, different type":               opMsg(1, 0, sec0(doc(eString("find", "a"), eInt32("find", 1)))),
		"two lsid":                                            opMsg(1, 0, sec0(doc(eString("find", "c"), eDoc("lsid", doc(eBinary("id", 4, make([]byte, 16)))), eDoc("lsid", doc())))),
		"two txnNumber":                                       opMsg(1, 0, sec0(doc(eString("find", "c"), eInt64("txnNumber", 1), eInt64("txnNumber", 2)))),
		"two autocommit":                                      opMsg(1, 0, sec0(doc(eString("find", "c"), eBool("autocommit", false), eBool("autocommit", true)))),
		"two startTransaction":                                opMsg(1, 0, sec0(doc(eString("find", "c"), eBool("startTransaction", true), eBool("startTransaction", true)))),
		"getMore with two collection fields":                  opMsg(1, 0, sec0(doc(eInt64("getMore", 5), eString("collection", "a"), eString("collection", "b"), eString("$db", "d")))),
		"kind-1 named $db":                                    opMsg(1, 0, sec0(doc(eString("insert", "c"))), sec1("$db", doc())),
		"kind-1 named lsid":                                   opMsg(1, 0, sec0(doc(eString("insert", "c"))), sec1("lsid", doc())),
		"kind-1 named txnNumber":                              opMsg(1, 0, sec0(doc(eString("insert", "c"))), sec1("txnNumber", doc())),
		"kind-1 named autocommit":                             opMsg(1, 0, sec0(doc(eString("insert", "c"))), sec1("autocommit", doc())),
		"kind-1 named startTransaction":                       opMsg(1, 0, sec0(doc(eString("insert", "c"))), sec1("startTransaction", doc())),
		"getMore with kind-1 collection":                      opMsg(1, 0, sec0(doc(eInt64("getMore", 5), eString("$db", "d"))), sec1("collection", doc())),
		"kind-1 inner length overrun":                         opMsg(1, 0, sec0(doc(eString("insert", "c"))), cat([]byte{1}, le32(4+10+4), cstr("documents"), le32(100))),
		"1025 levels in body":                                 opMsg(1, 0, sec0(doc(eString("find", "c"), eDoc("n", nested(1024, false))))),
		"1025 levels of arrays in body":                       opMsg(1, 0, sec0(doc(eString("find", "c"), el(0x04, "n", nested(1024, true))))),
		"1025 levels in a kind-1 document":                    opMsg(1, 0, sec0(doc(eString("insert", "c"))), sec1("documents", nested(1025, false))),
	}
	for name, m := range bad {
		t.Run("bad/"+name, func(t *testing.T) { verdict(t, m, "malformed_message") })
	}

	good := map[string][]byte{
		"insert with kind-1 documents":                    opMsg(1, 0, sec0(doc(eString("insert", "c"), eString("$db", "d"))), sec1("documents", doc(eInt32("_id", 1)))),
		"invalid UTF-8 in a field that is not inspected":  opMsg(1, 0, sec0(doc(eString("find", "c"), eString("filterNote", "\xff\xfe")))),
		"duplicate keys in a field that is not inspected": opMsg(1, 0, sec0(doc(eString("find", "c"), eString("x", "a"), eString("x", "b"), eDoc("o", doc(eInt32("k", 1), eInt32("k", 2)))))),
		"duplicated collection on find":                   opMsg(1, 0, sec0(doc(eString("find", "c"), eString("collection", "a"), eString("collection", "b")))),
		"duplicated pipeline on find":                     opMsg(1, 0, sec0(doc(eString("find", "c"), pipe, pipe))),
		"kind-1 pipeline on find":                         opMsg(1, 0, sec0(doc(eString("find", "c"))), sec1("pipeline", doc())),
		"kind-1 collection on find":                       opMsg(1, 0, sec0(doc(eString("find", "c"))), sec1("collection", doc())),
		"kind-1 named like the command (A-Q1)":            opMsg(1, 0, sec0(doc(eString("insert", "c"))), sec1("insert", doc(eInt32("_id", 1)))),
		"kind-1 named like another command":               opMsg(1, 0, sec0(doc(eString("find", "c"))), sec1("find", doc())),
		"nested $db and lsid duplicates are fine":         opMsg(1, 0, sec0(doc(eString("find", "c"), eDoc("o", doc(eString("$db", "a"), eString("$db", "b"), eString("find", "x"), eString("find", "y")))))),
		"duplicate inside lsid document":                  opMsg(1, 0, sec0(doc(eString("find", "c"), eDoc("lsid", doc(eBinary("id", 4, make([]byte, 16)), eInt32("id", 1)))))),
		"aggregate with a single pipeline":                opMsg(1, 0, sec0(doc(agg(pipe, eString("$db", "d"))))),
		"aggregate:1 with a single pipeline":              opMsg(1, 0, sec0(doc(eInt32("aggregate", 1), pipe, eString("$db", "d")))),
	}
	good["1024 levels in body"] = opMsg(1, 0, sec0(doc(eString("find", "c"), eDoc("n", nested(1023, false)))))
	good["1024 levels of arrays in body"] = opMsg(1, 0, sec0(doc(eString("find", "c"), el(0x04, "n", nested(1023, true)))))
	good["1024 levels in a kind-1 document"] = opMsg(1, 0, sec0(doc(eString("insert", "c"))), sec1("documents", nested(1024, false)))
	// R-6: the body and each kind-1 document are counted from level 1 on their
	// own; they do not add up.
	good["R-6 body 1024 + kind-1 1024"] = opMsg(1, 0, sec0(doc(eString("insert", "c"), eDoc("n", nested(1023, false)))), sec1("documents", nested(1024, false), nested(1024, true)))
	for name, m := range good {
		t.Run("good/"+name, func(t *testing.T) { verdict(t, m, "") })
	}
}

// The depth boundary counts the body's top-level document as level 1: a body
// of exactly 1024 levels is accepted and 1025 is rejected (FR-60).
func TestBodyDepthBoundary_FR60(t *testing.T) {
	// Build a body whose first element is a command string, so only the nested
	// element deepens. Body depth = 1 + depth(nested element value).
	ok := opMsg(1, 0, sec0(doc(eString("find", "c"), eDoc("n", nested(1023, false)))))
	tooDeep := opMsg(1, 0, sec0(doc(eString("find", "c"), eDoc("n", nested(1024, false)))))
	verdict(t, ok, "")
	verdict(t, tooDeep, "malformed_message")
}

// AC-49 (unit part): for a 5 MB aggregate whose second pipeline is in the last
// kilobytes, Write fails only on the chunk containing it and Done() is false;
// nothing earlier is rejected.
func TestLateDuplicateFailsOnItsChunk_AC49(t *testing.T) {
	pipe := eArray("pipeline", eDoc("0", doc(eDoc("$match", doc()))))
	big := strings.Repeat("x", 5<<20)
	m := opMsg(1, 0, sec0(doc(eString("aggregate", "c"), pipe, eString("pad", big), eString("$db", "d"), pipe)))
	// Offset (in the body after the header) at which the second pipeline key's
	// terminating NUL arrives.
	key := append([]byte{0x04}, cstr("pipeline")...)
	at := bytes.LastIndex(m, key) + len(key) // first byte after the key
	if at < len(m)-4096 {
		t.Fatalf("test setup: second pipeline is not near the end")
	}
	body := m[16:]
	v := wire.NewMsgValidator(hdr(m), false)
	const chunk = 4096
	failedAt := -1
	for off := 0; off < len(body); off += chunk {
		end := off + chunk
		if end > len(body) {
			end = len(body)
		}
		if err := v.Write(body[off:end]); err != nil {
			if detail(err) != "malformed_message" {
				t.Fatalf("detail %q", detail(err))
			}
			failedAt = off
			// The failing chunk must contain the end of the second key.
			if !(off+16 <= at && at <= end+16) {
				t.Fatalf("failed in chunk [%d,%d) but the second key ends at %d", off+16, end+16, at)
			}
			break
		}
		if v.Done() {
			t.Fatal("Done() true before the end")
		}
	}
	if failedAt < 0 {
		t.Fatal("no Write failed for the duplicate pipeline")
	}
	if v.Done() {
		t.Fatal("Done() true after a failure")
	}
}

// AC-49 (unit part): with a wrong checksum, Close returns checksum_mismatch
// and Done() is false; no Write fails earlier with a different detail.
func TestBadChecksumAtClose_AC49(t *testing.T) {
	m := withCRC(opMsg(1, 0, sec0(doc(eString("find", "c"), eString("pad", strings.Repeat("y", 1<<20))))), true)
	v := wire.NewMsgValidator(hdr(m), false)
	body := m[16:]
	var first error
	for off := 0; off < len(body) && first == nil; off += 8192 {
		end := off + 8192
		if end > len(body) {
			end = len(body)
		}
		first = v.Write(body[off:end])
	}
	if first == nil {
		first = v.Close()
	}
	if detail(first) != "checksum_mismatch" {
		t.Fatalf("detail %q, want checksum_mismatch", detail(first))
	}
	if v.Done() {
		t.Fatal("Done() true with a wrong checksum")
	}
}

// Validation is the same at every chunking for a mix of valid and invalid
// messages (random chunk sizes over a large message).
func TestChunkingIndependent_AC7(t *testing.T) {
	big := withCRC(opMsg(1, 0, sec0(doc(eString("insert", "c"), eString("$db", "d"), eString("pad", strings.Repeat("z", 3<<20)))), sec1("documents", doc(eInt32("_id", 1)))), false)
	bad := withCRC(opMsg(1, 0, sec0(doc(eString("insert", "c"), eString("$db", "d"), eString("pad", strings.Repeat("z", 3<<20)), eString("$db", "e")))), false)
	for name, tc := range map[string]struct {
		m    []byte
		want string
	}{"valid": {big, ""}, "dup $db at the end": {bad, "malformed_message"}} {
		for _, chunk := range []int{1 << 10, 4099, 1 << 16, len(tc.m)} {
			err, v := run(tc.m, chunk)
			if (err == nil) != (tc.want == "") || detail(err) != tc.want || (v.Done() != (tc.want == "")) {
				t.Fatalf("%s chunk %d: err=%v detail=%q done=%v", name, chunk, err, detail(err), v.Done())
			}
		}
	}
}

// Messages can be validated as the buffered prefix then streamed chunks: any
// split point gives the same verdict (small messages, every offset).
func TestEverySplit_AC7(t *testing.T) {
	msgs := map[string][]byte{
		"valid":     withCRC(opMsg(1, 0, sec0(doc(eString("insert", "c"), eString("$db", "d"))), sec1("documents", doc(eInt32("_id", 1)))), false),
		"bad crc":   withCRC(cmdMsg(eString("find", "c")), true),
		"dup $db":   cmdMsg(eString("find", "c"), eString("$db", "a"), eString("$db", "b")),
		"flag":      opMsg(1, 1<<4, sec0(doc(eString("find", "c")))),
		"kind-1 id": opMsg(1, 0, sec0(doc(eString("insert", "c"))), sec1("lsid", doc())),
	}
	for name, m := range msgs {
		wantErr, _ := run(m, len(m))
		for i := 0; i <= len(m)-16; i++ {
			v := wire.NewMsgValidator(hdr(m), false)
			var got error
			for _, part := range [][]byte{m[16 : 16+i], m[16+i:]} {
				if got == nil {
					got = v.Write(part)
				}
			}
			if got == nil {
				got = v.Close()
			}
			if (got == nil) != (wantErr == nil) || detail(got) != detail(wantErr) {
				t.Fatalf("%s split %d: got %q, want %q", name, i, detail(got), detail(wantErr))
			}
		}
	}
}

// Messages are never echoed into errors (FR-9, SPEC-001 FR-24).
func TestErrorsCarryNoMessageBytes_FR9(t *testing.T) {
	marker := "SYNTHETIC-MARKER-5d2c"
	m := cmdMsg(eString("find", marker), eString("$db", marker), eString("$db", marker))
	err, _ := run(m, len(m))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), marker) || strings.Contains(fmt.Sprintf("%+v %#v", err, err), marker) {
		t.Fatal("error contains message bytes")
	}
}

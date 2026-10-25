package wire_test

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/wire"
)

// Security regression (wave 2 veto): kind-1 (document sequence) sections.
//  1. Huge and negative length fields never panic, on 32-bit as well
//     (GOARCH=386 go test ./internal/domain/wire, covered by make test-386),
//     and give malformed_message.
//  2. ClassifyPrefix accepts messages with kind-1 sections after the body
//     (insert documents, update updates, delete deletes, bulkWrite ops and
//     nsInfo) and before it, and classifies them from the body.

var hugeSizes = []uint32{
	0x7FFFFFFF, 0x7FFFFFFE, 0x7FFFFFFD, 0x7FFFFFFC, 0x7FFFFFFB, 0x7FFFFFF0, 0x40000000,
	0x80000000, 0x80000001, 0xFFFFFFFF, 0xFFFFFFFE, 0xFFFFFFFB, 0xFFFFFFFC, 0, 1, 2, 3, 4,
}

func rawSec1(size uint32, id string, payload ...[]byte) []byte {
	return cat([]byte{1}, le32(int32(size)), cstr(id), cat(payload...))
}

func safeClassify(t *testing.T, label string, msg []byte, total int32) (wire.Request, error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s: ClassifyPrefix panicked: %v", label, r)
		}
	}()
	return wire.ClassifyPrefix(msg, total)
}

func safeValidate(t *testing.T, label string, msg []byte, chunk int) error {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s: MsgValidator panicked: %v", label, r)
		}
	}()
	err, _ := run(msg, chunk)
	return err
}

var insBody = doc(eString("insert", "coll"), eString("$db", "shop"))

// A kind-1 section whose size field is huge, negative or too small is
// malformed_message, in both section orders, through ClassifyPrefix and the
// MsgValidator, with no panic.
func TestKind1SizeFieldsHugeAndNegative_32bit(t *testing.T) {
	for _, size := range hugeSizes {
		after := opMsg(1, 0, sec0(insBody), rawSec1(size, "documents", doc(eInt32("_id", 1))))
		before := opMsg(1, 0, rawSec1(size, "documents", doc(eInt32("_id", 1))), sec0(insBody))
		for name, msg := range map[string][]byte{"after": after, "before": before} {
			label := fmt.Sprintf("%s size %#x", name, size)
			_, err := safeClassify(t, label, msg, int32(len(msg)))
			if name == "before" {
				if detail(err) != "malformed_message" {
					t.Errorf("%s: ClassifyPrefix detail %q, want malformed_message", label, detail(err))
				}
			} else if err != nil && detail(err) != "malformed_message" {
				// After the body, the kind-1 size is not needed for classification;
				// either the message classifies or it is malformed. It must never
				// be a different failure.
				t.Errorf("%s: ClassifyPrefix detail %q", label, detail(err))
			}
			if got := detail(safeValidate(t, label, msg, len(msg))); got != "malformed_message" {
				t.Errorf("%s: MsgValidator detail %q, want malformed_message", label, got)
			}
			if got := detail(safeValidate(t, label+" bytewise", msg, 1)); got != "malformed_message" {
				t.Errorf("%s: MsgValidator (bytewise) detail %q", label, got)
			}
			// A lying total (as 32-bit int would wrap) does not panic either.
			for _, total := range []int32{0x7FFFFFFF, -1, 16, 20, 0} {
				_, _ = safeClassify(t, label+" total", msg, total)
			}
		}
	}
}

// Inner document lengths inside a kind-1 section, huge or negative.
func TestKind1InnerDocLengths_32bit(t *testing.T) {
	for _, l := range hugeSizes {
		inner := cat(le32(int32(l)), []byte{0x10, 'a', 0, 1, 0, 0, 0, 0})
		secSize := uint32(4 + len("documents") + 1 + len(inner))
		for name, msg := range map[string][]byte{
			"after":  opMsg(1, 0, sec0(insBody), rawSec1(secSize, "documents", inner)),
			"before": opMsg(1, 0, rawSec1(secSize, "documents", inner), sec0(insBody)),
		} {
			label := fmt.Sprintf("%s inner %#x", name, l)
			_, _ = safeClassify(t, label, msg, int32(len(msg)))
			if got := detail(safeValidate(t, label, msg, len(msg))); got != "malformed_message" {
				t.Errorf("%s: MsgValidator detail %q, want malformed_message", label, got)
			}
		}
	}
}

// Every 4-byte field of a set of valid messages overwritten by every huge
// value: no input may panic, and any failure is one of the protocol details.
func TestLengthFieldSweepNeverPanics_32bit(t *testing.T) {
	bases := map[string][]byte{
		"insert body then kind-1": opMsg(1, 0, sec0(doc(eString("insert", "c"), eString("$db", "d"))), sec1("documents", doc(eInt32("_id", 1)), doc(eString("s", "abc")))),
		"kind-1 then body":        opMsg(1, 0, sec1("documents", doc(eInt32("_id", 1))), sec0(doc(eString("insert", "c"), eString("$db", "d")))),
		"checksummed":             withCRC(opMsg(1, 0, sec0(doc(eString("find", "c"))), sec1("x", doc())), false),
		"find with strings":       cmdMsg(eString("find", "c"), eString("$db", "d"), eBinary("b", 4, make([]byte, 16)), eDoc("lsid", doc(eBinary("id", 4, make([]byte, 16))))),
		"aggregate":               cmdMsg(eString("aggregate", "c"), eArray("pipeline", eDoc("0", doc(eDoc("$match", doc())))), eString("$db", "d")),
		"hello":                   cmdMsg(eInt32("hello", 1), eDoc("client", doc(eDoc("driver", doc(eString("name", "n"), eString("version", "v")))))),
		"op_query":                opQuery(1, 0, "admin.$cmd", 0, -1, doc(eInt32("isMaster", 1)), doc(eInt32("a", 1))),
		"op_query wrapped":        opQuery(1, 0, "admin.$cmd", 0, -1, doc(eDoc("$query", doc(eInt32("hello", 1))))),
	}
	allowedDetail := map[string]bool{"": true, "malformed_message": true, "message_too_large": true, "unsupported_opcode": true, "checksum_mismatch": true, "unexpected_response": true}
	for name, base := range bases {
		for off := 0; off+4 <= len(base); off++ {
			for _, v := range hugeSizes {
				msg := append([]byte(nil), base...)
				binary.LittleEndian.PutUint32(msg[off:], v)
				label := fmt.Sprintf("%s off %d val %#x", name, off, v)
				if _, err := safeClassify(t, label, msg, int32(len(msg))); !allowedDetail[detail(err)] {
					t.Fatalf("%s: ClassifyPrefix detail %q", label, detail(err))
				}
				if off >= 16 && len(msg) >= 16 {
					for _, chunk := range []int{len(msg), 5} {
						if err := safeValidate(t, label, msg, chunk); !allowedDetail[detail(err)] {
							t.Fatalf("%s: MsgValidator detail %q", label, detail(err))
						}
					}
				}
			}
		}
	}
}

// ---- body then kind-1 (and kind-1 then body) classify from the body ----

type k1case struct {
	name    string
	cmd     string
	coll    string
	hasColl bool
	body    []byte
	seqs    [][]byte
}

func k1cases() []k1case {
	lsid := eDoc("lsid", doc(eBinary("id", 4, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})))
	return []k1case{
		{"insert", "insert", "orders", true, doc(eString("insert", "orders"), eString("$db", "shop"), lsid, eInt64("txnNumber", 3)),
			[][]byte{sec1("documents", doc(eInt32("_id", 1)), doc(eInt32("_id", 2)))}},
		{"update", "update", "orders", true, doc(eString("update", "orders"), eString("$db", "shop")),
			[][]byte{sec1("updates", doc(eDoc("q", doc()), eDoc("u", doc(eDoc("$set", doc(eInt32("a", 1)))))))}},
		{"delete", "delete", "orders", true, doc(eString("delete", "orders"), eString("$db", "shop")),
			[][]byte{sec1("deletes", doc(eDoc("q", doc()), eInt32("limit", 0)))}},
		{"bulkWrite", "bulkWrite", "", false, doc(eInt32("bulkWrite", 1), eString("$db", "admin")),
			[][]byte{sec1("ops", doc(eInt32("insert", 0), eDoc("document", doc(eInt32("_id", 1))))), sec1("nsInfo", doc(eString("ns", "shop.orders")))}},
		{"insert with empty sequence", "insert", "c", true, doc(eString("insert", "c"), eString("$db", "d")), [][]byte{sec1("documents")}},
	}
}

func TestClassifyBodyThenKind1_FR7(t *testing.T) {
	for _, c := range k1cases() {
		t.Run(c.name, func(t *testing.T) {
			msg := opMsg(9, 0, append([][]byte{sec0(c.body)}, c.seqs...)...)
			r, err := wire.ClassifyPrefix(msg, int32(len(msg)))
			if err != nil {
				t.Fatalf("ClassifyPrefix rejected a valid body-then-kind-1 message: %v (%q)", err, detail(err))
			}
			if r.Kind != wire.KindOpMsg || r.Command != c.cmd || r.RequestID != 9 {
				t.Fatalf("request: %+v", r)
			}
			if r.HasCollection != c.hasColl || r.Collection != c.coll {
				t.Fatalf("collection %q (%v), want %q (%v)", r.Collection, r.HasCollection, c.coll, c.hasColl)
			}
			if !r.FitsPrefix {
				t.Fatal("FitsPrefix false")
			}
			// The inspected body fields are still read.
			if r.DB == "" {
				t.Fatal("$db not read")
			}
			// The same message validates cleanly.
			verdict(t, msg, "")
			// With a checksum.
			cm := withCRC(msg, false)
			if _, err := wire.ClassifyPrefix(cm, int32(len(cm))); err != nil {
				t.Fatalf("checksummed message: %v", err)
			}
			// Flags (moreToCome, exhaustAllowed) do not change it.
			fm := opMsg(9, flagMore|flagExhaust, append([][]byte{sec0(c.body)}, c.seqs...)...)
			if r2, err := wire.ClassifyPrefix(fm, int32(len(fm))); err != nil || r2.Command != c.cmd || r2.Flags != flagMore|flagExhaust {
				t.Fatalf("flags: %+v %v", r2, err)
			}
		})
	}
}

func TestClassifyKind1ThenBody_FR7(t *testing.T) {
	for _, c := range k1cases() {
		t.Run(c.name, func(t *testing.T) {
			secs := append(append([][]byte(nil), c.seqs...), sec0(c.body))
			msg := opMsg(9, 0, secs...)
			r, err := wire.ClassifyPrefix(msg, int32(len(msg)))
			if err != nil {
				t.Fatalf("ClassifyPrefix rejected kind-1-before-body: %v (%q)", err, detail(err))
			}
			if r.Command != c.cmd || r.HasCollection != c.hasColl || r.Collection != c.coll || r.DB == "" {
				t.Fatalf("request: %+v", r)
			}
			verdict(t, msg, "")
		})
	}
	// A kind-1 section between the body's neighbours: kind-1, body, kind-1.
	c := k1cases()[0]
	msg := opMsg(9, 0, sec1("documents", doc(eInt32("_id", 1))), sec0(c.body), sec1("other", doc()))
	if r, err := wire.ClassifyPrefix(msg, int32(len(msg))); err != nil || r.Command != "insert" {
		t.Fatalf("kind-1, body, kind-1: %+v %v", r, err)
	}
}

// A large kind-1 section after the body: the body is inside the prefix, so the
// message classifies; it just does not fit the prefix. Forwarding streams the
// rest.
func TestClassifyLargeKind1AfterBody_FR38(t *testing.T) {
	big := doc(eString("blob", strings.Repeat("z", 200_000)))
	msg := opMsg(9, 0, sec0(insBody), sec1("documents", big))
	r, err := classify(t, msg)
	if err != nil {
		t.Fatalf("large body-then-kind-1 rejected: %v (%q)", err, detail(err))
	}
	if r.FitsPrefix || r.Command != "insert" || !r.HasCollection || r.Collection != "coll" {
		t.Fatalf("request: %+v", r)
	}
}

// A kind-1 section before the body that pushes the body (the command name)
// outside the prefix is malformed_message: deciding fields must fit (FR-38).
func TestClassifyLargeKind1BeforeBody_FR38(t *testing.T) {
	big := doc(eString("blob", strings.Repeat("z", 200_000)))
	msg := opMsg(9, 0, sec1("documents", big), sec0(insBody))
	if _, err := classify(t, msg); detail(err) != "malformed_message" {
		t.Fatalf("detail %q, want malformed_message", detail(err))
	}
	// A local command with a big kind-1 section is message_too_large.
	hello := opMsg(9, 0, sec0(doc(eInt32("hello", 1), eString("$db", "admin"))), sec1("x", big))
	if _, err := classify(t, hello); detail(err) != "message_too_large" {
		t.Fatalf("local command with a big kind-1: detail %q", detail(err))
	}
}

// Multiple sections of each kind in the prefix, including a body whose first
// element is in the prefix while the kind-1 section is truncated by the prefix
// boundary.
func TestClassifyTruncatedKind1AfterBody_FR38(t *testing.T) {
	msg := opMsg(9, 0, sec0(insBody), sec1("documents", doc(eString("blob", strings.Repeat("q", 5000)))))
	cut := 16 + 4 + 1 + len(insBody) + 8 // inside the kind-1 section header
	r, err := wire.ClassifyPrefix(msg[:cut], int32(len(msg)))
	if err != nil {
		t.Fatalf("truncated kind-1 after the body: %v (%q)", err, detail(err))
	}
	if r.Command != "insert" || r.FitsPrefix {
		t.Fatalf("request: %+v", r)
	}
	// Cut right after the body: still fine.
	cut = 16 + 4 + 1 + len(insBody)
	if r, err := wire.ClassifyPrefix(msg[:cut], int32(len(msg))); err != nil || r.Command != "insert" {
		t.Fatalf("cut at the body's end: %+v %v", r, err)
	}
	// Cut inside the body: the command name may not be complete.
	if _, err := wire.ClassifyPrefix(msg[:16+4+1+6], int32(len(msg))); detail(err) != "malformed_message" {
		t.Fatalf("cut inside the body: detail %q", detail(err))
	}
}

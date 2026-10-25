package wire_test

import (
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/wire"
)

func seeds(f *testing.F) {
	body := doc(eString("find", "c"), eString("$db", "d"))
	ins := doc(eString("insert", "c"), eString("$db", "d"))
	for _, m := range [][]byte{
		opMsg(1, 0, sec0(body)),
		withCRC(opMsg(1, 0, sec0(ins), sec1("documents", doc(eInt32("_id", 1)))), false),
		withCRC(opMsg(1, 0, sec0(body)), true),
		opMsg(1, 1<<5, sec0(body)),
		cmdMsg(eString("find", "c"), eString("$db", "a"), eString("$db", "b")),
		opMsg(1, 0, sec0(doc(eString("aggregate", "c"))), sec1("pipeline", doc())),
		cmdMsg(eInt32("hello", 1), eBool("helloOk", true)),
		opQuery(1, 0, "admin.$cmd", 0, -1, doc(eInt32("isMaster", 1))),
		opQuery(1, 0, "admin.$cmd", 0, -1, doc(eDoc("$query", doc(eInt32("hello", 1))))),
		{}, {1, 2, 3}, hdrBytes(26, 1, 0, 2013),
		// Kind-1 sections: body then sequences, sequences then body, and huge
		// or negative section sizes (security regression).
		opMsg(1, 0, sec0(ins), sec1("documents", doc(eInt32("_id", 1)), doc(eInt32("_id", 2)))),
		opMsg(1, 0, sec0(doc(eString("update", "c"), eString("$db", "d"))), sec1("updates", doc(eDoc("q", doc()), eDoc("u", doc())))),
		opMsg(1, 0, sec0(doc(eString("delete", "c"), eString("$db", "d"))), sec1("deletes", doc(eDoc("q", doc()), eInt32("limit", 0)))),
		opMsg(1, 0, sec0(doc(eInt32("bulkWrite", 1), eString("$db", "admin"))), sec1("ops", doc(eInt32("insert", 0))), sec1("nsInfo", doc(eString("ns", "d.c")))),
		opMsg(1, 0, sec1("documents", doc(eInt32("_id", 1))), sec0(ins)),
		opMsg(1, 0, sec0(ins), rawSec1(0x7FFFFFFF, "documents", doc())),
		opMsg(1, 0, rawSec1(0x7FFFFFFF, "documents", doc()), sec0(ins)),
		opMsg(1, 0, sec0(ins), rawSec1(0xFFFFFFFB, "documents", doc())),
		opMsg(1, 0, rawSec1(0x80000000, "documents"), sec0(ins)),
		opMsg(1, 0, sec0(ins), cat([]byte{1}, le32(18), cstr("documents"), le32(0x7FFFFFFF))),
	} {
		f.Add(m, uint16(len(m)/2))
	}
}

// AC-7: none of the three entry points panics, whatever the bytes.
func FuzzClassifyPrefix(f *testing.F) {
	seeds(f)
	f.Fuzz(func(t *testing.T, data []byte, total uint16) {
		_, _ = wire.ClassifyPrefix(data, int32(total)*3-5)
		_, _ = wire.ClassifyPrefix(data, int32(len(data)))
	})
}

// AC-7: validation does not depend on how the bytes are chunked.
func FuzzMsgValidator(f *testing.F) {
	seeds(f)
	f.Fuzz(func(t *testing.T, data []byte, split uint16) {
		if len(data) < 16 {
			return
		}
		h := wire.ParseHeader(data[:16])
		body := data[16:]
		whole := wire.NewMsgValidator(h, false)
		err1 := whole.Write(body)
		if err1 == nil {
			err1 = whole.Close()
		}
		i := int(split) % (len(body) + 1)
		parts := wire.NewMsgValidator(h, false)
		err2 := parts.Write(body[:i])
		if err2 == nil {
			err2 = parts.Write(body[i:])
		}
		if err2 == nil {
			err2 = parts.Close()
		}
		if (err1 == nil) != (err2 == nil) || detail(err1) != detail(err2) {
			t.Fatalf("whole: %q, split at %d: %q", detail(err1), i, detail(err2))
		}
		if whole.Done() != parts.Done() || whole.Done() != (err1 == nil) {
			t.Fatalf("Done() disagrees: %v %v err=%v", whole.Done(), parts.Done(), err1)
		}
	})
}

// AC-7: the reply scanner never panics and its fields do not depend on
// chunking.
func FuzzReplyScanner(f *testing.F) {
	seeds(f)
	f.Add(reply(eDouble("ok", 1), eDoc("cursor", doc(eInt64("id", 3)))), uint16(9))
	f.Fuzz(func(t *testing.T, data []byte, split uint16) {
		if len(data) < 16 {
			return
		}
		h := wire.ParseHeader(data[:16])
		body := data[16:]
		a := wire.NewReplyScanner(h)
		errA := a.Write(body)
		if errA == nil {
			errA = a.Close()
		}
		i := int(split) % (len(body) + 1)
		b := wire.NewReplyScanner(h)
		errB := b.Write(body[:i])
		if errB == nil {
			errB = b.Write(body[i:])
		}
		if errB == nil {
			errB = b.Close()
		}
		if (errA == nil) != (errB == nil) {
			t.Fatalf("verdicts differ: %v vs %v", errA, errB)
		}
		if errA == nil && a.Fields() != b.Fields() {
			t.Fatalf("fields differ by chunking: %+v vs %+v", a.Fields(), b.Fields())
		}
	})
}

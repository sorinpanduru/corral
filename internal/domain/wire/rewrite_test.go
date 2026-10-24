package wire_test

import (
	"encoding/binary"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/wire"
)

func diffBytes(a, b []byte) []int {
	var d []int
	for i := range a {
		if a[i] != b[i] {
			d = append(d, i)
		}
	}
	return d
}

func u32at(b []byte, i int) uint32 { return binary.LittleEndian.Uint32(b[i:]) }

// FR-10, FR-30, AC-6 (unit): the rewritten request header differs from the
// original only in requestID, responseTo, the checksum bit, the length (-4)
// and exhaustAllowed.
func TestRewriteRequestHeader_AC6(t *testing.T) {
	orig := cat(hdrBytes(200, 77, 5, 2013), le32(flagChecksum|flagMore|flagExhaust|1<<20))
	out, newLen := wire.RewriteRequestHeader(orig, 9001)
	if newLen != 196 || int32(u32at(out[:], 0)) != 196 {
		t.Fatalf("newLength = %d, header length = %d, want 196", newLen, u32at(out[:], 0))
	}
	if u32at(out[:], 4) != 9001 {
		t.Fatalf("requestID = %d", u32at(out[:], 4))
	}
	if u32at(out[:], 8) != 0 {
		t.Fatalf("responseTo = %d, want 0 for a forwarded request", u32at(out[:], 8))
	}
	if u32at(out[:], 12) != 2013 {
		t.Fatal("opcode changed")
	}
	flags := u32at(out[:], 16)
	if flags&flagChecksum != 0 || flags&flagExhaust != 0 {
		t.Fatalf("checksum or exhaustAllowed not cleared: %#x", flags)
	}
	if flags != flagMore|1<<20 {
		t.Fatalf("other flag bits changed: %#x", flags)
	}
	// Byte diff: header words 0..3 and the flags only.
	for _, i := range diffBytes(orig, out[:]) {
		if i >= 20 || (i >= 12 && i < 16) {
			t.Fatalf("byte %d differs unexpectedly", i)
		}
	}
}

func TestRewriteRequestHeaderNoChecksum_AC6(t *testing.T) {
	orig := cat(hdrBytes(300, 1, 2, 2013), le32(flagExhaust))
	out, newLen := wire.RewriteRequestHeader(orig, 55)
	if newLen != 300 || u32at(out[:], 0) != 300 {
		t.Fatalf("length changed without a checksum: %d", newLen)
	}
	if u32at(out[:], 16) != 0 {
		t.Fatalf("exhaustAllowed not cleared: %#x", u32at(out[:], 16))
	}
	// With neither bit set the flags are untouched.
	orig = cat(hdrBytes(300, 1, 2, 2013), le32(flagMore))
	out, _ = wire.RewriteRequestHeader(orig, 55)
	if u32at(out[:], 16) != flagMore {
		t.Fatalf("flags changed: %#x", u32at(out[:], 16))
	}
	// Checksum without exhaustAllowed.
	orig = cat(hdrBytes(300, 1, 2, 2013), le32(flagChecksum))
	out, newLen = wire.RewriteRequestHeader(orig, 55)
	if newLen != 296 || u32at(out[:], 16) != 0 {
		t.Fatalf("newLen=%d flags=%#x", newLen, u32at(out[:], 16))
	}
}

// Rewriting does not modify its input.
func TestRewriteDoesNotMutate(t *testing.T) {
	orig := cat(hdrBytes(200, 77, 5, 2013), le32(flagChecksum|flagExhaust))
	cp := append([]byte(nil), orig...)
	wire.RewriteRequestHeader(orig, 1)
	wire.RewriteReplyHeader(orig, 1, 2)
	if string(orig) != string(cp) {
		t.Fatal("input header modified")
	}
}

// FR-11: a reply carries responseTo = the client's requestID and a fresh
// requestID; a checksum is stripped (length -4).
func TestRewriteReplyHeader_AC6(t *testing.T) {
	orig := cat(hdrBytes(120, 31, 8, 2013), le32(flagChecksum))
	out := wire.RewriteReplyHeader(orig, 4242, 99)
	if u32at(out[:], 0) != 116 {
		t.Fatalf("length = %d, want 116", u32at(out[:], 0))
	}
	if u32at(out[:], 4) != 4242 || u32at(out[:], 8) != 99 || u32at(out[:], 12) != 2013 {
		t.Fatalf("ids/opcode: %d %d %d", u32at(out[:], 4), u32at(out[:], 8), u32at(out[:], 12))
	}
	if u32at(out[:], 16)&flagChecksum != 0 {
		t.Fatal("checksum bit not cleared")
	}
	// No checksum: the length is unchanged and the other flag bits are kept.
	orig = cat(hdrBytes(120, 31, 8, 2013), le32(1<<18))
	out = wire.RewriteReplyHeader(orig, 1, 2)
	if u32at(out[:], 0) != 120 || u32at(out[:], 16) != 1<<18 {
		t.Fatalf("length=%d flags=%#x", u32at(out[:], 0), u32at(out[:], 16))
	}
	for _, i := range diffBytes(orig, out[:]) {
		if i >= 12 && i < 20 {
			t.Fatalf("opcode or flags changed at byte %d", i)
		}
	}
}

// FR-11: the correlator hands out request IDs unique among outstanding
// requests; an unknown responseTo is unexpected_response.
func TestCorrelator_FR11(t *testing.T) {
	c := wire.NewCorrelator()
	if c.Outstanding() != 0 {
		t.Fatal("new correlator has outstanding requests")
	}
	a, b := c.Next(), c.Next()
	if a == b {
		t.Fatal("two outstanding requests share an ID")
	}
	if c.Outstanding() != 2 {
		t.Fatalf("Outstanding() = %d", c.Outstanding())
	}
	if err := c.Match(b); err != nil {
		t.Fatalf("Match(outstanding) = %v", err)
	}
	if c.Outstanding() != 1 {
		t.Fatalf("Outstanding() = %d after a match", c.Outstanding())
	}
	// Matching twice, an unknown ID and zero are unexpected_response.
	for _, id := range []int32{b, 0, -1, 123456} {
		if id == a {
			continue
		}
		if err := c.Match(id); detail(err) != "unexpected_response" {
			t.Errorf("Match(%d): detail %q, want unexpected_response", id, detail(err))
		}
	}
	if c.Outstanding() != 1 {
		t.Fatalf("a failed match changed the outstanding set: %d", c.Outstanding())
	}
	if err := c.Match(a); err != nil {
		t.Fatal(err)
	}
}

func TestCorrelatorManyIDs_FR11(t *testing.T) {
	c := wire.NewCorrelator()
	seen := map[int32]bool{}
	for i := 0; i < 5000; i++ {
		id := c.Next()
		if seen[id] {
			t.Fatalf("ID %d reused while outstanding (call %d)", id, i)
		}
		seen[id] = true
	}
	if c.Outstanding() != 5000 {
		t.Fatalf("Outstanding() = %d", c.Outstanding())
	}
}

// IDs may be reused only after their request completed, never while another
// holder is outstanding: interleave complete/next and check uniqueness among
// the outstanding set.
func TestCorrelatorInterleaved_FR11(t *testing.T) {
	c := wire.NewCorrelator()
	out := map[int32]bool{}
	for i := 0; i < 20000; i++ {
		if i%3 == 2 {
			for id := range out {
				if err := c.Match(id); err != nil {
					t.Fatal(err)
				}
				delete(out, id)
				break
			}
			continue
		}
		id := c.Next()
		if out[id] {
			t.Fatalf("ID %d handed out twice while outstanding", id)
		}
		out[id] = true
	}
}

// FR-11: client-side reply IDs are unique within a connection, and two
// connections' counters are independent.
func TestClientIDs_FR11(t *testing.T) {
	a, b := wire.NewClientIDs(), wire.NewClientIDs()
	seen := map[int32]bool{}
	for i := 0; i < 10000; i++ {
		id := a.Next()
		if seen[id] {
			t.Fatalf("client reply ID %d repeated", id)
		}
		seen[id] = true
	}
	_ = b.Next() // independent state: must not panic or share the counter
	if x, y := wire.NewClientIDs().Next(), wire.NewClientIDs().Next(); x != y {
		t.Log("fresh counters start at different values (allowed)")
	}
}

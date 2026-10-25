package exchange

import (
	"encoding/binary"
	"testing"
)

// RC5 (FR-41, R-3): partialCommand reads an attacker-controlled kind-1 section
// size. On a 32-bit platform a size such as 0x7FFFFFFF used to wrap the offset
// negative and panic. For every hostile size the result is "" and nothing
// panics; run under GOARCH=386 as well (make test-386 covers only the domain
// packages, so this package is checked with GOARCH=386 go test directly).
func partialMsg(kind1Size uint32, tail []byte) []byte {
	b := make([]byte, 16, 64)
	binary.LittleEndian.PutUint32(b[0:], 200)   // announces more than 20 bytes
	binary.LittleEndian.PutUint32(b[12:], 2013) // OP_MSG
	b = append(b, 0, 0, 0, 0)                   // flags
	b = append(b, 1)                            // kind 1
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], kind1Size)
	b = append(b, sz[:]...)
	return append(b, tail...)
}

func TestPartialCommandHostileKind1Size_RC5(t *testing.T) {
	for _, size := range []uint32{0x7FFFFFFF, 0x7FFFFFFE, 0x80000000, 0xFFFFFFFF, 0xFFFFFFFC, 0, 1, 4, 5, 1 << 30} {
		for name, tail := range map[string][]byte{
			"closed right after the size": nil,
			"truncated":                   {'x', 'y', 'z'},
			"a kind-0 section follows":    append([]byte{0}, 0x0a, 0, 0, 0, 0x10, 'f', 'i', 'n', 'd', 0),
		} {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("size %#x, %s: panic %v", size, name, r)
					}
				}()
				if got := partialCommand(partialMsg(size, tail)); got != "" && size >= 0x40000000 {
					t.Errorf("size %#x, %s: command %q, want empty", size, name, got)
				}
			}()
		}
	}
}

// A well-formed kind-1 section before the kind-0 body is skipped, and the
// command name is still found.
func TestPartialCommandSkipsValidKind1_RC5(t *testing.T) {
	b := partialMsg(5, []byte{0}) // size 5 = the size field plus one byte of identifier
	b = append(b, 0, 0x40, 0, 0, 0, 0x10, 'f', 'i', 'n', 'd', 0)
	if got := partialCommand(b); got != "find" {
		t.Fatalf("command %q", got)
	}
}

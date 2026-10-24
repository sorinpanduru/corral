package bson

import (
	"encoding/binary"
	"math"
	"time"
)

// Builder appends elements to a document. Keys are cut at their first NUL,
// since a cstring cannot hold one. Nested containers are built with
// StartDocument/EndDocument and StartArray/EndArray (balanced by the caller).
type Builder struct {
	buf    []byte
	starts []int // offsets of open container length prefixes
}

// NewBuilder returns an empty document builder.
func NewBuilder() *Builder {
	return &Builder{buf: make([]byte, 4, 64)}
}

func (b *Builder) head(t Type, k string) {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			k = k[:i]
			break
		}
	}
	b.buf = append(b.buf, byte(t))
	b.buf = append(b.buf, k...)
	b.buf = append(b.buf, 0)
}

func (b *Builder) u32(v uint32) { b.buf = binary.LittleEndian.AppendUint32(b.buf, v) }
func (b *Builder) u64(v uint64) { b.buf = binary.LittleEndian.AppendUint64(b.buf, v) }

// AppendInt32 appends an int32 element.
func (b *Builder) AppendInt32(k string, v int32) { b.head(TypeInt32, k); b.u32(uint32(v)) }

// AppendInt64 appends an int64 element.
func (b *Builder) AppendInt64(k string, v int64) { b.head(TypeInt64, k); b.u64(uint64(v)) }

// AppendDouble appends a double element.
func (b *Builder) AppendDouble(k string, v float64) {
	b.head(TypeDouble, k)
	b.u64(math.Float64bits(v))
}

// AppendBool appends a boolean element.
func (b *Builder) AppendBool(k string, v bool) {
	b.head(TypeBool, k)
	if v {
		b.buf = append(b.buf, 1)
	} else {
		b.buf = append(b.buf, 0)
	}
}

// AppendString appends a string element.
func (b *Builder) AppendString(k, v string) {
	b.head(TypeString, k)
	b.u32(uint32(len(v) + 1))
	b.buf = append(b.buf, v...)
	b.buf = append(b.buf, 0)
}

// AppendDateTime appends a UTC datetime element (milliseconds since epoch).
func (b *Builder) AppendDateTime(k string, t time.Time) {
	b.head(TypeDateTime, k)
	b.u64(uint64(t.UnixMilli()))
}

// AppendNull appends a null element.
func (b *Builder) AppendNull(k string) { b.head(TypeNull, k) }

// AppendDocument appends a copy of an encoded document.
func (b *Builder) AppendDocument(k string, doc []byte) {
	b.head(TypeDocument, k)
	b.buf = append(b.buf, doc...)
}

// AppendArray appends a copy of an encoded array.
func (b *Builder) AppendArray(k string, arr []byte) {
	b.head(TypeArray, k)
	b.buf = append(b.buf, arr...)
}

// AppendBinary appends a binary element.
func (b *Builder) AppendBinary(k string, subtype byte, data []byte) {
	b.head(TypeBinary, k)
	b.u32(uint32(len(data)))
	b.buf = append(b.buf, subtype)
	b.buf = append(b.buf, data...)
}

func (b *Builder) start(t Type, k string) {
	b.head(t, k)
	b.starts = append(b.starts, len(b.buf))
	b.u32(0)
}

func (b *Builder) end() {
	if len(b.starts) == 0 {
		panic("bson: End without a matching Start")
	}
	s := b.starts[len(b.starts)-1]
	b.starts = b.starts[:len(b.starts)-1]
	b.buf = append(b.buf, 0)
	binary.LittleEndian.PutUint32(b.buf[s:], uint32(len(b.buf)-s))
}

// StartDocument opens an embedded document.
func (b *Builder) StartDocument(k string) { b.start(TypeDocument, k) }

// EndDocument closes the innermost open document.
func (b *Builder) EndDocument() { b.end() }

// StartArray opens an embedded array.
func (b *Builder) StartArray(k string) { b.start(TypeArray, k) }

// EndArray closes the innermost open array.
func (b *Builder) EndArray() { b.end() }

// Bytes returns the finished document as a new slice. The builder stays
// usable. It panics if a container is still open.
func (b *Builder) Bytes() []byte {
	if len(b.starts) != 0 {
		panic("bson: Bytes with open containers")
	}
	out := make([]byte, len(b.buf)+1)
	copy(out, b.buf)
	binary.LittleEndian.PutUint32(out, uint32(len(out)))
	return out
}

package bson

import (
	"encoding/binary"
	"math"
	"math/bits"
)

func float64frombits(b uint64) float64 { return math.Float64frombits(b) }

func trailingZeros(m uint64) int { return bits.TrailingZeros64(m) }

// Element is a view of one element inside a document. It does not copy: the
// slices alias the document passed to FirstElement or Lookup.
type Element struct {
	key []byte
	typ Type
	val []byte
}

// Key returns the element key.
func (e Element) Key() string { return string(e.key) }

// Type returns the element type.
func (e Element) Type() Type { return e.typ }

// Int32 returns the value if the element is an int32.
func (e Element) Int32() (int32, bool) {
	if e.typ != TypeInt32 || len(e.val) != 4 {
		return 0, false
	}
	return int32(binary.LittleEndian.Uint32(e.val)), true
}

// Int64 returns the value if the element is an int64.
func (e Element) Int64() (int64, bool) {
	if e.typ != TypeInt64 || len(e.val) != 8 {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(e.val)), true
}

// Double returns the value if the element is a double.
func (e Element) Double() (float64, bool) {
	if e.typ != TypeDouble || len(e.val) != 8 {
		return 0, false
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(e.val)), true
}

// Number returns an int32, int64 or double value as a float64.
func (e Element) Number() (float64, bool) {
	if n, ok := e.Int32(); ok {
		return float64(n), true
	}
	if n, ok := e.Int64(); ok {
		return float64(n), true
	}
	return e.Double()
}

// Bool returns the value if the element is a boolean.
func (e Element) Bool() (bool, bool) {
	if e.typ != TypeBool || len(e.val) != 1 {
		return false, false
	}
	return e.val[0] == 1, true
}

// StringValue returns the value if the element is a string.
func (e Element) StringValue() (string, bool) {
	if e.typ != TypeString || len(e.val) < 5 {
		return "", false
	}
	return string(e.val[4 : len(e.val)-1]), true
}

// Document returns the embedded document bytes (a view).
func (e Element) Document() ([]byte, bool) {
	if e.typ != TypeDocument {
		return nil, false
	}
	return e.val, true
}

// Array returns the embedded array bytes (a view).
func (e Element) Array() ([]byte, bool) {
	if e.typ != TypeArray {
		return nil, false
	}
	return e.val, true
}

// Binary returns the subtype and data (a view) if the element is binary.
func (e Element) Binary() (subtype byte, data []byte, ok bool) {
	if e.typ != TypeBinary || len(e.val) < 5 {
		return 0, nil, false
	}
	return e.val[4], e.val[5:], true
}

// ObjectID returns the value if the element is an ObjectID.
func (e Element) ObjectID() ([12]byte, bool) {
	var id [12]byte
	if e.typ != TypeObjectID || len(e.val) != 12 {
		return id, false
	}
	copy(id[:], e.val)
	return id, true
}

// checkHeader verifies the document length prefix and terminator only.
func checkHeader(doc []byte) error {
	if len(doc) < 5 || int64(int32(binary.LittleEndian.Uint32(doc))) != int64(len(doc)) || doc[len(doc)-1] != 0 {
		return ErrMalformed
	}
	return nil
}

// elementAt reads the element whose type byte is at doc[p]. limit is the
// offset of the document terminator. It bounds-checks the element but does
// not validate nested documents. It returns the offset after the element.
func elementAt(doc []byte, p, limit int) (Element, int, error) {
	t := Type(doc[p])
	if !validType(t) {
		return Element{}, 0, ErrMalformed
	}
	ks := p + 1
	ke := ks
	for ke < limit && doc[ke] != 0 {
		ke++
	}
	if ke >= limit {
		return Element{}, 0, ErrMalformed
	}
	vs := ke + 1
	size, err := payloadSize(t, doc[vs:limit])
	if err != nil {
		return Element{}, 0, err
	}
	return Element{key: doc[ks:ke], typ: t, val: doc[vs : vs+size : vs+size]}, vs + size, nil
}

// payloadSize returns (all bounds are written as l > len(b)-k so nothing can
// wrap on 32-bit platforms) the payload length of an element of type t at the start
// of b (b runs to the document terminator).
func payloadSize(t Type, b []byte) (int, error) {
	if n, ok := fixedSize(t); ok {
		if n > len(b) {
			return 0, ErrMalformed
		}
		return n, nil
	}
	i32 := func() (int, bool) {
		if len(b) < 4 {
			return 0, false
		}
		return int(int32(binary.LittleEndian.Uint32(b))), true
	}
	switch t {
	case TypeString, TypeJavaScript, TypeSymbol, TypeDBPointer:
		l, ok := i32()
		if !ok || l < 1 || l > len(b)-4 || b[3+l] != 0 {
			return 0, ErrMalformed
		}
		if t == TypeDBPointer {
			if l > len(b)-16 {
				return 0, ErrMalformed
			}
			return 4 + l + 12, nil
		}
		return 4 + l, nil
	case TypeDocument, TypeArray:
		l, ok := i32()
		if !ok || l < 5 || l > len(b) || b[l-1] != 0 {
			return 0, ErrMalformed
		}
		return l, nil
	case TypeBinary:
		l, ok := i32()
		if !ok || l < 0 || l > len(b)-5 {
			return 0, ErrMalformed
		}
		return 5 + l, nil
	case TypeRegex:
		n := 0
		for k := 0; k < 2; k++ {
			for n < len(b) && b[n] != 0 {
				n++
			}
			if n >= len(b) {
				return 0, ErrMalformed
			}
			n++
		}
		return n, nil
	case TypeCodeScope:
		l, ok := i32()
		if !ok || l < 14 || l > len(b) {
			return 0, ErrMalformed
		}
		return l, nil
	}
	return 0, ErrMalformed
}

// FirstElement returns the first element of doc (for example the command
// name). An empty or malformed document is an error.
func FirstElement(doc []byte) (Element, error) {
	if err := checkHeader(doc); err != nil {
		return Element{}, err
	}
	if len(doc) == 5 {
		return Element{}, ErrMalformed
	}
	e, _, err := elementAt(doc, 4, len(doc)-1)
	return e, err
}

// Lookup finds the element at path, following documents and arrays (array
// items use index keys). It returns ok=false when the path is absent or
// passes through a scalar. Duplicate keys: the first occurrence wins. Only
// the elements scanned up to the target are checked.
func Lookup(doc []byte, path ...string) (Element, bool, error) {
	if len(path) == 0 {
		return Element{}, false, nil
	}
	for depth := 0; ; depth++ {
		if err := checkHeader(doc); err != nil {
			return Element{}, false, err
		}
		limit := len(doc) - 1
		var hit Element
		found := false
		for p := 4; p < limit; {
			e, next, err := elementAt(doc, p, limit)
			if err != nil {
				return Element{}, false, err
			}
			if string(e.key) == path[depth] {
				hit, found = e, true
				break
			}
			p = next
		}
		if !found {
			return Element{}, false, nil
		}
		if depth == len(path)-1 {
			return hit, true, nil
		}
		if hit.typ != TypeDocument && hit.typ != TypeArray {
			return Element{}, false, nil
		}
		doc = hit.val
	}
}

// PartialStatus says what FirstElementPartial found.
type PartialStatus int

const (
	// PartialOK means the first element is complete.
	PartialOK PartialStatus = iota
	// PartialNeedMore means the bytes end before the first element does.
	PartialNeedMore
	// PartialEmpty means the document is complete and has no elements.
	PartialEmpty
)

// FirstElementPartial reads the first element of a document of which only a
// prefix may be available (FR-38 classification). It returns ErrMalformed only
// for input that is wrong whatever bytes follow.
func FirstElementPartial(b []byte) (Element, PartialStatus, error) {
	if len(b) < 5 {
		return Element{}, PartialNeedMore, nil
	}
	l := int(int32(binary.LittleEndian.Uint32(b)))
	if l < 5 {
		return Element{}, PartialNeedMore, ErrMalformed
	}
	complete := len(b) >= l
	if complete {
		b = b[:l]
	}
	if b[4] == 0 {
		if l == 5 {
			return Element{}, PartialEmpty, nil
		}
		return Element{}, PartialNeedMore, ErrMalformed
	}
	limit := len(b)
	if complete {
		limit = l - 1
	}
	t := Type(b[4])
	if !validType(t) {
		return Element{}, PartialNeedMore, ErrMalformed
	}
	ke := 5
	for ke < limit && b[ke] != 0 {
		ke++
	}
	if ke >= limit {
		if complete {
			return Element{}, PartialNeedMore, ErrMalformed
		}
		return Element{}, PartialNeedMore, nil
	}
	vs := ke + 1
	size, err := payloadSize(t, b[vs:limit])
	if err != nil {
		if complete {
			return Element{}, PartialNeedMore, ErrMalformed
		}
		return Element{}, PartialNeedMore, nil
	}
	return Element{key: b[5:ke], typ: t, val: b[vs : vs+size : vs+size]}, PartialOK, nil
}

// ForEach calls fn for each top-level element of doc until fn returns false.
// Elements are bounds-checked but nested documents are not validated.
func ForEach(doc []byte, fn func(Element) bool) error {
	if err := checkHeader(doc); err != nil {
		return err
	}
	limit := len(doc) - 1
	for p := 4; p < limit; {
		e, next, err := elementAt(doc, p, limit)
		if err != nil {
			return err
		}
		if !fn(e) {
			return nil
		}
		p = next
	}
	return nil
}

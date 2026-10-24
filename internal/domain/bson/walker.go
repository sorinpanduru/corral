package bson

import "encoding/binary"

// CaptureMode selects what a Capture records.
type CaptureMode int

const (
	// CaptureValue records the scalar value at Path (containers are
	// reported as present with no data).
	CaptureValue CaptureMode = iota
	// CaptureFirstKey records the first key of the document (or array) at
	// Path, for example pipeline.0.
	CaptureFirstKey
	// CapturePresence records only that Path exists, with its type.
	CapturePresence
)

// Capture names one path to record while walking. Path components are keys;
// array items use their index keys ("0", "1"). Only the first occurrence of a
// duplicated key counts. At most 64 captures are honoured.
type Capture struct {
	Path []string
	Mode CaptureMode
}

// Value is a captured scalar: its type plus a bounded copy.
type Value struct {
	Type    Type
	TooLong bool // value exceeded MaxCaptureBytes; no data is kept
	// Count is the number of times the key occurs at the top level, for
	// captures whose path is a single key (0 otherwise). The value is the
	// first occurrence (FR-60).
	Count int
	sub   byte
	data  []byte
}

// Int32 returns the value if it is an int32.
func (v Value) Int32() (int32, bool) {
	if v.Type != TypeInt32 || len(v.data) != 4 {
		return 0, false
	}
	return int32(binary.LittleEndian.Uint32(v.data)), true
}

// Int64 returns the value if it is an int64.
func (v Value) Int64() (int64, bool) {
	if v.Type != TypeInt64 || len(v.data) != 8 {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(v.data)), true
}

// Double returns the value if it is a double.
func (v Value) Double() (float64, bool) {
	if v.Type != TypeDouble || len(v.data) != 8 {
		return 0, false
	}
	return float64frombits(binary.LittleEndian.Uint64(v.data)), true
}

// Number returns an int32, int64 or double value as a float64.
func (v Value) Number() (float64, bool) {
	if n, ok := v.Int32(); ok {
		return float64(n), true
	}
	if n, ok := v.Int64(); ok {
		return float64(n), true
	}
	return v.Double()
}

// Bool returns the value if it is a boolean.
func (v Value) Bool() (bool, bool) {
	if v.Type != TypeBool || len(v.data) != 1 {
		return false, false
	}
	return v.data[0] == 1, true
}

// StringValue returns the value if it is a string that was kept in full.
func (v Value) StringValue() (string, bool) {
	if v.Type != TypeString || v.TooLong {
		return "", false
	}
	return string(v.data), true
}

// Binary returns the subtype and data if the value is binary kept in full.
func (v Value) Binary() (subtype byte, data []byte, ok bool) {
	if v.Type != TypeBinary || v.TooLong {
		return 0, nil, false
	}
	return v.sub, v.data, true
}

type state uint8

const (
	sLen     state = iota // 4-byte document length
	sType                 // element type byte
	sKey                  // element key cstring
	sFixed                // fixed-size payload
	sStrLen               // 4-byte string length
	sStrBody              // string bytes, last must be NUL
	sBinLen               // 4-byte binary length
	sBinSub               // binary subtype byte
	sBinBody              // binary bytes
	sRegex1               // regex pattern cstring
	sRegex2               // regex options cstring
	sCWSLen               // 4-byte code-with-scope total
	sDone                 // top-level document complete
)

type frame struct {
	docEnd int64  // absolute offset one past the terminator
	alive  uint64 // captures whose path matches down to this frame
	fk     uint64 // first-key captures targeting this frame
	first  bool   // no key seen yet
	seen   uint64 // captures whose key already matched at this level (first wins)
}

// Walker is a push parser: Write feeds chunks, Close ends the stream. It
// applies the Validate rules incrementally, with constant memory per nesting
// level and no recursion, and records the configured captures. It does not
// retain or modify the caller's chunks. Errors are sticky.
type Walker struct {
	err   error
	st    state
	pos   int64
	stack []frame

	acc  [4]byte
	accN int

	scopeNext bool // the pending sLen is a code-with-scope scope document
	cwsEnd    int64
	curType   Type
	rem       int64 // bytes left in the current payload state
	bodyLen   int64 // total body length (strings: incl. NUL; binary: data)
	bodyOff   int64
	regexLeft int

	key     [MaxCaptureBytes]byte
	keyN    int  // bytes stored
	keyLen  int  // bytes seen
	pendLen bool // pending sLen belongs to a nested container

	caps        []Capture
	found       []bool
	vals        []Value
	counts      []int
	topFirst    [MaxCaptureBytes]byte // first top-level key, bounded
	topFirstLen int
	topRepeats  int
	capMask     uint64 // captures recording the current element's payload
	pendLive    uint64
	pendFK      uint64
	cbuf        [MaxCaptureBytes]byte
	cn          int
	cTooLong    bool
	cSub        byte
}

// NewWalker returns a Walker recording captures (may be nil).
func NewWalker(captures []Capture) *Walker {
	w := &Walker{}
	if n := len(captures); n > 0 {
		if n > 64 {
			n = 64
		}
		w.caps = captures[:n]
		w.found = make([]bool, n)
		w.vals = make([]Value, n)
		w.counts = make([]int, n)
	}
	return w
}

// Consumed returns the number of bytes accepted so far.
func (w *Walker) Consumed() int64 { return w.pos }

// Captured returns capture i, if it was found.
func (w *Walker) Captured(i int) (Value, bool) {
	if i < 0 || i >= len(w.found) || !w.found[i] {
		return Value{}, false
	}
	v := w.vals[i]
	if len(w.caps[i].Path) == 1 {
		v.Count = w.counts[i]
	}
	return v, true
}

// TopLevelKeyEqualsFirst returns how many later top-level keys equal the
// first top-level key (a repeated command name). Keys are compared up to
// MaxCaptureBytes bytes plus their full length.
func (w *Walker) TopLevelKeyEqualsFirst() int { return w.topRepeats }

func (w *Walker) fail(err error) error {
	w.err = err
	return err
}

// Close ends the stream: the document must be complete.
func (w *Walker) Close() error {
	if w.err != nil {
		return w.err
	}
	if w.st != sDone {
		return w.fail(ErrMalformed)
	}
	return nil
}

func (w *Walker) top() *frame { return &w.stack[len(w.stack)-1] }

// limit is the offset past which a payload may not extend.
func (w *Walker) limit() int64 {
	if w.curType == TypeCodeScope {
		return w.cwsEnd
	}
	return w.top().docEnd - 1
}

// Write feeds the next chunk of the document.
func (w *Walker) Write(p []byte) error {
	if w.err != nil {
		return w.err
	}
	for len(p) > 0 {
		switch w.st {
		case sDone:
			return w.fail(ErrMalformed)

		case sLen, sStrLen, sBinLen, sCWSLen:
			for w.accN < 4 && len(p) > 0 {
				w.acc[w.accN] = p[0]
				w.accN++
				w.pos++
				p = p[1:]
			}
			if w.accN < 4 {
				return nil
			}
			w.accN = 0
			v := int32(binary.LittleEndian.Uint32(w.acc[:]))
			var err error
			switch w.st {
			case sLen:
				err = w.onDocLen(v)
			case sStrLen:
				err = w.onStrLen(v)
			case sBinLen:
				err = w.onBinLen(v)
			default:
				err = w.onCWSLen(v)
			}
			if err != nil {
				return w.fail(err)
			}

		case sType:
			b := Type(p[0])
			p = p[1:]
			if b == 0 {
				if w.pos != w.top().docEnd-1 {
					return w.fail(ErrMalformed)
				}
				w.pos++
				w.stack = w.stack[:len(w.stack)-1]
				if len(w.stack) == 0 {
					w.st = sDone
				} else {
					w.st = sType
				}
				continue
			}
			w.pos++
			if !validType(b) {
				return w.fail(ErrMalformed)
			}
			w.curType = b
			w.keyN, w.keyLen = 0, 0
			w.st = sKey

		case sKey:
			end := w.top().docEnd - 1
			for len(p) > 0 && w.st == sKey {
				if w.pos >= end {
					return w.fail(ErrMalformed)
				}
				b := p[0]
				p = p[1:]
				w.pos++
				if b != 0 {
					if w.keyN < len(w.key) {
						w.key[w.keyN] = b
						w.keyN++
					}
					w.keyLen++
					continue
				}
				if err := w.onKey(); err != nil {
					return w.fail(err)
				}
			}

		case sFixed:
			n := int64(len(p))
			if n > w.rem {
				n = w.rem
			}
			if w.curType == TypeBool && (p[0] > 1) {
				return w.fail(ErrMalformed)
			}
			w.capture(p[:n])
			p = p[n:]
			w.pos += n
			w.rem -= n
			if w.rem == 0 {
				w.elemDone()
			}

		case sStrBody:
			n := int64(len(p))
			if n > w.rem {
				n = w.rem
			}
			if w.capMask != 0 && w.curType == TypeString {
				// Content excludes the trailing NUL.
				if c := w.bodyLen - 1 - w.bodyOff; c > 0 {
					if c > n {
						c = n
					}
					w.capture(p[:c])
				}
			}
			last := w.rem == n
			if last && p[n-1] != 0 {
				return w.fail(ErrMalformed)
			}
			w.bodyOff += n
			w.pos += n
			w.rem -= n
			p = p[n:]
			if last {
				switch w.curType {
				case TypeDBPointer:
					if w.pos+12 > w.limit() {
						return w.fail(ErrMalformed)
					}
					w.st, w.rem = sFixed, 12
				case TypeCodeScope:
					w.st, w.scopeNext = sLen, true
				default:
					w.elemDone()
				}
			}

		case sBinSub:
			w.cSub = p[0]
			p = p[1:]
			w.pos++
			if w.bodyLen == 0 {
				w.elemDone()
			} else {
				w.st, w.rem = sBinBody, w.bodyLen
			}

		case sBinBody:
			n := int64(len(p))
			if n > w.rem {
				n = w.rem
			}
			w.capture(p[:n])
			p = p[n:]
			w.pos += n
			w.rem -= n
			if w.rem == 0 {
				w.elemDone()
			}

		case sRegex1, sRegex2:
			end := w.top().docEnd - 1
			for len(p) > 0 && (w.st == sRegex1 || w.st == sRegex2) {
				if w.pos >= end {
					return w.fail(ErrMalformed)
				}
				b := p[0]
				p = p[1:]
				w.pos++
				if b != 0 {
					continue
				}
				if w.st == sRegex1 {
					w.st = sRegex2
				} else {
					w.elemDone()
				}
			}
		}
	}
	return nil
}

// onDocLen handles the length of a top-level, nested or scope document.
func (w *Walker) onDocLen(v int32) error {
	if v < 5 {
		return ErrMalformed
	}
	end := w.pos - 4 + int64(v)
	f := frame{docEnd: end, first: true}
	if len(w.stack) == 0 {
		for i := range w.caps {
			if len(w.caps[i].Path) > 0 {
				f.alive |= 1 << uint(i)
			}
		}
	} else {
		if w.scopeNext {
			if end != w.cwsEnd {
				return ErrMalformed
			}
		} else if end > w.top().docEnd-1 {
			return ErrMalformed
		}
		if !w.scopeNext {
			f.alive, f.fk = w.pendLive, w.pendFK
		}
	}
	w.scopeNext = false
	w.pendLive, w.pendFK = 0, 0
	if len(w.stack) >= MaxDepth {
		return ErrTooDeep
	}
	w.stack = append(w.stack, f)
	w.st = sType
	return nil
}

func (w *Walker) onStrLen(v int32) error {
	if v < 1 || w.pos+int64(v) > w.limit() {
		return ErrMalformed
	}
	w.bodyLen, w.bodyOff, w.rem = int64(v), 0, int64(v)
	if w.capMask != 0 && w.curType == TypeString && v-1 > MaxCaptureBytes {
		w.cTooLong = true
	}
	w.st = sStrBody
	return nil
}

func (w *Walker) onBinLen(v int32) error {
	if v < 0 || w.pos+1+int64(v) > w.limit() {
		return ErrMalformed
	}
	w.bodyLen = int64(v)
	if w.capMask != 0 && v > MaxCaptureBytes {
		w.cTooLong = true
	}
	w.st = sBinSub
	return nil
}

func (w *Walker) onCWSLen(v int32) error {
	if v < 14 {
		return ErrMalformed
	}
	end := w.pos - 4 + int64(v)
	if end > w.top().docEnd-1 {
		return ErrMalformed
	}
	w.cwsEnd = end
	w.st = sStrLen
	return nil
}

// onKey runs when an element key is complete: capture matching, then the
// payload state for the element type.
func (w *Walker) onKey() error {
	f := w.top()
	d := len(w.stack) - 1
	if d == 0 {
		if f.first {
			w.topFirstLen = w.keyLen
			copy(w.topFirst[:], w.key[:w.keyN])
		} else if w.keyLen == w.topFirstLen && string(w.key[:w.keyN]) == string(w.topFirst[:min(w.topFirstLen, MaxCaptureBytes)]) {
			w.topRepeats++
		}
	}
	if f.first {
		f.first = false
		for m := f.fk; m != 0; m &= m - 1 {
			i := trailingZeros(m)
			w.found[i] = true
			v := Value{Type: TypeString}
			if w.keyLen > MaxCaptureBytes {
				v.TooLong = true
			} else {
				v.data = append([]byte(nil), w.key[:w.keyN]...)
			}
			w.vals[i] = v
		}
	}
	w.capMask, w.pendLive, w.pendFK = 0, 0, 0
	w.cn, w.cTooLong = 0, false
	container := w.curType == TypeDocument || w.curType == TypeArray
	for m := f.alive; m != 0; m &= m - 1 {
		i := trailingZeros(m)
		path := w.caps[i].Path
		if d >= len(path) || w.keyLen != len(path[d]) || w.keyLen > MaxCaptureBytes ||
			string(w.key[:w.keyN]) != path[d] {
			continue
		}
		bit := uint64(1) << uint(i)
		if f.seen&bit != 0 {
			// Duplicate key at this level: the first occurrence wins, for
			// intermediate and terminal keys alike (ADR-0017). Top-level
			// single-key captures still count it (FR-60).
			if d == 0 && len(path) == 1 {
				w.counts[i]++
			}
			continue
		}
		f.seen |= bit
		if len(path) > d+1 {
			if container {
				w.pendLive |= bit
			}
			continue
		}
		// Terminal match (first occurrence at this level).
		if d == 0 && len(path) == 1 {
			w.counts[i]++
		}
		switch w.caps[i].Mode {
		case CapturePresence:
			w.found[i], w.vals[i] = true, Value{Type: w.curType}
		case CaptureFirstKey:
			if container {
				w.pendFK |= bit
			}
		default:
			if capturable(w.curType) {
				w.capMask |= bit
			} else {
				w.found[i], w.vals[i] = true, Value{Type: w.curType}
			}
		}
	}

	if n, ok := fixedSize(w.curType); ok {
		if w.pos+int64(n) > f.docEnd-1 {
			return ErrMalformed
		}
		if n == 0 {
			w.elemDone()
		} else {
			w.st, w.rem = sFixed, int64(n)
		}
		return nil
	}
	switch w.curType {
	case TypeString, TypeJavaScript, TypeSymbol, TypeDBPointer:
		w.st = sStrLen
	case TypeBinary:
		w.st = sBinLen
	case TypeRegex:
		w.st = sRegex1
	case TypeDocument, TypeArray:
		w.st = sLen
	case TypeCodeScope:
		w.st = sCWSLen
	}
	return nil
}

func capturable(t Type) bool {
	switch t {
	case TypeDouble, TypeString, TypeBinary, TypeObjectID, TypeBool, TypeDateTime,
		TypeInt32, TypeTimestamp, TypeInt64, TypeDecimal128:
		return true
	}
	return false
}

// capture appends payload bytes to the capture buffer, bounded.
func (w *Walker) capture(b []byte) {
	if w.capMask == 0 || w.cTooLong {
		return
	}
	w.cn += copy(w.cbuf[w.cn:], b)
}

// elemDone finishes the current scalar element.
func (w *Walker) elemDone() {
	if w.capMask != 0 {
		for m := w.capMask; m != 0; m &= m - 1 {
			i := trailingZeros(m)
			v := Value{Type: w.curType, sub: w.cSub}
			if w.cTooLong {
				v.TooLong = true
			} else {
				v.data = append([]byte(nil), w.cbuf[:w.cn]...)
			}
			w.found[i], w.vals[i] = true, v
		}
		w.capMask = 0
	}
	w.st = sType
}

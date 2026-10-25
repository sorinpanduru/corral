package wire

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/sorinpanduru/corral/internal/domain/bson"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

type vstate uint8

const (
	vFlags vstate = iota
	vKind
	vSecSize
	vSecID
	vDocLen
	vDocBody
)

// Inspected-field candidates (FR-60). The first five apply to every request,
// pipeline to aggregate and collection to getMore.
var inspected = [...]string{"$db", "lsid", "txnNumber", "autocommit", "startTransaction", "pipeline", "collection"}

const (
	idxPipeline   = 5
	idxCollection = 6
	maxIDLen      = 16 // longest candidate name ("startTransaction")
)

func requestCaptures() []bson.Capture {
	c := make([]bson.Capture, len(inspected))
	for i, n := range inspected {
		c[i] = bson.Capture{Path: []string{n}, Mode: bson.CapturePresence}
	}
	return c
}

// Reply capture indexes.
const (
	rcOK = iota
	rcCode
	rcCodeName
	rcWCECode
	rcCursorID
)

func replyCaptures() []bson.Capture {
	return []bson.Capture{
		{Path: []string{"ok"}, Mode: bson.CaptureValue},
		{Path: []string{"code"}, Mode: bson.CaptureValue},
		{Path: []string{"codeName"}, Mode: bson.CaptureValue},
		{Path: []string{"writeConcernError", "code"}, Mode: bson.CaptureValue},
		{Path: []string{"cursor", "id"}, Mode: bson.CaptureValue},
	}
}

// MsgValidator validates an OP_MSG incrementally (FR-5, FR-7, FR-60). Write
// takes the bytes after the 16-byte header, in any chunking; the verdict and
// the error detail do not depend on the chunking. Errors are sticky. For
// requests it applies the inspected-field integrity rules; for replies it
// applies none of them, and a moreToCome reply is unexpected_response.
type MsgValidator struct {
	h       Header
	isReply bool
	err     error
	done    bool

	total   int64 // body bytes after the header, including flags and checksum
	pos     int64 // body bytes consumed
	st      vstate
	acc     [4]byte
	accN    int
	flags   uint32
	dataEnd int64
	crcOn   bool
	crc     uint32
	csum    [4]byte
	csumN   int

	bodies  int
	isBody  bool
	secEnd  int64
	idBuf   [maxIDLen]byte
	idN     int
	idLong  bool
	docEnd  int64
	docLeft int64

	caps    []bson.Capture
	body    *bson.Walker
	scratch *bson.Walker // kind-1 documents, reused

	cmd      string
	cmdKnown bool
	kind1    uint8
}

// NewMsgValidator returns a validator for the message whose header is h.
func NewMsgValidator(h Header, isReply bool) *MsgValidator {
	v := &MsgValidator{h: h, isReply: isReply, total: int64(h.Length) - headerLen}
	if isReply {
		v.caps = replyCaptures()
	} else {
		v.caps = requestCaptures()
	}
	return v
}

// Done reports whether every check, including the checksum, has passed.
func (v *MsgValidator) Done() bool { return v.done }

func (v *MsgValidator) fail(e *ProtocolError) error {
	v.err = e
	v.done = false
	return e
}

// Write feeds the next bytes of the message body.
func (v *MsgValidator) Write(p []byte) error {
	if v.err != nil {
		return v.err
	}
	if v.total < 4 {
		return v.fail(ErrMalformed())
	}
	for len(p) > 0 {
		if v.pos >= v.total {
			return v.fail(ErrMalformed())
		}
		if v.st == vFlags {
			for v.accN < 4 && len(p) > 0 {
				v.acc[v.accN] = p[0]
				v.accN++
				v.pos++
				p = p[1:]
			}
			if v.accN < 4 {
				return nil
			}
			if err := v.onFlags(); err != nil {
				return err
			}
			continue
		}
		if left := v.dataEnd - v.pos; left > 0 {
			n := int64(len(p))
			if n > left {
				n = left
			}
			if v.crcOn {
				v.crc = crc32.Update(v.crc, castagnoli, p[:n])
			}
			if err := v.feed(p[:n]); err != nil {
				return err
			}
			p = p[n:]
			continue
		}
		// Checksum bytes.
		v.csum[v.csumN] = p[0]
		v.csumN++
		v.pos++
		p = p[1:]
	}
	return nil
}

func (v *MsgValidator) onFlags() error {
	v.flags = binary.LittleEndian.Uint32(v.acc[:])
	v.accN = 0
	if v.flags&requiredFlagMsk != 0 {
		return v.fail(ErrMalformed())
	}
	if v.isReply && v.flags&FlagMoreToCome != 0 {
		return v.fail(ErrUnexpectedResponse())
	}
	v.dataEnd = v.total
	if v.flags&FlagChecksumPresent != 0 {
		v.dataEnd -= 4
		if v.dataEnd < 4 {
			return v.fail(ErrMalformed())
		}
		v.crcOn = true
		v.crc = crc32.Update(0, castagnoli, v.h.AppendTo(nil))
		v.crc = crc32.Update(v.crc, castagnoli, v.acc[:])
	}
	v.st = vKind
	return nil
}

// take copies up to n bytes of b into acc and returns the rest.
func (v *MsgValidator) gather(b []byte) []byte {
	for v.accN < 4 && len(b) > 0 {
		v.acc[v.accN] = b[0]
		v.accN++
		v.pos++
		b = b[1:]
	}
	return b
}

// feed consumes data-region bytes. v.pos is advanced as bytes are used.
func (v *MsgValidator) feed(b []byte) error {
	for len(b) > 0 {
		switch v.st {
		case vKind:
			k := b[0]
			b = b[1:]
			v.pos++
			switch k {
			case 0:
				if v.bodies >= 1 {
					return v.fail(ErrMalformed())
				}
				v.isBody = true
				v.beginDoc()
			case 1:
				v.st = vSecSize
			default:
				return v.fail(ErrMalformed())
			}

		case vSecSize:
			b = v.gather(b)
			if v.accN < 4 {
				return nil
			}
			size := int64(int32(binary.LittleEndian.Uint32(v.acc[:])))
			v.accN = 0
			start := v.pos - 4
			if size < 5 || start+size > v.dataEnd {
				return v.fail(ErrMalformed())
			}
			v.secEnd = start + size
			v.idN, v.idLong = 0, false
			v.st = vSecID

		case vSecID:
			for len(b) > 0 && v.st == vSecID {
				if v.pos >= v.secEnd {
					return v.fail(ErrMalformed())
				}
				c := b[0]
				b = b[1:]
				v.pos++
				if c != 0 {
					if v.idN < maxIDLen {
						v.idBuf[v.idN] = c
						v.idN++
					} else {
						v.idLong = true
					}
					continue
				}
				if err := v.onKind1ID(); err != nil {
					return err
				}
				v.isBody = false
				v.afterSectionStart()
				if v.err != nil {
					return v.err
				}
			}

		case vDocLen:
			b = v.feedDocLen(b)
			if v.err != nil {
				return v.err
			}
			if v.accN < 4 {
				return nil
			}
			if err := v.onDocLen(); err != nil {
				return err
			}

		case vDocBody:
			n := int64(len(b))
			if n > v.docLeft {
				n = v.docLeft
			}
			w := v.walker()
			if err := w.Write(b[:n]); err != nil {
				return v.fail(ErrMalformed())
			}
			b = b[n:]
			v.pos += n
			v.docLeft -= n
			if v.isBody {
				if err := v.rules(); err != nil {
					return err
				}
			}
			if v.docLeft == 0 {
				if err := w.Close(); err != nil {
					return v.fail(ErrMalformed())
				}
				if err := v.afterDoc(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (v *MsgValidator) walker() *bson.Walker {
	if v.isBody {
		return v.body
	}
	return v.scratch
}

// beginDoc prepares to read a document length and a fresh walker for it.
func (v *MsgValidator) beginDoc() {
	if v.isBody {
		if v.body == nil {
			v.body = bson.NewWalker(v.caps)
		}
	} else if v.scratch == nil {
		v.scratch = bson.NewWalker(nil)
	} else {
		v.scratch.Reset(nil)
	}
	v.accN = 0
	v.st = vDocLen
}

// afterSectionStart runs after a kind-1 identifier: documents follow, or the
// section is already complete.
func (v *MsgValidator) afterSectionStart() {
	if v.pos == v.secEnd {
		v.st = vKind
		return
	}
	if v.secEnd-v.pos < 5 {
		v.fail(ErrMalformed())
		return
	}
	v.beginDoc()
}

// feedDocLen collects the 4 length bytes and also feeds them to the walker.
func (v *MsgValidator) feedDocLen(b []byte) []byte {
	before := v.accN
	rest := v.gather(b)
	got := v.accN - before
	if got > 0 {
		if err := v.walker().Write(v.acc[before:v.accN]); err != nil {
			v.fail(ErrMalformed())
		}
	}
	return rest
}

func (v *MsgValidator) onDocLen() error {
	l := int64(int32(binary.LittleEndian.Uint32(v.acc[:])))
	v.accN = 0
	start := v.pos - 4
	limit := v.dataEnd
	if !v.isBody {
		limit = v.secEnd
	}
	if l < 5 || start+l > limit {
		return v.fail(ErrMalformed())
	}
	v.docLeft = l - 4
	v.docEnd = start + l
	v.st = vDocBody
	return nil
}

// afterDoc runs when a document has been closed successfully.
func (v *MsgValidator) afterDoc() error {
	if v.isBody {
		v.bodies++
		v.st = vKind
		return v.rules()
	}
	if v.pos == v.secEnd {
		v.st = vKind
		return nil
	}
	if v.secEnd-v.pos < 5 {
		return v.fail(ErrMalformed())
	}
	v.beginDoc()
	return nil
}

func (v *MsgValidator) onKind1ID() error {
	if v.isReply || v.idLong {
		return nil
	}
	id := string(v.idBuf[:v.idN])
	for i, n := range inspected {
		if id == n {
			v.kind1 |= 1 << uint(i)
		}
	}
	return v.rules()
}

func (v *MsgValidator) applicable(i int) bool {
	switch i {
	case idxPipeline:
		return v.cmd == "aggregate"
	case idxCollection:
		return v.cmd == "getMore"
	}
	return true
}

// rules applies the FR-60 inspected-field integrity rules to what has been
// read so far. Everything it reports is final: a duplicate or a kind-1
// identifier cannot be undone by later bytes.
func (v *MsgValidator) rules() error {
	if v.isReply {
		return nil
	}
	if !v.cmdKnown && v.body != nil {
		if k, ok := v.body.FirstKey(); ok {
			v.cmd, v.cmdKnown = k, true
		}
	}
	if !v.cmdKnown {
		return nil
	}
	if v.body.TopLevelKeyEqualsFirst() > 0 {
		return v.fail(ErrMalformed())
	}
	for i := range inspected {
		if !v.applicable(i) {
			continue
		}
		if v.kind1&(1<<uint(i)) != 0 {
			return v.fail(ErrMalformed())
		}
		if val, ok := v.body.Captured(i); ok && val.Count > 1 {
			return v.fail(ErrMalformed())
		}
	}
	return nil
}

// Close ends the message: it must be complete, and the checksum must match.
func (v *MsgValidator) Close() error {
	if v.err != nil {
		return v.err
	}
	if v.total < 4 || v.st == vFlags || v.pos != v.total || v.st != vKind || v.bodies != 1 {
		return v.fail(ErrMalformed())
	}
	if !v.isReply {
		if !v.cmdKnown {
			return v.fail(ErrMalformed())
		}
		if err := v.rules(); err != nil {
			return err
		}
	}
	if v.crcOn && binary.LittleEndian.Uint32(v.csum[:]) != v.crc {
		return v.fail(ErrChecksum())
	}
	v.done = true
	return nil
}

// ReplyFields are the only values read from an upstream reply (FR-9). There
// is deliberately no field for writeErrors (FR-9, G-20).
type ReplyFields struct {
	HasOK       bool
	OK          float64
	HasCode     bool
	Code        int32
	CodeName    string
	HasWCECode  bool
	WCECode     int32
	HasCursorID bool
	CursorID    int64
}

// ReplyScanner validates an upstream OP_MSG reply as it streams and reads the
// FR-9 reply fields. Write, Close and Done come from the embedded validator.
type ReplyScanner struct {
	*MsgValidator
}

// NewReplyScanner returns a scanner for the reply whose header is h.
func NewReplyScanner(h Header) *ReplyScanner {
	return &ReplyScanner{NewMsgValidator(h, true)}
}

// Fields returns what has been read so far.
func (s *ReplyScanner) Fields() ReplyFields {
	var f ReplyFields
	w := s.body
	if w == nil {
		return f
	}
	if v, ok := w.Captured(rcOK); ok {
		if n, isNum := v.Number(); isNum {
			f.HasOK, f.OK = true, n
		}
	}
	if v, ok := w.Captured(rcCode); ok {
		if n, isNum := v.Number(); isNum {
			f.HasCode, f.Code = true, int32(n)
		}
	}
	if v, ok := w.Captured(rcCodeName); ok {
		if str, isStr := v.StringValue(); isStr {
			f.CodeName = str
		}
	}
	if v, ok := w.Captured(rcWCECode); ok {
		if n, isNum := v.Number(); isNum {
			f.HasWCECode, f.WCECode = true, int32(n)
		}
	}
	if v, ok := w.Captured(rcCursorID); ok {
		if n, isNum := v.Int64(); isNum {
			f.HasCursorID, f.CursorID = true, n
		}
	}
	return f
}

package faultproxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	opReply      int32 = 1
	opQuery      int32 = 2004
	opCompressed int32 = 2012
	opMsg        int32 = 2013

	flagChecksum   uint32 = 1 << 0
	flagMoreToCome uint32 = 1 << 1
)

const maxFrame = 128 << 20

func readFrame(r io.Reader) ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	n := int(int32(binary.LittleEndian.Uint32(l[:])))
	if n < 16 || n > maxFrame {
		return nil, fmt.Errorf("faultproxy: bad messageLength %d", n)
	}
	b := make([]byte, n)
	copy(b, l[:])
	if _, err := io.ReadFull(r, b[4:]); err != nil {
		return nil, err
	}
	return b, nil
}

// info is what the proxy understands about one frame.
type info struct {
	requestID  int32
	responseTo int32
	opcode     int32
	flags      uint32
	parsed     bool
	body       bson.Raw // kind-0 document (OP_MSG), query (OP_QUERY) or first reply document
	command    string   // requests only
	db         string
	lsid       []byte
	hasTxn     bool
	txn        int64
	startTxn   bool
}

func parseFrame(raw []byte, isRequest bool) info {
	in := info{
		requestID:  int32(binary.LittleEndian.Uint32(raw[4:])),
		responseTo: int32(binary.LittleEndian.Uint32(raw[8:])),
		opcode:     int32(binary.LittleEndian.Uint32(raw[12:])),
	}
	var err error
	switch in.opcode {
	case opMsg:
		in.body, in.flags, _, err = splitOpMsg(raw)
	case opQuery:
		if len(raw) < 21 {
			return in
		}
		in.flags = binary.LittleEndian.Uint32(raw[16:])
		rest := raw[20:]
		i := strings.IndexByte(string(rest), 0)
		if i < 0 || len(rest) < i+1+8+5 {
			return in
		}
		ns := string(rest[:i])
		in.db, _, _ = strings.Cut(ns, ".")
		in.body, err = cutDoc(rest[i+1+8:])
	case opReply:
		if len(raw) < 36+5 {
			return in
		}
		in.body, err = cutDoc(raw[36:])
	default:
		return in
	}
	if err != nil || in.body == nil {
		in.body = nil
		return in
	}
	in.parsed = true
	if isRequest {
		if e, err := in.body.IndexErr(0); err == nil {
			in.command = e.Key()
		}
		if in.opcode == opMsg {
			in.db, _ = in.body.Lookup("$db").StringValueOK()
		}
		if v, err := in.body.LookupErr("lsid", "id"); err == nil {
			if _, b, ok := v.BinaryOK(); ok {
				in.lsid = append([]byte(nil), b...)
			}
		}
		if v, err := in.body.LookupErr("txnNumber"); err == nil {
			in.hasTxn = true
			in.txn, _ = v.AsInt64OK()
		}
		in.startTxn, _ = in.body.Lookup("startTransaction").BooleanOK()
	}
	return in
}

func cutDoc(b []byte) (bson.Raw, error) {
	if len(b) < 5 {
		return nil, errors.New("short doc")
	}
	n := int(int32(binary.LittleEndian.Uint32(b)))
	if n < 5 || n > len(b) {
		return nil, errors.New("bad doc length")
	}
	d := bson.Raw(b[:n])
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return d, nil
}

// splitOpMsg returns the kind-0 doc, the flags and the raw sections (without
// flagBits and checksum).
func splitOpMsg(raw []byte) (body bson.Raw, flags uint32, sections []byte, err error) {
	if len(raw) < 21 {
		return nil, 0, nil, errors.New("short OP_MSG")
	}
	flags = binary.LittleEndian.Uint32(raw[16:])
	end := len(raw)
	if flags&flagChecksum != 0 {
		end -= 4
	}
	sections = raw[20:end]
	s := sections
	for len(s) > 0 {
		switch s[0] {
		case 0:
			d, e := cutDoc(s[1:])
			if e != nil {
				return nil, flags, sections, e
			}
			if body == nil {
				body = d
			}
			s = s[1+len(d):]
		case 1:
			if len(s) < 5 {
				return nil, flags, sections, errors.New("short kind-1")
			}
			n := int(int32(binary.LittleEndian.Uint32(s[1:])))
			if n < 5 || 1+n > len(s) {
				return nil, flags, sections, errors.New("bad kind-1")
			}
			s = s[1+n:]
		default:
			return nil, flags, sections, errors.New("bad section kind")
		}
	}
	return body, flags, sections, nil
}

// rewriteReply replaces the first document of a reply frame with doc and
// returns a frame with a correct messageLength and no checksum.
func rewriteReply(raw []byte, doc []byte) ([]byte, error) {
	opcode := int32(binary.LittleEndian.Uint32(raw[12:]))
	var out []byte
	switch opcode {
	case opMsg:
		body, flags, sections, err := splitOpMsg(raw)
		if err != nil || body == nil {
			return nil, errors.New("cannot rewrite")
		}
		// Rebuild sections with the kind-0 document swapped.
		var secs []byte
		s := sections
		for len(s) > 0 {
			if s[0] == 0 {
				n := int(int32(binary.LittleEndian.Uint32(s[1:])))
				secs = append(append(secs, 0), doc...)
				s = s[1+n:]
			} else {
				n := int(int32(binary.LittleEndian.Uint32(s[1:])))
				secs = append(secs, s[:1+n]...)
				s = s[1+n:]
			}
		}
		out = make([]byte, 20+len(secs))
		copy(out, raw[:16])
		binary.LittleEndian.PutUint32(out[16:], flags&^flagChecksum)
		copy(out[20:], secs)
	case opReply:
		old := int(int32(binary.LittleEndian.Uint32(raw[36:])))
		out = append(append([]byte(nil), raw[:36]...), doc...)
		out = append(out, raw[36+old:]...)
	default:
		return nil, errors.New("cannot rewrite opcode")
	}
	binary.LittleEndian.PutUint32(out, uint32(len(out)))
	return out, nil
}

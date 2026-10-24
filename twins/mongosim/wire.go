package mongosim

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Wire opcodes.
const (
	OpReply      int32 = 1
	OpQuery      int32 = 2004
	OpCompressed int32 = 2012
	OpMsg        int32 = 2013
)

// OP_MSG flag bits.
const (
	FlagChecksumPresent uint32 = 1 << 0
	FlagMoreToCome      uint32 = 1 << 1
	FlagExhaustAllowed  uint32 = 1 << 16
)

// maxFrame bounds what the twin will read from a peer.
const maxFrame = 128 << 20

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// readFrame reads one complete wire message (header included).
func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(int32(binary.LittleEndian.Uint32(hdr[:])))
	if n < 16 || n > maxFrame {
		return nil, fmt.Errorf("mongosim: bad messageLength %d", n)
	}
	buf := make([]byte, n)
	copy(buf, hdr[:])
	if _, err := io.ReadFull(r, buf[4:]); err != nil {
		return nil, err
	}
	return buf, nil
}

type header struct {
	length     int32
	requestID  int32
	responseTo int32
	opcode     int32
}

func parseHeader(b []byte) header {
	return header{
		length:     int32(binary.LittleEndian.Uint32(b[0:])),
		requestID:  int32(binary.LittleEndian.Uint32(b[4:])),
		responseTo: int32(binary.LittleEndian.Uint32(b[8:])),
		opcode:     int32(binary.LittleEndian.Uint32(b[12:])),
	}
}

func putHeader(b []byte, h header) {
	binary.LittleEndian.PutUint32(b[0:], uint32(h.length))
	binary.LittleEndian.PutUint32(b[4:], uint32(h.requestID))
	binary.LittleEndian.PutUint32(b[8:], uint32(h.responseTo))
	binary.LittleEndian.PutUint32(b[12:], uint32(h.opcode))
}

// parsed is a decoded request.
type parsed struct {
	hdr   header
	flags uint32
	body  bson.Raw
	seqs  map[string][]bson.Raw
	db    string
}

func parseRequest(raw []byte) (*parsed, error) {
	h := parseHeader(raw)
	p := &parsed{hdr: h}
	switch h.opcode {
	case OpMsg:
		if len(raw) < 16+4+1 {
			return nil, errors.New("short OP_MSG")
		}
		p.flags = binary.LittleEndian.Uint32(raw[16:])
		end := len(raw)
		if p.flags&FlagChecksumPresent != 0 {
			end -= 4
		}
		secs := raw[20:end]
		for len(secs) > 0 {
			kind := secs[0]
			secs = secs[1:]
			switch kind {
			case 0:
				doc, rest, err := cutDoc(secs)
				if err != nil {
					return nil, err
				}
				if p.body != nil {
					return nil, errors.New("two kind-0 sections")
				}
				p.body, secs = doc, rest
			case 1:
				if len(secs) < 4 {
					return nil, errors.New("short kind-1 section")
				}
				sz := int(int32(binary.LittleEndian.Uint32(secs)))
				if sz < 5 || sz > len(secs) {
					return nil, errors.New("bad kind-1 size")
				}
				sec := secs[4:sz]
				secs = secs[sz:]
				i := strings.IndexByte(string(sec), 0)
				if i < 0 {
					return nil, errors.New("kind-1 identifier unterminated")
				}
				id := string(sec[:i])
				sec = sec[i+1:]
				for len(sec) > 0 {
					doc, rest, err := cutDoc(sec)
					if err != nil {
						return nil, err
					}
					if p.seqs == nil {
						p.seqs = map[string][]bson.Raw{}
					}
					p.seqs[id] = append(p.seqs[id], doc)
					sec = rest
				}
			default:
				return nil, fmt.Errorf("unknown section kind %d", kind)
			}
		}
		if p.body == nil {
			return nil, errors.New("no kind-0 section")
		}
		p.db, _ = p.body.Lookup("$db").StringValueOK()
	case OpQuery:
		if len(raw) < 16+4+1 {
			return nil, errors.New("short OP_QUERY")
		}
		p.flags = binary.LittleEndian.Uint32(raw[16:])
		rest := raw[20:]
		i := strings.IndexByte(string(rest), 0)
		if i < 0 {
			return nil, errors.New("OP_QUERY namespace unterminated")
		}
		ns := string(rest[:i])
		rest = rest[i+1:]
		if len(rest) < 8 {
			return nil, errors.New("short OP_QUERY")
		}
		doc, _, err := cutDoc(rest[8:])
		if err != nil {
			return nil, err
		}
		p.body = doc
		p.db, _, _ = strings.Cut(ns, ".")
	default:
		return nil, fmt.Errorf("unsupported opcode %d", h.opcode)
	}
	return p, nil
}

func cutDoc(b []byte) (bson.Raw, []byte, error) {
	if len(b) < 5 {
		return nil, nil, errors.New("short document")
	}
	n := int(int32(binary.LittleEndian.Uint32(b)))
	if n < 5 || n > len(b) {
		return nil, nil, errors.New("bad document length")
	}
	d := bson.Raw(b[:n])
	if err := d.Validate(); err != nil {
		return nil, nil, err
	}
	return d, b[n:], nil
}

// checksum modes for buildOpMsg.
const (
	crcNone = iota
	crcValid
	crcInvalid
)

// buildOpMsg assembles an OP_MSG from raw sections (everything after flagBits).
func buildOpMsg(requestID, responseTo int32, flags uint32, sections []byte, crc int) []byte {
	if crc != crcNone {
		flags |= FlagChecksumPresent
	}
	n := 16 + 4 + len(sections)
	if crc != crcNone {
		n += 4
	}
	b := make([]byte, n)
	putHeader(b, header{length: int32(n), requestID: requestID, responseTo: responseTo, opcode: OpMsg})
	binary.LittleEndian.PutUint32(b[16:], flags)
	copy(b[20:], sections)
	if crc != crcNone {
		sum := crc32.Checksum(b[:n-4], castagnoli)
		if crc == crcInvalid {
			sum ^= 0xdeadbeef
		}
		binary.LittleEndian.PutUint32(b[n-4:], sum)
	}
	return b
}

func kind0(doc []byte) []byte {
	return append([]byte{0}, doc...)
}

// buildOpReply assembles a legacy OP_REPLY with one document.
func buildOpReply(requestID, responseTo int32, doc []byte) []byte {
	n := 16 + 4 + 8 + 4 + 4 + len(doc)
	b := make([]byte, n)
	putHeader(b, header{length: int32(n), requestID: requestID, responseTo: responseTo, opcode: OpReply})
	binary.LittleEndian.PutUint32(b[16:], 8) // AwaitCapable
	binary.LittleEndian.PutUint32(b[32:], 1) // numberReturned
	copy(b[36:], doc)
	return b
}

// commandName returns the first key of the command document.
func commandName(body bson.Raw) string {
	e, err := body.IndexErr(0)
	if err != nil {
		return ""
	}
	return e.Key()
}

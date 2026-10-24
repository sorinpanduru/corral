// Package rawwire is a raw MongoDB wire-protocol client for tests (ADR-0022).
// It builds OP_MSG, OP_QUERY and arbitrary bytes, sends them whole or split
// with pauses, and parses replies. The encoding is independent of
// internal/domain: it uses the official driver's bson package. Test
// infrastructure only; data is synthetic.
package rawwire

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"net"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Opcodes.
const (
	OpCodeReply = 1
	OpCodeQuery = 2004
	OpCodeMsg   = 2013
)

// ErrClosed means the peer closed the connection (possibly mid-message).
// ErrTimeout means nothing (or not enough) arrived in time.
var (
	ErrClosed  = errors.New("rawwire: connection closed")
	ErrTimeout = errors.New("rawwire: timeout")
)

const maxMessage = 64 << 20

// Header is a parsed message header.
type Header struct {
	Length     int32
	RequestID  int32
	ResponseTo int32
	OpCode     int32
}

// Message is a parsed reply: the header, the flag word (OP_MSG flagBits or
// OP_REPLY responseFlags), every document in order, and the raw bytes.
type Message struct {
	Header Header
	Flags  uint32
	Docs   []bson.Raw
	Raw    []byte
}

// Conn is a client connection.
type Conn struct {
	tb testing.TB
	c  net.Conn
}

// Dial connects over plain TCP; the connection closes with the test.
func Dial(tb testing.TB, addr string) *Conn {
	tb.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		tb.Fatalf("rawwire: dial failed: %v", err)
	}
	tb.Cleanup(func() { c.Close() })
	return &Conn{tb: tb, c: c}
}

// DialTLS connects and completes a TLS handshake using cfg.
func DialTLS(tb testing.TB, addr string, cfg *tls.Config) *Conn {
	tb.Helper()
	raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		tb.Fatalf("rawwire: dial failed: %v", err)
	}
	c := tls.Client(raw, cfg)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := c.Handshake(); err != nil {
		raw.Close()
		tb.Fatalf("rawwire: TLS handshake failed: %v", err)
	}
	_ = c.SetDeadline(time.Time{})
	tb.Cleanup(func() { c.Close() })
	return &Conn{tb: tb, c: c}
}

// DialStalledClientHello opens a TCP connection and sends only the start of a
// TLS ClientHello record, then sends nothing more. The caller closes it.
func DialStalledClientHello(tb testing.TB, addr string) net.Conn {
	tb.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		tb.Fatalf("rawwire: dial failed: %v", err)
	}
	tb.Cleanup(func() { c.Close() })
	// Record header: handshake, TLS 1.0 record version, claimed length 512;
	// then the ClientHello type, a 3-byte length and a few bytes.
	partial := []byte{0x16, 0x03, 0x01, 0x02, 0x00, 0x01, 0x00, 0x01, 0xfc, 0x03, 0x03, 0, 0, 0, 0, 0}
	if _, err := c.Write(partial); err != nil {
		tb.Fatalf("rawwire: write failed: %v", err)
	}
	return c
}

// NetConn returns the underlying connection.
func (c *Conn) NetConn() net.Conn { return c.c }

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Close() }

// Send writes raw in one write.
func (c *Conn) Send(raw []byte) error {
	_, err := c.c.Write(raw)
	return err
}

// SendSplit writes raw in chunks of the given sizes (any remainder in a final
// chunk), sleeping pause between chunks.
func (c *Conn) SendSplit(raw []byte, sizes []int, pause time.Duration) error {
	off := 0
	for _, n := range sizes {
		if n > len(raw)-off {
			n = len(raw) - off
		}
		if n <= 0 {
			continue
		}
		if off > 0 {
			time.Sleep(pause)
		}
		if _, err := c.c.Write(raw[off : off+n]); err != nil {
			return err
		}
		off += n
	}
	if off < len(raw) {
		if off > 0 {
			time.Sleep(pause)
		}
		_, err := c.c.Write(raw[off:])
		return err
	}
	return nil
}

func mapErr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrTimeout
	}
	return ErrClosed
}

// ReadMessage reads one message within timeout. It returns ErrTimeout or
// ErrClosed (including EOF in the middle of a message).
func (c *Conn) ReadMessage(timeout time.Duration) (*Message, error) {
	_ = c.c.SetReadDeadline(time.Now().Add(timeout))
	defer c.c.SetReadDeadline(time.Time{})
	var lb [4]byte
	if _, err := io.ReadFull(c.c, lb[:]); err != nil {
		return nil, mapErr(err)
	}
	n := int(int32(binary.LittleEndian.Uint32(lb[:])))
	if n < 16 || n > maxMessage {
		return nil, errors.New("rawwire: bad message length")
	}
	raw := make([]byte, n)
	copy(raw, lb[:])
	if _, err := io.ReadFull(c.c, raw[4:]); err != nil {
		return nil, mapErr(err)
	}
	return parse(raw)
}

// ExpectClosedNoBytes succeeds only if the peer closes the connection without
// sending a byte within timeout.
func (c *Conn) ExpectClosedNoBytes(timeout time.Duration) error {
	_ = c.c.SetReadDeadline(time.Now().Add(timeout))
	defer c.c.SetReadDeadline(time.Time{})
	var b [1]byte
	n, err := c.c.Read(b[:])
	if n > 0 {
		return errors.New("rawwire: bytes arrived before the close")
	}
	if err == nil {
		return errors.New("rawwire: unexpected empty read")
	}
	if mapErr(err) == ErrTimeout {
		return errors.New("rawwire: connection still open")
	}
	return nil
}

func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

func parse(raw []byte) (*Message, error) {
	m := &Message{Raw: raw, Header: Header{
		Length: int32(le32(raw)), RequestID: int32(le32(raw[4:])),
		ResponseTo: int32(le32(raw[8:])), OpCode: int32(le32(raw[12:])),
	}}
	body := raw[16:]
	switch m.Header.OpCode {
	case OpCodeMsg:
		if len(body) < 4 {
			return nil, errors.New("rawwire: short OP_MSG")
		}
		m.Flags = le32(body)
		body = body[4:]
		if m.Flags&1 != 0 && len(body) >= 4 {
			body = body[:len(body)-4]
		}
		for len(body) > 0 {
			kind := body[0]
			body = body[1:]
			switch kind {
			case 0:
				d, rest, err := takeDoc(body)
				if err != nil {
					return nil, err
				}
				m.Docs, body = append(m.Docs, d), rest
			case 1:
				if len(body) < 4 {
					return nil, errors.New("rawwire: short kind-1 section")
				}
				size := int(le32(body))
				if size < 5 || size > len(body) {
					return nil, errors.New("rawwire: bad kind-1 size")
				}
				sec := body[4:size]
				body = body[size:]
				i := 0
				for i < len(sec) && sec[i] != 0 {
					i++
				}
				sec = sec[min(i+1, len(sec)):]
				for len(sec) > 0 {
					d, rest, err := takeDoc(sec)
					if err != nil {
						return nil, err
					}
					m.Docs, sec = append(m.Docs, d), rest
				}
			default:
				return nil, errors.New("rawwire: unknown section kind")
			}
		}
	case OpCodeReply:
		if len(body) < 20 {
			return nil, errors.New("rawwire: short OP_REPLY")
		}
		m.Flags = le32(body)
		body = body[20:]
		for len(body) > 0 {
			d, rest, err := takeDoc(body)
			if err != nil {
				return nil, err
			}
			m.Docs, body = append(m.Docs, d), rest
		}
	}
	return m, nil
}

func takeDoc(b []byte) (bson.Raw, []byte, error) {
	if len(b) < 5 {
		return nil, nil, errors.New("rawwire: short document")
	}
	n := int(le32(b))
	if n < 5 || n > len(b) {
		return nil, nil, errors.New("rawwire: bad document length")
	}
	return bson.Raw(b[:n]), b[n:], nil
}

// ChecksumMode selects the OP_MSG trailing checksum.
type ChecksumMode int

const (
	ChecksumNone ChecksumMode = iota
	ChecksumValid
	ChecksumInvalid
)

// Sequence is a kind-1 document sequence.
type Sequence struct {
	ID   string
	Docs []bson.D
}

// OpMsgOpts describes an OP_MSG. Body (any value bson.Marshal accepts, such
// as an ordered bson.D, which keeps duplicate keys) or BodyRaw supplies the
// kind-0 document.
type OpMsgOpts struct {
	RequestID      int32
	ResponseTo     int32
	Flags          uint32
	Body           any
	BodyRaw        []byte
	Sequences      []Sequence
	SequencesFirst bool
	Checksum       ChecksumMode
}

func must(v any) []byte {
	b, err := bson.Marshal(v)
	if err != nil {
		panic("rawwire: cannot marshal test document")
	}
	return b
}

func put32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }

// OpMsg builds an OP_MSG with a correct header length.
func OpMsg(o OpMsgOpts) []byte {
	body := o.BodyRaw
	if body == nil {
		body = must(o.Body)
	}
	kind0 := append([]byte{0}, body...)
	var seqs []byte
	for _, s := range o.Sequences {
		var docs []byte
		for _, d := range s.Docs {
			docs = append(docs, must(d)...)
		}
		sec := append([]byte{1}, put32(nil, uint32(4+len(s.ID)+1+len(docs)))...)
		sec = append(sec, s.ID...)
		sec = append(sec, 0)
		seqs = append(seqs, append(sec, docs...)...)
	}
	flags := o.Flags
	if o.Checksum != ChecksumNone {
		flags |= 1
	}
	payload := put32(nil, flags)
	if o.SequencesFirst {
		payload = append(append(payload, seqs...), kind0...)
	} else {
		payload = append(append(payload, kind0...), seqs...)
	}
	total := 16 + len(payload)
	if o.Checksum != ChecksumNone {
		total += 4
	}
	msg := put32(nil, uint32(total))
	msg = put32(msg, uint32(o.RequestID))
	msg = put32(msg, uint32(o.ResponseTo))
	msg = put32(msg, OpCodeMsg)
	msg = append(msg, payload...)
	if o.Checksum != ChecksumNone {
		sum := crc32.Checksum(msg, crc32.MakeTable(crc32.Castagnoli))
		if o.Checksum == ChecksumInvalid {
			sum ^= 0xFFFFFFFF
		}
		msg = put32(msg, sum)
	}
	return msg
}

// OpQueryOpts describes an OP_QUERY, with switches for malformed variants.
type OpQueryOpts struct {
	RequestID             int32
	FullCollection        string
	NumberToReturn        int32
	Query                 any
	WrapInQuery           bool
	OmitNumberToReturn    bool
	OmitQuery             bool
	Extra                 []byte
	UnterminatedNamespace bool
}

// OpQuery builds an OP_QUERY; the header length always matches the bytes.
func OpQuery(o OpQueryOpts) []byte {
	payload := put32(nil, 0) // flags
	payload = append(payload, o.FullCollection...)
	if !o.UnterminatedNamespace {
		payload = append(payload, 0)
	}
	payload = put32(payload, 0) // numberToSkip
	if !o.OmitNumberToReturn {
		payload = put32(payload, uint32(o.NumberToReturn))
	}
	if !o.OmitQuery {
		q := o.Query
		if o.WrapInQuery {
			q = bson.D{{Key: "$query", Value: o.Query}}
		}
		payload = append(payload, must(q)...)
	}
	payload = append(payload, o.Extra...)
	msg := put32(nil, uint32(16+len(payload)))
	msg = put32(msg, uint32(o.RequestID))
	msg = put32(msg, 0)
	msg = put32(msg, OpCodeQuery)
	return append(msg, payload...)
}

// RawHeader builds a header with exactly the given values (length is not
// computed) followed by body.
func RawHeader(length, requestID, responseTo, opCode int32, body []byte) []byte {
	b := put32(nil, uint32(length))
	b = put32(b, uint32(requestID))
	b = put32(b, uint32(responseTo))
	b = put32(b, uint32(opCode))
	return append(b, body...)
}

// HTTPGet returns a plain HTTP request, for sending to a wire port.
func HTTPGet() []byte {
	return []byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
}

// NestedBody returns a document nested n levels deep (the top-level document
// is level 1) ending in one int32. With mixed, levels alternate documents and
// arrays. It is built in one linear pass, so very large n is cheap.
func NestedBody(n int, mixed bool) []byte {
	leaf := []byte{15, 0, 0, 0, 0x10, 'l', 'e', 'a', 'f', 0, 1, 0, 0, 0, 0}
	if n < 1 {
		n = 1
	}
	out := make([]byte, 0, n*8+len(leaf))
	for k := 1; k < n; k++ {
		size := len(leaf) + 8*(n-k)
		typ, key := byte(0x03), byte('d')
		if mixed && k%2 == 0 {
			typ, key = 0x04, '0'
		}
		out = put32(out, uint32(size))
		out = append(out, typ, key, 0)
	}
	out = append(out, leaf...)
	for k := 1; k < n; k++ {
		out = append(out, 0)
	}
	return out
}

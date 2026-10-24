package rawwire_test

// Self-test for rawwire. The API below is the interface fixed by this test
// (the plan leaves exact signatures to the implementer; see the test-writer
// report). Wire encoding is independent of internal/domain: it uses the
// official driver's bson package. Round trips go through a loopback server
// owned by this file.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"net"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

const (
	opMsg   = 2013
	opQuery = 2004
	opReply = 1
)

func u32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

// echoServer accepts one connection and runs fn on it.
func server(t *testing.T, fn func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		fn(c)
	}()
	return ln.Addr().String()
}

// readFrame reads one length-prefixed message from c.
func readFrame(c net.Conn) ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return nil, err
	}
	n := int(u32(l[:]))
	if n < 4 || n > 1<<26 {
		return nil, errors.New("bad frame length")
	}
	buf := make([]byte, n)
	copy(buf, l[:])
	_, err := io.ReadFull(c, buf[4:])
	return buf, err
}

func echo(c net.Conn) {
	for {
		m, err := readFrame(c)
		if err != nil {
			return
		}
		c.Write(m)
	}
}

func mustRaw(t *testing.T, v any) bson.Raw {
	t.Helper()
	b, err := bson.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// OpMsg builds a header-correct OP_MSG with the chosen request ID, flags and
// kind-0 body, and ReadMessage parses it back.
func TestOpMsgRoundTrip(t *testing.T) {
	addr := server(t, echo)
	c := rawwire.Dial(t, addr)
	raw := rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 77, ResponseTo: 5, Flags: 0, Body: bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: "admin"}}})
	if int(u32(raw)) != len(raw) || u32(raw[4:]) != 77 || u32(raw[8:]) != 5 || u32(raw[12:]) != opMsg {
		t.Fatalf("header wrong: %v", raw[:16])
	}
	if err := c.Send(raw); err != nil {
		t.Fatal(err)
	}
	m, err := c.ReadMessage(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.OpCode != opMsg || m.Header.RequestID != 77 || m.Header.ResponseTo != 5 || int(m.Header.Length) != len(raw) {
		t.Fatalf("parsed header %+v", m.Header)
	}
	if !bytes.Equal(m.Raw, raw) {
		t.Fatal("Raw differs from the bytes sent")
	}
	if len(m.Docs) != 1 {
		t.Fatalf("Docs = %d, want 1", len(m.Docs))
	}
	if v, err := m.Docs[0].LookupErr("$db"); err != nil || v.StringValue() != "admin" {
		t.Fatalf("body not parsed: %v", err)
	}
}

// Flags are written as given (moreToCome, exhaustAllowed, unknown bits).
func TestOpMsgFlags(t *testing.T) {
	for _, f := range []uint32{0, 1 << 1, 1 << 16, 1 << 2, 0x80} {
		raw := rawwire.OpMsg(rawwire.OpMsgOpts{Flags: f, Body: bson.D{{Key: "ping", Value: 1}}})
		if got := u32(raw[16:]); got != f {
			t.Fatalf("flags = %#x, want %#x", got, f)
		}
	}
}

// Checksum modes: none leaves the flag clear; valid sets checksumPresent
// (bit 0) and appends the CRC-32C of the message; invalid appends a wrong one.
func TestOpMsgChecksum(t *testing.T) {
	body := bson.D{{Key: "ping", Value: 1}}
	none := rawwire.OpMsg(rawwire.OpMsgOpts{Body: body})
	if u32(none[16:])&1 != 0 {
		t.Fatal("checksumPresent set without a checksum")
	}
	valid := rawwire.OpMsg(rawwire.OpMsgOpts{Body: body, Checksum: rawwire.ChecksumValid})
	if u32(valid[16:])&1 != 1 || int(u32(valid)) != len(valid) || len(valid) != len(none)+4 {
		t.Fatal("valid checksum layout wrong")
	}
	want := crc32.Checksum(valid[:len(valid)-4], crc32.MakeTable(crc32.Castagnoli))
	if got := u32(valid[len(valid)-4:]); got != want {
		t.Fatalf("checksum = %#x, want %#x", got, want)
	}
	bad := rawwire.OpMsg(rawwire.OpMsgOpts{Body: body, Checksum: rawwire.ChecksumInvalid})
	if u32(bad[16:])&1 != 1 || len(bad) != len(valid) {
		t.Fatal("invalid checksum layout wrong")
	}
	if got := u32(bad[len(bad)-4:]); got == crc32.Checksum(bad[:len(bad)-4], crc32.MakeTable(crc32.Castagnoli)) {
		t.Fatal("ChecksumInvalid produced a valid checksum")
	}
}

// Kind-1 sequences go after the body by default and before it with
// SequencesFirst (Amendment 1 builder); the parsed Docs list the body first
// in either case only for messages the server echoes back unchanged, so the
// test checks the raw section order instead.
func TestOpMsgSectionOrder(t *testing.T) {
	seq := rawwire.Sequence{ID: "documents", Docs: []bson.D{{{Key: "_id", Value: 1}}, {{Key: "_id", Value: 2}}}}
	body := bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "d"}}

	after := rawwire.OpMsg(rawwire.OpMsgOpts{Body: body, Sequences: []rawwire.Sequence{seq}})
	before := rawwire.OpMsg(rawwire.OpMsgOpts{Body: body, Sequences: []rawwire.Sequence{seq}, SequencesFirst: true})
	if len(after) != len(before) {
		t.Fatal("same content must have the same length")
	}
	if after[20] != 0 {
		t.Fatalf("default: first section kind = %d, want 0", after[20])
	}
	if before[20] != 1 {
		t.Fatalf("SequencesFirst: first section kind = %d, want 1", before[20])
	}
	// Kind-1 layout: kind byte, int32 size (includes itself), cstring id, docs.
	size := int(u32(before[21:]))
	if string(before[25:25+len("documents")]) != "documents" || before[25+len("documents")] != 0 {
		t.Fatal("kind-1 identifier not a cstring")
	}
	if before[21+size] != 0 {
		t.Fatalf("kind-1 size does not lead to the body section")
	}
}

// Raw documents can be supplied as bytes (for deliberately odd BSON), and
// duplicate top-level keys survive encoding in order.
func TestOpMsgDuplicateKeys(t *testing.T) {
	addr := server(t, echo)
	c := rawwire.Dial(t, addr)
	body := bson.D{{Key: "find", Value: "a"}, {Key: "$db", Value: "d"}, {Key: "find", Value: "b"}, {Key: "lsid", Value: bson.D{{Key: "id", Value: 1}}}, {Key: "lsid", Value: bson.D{{Key: "id", Value: 2}}}}
	if err := c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{Body: body})); err != nil {
		t.Fatal(err)
	}
	m, err := c.ReadMessage(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	els, err := m.Docs[0].Elements()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range els {
		keys = append(keys, e.Key())
	}
	want := []string{"find", "$db", "find", "lsid", "lsid"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v", keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys = %v, want %v", keys, want)
		}
	}
	raw := rawwire.OpMsg(rawwire.OpMsgOpts{BodyRaw: mustRaw(t, bson.D{{Key: "ping", Value: 1}})})
	if u32(raw[12:]) != opMsg || raw[20] != 0 {
		t.Fatal("BodyRaw not used as the kind-0 section")
	}
}

// NestedBody builds a body nested N levels (top-level document is level 1)
// with documents and arrays alternating, without recursion in the encoder.
func TestNestedBody(t *testing.T) {
	for _, n := range []int{1, 2, 10, 1024, 1025} {
		b := rawwire.NestedBody(n, true)
		if int(binary.LittleEndian.Uint32(b)) != len(b) || b[len(b)-1] != 0 {
			t.Fatalf("n=%d: not a document", n)
		}
		depth, sawDoc, sawArr := 1, false, false
		cur := []byte(b)
		for {
			// Find a nested document or array element in cur (skip the 4-byte
			// length); stop when the only element is the scalar leaf.
			if cur[4] == 0x03 || cur[4] == 0x04 {
				sawDoc = sawDoc || cur[4] == 0x03
				sawArr = sawArr || cur[4] == 0x04
				i := 5
				for cur[i] != 0 {
					i++
				}
				cur = cur[i+1:]
				depth++
				continue
			}
			break
		}
		if depth != n {
			t.Fatalf("NestedBody(%d) has depth %d", n, depth)
		}
		if n >= 3 && (!sawDoc || !sawArr) {
			t.Fatalf("n=%d: mixed nesting expected (doc=%v arr=%v)", n, sawDoc, sawArr)
		}
	}
	// Non-mixed: documents only.
	b := rawwire.NestedBody(5, false)
	if b[4] != 0x03 {
		t.Fatal("mixed=false should nest documents only")
	}
	// Depth must not be capped by the builder: 100000 levels is just bytes.
	if len(rawwire.NestedBody(100000, true)) < 100000*4 {
		t.Fatal("NestedBody(100000) too short")
	}
}

// OpQuery: layout, $query wrapper, and the malformed variants.
func TestOpQuery(t *testing.T) {
	q := bson.D{{Key: "hello", Value: 1}, {Key: "helloOk", Value: true}}
	raw := rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: 9, FullCollection: "admin.$cmd", NumberToReturn: -1, Query: q})
	if int(u32(raw)) != len(raw) || u32(raw[4:]) != 9 || u32(raw[12:]) != opQuery {
		t.Fatal("header wrong")
	}
	if u32(raw[16:]) != 0 {
		t.Fatalf("flags = %#x", u32(raw[16:]))
	}
	ns := "admin.$cmd"
	if string(raw[20:20+len(ns)]) != ns || raw[20+len(ns)] != 0 {
		t.Fatal("full collection name not a cstring")
	}
	off := 21 + len(ns)
	if int32(u32(raw[off:])) != 0 || int32(u32(raw[off+4:])) != -1 {
		t.Fatal("numberToSkip/numberToReturn wrong")
	}
	doc := bson.Raw(raw[off+8:])
	if _, err := doc.LookupErr("hello"); err != nil {
		t.Fatalf("query doc: %v", err)
	}

	wrapped := rawwire.OpQuery(rawwire.OpQueryOpts{FullCollection: "admin.$cmd", NumberToReturn: -1, Query: q, WrapInQuery: true})
	w := bson.Raw(wrapped[off+8:])
	inner, err := w.LookupErr("$query")
	if err != nil {
		t.Fatalf("no $query wrapper: %v", err)
	}
	if _, err := inner.Document().LookupErr("hello"); err != nil {
		t.Fatal("wrapped query lost its fields")
	}

	// Malformed variants keep a correct header length but break the body.
	for name, o := range map[string]rawwire.OpQueryOpts{
		"missing numberToReturn": {FullCollection: "admin.$cmd", Query: q, OmitNumberToReturn: true},
		"missing query":          {FullCollection: "admin.$cmd", NumberToReturn: -1, OmitQuery: true},
		"extra trailing bytes":   {FullCollection: "admin.$cmd", NumberToReturn: -1, Query: q, Extra: []byte{1, 2, 3}},
		"unterminated namespace": {FullCollection: "admin.$cmd", NumberToReturn: -1, Query: q, UnterminatedNamespace: true},
	} {
		m := rawwire.OpQuery(o)
		if int(u32(m)) != len(m) {
			t.Errorf("%s: header length %d != %d", name, u32(m), len(m))
		}
	}
	if a, b := rawwire.OpQuery(rawwire.OpQueryOpts{FullCollection: "a.b", NumberToReturn: -1, Query: q}), rawwire.OpQuery(rawwire.OpQueryOpts{FullCollection: "a.b", NumberToReturn: -1, Query: q, Extra: []byte{1, 2, 3}}); len(b) != len(a)+3 {
		t.Fatal("Extra not appended")
	}
}

// RawHeader and HTTPGet build arbitrary bytes.
func TestRawBuilders(t *testing.T) {
	r := rawwire.RawHeader(1000, 3, 4, 9999, []byte{1, 2})
	if len(r) != 18 || int32(u32(r)) != 1000 || u32(r[4:]) != 3 || u32(r[8:]) != 4 || u32(r[12:]) != 9999 {
		t.Fatalf("RawHeader = %v", r)
	}
	if !bytes.HasPrefix(rawwire.HTTPGet(), []byte("GET / HTTP/1.1\r\n")) {
		t.Fatal("HTTPGet is not an HTTP request")
	}
}

// Send in one write, or split with pauses: the server sees the same bytes, and
// the pauses are real.
func TestSendSplit(t *testing.T) {
	got := make(chan []byte, 1)
	stamps := make(chan time.Time, 16)
	addr := server(t, func(c net.Conn) {
		var all []byte
		buf := make([]byte, 64)
		for len(all) < 40 {
			n, err := c.Read(buf)
			if n > 0 {
				all = append(all, buf[:n]...)
				stamps <- time.Now()
			}
			if err != nil {
				break
			}
		}
		got <- all
	})
	c := rawwire.Dial(t, addr)
	raw := make([]byte, 40)
	for i := range raw {
		raw[i] = byte(i)
	}
	start := time.Now()
	if err := c.SendSplit(raw, []int{10, 10, 20}, 60*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("pauses between chunks were not applied")
	}
	select {
	case all := <-got:
		if !bytes.Equal(all, raw) {
			t.Fatal("bytes differ after a split send")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not receive the bytes")
	}
}

// ReadMessage: ErrTimeout when quiet, ErrClosed on EOF, and parsing of an
// OP_REPLY (the code 352 refusal for legacy opcodes).
func TestReadMessageErrorsAndReply(t *testing.T) {
	reply := func() []byte {
		doc := mustRaw(t, bson.D{{Key: "ok", Value: 0.0}, {Key: "code", Value: 352}})
		body := make([]byte, 20)
		binary.LittleEndian.PutUint32(body[0:], 8)  // responseFlags: QueryFailure? any
		binary.LittleEndian.PutUint32(body[16:], 1) // numberReturned
		body = append(body, doc...)
		hdr := make([]byte, 16)
		binary.LittleEndian.PutUint32(hdr[0:], uint32(16+len(body)))
		binary.LittleEndian.PutUint32(hdr[4:], 100)
		binary.LittleEndian.PutUint32(hdr[8:], 9)
		binary.LittleEndian.PutUint32(hdr[12:], opReply)
		return append(hdr, body...)
	}()
	release := make(chan struct{})
	addr := server(t, func(c net.Conn) {
		<-release
		c.Write(reply)
		time.Sleep(50 * time.Millisecond)
	})
	c := rawwire.Dial(t, addr)
	if _, err := c.ReadMessage(100 * time.Millisecond); !errors.Is(err, rawwire.ErrTimeout) {
		t.Fatalf("quiet connection: %v, want ErrTimeout", err)
	}
	close(release)
	m, err := c.ReadMessage(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.OpCode != opReply || m.Header.ResponseTo != 9 || len(m.Docs) != 1 {
		t.Fatalf("OP_REPLY parsed wrong: %+v docs=%d", m.Header, len(m.Docs))
	}
	if v, err := m.Docs[0].LookupErr("code"); err != nil || v.Int32() != 352 {
		t.Fatal("reply document not parsed")
	}
	if _, err := c.ReadMessage(2 * time.Second); !errors.Is(err, rawwire.ErrClosed) {
		t.Fatalf("after the server closed: %v, want ErrClosed", err)
	}
}

// A truncated message followed by EOF is ErrClosed, not a parse panic.
func TestReadMessageTruncated(t *testing.T) {
	addr := server(t, func(c net.Conn) { c.Write([]byte{100, 0, 0, 0, 1, 2}) })
	c := rawwire.Dial(t, addr)
	if _, err := c.ReadMessage(2 * time.Second); !errors.Is(err, rawwire.ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

// ExpectClosedNoBytes: nil when the peer closes without sending anything; an
// error when bytes arrive, or when the connection stays open.
func TestExpectClosedNoBytes(t *testing.T) {
	addr := server(t, func(c net.Conn) {})
	if err := rawwire.Dial(t, addr).ExpectClosedNoBytes(2 * time.Second); err != nil {
		t.Fatalf("clean close: %v", err)
	}
	addr = server(t, func(c net.Conn) { c.Write([]byte("x")) })
	if err := rawwire.Dial(t, addr).ExpectClosedNoBytes(2 * time.Second); err == nil {
		t.Fatal("bytes before the close were not reported")
	}
	addr = server(t, func(c net.Conn) { time.Sleep(500 * time.Millisecond) })
	if err := rawwire.Dial(t, addr).ExpectClosedNoBytes(100 * time.Millisecond); err == nil {
		t.Fatal("an open connection was reported as closed")
	}
}

// PartialClientHello: a stalled TLS handshake. The helper sends only the first
// bytes of a ClientHello and then keeps the socket open without more bytes.
func TestStalledClientHello(t *testing.T) {
	got := make(chan []byte, 1)
	addr := server(t, func(c net.Conn) {
		c.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
		buf := make([]byte, 4096)
		var all []byte
		for {
			n, err := c.Read(buf)
			all = append(all, buf[:n]...)
			if err != nil {
				break
			}
		}
		got <- all
	})
	conn := rawwire.DialStalledClientHello(t, addr)
	defer conn.Close()
	all := <-got
	if len(all) < 6 || all[0] != 0x16 || all[1] != 0x03 {
		t.Fatalf("not the start of a TLS handshake record: %v", all[:min(len(all), 6)])
	}
	recLen := int(all[3])<<8 | int(all[4])
	if len(all)-5 >= recLen {
		t.Fatalf("ClientHello is complete (%d of %d bytes); it must stall mid-record", len(all)-5, recLen)
	}
	if all[5] != 0x01 {
		t.Fatalf("handshake type %#x, want ClientHello (1)", all[5])
	}
}

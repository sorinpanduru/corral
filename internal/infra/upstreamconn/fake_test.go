package upstreamconn_test

// A tiny fake upstream speaking just enough OP_MSG for the connector: it
// records what it sees (including whether a hello carries client metadata),
// enforces code 186 for client metadata on an already-greeted connection (as a
// real server does), and can misbehave in scripted ways.

import (
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	xscram "github.com/xdg-go/scram"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type seenMsg struct {
	Conn      int
	Command   string
	HasClient bool
	Body      bson.Raw
}

type fakeUpstream struct {
	t    *testing.T
	ln   net.Listener
	Addr string

	mu        sync.Mutex
	seen      []seenMsg
	conns     int
	continues int

	pingTail    []byte // appended after the ping reply's body (a kind-1 section)
	endlessSASL bool
	scramUser   string
	scramPass   string
}

func newFake(t *testing.T) *fakeUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeUpstream{t: t, ln: ln, Addr: ln.Addr().String()}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			idx := f.conns
			f.conns++
			f.mu.Unlock()
			go f.serve(c, idx)
		}
	}()
	return f
}

func (f *fakeUpstream) messages() []seenMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenMsg(nil), f.seen...)
}

func (f *fakeUpstream) count(cmd string) int {
	n := 0
	for _, m := range f.messages() {
		if m.Command == cmd {
			n++
		}
	}
	return n
}

func (f *fakeUpstream) connections() int { f.mu.Lock(); defer f.mu.Unlock(); return f.conns }
func (f *fakeUpstream) saslContinues() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.continues
}

func writeReply(c net.Conn, respTo int32, doc []byte, tail []byte) {
	body := append([]byte{0, 0, 0, 0, 0}, doc...)
	body = append(body, tail...)
	h := make([]byte, 16)
	binary.LittleEndian.PutUint32(h[0:], uint32(16+len(body)))
	binary.LittleEndian.PutUint32(h[4:], 9000)
	binary.LittleEndian.PutUint32(h[8:], uint32(respTo))
	binary.LittleEndian.PutUint32(h[12:], 2013)
	c.Write(append(h, body...))
}

func marshal(d bson.D) []byte {
	b, err := bson.Marshal(d)
	if err != nil {
		panic(err)
	}
	return b
}

func helloDoc() []byte {
	return marshal(bson.D{{Key: "isWritablePrimary", Value: true}, {Key: "msg", Value: "isdbgrid"}, {Key: "maxBsonObjectSize", Value: int32(16777216)},
		{Key: "maxMessageSizeBytes", Value: int32(48000000)}, {Key: "maxWriteBatchSize", Value: int32(100000)},
		{Key: "logicalSessionTimeoutMinutes", Value: int32(30)}, {Key: "minWireVersion", Value: int32(0)}, {Key: "maxWireVersion", Value: int32(21)}, {Key: "ok", Value: 1.0}})
}

func (f *fakeUpstream) serve(c net.Conn, idx int) {
	defer c.Close()
	greeted := false
	var sc *xscram.ServerConversation
	for {
		var h [16]byte
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return
		}
		n := int(binary.LittleEndian.Uint32(h[:]))
		reqID := int32(binary.LittleEndian.Uint32(h[4:]))
		buf := make([]byte, n-16)
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		doc := bson.Raw(buf[5:])
		els, err := doc.Elements()
		if err != nil || len(els) == 0 {
			return
		}
		cmd := els[0].Key()
		_, cerr := doc.LookupErr("client")
		f.mu.Lock()
		f.seen = append(f.seen, seenMsg{Conn: idx, Command: cmd, HasClient: cerr == nil, Body: append(bson.Raw(nil), doc...)})
		f.mu.Unlock()
		switch cmd {
		case "hello", "isMaster", "ismaster":
			if cerr == nil && greeted {
				writeReply(c, reqID, marshal(bson.D{{Key: "ok", Value: 0.0}, {Key: "errmsg", Value: "client metadata can only be sent once"}, {Key: "code", Value: int32(186)}, {Key: "codeName", Value: "ClientMetadataCannotBeMutated"}}), nil)
				continue
			}
			greeted = true
			writeReply(c, reqID, helloDoc(), nil)
		case "ping":
			writeReply(c, reqID, marshal(bson.D{{Key: "ok", Value: 1.0}}), f.pingTail)
		case "saslStart", "saslContinue":
			if !f.endlessSASL {
				writeReply(c, reqID, marshal(bson.D{{Key: "ok", Value: 0.0}, {Key: "code", Value: int32(18)}, {Key: "codeName", Value: "AuthenticationFailed"}}), nil)
				continue
			}
			if cmd == "saslStart" {
				srv, err := xscram.SHA256.NewServer(func(user string) (xscram.StoredCredentials, error) {
					cl, _ := xscram.SHA256.NewClientUnprepped(f.scramUser, f.scramPass, "")
					return cl.GetStoredCredentials(xscram.KeyFactors{Salt: "synthetic-salt-16b", Iters: 4096}), nil
				})
				if err != nil {
					return
				}
				sc = srv.NewConversation()
				resp, err := sc.Step(string(binaryPayload(doc)))
				if err != nil {
					return
				}
				writeReply(c, reqID, marshal(bson.D{{Key: "conversationId", Value: int32(1)}, {Key: "done", Value: false}, {Key: "payload", Value: bson.Binary{Data: []byte(resp)}}, {Key: "ok", Value: 1.0}}), nil)
				continue
			}
			f.mu.Lock()
			f.continues++
			f.mu.Unlock()
			var out []byte
			if sc != nil && !sc.Done() {
				resp, err := sc.Step(string(binaryPayload(doc)))
				if err != nil {
					return
				}
				out = []byte(resp)
			}
			// Never finish: done stays false forever.
			writeReply(c, reqID, marshal(bson.D{{Key: "conversationId", Value: int32(1)}, {Key: "done", Value: false}, {Key: "payload", Value: bson.Binary{Data: out}}, {Key: "ok", Value: 1.0}}), nil)
		default:
			writeReply(c, reqID, marshal(bson.D{{Key: "ok", Value: 1.0}}), nil)
		}
	}
}

func binaryPayload(doc bson.Raw) []byte {
	_, b := doc.Lookup("payload").Binary()
	return b
}

var _ = base64.StdEncoding
var _ = time.Second

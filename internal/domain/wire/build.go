package wire

import (
	"encoding/binary"

	"github.com/sorinpanduru/corral/internal/domain/bson"
)

// BuildMsg frames body (one BSON document) as an OP_MSG with a single kind-0
// section and no checksum.
func BuildMsg(requestID, responseTo int32, flags uint32, body []byte) []byte {
	h := Header{Length: int32(headerLen + 4 + 1 + len(body)), RequestID: requestID, ResponseTo: responseTo, OpCode: OpMsg}
	out := h.AppendTo(make([]byte, 0, h.Length))
	out = binary.LittleEndian.AppendUint32(out, flags)
	out = append(out, 0)
	return append(out, body...)
}

// BuildReply frames doc as an OP_REPLY with cursorID 0, startingFrom 0 and
// numberReturned 1 (FR-8).
func BuildReply(requestID, responseTo, responseFlags int32, doc []byte) []byte {
	h := Header{Length: int32(headerLen + 20 + len(doc)), RequestID: requestID, ResponseTo: responseTo, OpCode: OpReply}
	out := h.AppendTo(make([]byte, 0, h.Length))
	out = binary.LittleEndian.AppendUint32(out, uint32(responseFlags))
	out = binary.LittleEndian.AppendUint64(out, 0)
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = binary.LittleEndian.AppendUint32(out, 1)
	return append(out, doc...)
}

// PingBody is corral's own barrier ping (no session, FR-29).
func PingBody() []byte {
	b := bson.NewBuilder()
	b.AppendInt32("ping", 1)
	b.AppendString("$db", "admin")
	return b.Bytes()
}

// HelloBody is corral's own monitoring and connection handshake.
func HelloBody(app, driverVersion, osType string) []byte {
	b := bson.NewBuilder()
	b.AppendInt32("hello", 1)
	b.AppendBool("helloOk", true)
	b.StartDocument("client")
	b.StartDocument("application")
	b.AppendString("name", app)
	b.EndDocument()
	b.StartDocument("driver")
	b.AppendString("name", "corral")
	b.AppendString("version", driverVersion)
	b.EndDocument()
	b.StartDocument("os")
	b.AppendString("type", osType)
	b.EndDocument()
	b.EndDocument()
	b.AppendString("$db", "admin")
	return b.Bytes()
}

// HelloPollBody is corral's repeated monitoring hello on an established
// connection. The client metadata document is sent only once per connection
// (a second one is refused with code 186), so later checks omit it.
func HelloPollBody() []byte {
	b := bson.NewBuilder()
	b.AppendInt32("hello", 1)
	b.AppendBool("helloOk", true)
	b.AppendString("$db", "admin")
	return b.Bytes()
}

// SaslStartBody starts a SCRAM-SHA-256 conversation against source.
func SaslStartBody(source string, payload []byte) []byte {
	b := bson.NewBuilder()
	b.AppendInt32("saslStart", 1)
	b.AppendString("mechanism", "SCRAM-SHA-256")
	b.AppendBinary("payload", 0, payload)
	b.StartDocument("options")
	b.AppendBool("skipEmptyExchange", true)
	b.EndDocument()
	b.AppendString("$db", source)
	return b.Bytes()
}

// SaslContinueBody continues conversation convID.
func SaslContinueBody(source string, convID int32, payload []byte) []byte {
	b := bson.NewBuilder()
	b.AppendInt32("saslContinue", 1)
	b.AppendInt32("conversationId", convID)
	b.AppendBinary("payload", 0, payload)
	b.AppendString("$db", source)
	return b.Bytes()
}

// HelloReply holds the fields corral reads from an upstream hello reply
// (FR-9 own-exchange fields plus the upstream parameters). Nothing else is
// kept (not `me`, tags, or compression).
type HelloReply struct {
	IsWritablePrimary bool
	Secondary         bool
	ArbiterOnly       bool
	IsMongos          bool
	SetName           string
	Hosts             []string
	Passives          []string
	Primary           string
	HasElectionID     bool
	ElectionID        [12]byte
	HasSetVersion     bool
	SetVersion        int32

	MaxBsonObjectSize            int32
	MaxMessageSizeBytes          int32
	MaxWriteBatchSize            int32
	HasLSTM                      bool
	LogicalSessionTimeoutMinutes int32
	MinWire                      int32
	MaxWire                      int32
}

func stringArray(e bson.Element) []string {
	arr, ok := e.Array()
	if !ok {
		return nil
	}
	var out []string
	_ = bson.ForEach(arr, func(it bson.Element) bool {
		if s, ok := it.StringValue(); ok {
			out = append(out, s)
		}
		return true
	})
	return out
}

func okValue(doc []byte) (ok bool, found bool, err error) {
	err = bson.ForEach(doc, func(e bson.Element) bool {
		if e.Key() == "ok" {
			if n, isNum := e.Number(); isNum && !found {
				found, ok = true, n == 1
			}
		}
		return true
	})
	return
}

// ParseHelloReply reads a hello reply document. An ok:0 reply, or one with no
// ok, is an error.
func ParseHelloReply(doc []byte) (HelloReply, error) {
	var r HelloReply
	okv, found, err := okValue(doc)
	if err != nil {
		return r, ErrMalformed()
	}
	if !found || !okv {
		return r, ErrUnexpectedResponse()
	}
	seen := map[string]bool{}
	err = bson.ForEach(doc, func(e bson.Element) bool {
		k := e.Key()
		if seen[k] {
			return true // first occurrence wins
		}
		seen[k] = true
		i32 := func() int32 { n, _ := e.Number(); return int32(n) }
		switch k {
		case "isWritablePrimary", "ismaster":
			if b, ok := e.Bool(); ok && (k == "isWritablePrimary" || !seen["isWritablePrimary"]) {
				r.IsWritablePrimary = r.IsWritablePrimary || b
			}
		case "secondary":
			r.Secondary, _ = e.Bool()
		case "arbiterOnly":
			r.ArbiterOnly, _ = e.Bool()
		case "msg":
			if s, ok := e.StringValue(); ok && s == "isdbgrid" {
				r.IsMongos = true
			}
		case "setName":
			r.SetName, _ = e.StringValue()
		case "hosts":
			r.Hosts = stringArray(e)
		case "passives":
			r.Passives = stringArray(e)
		case "primary":
			r.Primary, _ = e.StringValue()
		case "electionId":
			r.ElectionID, r.HasElectionID = e.ObjectID()
		case "setVersion":
			if _, ok := e.Number(); ok {
				r.SetVersion, r.HasSetVersion = i32(), true
			}
		case "maxBsonObjectSize":
			r.MaxBsonObjectSize = i32()
		case "maxMessageSizeBytes":
			r.MaxMessageSizeBytes = i32()
		case "maxWriteBatchSize":
			r.MaxWriteBatchSize = i32()
		case "logicalSessionTimeoutMinutes":
			if _, ok := e.Number(); ok {
				r.LogicalSessionTimeoutMinutes, r.HasLSTM = i32(), true
			}
		case "minWireVersion":
			r.MinWire = i32()
		case "maxWireVersion":
			r.MaxWire = i32()
		}
		return true
	})
	if err != nil {
		return HelloReply{}, ErrMalformed()
	}
	return r, nil
}

// SaslReply holds the fields of a saslStart or saslContinue reply that corral
// reads (FR-22). The errmsg is never kept.
type SaslReply struct {
	OK             bool
	Code           int32
	CodeName       string
	ConversationID int32
	Done           bool
	Payload        []byte
}

// ParseSaslReply reads a SASL reply. An error reply parses, so the caller can
// read its code.
func ParseSaslReply(doc []byte) (SaslReply, error) {
	var r SaslReply
	seen := map[string]bool{}
	err := bson.ForEach(doc, func(e bson.Element) bool {
		k := e.Key()
		if seen[k] {
			return true
		}
		seen[k] = true
		switch k {
		case "ok":
			if n, ok := e.Number(); ok {
				r.OK = n == 1
			}
		case "code":
			if n, ok := e.Number(); ok {
				r.Code = int32(n)
			}
		case "codeName":
			r.CodeName, _ = e.StringValue()
		case "conversationId":
			if n, ok := e.Number(); ok {
				r.ConversationID = int32(n)
			}
		case "done":
			r.Done, _ = e.Bool()
		case "payload":
			if _, data, ok := e.Binary(); ok {
				r.Payload = append([]byte(nil), data...)
			}
		}
		return true
	})
	if err != nil {
		return SaslReply{}, ErrMalformed()
	}
	return r, nil
}

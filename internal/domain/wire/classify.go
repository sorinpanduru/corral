package wire

import (
	"encoding/binary"
	"slices"
	"strings"

	"github.com/sorinpanduru/corral/internal/domain/bson"
)

// Kind says how a request arrived and what it is.
type Kind int

const (
	// KindOpMsg is an OP_MSG request.
	KindOpMsg Kind = iota
	// KindOpQueryHandshake is an OP_QUERY hello, isMaster or ismaster.
	KindOpQueryHandshake
	// KindOpQueryOther is any other valid OP_QUERY (answered with 352).
	KindOpQueryOther
)

// Handshake holds the FR-9 handshake fields. Nothing else of the message is
// kept: not saslSupportedMechs values, client.os, or compression names.
type Handshake struct {
	Name                  string // hello, isMaster or ismaster
	HelloOK               bool
	LoadBalanced          bool
	HasTopologyVersion    bool
	HasMaxAwaitTimeMS     bool
	HasClient             bool
	HasCompression        bool
	HasSpeculativeAuth    bool
	HasSaslSupportedMechs bool
	AppName               string // raw, at most 256 bytes ("" when longer)
	DriverName            string
	DriverVersion         string
	ViaOpQuery            bool
}

// Request is the FR-9 view of a classified request. It holds only these
// fields (a reflection test pins the list).
type Request struct {
	Kind      Kind
	RequestID int32
	Flags     uint32
	Command   string

	DB               string
	HasLSID          bool
	LSID             [16]byte
	HasTxnNumber     bool
	AutocommitFalse  bool
	StartTransaction bool

	Collection    string
	HasCollection bool
	CursorID      int64
	HasCursorID   bool
	FirstStage    string

	Handshake  *Handshake
	FitsPrefix bool
}

var collectionCommands = []string{"find", "insert", "update", "delete", "aggregate", "count", "distinct",
	"findAndModify", "create", "drop", "createIndexes", "dropIndexes", "listIndexes", "collMod", "killCursors"}

// CollectionCommands returns the closed FR-40 list of commands whose first
// string value is a collection name. SPEC-001 FR-40 owns the contents.
func CollectionCommands() []string { return slices.Clone(collectionCommands) }

var localCommands = []string{"hello", "isMaster", "ismaster", "saslStart", "saslContinue", "authenticate",
	"getnonce", "logout", "currentOp", "killOp", "killAllSessions", "killAllSessionsByPattern"}

func isHandshakeName(s string) bool { return s == "hello" || s == "isMaster" || s == "ismaster" }

// capPlan builds a capture list and remembers each capture's index by name.
type capPlan struct {
	caps   []bson.Capture
	prefix []string
	idx    map[string]int
}

func (p *capPlan) add(name string, mode bson.CaptureMode, path ...string) {
	full := append(append([]string(nil), p.prefix...), path...)
	if p.idx == nil {
		p.idx = map[string]int{}
	}
	p.idx[name] = len(p.caps)
	p.caps = append(p.caps, bson.Capture{Path: full, Mode: mode})
}

func (p *capPlan) get(w *bson.Walker, name string) (bson.Value, bool) {
	i, ok := p.idx[name]
	if !ok {
		return bson.Value{}, false
	}
	return w.Captured(i)
}

func (p *capPlan) str(w *bson.Walker, name string) string {
	if v, ok := p.get(w, name); ok {
		if s, isStr := v.StringValue(); isStr {
			return s
		}
	}
	return ""
}

func (p *capPlan) flag(w *bson.Walker, name string) bool {
	v, ok := p.get(w, name)
	if !ok {
		return false
	}
	b, isBool := v.Bool()
	return isBool && b
}

func (p *capPlan) present(w *bson.Walker, name string) bool {
	_, ok := p.get(w, name)
	return ok
}

func (p *capPlan) addHandshake() {
	p.add("helloOk", bson.CaptureValue, "helloOk")
	p.add("loadBalanced", bson.CaptureValue, "loadBalanced")
	p.add("topologyVersion", bson.CapturePresence, "topologyVersion")
	p.add("maxAwaitTimeMS", bson.CapturePresence, "maxAwaitTimeMS")
	p.add("client", bson.CapturePresence, "client")
	p.add("appName", bson.CaptureValue, "client", "application", "name")
	p.add("driverName", bson.CaptureValue, "client", "driver", "name")
	p.add("driverVersion", bson.CaptureValue, "client", "driver", "version")
	p.add("compression", bson.CapturePresence, "compression")
	p.add("speculative", bson.CapturePresence, "speculativeAuthenticate")
	p.add("saslMechs", bson.CapturePresence, "saslSupportedMechs")
}

func (p *capPlan) handshake(w *bson.Walker, name string, viaQuery bool) *Handshake {
	return &Handshake{
		Name:                  name,
		HelloOK:               p.flag(w, "helloOk"),
		LoadBalanced:          p.flag(w, "loadBalanced"),
		HasTopologyVersion:    p.present(w, "topologyVersion"),
		HasMaxAwaitTimeMS:     p.present(w, "maxAwaitTimeMS"),
		HasClient:             p.present(w, "client"),
		HasCompression:        p.present(w, "compression"),
		HasSpeculativeAuth:    p.present(w, "speculative"),
		HasSaslSupportedMechs: p.present(w, "saslMechs"),
		AppName:               p.str(w, "appName"),
		DriverName:            p.str(w, "driverName"),
		DriverVersion:         p.str(w, "driverVersion"),
		ViaOpQuery:            viaQuery,
	}
}

// ClassifyPrefix makes the FR-38 decision from the buffered prefix alone.
// prefix starts with the 16-byte header; total is the message's declared
// length. It reads only the fields FR-9 lists. A deciding field (the command
// name, or the first pipeline stage of aggregate) outside the prefix is
// malformed_message; a message answered locally that does not fit the prefix
// entirely is message_too_large.
func ClassifyPrefix(prefix []byte, total int32) (Request, error) {
	if len(prefix) < headerLen {
		return Request{}, ErrMalformed()
	}
	h := ParseHeader(prefix)
	fits := int64(total) <= int64(len(prefix))
	end := len(prefix)
	if int64(total) < int64(end) {
		end = int(total)
	}
	if end < headerLen {
		return Request{}, ErrMalformed()
	}
	prefix = prefix[:end]
	switch h.OpCode {
	case OpMsg:
		return classifyMsg(h, prefix, fits)
	case OpQuery:
		return classifyQuery(h, prefix, fits)
	}
	return Request{}, ErrUnsupportedOpcode()
}

func classifyMsg(h Header, prefix []byte, fits bool) (Request, error) {
	if len(prefix) < 21 {
		return Request{}, ErrMalformed()
	}
	r := Request{Kind: KindOpMsg, RequestID: h.RequestID, Flags: binary.LittleEndian.Uint32(prefix[16:]), FitsPrefix: fits}
	pos := 20
	for {
		if pos >= len(prefix) {
			return Request{}, ErrMalformed()
		}
		kind := prefix[pos]
		if kind == 0 {
			pos++
			break
		}
		if kind != 1 || pos+5 > len(prefix) {
			return Request{}, ErrMalformed()
		}
		size := int(int32(binary.LittleEndian.Uint32(prefix[pos+1:])))
		if size < 5 || pos+1+size > len(prefix) {
			return Request{}, ErrMalformed()
		}
		pos += 1 + size
	}
	doc := prefix[pos:]
	first, st, err := bson.FirstElementPartial(doc)
	if err != nil || st != bson.PartialOK {
		return Request{}, ErrMalformed()
	}
	r.Command = first.Key()
	cmd := r.Command

	var p capPlan
	p.add("db", bson.CaptureValue, "$db")
	p.add("lsid", bson.CaptureValue, "lsid", "id")
	p.add("txn", bson.CapturePresence, "txnNumber")
	p.add("autocommit", bson.CaptureValue, "autocommit")
	p.add("startTxn", bson.CaptureValue, "startTransaction")
	switch {
	case cmd == "getMore":
		p.add("collection", bson.CaptureValue, "collection")
	case cmd == "aggregate":
		p.add("stage", bson.CaptureFirstKey, "pipeline", "0")
	case isHandshakeName(cmd):
		p.addHandshake()
	}
	w := bson.NewWalker(p.caps)
	if err := w.Write(doc); err != nil {
		return Request{}, ErrMalformed()
	}

	r.DB = p.str(w, "db")
	if v, ok := p.get(w, "lsid"); ok {
		if sub, data, isBin := v.Binary(); isBin && sub == 4 && len(data) == 16 {
			r.HasLSID = true
			copy(r.LSID[:], data)
		}
	}
	r.HasTxnNumber = p.present(w, "txn")
	if v, ok := p.get(w, "autocommit"); ok {
		if b, isBool := v.Bool(); isBool && !b {
			r.AutocommitFalse = true
		}
	}
	r.StartTransaction = p.flag(w, "startTxn")

	if slices.Contains(collectionCommands, cmd) {
		if s, ok := first.StringValue(); ok {
			r.Collection, r.HasCollection = s, true
		}
	}
	switch {
	case cmd == "getMore":
		if n, ok := first.Int64(); ok {
			r.CursorID, r.HasCursorID = n, true
		}
		if s := p.str(w, "collection"); s != "" {
			r.Collection, r.HasCollection = s, true
		}
	case cmd == "aggregate":
		if v, ok := p.get(w, "stage"); ok {
			r.FirstStage, _ = v.StringValue()
		} else if !w.Complete() {
			return Request{}, ErrMalformed() // first stage outside the prefix
		}
	case isHandshakeName(cmd):
		r.Handshake = p.handshake(w, cmd, false)
	}

	local := r.Handshake != nil || slices.Contains(localCommands, cmd) ||
		(cmd == "aggregate" && r.FirstStage == "$currentOp")
	if local && !fits {
		return Request{}, ErrMessageTooLarge()
	}
	return r, nil
}

func classifyQuery(h Header, prefix []byte, fits bool) (Request, error) {
	r := Request{Kind: KindOpQueryOther, RequestID: h.RequestID, Flags: 0, FitsPrefix: fits}
	pos := 20 // after flags
	if len(prefix) < pos {
		return Request{}, ErrMalformed()
	}
	nul := slices.Index(prefix[pos:], 0)
	if nul < 0 {
		return Request{}, ErrMalformed()
	}
	ns := string(prefix[pos : pos+nul])
	pos += nul + 1
	if pos+8 > len(prefix) {
		return Request{}, ErrMalformed()
	}
	pos += 8 // numberToSkip, numberToReturn

	qdoc, rest, err := splitDoc(prefix[pos:], fits)
	if err != nil {
		return Request{}, err
	}
	first, st, ferr := bson.FirstElementPartial(qdoc)
	if ferr != nil {
		return Request{}, ErrMalformed()
	}
	var p capPlan
	wrapped := false
	cmd := ""
	switch st {
	case bson.PartialOK:
		cmd = first.Key()
		if cmd == "$query" {
			if inner, ok := first.Document(); ok {
				if ie, ist, ierr := bson.FirstElementPartial(inner); ierr == nil && ist == bson.PartialOK {
					wrapped, cmd = true, ie.Key()
				} else if ierr != nil {
					return Request{}, ErrMalformed()
				}
			}
		}
	case bson.PartialNeedMore:
		if fits {
			return Request{}, ErrMalformed()
		}
	}
	if wrapped {
		p.prefix = []string{"$query"}
	}
	handshake := strings.HasSuffix(ns, ".$cmd") && len(ns) > len(".$cmd") && isHandshakeName(cmd)
	if handshake {
		p.addHandshake()
	}
	w := bson.NewWalker(p.caps)
	if err := w.Write(qdoc); err != nil {
		return Request{}, ErrMalformed()
	}
	if fits {
		if err := w.Close(); err != nil {
			return Request{}, ErrMalformed()
		}
		// Optional single returnFieldsSelector, then nothing.
		if len(rest) > 0 {
			sel, after, serr := splitDoc(rest, true)
			if serr != nil || len(after) != 0 {
				return Request{}, ErrMalformed()
			}
			sw := bson.NewWalker(nil)
			if err := sw.Write(sel); err != nil || sw.Close() != nil {
				return Request{}, ErrMalformed()
			}
		}
	}
	r.Command = cmd
	if handshake {
		r.Kind = KindOpQueryHandshake
		r.Handshake = p.handshake(w, cmd, true)
	}
	if !fits {
		return Request{}, ErrMessageTooLarge() // OP_QUERY is always answered locally
	}
	return r, nil
}

// splitDoc cuts one document off the front of b by its declared length. When
// the whole message is available (fits) a length that overruns b is
// malformed; otherwise the visible part is returned and rest is empty.
func splitDoc(b []byte, fits bool) (doc, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, ErrMalformed()
	}
	l := int(int32(binary.LittleEndian.Uint32(b)))
	if l < 5 {
		return nil, nil, ErrMalformed()
	}
	if l > len(b) {
		if fits {
			return nil, nil, ErrMalformed()
		}
		return b, nil, nil
	}
	return b[:l], b[l:], nil
}

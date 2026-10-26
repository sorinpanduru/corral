package mongosim

import (
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type conn struct {
	s    *Server
	idx  int
	nc   net.Conn
	in   chan []byte
	done chan struct{}
	once sync.Once

	// guarded by s.mu
	appName       string
	driverName    string
	driverVersion string
	helloSeen     bool
	authenticated bool
	authMech      string
	authUser      string
	open          bool
	blackholed    bool
	opened        time.Time
	closed        time.Time
	nmsgs         int
	inflight      bool
	partial       atomic.Int64 // bytes of the incomplete frame being read
	recvTotal     atomic.Int64 // all bytes read
	sigForged     bool
	wmu           sync.Mutex // serialises writes (serve goroutine and SendRaw)

	tlsMode   TLSMode       // mode in force when accepted
	stallNext time.Duration // guarded by s.mu: delay for the next hello reply

	conv *scramConv // owned by the serve goroutine
}

func (c *conn) infoLocked() ConnInfo {
	return ConnInfo{
		Index: c.idx, Remote: c.nc.RemoteAddr().String(), AppName: c.appName,
		DriverName: c.driverName, DriverVersion: c.driverVersion,
		Authenticated: c.authenticated, AuthMechanism: c.authMech, AuthUser: c.authUser,
		Open: c.open, InFlight: c.inflight || (c.open && (c.partial.Load() > 0 || len(c.in) > 0)), SignatureForged: c.sigForged,
		PartialBytes: int(c.partial.Load()), BytesReceived: c.recvTotal.Load(), Blackholed: c.blackholed, Opened: c.opened, Closed: c.closed, Messages: c.nmsgs,
	}
}

// close closes the connection; reset sends a TCP RST instead of a FIN.
func (c *conn) close(reset bool) {
	c.once.Do(func() {
		var base net.Conn = c.nc
		if tl, ok := base.(*tls.Conn); ok {
			base = tl.NetConn()
		}
		if tc, ok := base.(*net.TCPConn); ok && reset {
			_ = tc.SetLinger(0)
		}
		_ = c.nc.Close()
		c.s.mu.Lock()
		c.open = false
		c.closed = time.Now()
		c.s.mu.Unlock()
		close(c.done)
	})
}

// progressReader counts bytes as they arrive so tests can observe partial
// messages (frame bytes read before the frame completes).
type progressReader struct {
	r interface{ Read([]byte) (int, error) }
	c *conn
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.c.partial.Add(int64(n))
	p.c.recvTotal.Add(int64(n))
	return n, err
}

func (c *conn) readLoop() {
	defer close(c.in)
	if tl, ok := c.nc.(*tls.Conn); ok {
		rec := TLSHandshake{Start: time.Now(), Remote: c.nc.RemoteAddr().String(), ConnIndex: c.idx, Mode: c.tlsMode}
		_ = tl.SetDeadline(time.Now().Add(5 * time.Second))
		err := tl.Handshake()
		_ = tl.SetDeadline(time.Time{})
		rec.End = time.Now()
		rec.OK = err == nil
		if err != nil {
			rec.Err = err.Error()
		} else {
			rec.Version = tl.ConnectionState().Version
		}
		c.s.mu.Lock()
		c.s.hsLog = append(c.s.hsLog, rec)
		if err != nil {
			c.s.tlsFailures++
		}
		c.s.mu.Unlock()
		if err != nil {
			c.close(false)
			return
		}
	}
	for {
		raw, err := readFrame(&progressReader{r: c.nc, c: c})
		if err != nil {
			// Keep PartialBytes: tests assert on bytes received before a
			// close in the middle of a frame. No message is recorded.
			c.close(false)
			return
		}
		select {
		case c.in <- raw:
			c.partial.Store(0)
		case <-c.done:
			return
		}
	}
}

func (c *conn) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-c.done:
		return false
	}
}

func (c *conn) send(b []byte) bool {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.nc.Write(b); err != nil {
		c.close(false)
		return false
	}
	return true
}

func (c *conn) setInflight(b bool) {
	c.s.mu.Lock()
	c.inflight = b
	c.s.mu.Unlock()
}

func (c *conn) serve() {
	for {
		select {
		case raw, ok := <-c.in:
			if !ok {
				return
			}
			c.setInflight(true)
			cont := c.handle(raw)
			c.setInflight(false)
			if !cont {
				return
			}
		case <-c.done:
			return
		}
	}
}

func metaString(body bson.Raw, path ...string) string {
	v, err := body.LookupErr(path...)
	if err != nil {
		return ""
	}
	s, _ := v.StringValueOK()
	return s
}

// handle processes one request; false ends the connection loop.
func (c *conn) handle(raw []byte) bool {
	s := c.s
	p, err := parseRequest(raw)
	hdr := parseHeader(raw)
	cmd := ""
	if err == nil {
		cmd = commandName(p.body)
	}
	lower := strings.ToLower(cmd)
	// Handshake metadata and attempt accounting.
	s.mu.Lock()
	if err == nil && (lower == "hello" || lower == "ismaster") {
		if !c.helloSeen {
			c.helloSeen = true
			c.appName = metaString(p.body, "client", "application", "name")
			c.driverName = metaString(p.body, "client", "driver", "name")
			c.driverVersion = metaString(p.body, "client", "driver", "version")
			s.attemptsBy[c.appName]++
		}
	}
	msg := Message{
		Seq: len(s.msgs), ConnIndex: c.idx, AppName: c.appName, Authenticated: c.authenticated,
		OpCode: hdr.opcode, RequestID: hdr.requestID, ResponseTo: hdr.responseTo,
		Raw: raw, Time: time.Now(),
	}
	if err == nil {
		msg.Flags, msg.Command, msg.DB, msg.Body, msg.Sequences = p.flags, cmd, p.db, p.body, p.seqs
		_, e1 := p.body.LookupErr("lsid")
		_, e2 := p.body.LookupErr("txnNumber")
		msg.HasLsid, msg.HasTxnNumber = e1 == nil, e2 == nil
		msg.Marker, _ = p.body.Lookup("corraltestMarker").StringValueOK()
	}
	s.msgs = append(s.msgs, msg)
	c.nmsgs++
	appName := c.appName
	s.mu.Unlock()
	if err != nil {
		// Unsupported opcode (for example OP_COMPRESSED) or malformed request.
		c.close(true)
		return false
	}

	if (lower == "hello" || lower == "ismaster") && s.refusesApp(appName) {
		c.close(true)
		return false
	}

	// Monitoring-check failures.
	if lower == "hello" || lower == "ismaster" {
		s.mu.Lock()
		stall := c.stallNext
		c.stallNext = 0
		s.mu.Unlock()
		if stall > 0 && !c.sleep(stall) {
			return false
		}
		if mode, gate, ok := s.takeCheck(appName); ok {
			switch mode.kind {
			case 1:
				c.close(true)
				return false
			case 2:
				select {
				case <-gate:
				case <-c.done:
					return false
				}
			case 3:
				if !c.sleep(mode.d) {
					return false
				}
			case 4:
				return c.replyMalformed(p)
			case 5:
				return c.reply(p, errorDoc(mode.code, mode.name, ""), crcNone, hdr.requestID)
			}
		}
	}

	// Rules.
	if r := s.matchRule(cmd, msg.Marker, appName, p.db); r != nil {
		a := r.a
		switch a.kind {
		case actHold:
			r.mu.Lock()
			rel := r.relCh
			if !r.released {
				r.held++
			}
			isHeld := !r.released
			r.mu.Unlock()
			if isHeld {
				select {
				case <-rel:
				case <-c.done:
				}
				r.mu.Lock()
				r.held--
				r.mu.Unlock()
				select {
				case <-c.done:
					return false
				default:
				}
			}
		case actDelay:
			if !c.sleep(a.d) {
				return false
			}
		case actResetAfterReceive:
			c.close(true)
			return false
		case actNoReply:
			return true
		case actError:
			return c.reply(p, errorDoc(a.code, a.name, ""), crcNone, hdr.requestID)
		case actErrorMessage:
			return c.reply(p, mustMarshal(bson.D{{Key: "ok", Value: float64(0)}, {Key: "errmsg", Value: a.errmsg}}), crcNone, hdr.requestID)
		case actWriteConcernError:
			return c.reply(p, mustMarshal(bson.D{
				{Key: "ok", Value: float64(1)}, {Key: "n", Value: int32(1)},
				{Key: "writeConcernError", Value: bson.D{
					{Key: "code", Value: a.code}, {Key: "codeName", Value: codeNameFor(a.code)},
					{Key: "errmsg", Value: "simulated write concern error"},
				}},
			}), crcNone, hdr.requestID)
		case actWriteErrors:
			return c.reply(p, mustMarshal(bson.D{
				{Key: "ok", Value: float64(1)}, {Key: "n", Value: int32(0)},
				{Key: "writeErrors", Value: bson.A{bson.D{
					{Key: "index", Value: int32(0)}, {Key: "code", Value: a.code},
					{Key: "errmsg", Value: "simulated write error"},
				}}},
			}), crcNone, hdr.requestID)
		case actUnknownResponseTo:
			return c.reply(p, c.defaultReply(p, cmd, lower), crcNone, hdr.requestID+1000)
		case actMoreToCome:
			return c.replyFlags(p, c.defaultReply(p, cmd, lower), FlagMoreToCome, crcNone, hdr.requestID)
		case actOversize:
			return c.replyOversize(p, c.defaultReply(p, cmd, lower))
		case actChecksum:
			crc := crcInvalid
			if a.valid {
				crc = crcValid
			}
			return c.reply(p, c.defaultReply(p, cmd, lower), crc, hdr.requestID)
		case actRaw:
			if hdr.opcode != OpMsg {
				return c.reply(p, c.defaultReply(p, cmd, lower), crcNone, hdr.requestID)
			}
			if p.flags&FlagMoreToCome != 0 {
				return true
			}
			return c.send(buildOpMsg(s.nextRequestID(), hdr.requestID, 0, a.raw, crcNone))
		case actMalformed:
			return c.replyMalformed(p)
		}
	}

	// Default handling. Like servers 6.0+, OP_QUERY only carries hello/isMaster.
	if hdr.opcode == OpQuery && lower != "hello" && lower != "ismaster" {
		return c.reply(p, errorDoc(352, "UnsupportedOpQueryCommand",
			"Unsupported OP_QUERY command: "+cmd+". The client driver may require an upgrade."), crcNone, hdr.requestID)
	}
	switch lower {
	case "saslstart", "saslcontinue":
		doc, reset := c.handleSasl(p, lower)
		if reset {
			c.close(true)
			return false
		}
		if doc == nil {
			return false
		}
		return c.reply(p, doc, crcNone, hdr.requestID)
	}
	return c.reply(p, c.defaultReply(p, cmd, lower), crcNone, hdr.requestID)
}

// defaultReply computes the reply document for non-fault handling. Hello may
// block (awaitable hello); it returns nil if the connection closed meanwhile.
func (c *conn) defaultReply(p *parsed, cmd, lower string) []byte {
	s := c.s
	switch lower {
	case "hello", "ismaster":
		return c.helloReply(p, lower == "hello")
	case "ping":
		return mustMarshal(bson.D{{Key: "ok", Value: float64(1)}})
	}
	if s.opts.RequireAuth {
		s.mu.Lock()
		authed := c.authenticated
		s.mu.Unlock()
		if !authed && lower != "endsessions" && lower != "buildinfo" {
			return errorDoc(13, "Unauthorized", "command "+cmd+" requires authentication")
		}
	}
	d := bson.D{{Key: "ok", Value: float64(1)}, {Key: "n", Value: int32(1)}}
	if v, err := p.body.LookupErr("corraltestMarker"); err == nil {
		d = append(d, bson.E{Key: "corraltestEcho", Value: v})
	}
	ns := p.db + ".coll"
	if coll, ok := p.body.Lookup(cmd).StringValueOK(); ok {
		ns = p.db + "." + coll
	}
	n := int64(0)
	if v, err := p.body.LookupErr("corraltestReplyBytes"); err == nil {
		n, _ = asInt64(v)
	}
	switch lower {
	case "find", "aggregate":
		d = append(d, bson.E{Key: "cursor", Value: bson.D{
			{Key: "id", Value: int64(0)}, {Key: "ns", Value: ns}, {Key: "firstBatch", Value: padBatch(n)},
		}})
	case "getmore":
		d = append(d, bson.E{Key: "cursor", Value: bson.D{
			{Key: "id", Value: int64(0)}, {Key: "ns", Value: ns}, {Key: "nextBatch", Value: bson.A{}},
		}})
	}
	return mustMarshal(d)
}

func asInt64(v bson.RawValue) (int64, bool) {
	if i, ok := v.AsInt64OK(); ok {
		return i, true
	}
	if f, ok := v.DoubleOK(); ok {
		return int64(f), true
	}
	return 0, false
}

const padChunk = 1 << 20

// padBatch returns documents totalling about n bytes of a deterministic
// pattern (each document at most 1 MiB). n <= 0 gives an empty batch.
func padBatch(n int64) bson.A {
	batch := bson.A{}
	for i := int32(0); n > 0; i++ {
		sz := n
		if sz > padChunk {
			sz = padChunk
		}
		b := make([]byte, sz)
		for j := range b {
			b[j] = byte('a' + (int64(i)*7+int64(j))%26)
		}
		batch = append(batch, bson.D{{Key: "_id", Value: i}, {Key: "pad", Value: string(b)}})
		n -= sz
	}
	return batch
}

func (c *conn) helloReply(p *parsed, isHello bool) []byte {
	s := c.s
	helloOK := false
	if v, ok := p.body.Lookup("helloOk").BooleanOK(); ok {
		helloOK = v
	}
	// Awaitable hello.
	if tv, err := p.body.LookupErr("topologyVersion"); err == nil && isHello {
		if doc, ok := tv.DocumentOK(); ok {
			pid, _ := doc.Lookup("processId").ObjectIDOK()
			cnt, _ := doc.Lookup("counter").AsInt64OK()
			wait, _ := asInt64(p.body.Lookup("maxAwaitTimeMS"))
			s.mu.Lock()
			same := pid == s.processID && cnt == s.counter
			ch := s.topoCh
			s.mu.Unlock()
			if same && wait > 0 {
				t := time.NewTimer(time.Duration(wait) * time.Millisecond)
				select {
				case <-ch:
				case <-t.C:
				case <-c.done:
					t.Stop()
					return nil
				}
				t.Stop()
			}
		}
	}
	var mechs bson.A
	if sm, ok := p.body.Lookup("saslSupportedMechs").StringValueOK(); ok {
		if _, user, found := strings.Cut(sm, "."); found {
			if _, known := s.opts.Users[user]; known {
				mechs = bson.A{"SCRAM-SHA-256"}
			}
		}
	}
	s.mu.Lock()
	spec, counter := s.spec, s.counter
	s.mu.Unlock()
	return mustMarshal(s.helloDoc(spec, counter, c.idx, helloOK, !isHello, mechs))
}

func (c *conn) handleSasl(p *parsed, lower string) (doc []byte, reset bool) {
	s := c.s
	fail := func(code int32, name, msg string) ([]byte, bool) { return errorDoc(code, name, msg), false }
	apply := func(o AuthOutcome) ([]byte, bool, bool) { // doc, reset, handled
		switch o.kind {
		case authError:
			d, r := fail(o.code, o.name, "")
			return d, r, true
		case authReset:
			return nil, true, true
		case authDelay:
			if !c.sleep(o.delay) {
				return nil, false, true
			}
		}
		return nil, false, false
	}
	payload := func() string {
		if _, b, ok := p.body.Lookup("payload").BinaryOK(); ok {
			return string(b)
		}
		s, _ := p.body.Lookup("payload").StringValueOK()
		return s
	}
	bin := func(s string) bson.Binary { return bson.Binary{Subtype: 0, Data: []byte(s)} }

	if lower == "saslstart" {
		o := s.nextAuthOutcome()
		conv := &scramConv{outcome: o, step: 1}
		conv.mechanism, _ = p.body.Lookup("mechanism").StringValueOK()
		if opt, ok := p.body.Lookup("options").DocumentOK(); ok {
			conv.skipEmpty, _ = opt.Lookup("skipEmptyExchange").BooleanOK()
		}
		c.conv = conv
		if o.stage == 0 {
			if d, r, h := apply(o); h {
				c.conv = nil
				return d, r
			}
		}
		if conv.mechanism != "SCRAM-SHA-256" {
			c.conv = nil
			return fail(334, "MechanismUnavailable", "Unsupported mechanism "+conv.mechanism)
		}
		first, err := s.scramStart(conv, payload())
		if err != nil {
			c.conv = nil
			return fail(18, "AuthenticationFailed", "Authentication failed.")
		}
		return mustMarshal(bson.D{
			{Key: "conversationId", Value: int32(1)}, {Key: "done", Value: false},
			{Key: "payload", Value: bin(first)}, {Key: "ok", Value: float64(1)},
		}), false
	}

	conv := c.conv
	if conv == nil {
		return fail(2, "BadValue", "no SASL conversation in progress")
	}
	switch conv.step {
	case 1:
		if conv.outcome.stage == 1 {
			if d, r, h := apply(conv.outcome); h {
				c.conv = nil
				return d, r
			}
		}
		final, err := s.scramFinish(conv, payload())
		if err != nil {
			c.conv = nil
			return fail(18, "AuthenticationFailed", "Authentication failed.")
		}
		done := conv.skipEmpty
		if done {
			c.markAuth(conv)
			c.conv = nil
		} else {
			conv.step = 2
		}
		return mustMarshal(bson.D{
			{Key: "conversationId", Value: int32(1)}, {Key: "done", Value: done},
			{Key: "payload", Value: bin(final)}, {Key: "ok", Value: float64(1)},
		}), false
	default:
		c.markAuth(conv)
		c.conv = nil
		return mustMarshal(bson.D{
			{Key: "conversationId", Value: int32(1)}, {Key: "done", Value: true},
			{Key: "payload", Value: bin("")}, {Key: "ok", Value: float64(1)},
		}), false
	}
}

func (c *conn) markAuth(conv *scramConv) {
	if conv.outcome.kind == authBadSig {
		// The client rejects the forged signature; the server never treats
		// the connection as authenticated in a way tests should rely on.
		c.s.mu.Lock()
		c.sigForged = true
		c.s.mu.Unlock()
		return
	}
	c.s.mu.Lock()
	c.authenticated = true
	c.authMech = conv.mechanism
	c.authUser = conv.user
	c.s.mu.Unlock()
}

// ---- reply encoding ----

func mustMarshal(v any) []byte {
	b, err := bson.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

var errMsgs = map[int32]string{
	10107: "not primary", 13435: "not primary and secondaryOk=false",
	11602: "interrupted due to repl state change", 13436: "node is not in primary or recovering state",
	189: "primary stepped down while waiting for replication", 91: "shutdown in progress",
	11600: "interrupted at shutdown", 18: "Authentication failed.",
}

var codeNames = map[int32]string{
	2: "BadValue", 6: "HostUnreachable", 7: "HostNotFound", 13: "Unauthorized", 18: "AuthenticationFailed",
	43: "CursorNotFound", 50: "MaxTimeMSExpired", 59: "CommandNotFound", 64: "WriteConcernFailed",
	79: "UnknownReplWriteConcern", 89: "NetworkTimeout", 91: "ShutdownInProgress", 100: "UnsatisfiableWriteConcern",
	115: "CommandNotSupported", 352: "UnsupportedOpQueryCommand", 189: "PrimarySteppedDown", 334: "MechanismUnavailable",
	9001: "SocketException", 10107: "NotWritablePrimary", 11600: "InterruptedAtShutdown",
	11602: "InterruptedDueToReplStateChange", 13435: "NotPrimaryNoSecondaryOk", 13436: "NotPrimaryOrSecondary",
}

func codeNameFor(code int32) string {
	if n, ok := codeNames[code]; ok {
		return n
	}
	return "SimulatedError"
}

func errorDoc(code int32, name, msg string) []byte {
	if msg == "" {
		msg = errMsgs[code]
	}
	if msg == "" {
		msg = "simulated error"
	}
	if name == "" {
		name = codeNameFor(code)
	}
	return mustMarshal(bson.D{
		{Key: "ok", Value: float64(0)}, {Key: "errmsg", Value: msg},
		{Key: "code", Value: code}, {Key: "codeName", Value: name},
	})
}

func (c *conn) reply(p *parsed, doc []byte, crc int, responseTo int32) bool {
	return c.replyFlags(p, doc, 0, crc, responseTo)
}

func (c *conn) replyFlags(p *parsed, doc []byte, flags uint32, crc int, responseTo int32) bool {
	if doc == nil { // connection closed while computing
		return false
	}
	if p.hdr.opcode == OpMsg {
		if p.flags&FlagMoreToCome != 0 {
			return true // fire-and-forget: no reply
		}
		return c.send(buildOpMsg(c.s.nextRequestID(), responseTo, flags, kind0(doc), crc))
	}
	return c.send(buildOpReply(c.s.nextRequestID(), responseTo, doc))
}

func (c *conn) replyMalformed(p *parsed) bool {
	// A document whose declared length exceeds the available bytes.
	bad := []byte{0xff, 0x00, 0x00, 0x00, 0x10, 'o', 'k', 0x00, 0x01}
	if p.hdr.opcode == OpMsg {
		if p.flags&FlagMoreToCome != 0 {
			return true
		}
		return c.send(buildOpMsg(c.s.nextRequestID(), p.hdr.requestID, 0, kind0(bad), crcNone))
	}
	return c.send(buildOpReply(c.s.nextRequestID(), p.hdr.requestID, bad))
}

func (c *conn) replyOversize(p *parsed, doc []byte) bool {
	if doc == nil || p.hdr.opcode != OpMsg {
		return doc != nil && c.reply(p, doc, crcNone, p.hdr.requestID)
	}
	b := buildOpMsg(c.s.nextRequestID(), p.hdr.requestID, 0, kind0(doc), crcNone)
	c.s.mu.Lock()
	limit := c.s.spec.maxMsg()
	c.s.mu.Unlock()
	putHeader(b, header{length: limit + 1, requestID: parseHeader(b).requestID, responseTo: p.hdr.requestID, opcode: OpMsg})
	return c.send(b)
}

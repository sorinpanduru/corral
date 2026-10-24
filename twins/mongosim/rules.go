package mongosim

import (
	"strings"
	"sync"
	"time"
)

type actionKind int

const (
	actHold actionKind = iota + 1
	actDelay
	actResetAfterReceive
	actError
	actErrorMessage
	actWriteConcernError
	actUnknownResponseTo
	actMoreToCome
	actOversize
	actChecksum
	actRaw
	actMalformed
	actNoReply
)

// Action is what a rule does to a matching request.
type Action struct {
	kind   actionKind
	d      time.Duration
	code   int32
	name   string
	errmsg string
	valid  bool
	raw    []byte
}

// Hold keeps the request unanswered until RuleHandle.Release. After the
// release the request is answered normally. The wait also ends if the
// connection closes.
func Hold() Action { return Action{kind: actHold} }

// Delay waits d, then answers normally.
func Delay(d time.Duration) Action { return Action{kind: actDelay, d: d} }

// ResetAfterReceive closes the connection (TCP reset) after reading the request.
func ResetAfterReceive() Action { return Action{kind: actResetAfterReceive} }

// ReplyError answers {ok: 0, errmsg, code, codeName}. The errmsg is the one a
// real server uses for the code when known (10107, 13435, 11602, 13436, 189,
// 91, 11600), else "simulated error".
func ReplyError(code int32, codeName string) Action {
	return Action{kind: actError, code: code, name: codeName}
}

// ReplyErrorMessage answers {ok: 0, errmsg} with no code, like pre-4.2 servers
// answering "not master".
func ReplyErrorMessage(errmsg string) Action {
	return Action{kind: actErrorMessage, errmsg: errmsg}
}

// ReplyWriteConcernError answers {ok: 1, n: 1, writeConcernError: {code, codeName, errmsg}}.
func ReplyWriteConcernError(code int32) Action {
	return Action{kind: actWriteConcernError, code: code}
}

// ReplyUnknownResponseTo answers with a responseTo that matches no request.
func ReplyUnknownResponseTo() Action { return Action{kind: actUnknownResponseTo} }

// ReplyMoreToCome answers with the moreToCome flag set.
func ReplyMoreToCome() Action { return Action{kind: actMoreToCome} }

// ReplyOversize answers with a header messageLength above maxMessageSizeBytes
// (only the header and a short body are sent).
func ReplyOversize() Action { return Action{kind: actOversize} }

// ReplyChecksum answers with checksumPresent set and a CRC-32C that is
// correct (valid) or wrong (!valid).
func ReplyChecksum(valid bool) Action { return Action{kind: actChecksum, valid: valid} }

// ReplyRaw answers with an OP_MSG whose sections (everything after flagBits)
// are exactly the given bytes. Header, responseTo and flags are set correctly.
func ReplyRaw(sections []byte) Action {
	return Action{kind: actRaw, raw: append([]byte(nil), sections...)}
}

// ReplyMalformed answers with an OP_MSG whose body is not valid BSON.
func ReplyMalformed() Action { return Action{kind: actMalformed} }

// NoReply never answers (the connection stays open).
func NoReply() Action { return Action{kind: actNoReply} }

// Match selects requests. Zero fields match everything. An empty Command
// matches any command except the handshake family (hello, isMaster, ismaster,
// saslStart, saslContinue); name those explicitly to match them.
type Match struct {
	Command string // case-insensitive
	Marker  string // value of the request's corraltestMarker string field
	AppName string // handshake appName of the connection
	DB      string
	// Nth, when > 0, restricts the rule to the Nth matching request (1-based).
	Nth int
	// Once consumes the rule after its first application.
	Once bool
}

// RuleHandle controls an installed rule.
type RuleHandle struct {
	s        *Server
	m        Match
	a        Action
	mu       sync.Mutex
	seen     int
	hits     int
	held     int
	used     bool
	removed  bool
	released bool
	relCh    chan struct{}
}

// Release lets every held request (and any future one) through.
func (h *RuleHandle) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.released {
		h.released = true
		close(h.relCh)
	}
}

// Remove uninstalls the rule. Held requests are released.
func (h *RuleHandle) Remove() {
	h.mu.Lock()
	h.removed = true
	h.mu.Unlock()
	h.Release()
}

// Hits is how many requests the rule has been applied to.
func (h *RuleHandle) Hits() int { h.mu.Lock(); defer h.mu.Unlock(); return h.hits }

// Held is how many requests are currently held.
func (h *RuleHandle) Held() int { h.mu.Lock(); defer h.mu.Unlock(); return h.held }

// WaitHeld waits until at least n requests are held.
func (h *RuleHandle) WaitHeld(n int, timeout time.Duration) bool {
	return poll(timeout, func() bool { return h.Held() >= n })
}

// WaitHits waits until the rule has been applied at least n times.
func (h *RuleHandle) WaitHits(n int, timeout time.Duration) bool {
	return poll(timeout, func() bool { return h.Hits() >= n })
}

func poll(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func isHandshakeCmd(lower string) bool {
	switch lower {
	case "hello", "ismaster", "saslstart", "saslcontinue":
		return true
	}
	return false
}

// AddRule installs a rule. Rules are consulted in installation order; the
// first one that matches (and is not consumed) is applied.
func (s *Server) AddRule(m Match, a Action) *RuleHandle {
	h := &RuleHandle{s: s, m: m, a: a, relCh: make(chan struct{})}
	s.mu.Lock()
	s.rules = append(s.rules, h)
	s.mu.Unlock()
	return h
}

// ClearRules removes every rule (held requests are released).
func (s *Server) ClearRules() {
	s.mu.Lock()
	rs := s.rules
	s.rules = nil
	s.mu.Unlock()
	for _, r := range rs {
		r.Remove()
	}
}

// matchRule finds the rule to apply to msg, or nil.
func (s *Server) matchRule(cmd, marker, appName, db string) *RuleHandle {
	lower := strings.ToLower(cmd)
	s.mu.Lock()
	rules := append([]*RuleHandle(nil), s.rules...)
	s.mu.Unlock()
	var picked *RuleHandle
	for _, r := range rules {
		m := r.m
		if m.Command == "" {
			if isHandshakeCmd(lower) {
				continue
			}
		} else if !strings.EqualFold(m.Command, cmd) {
			continue
		}
		if m.Marker != "" && m.Marker != marker {
			continue
		}
		if m.AppName != "" && m.AppName != appName {
			continue
		}
		if m.DB != "" && m.DB != db {
			continue
		}
		r.mu.Lock()
		if r.removed || (r.m.Once && r.used) {
			r.mu.Unlock()
			continue
		}
		r.seen++
		if (m.Nth > 0 && r.seen != m.Nth) || picked != nil {
			// Not applied, but the request still counted towards Nth.
			r.mu.Unlock()
			continue
		}
		r.used = true
		r.hits++
		r.mu.Unlock()
		picked = r
	}
	return picked
}

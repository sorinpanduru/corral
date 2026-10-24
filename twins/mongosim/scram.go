package mongosim

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// AuthOutcome scripts what the SCRAM server does with the next conversation.
type AuthOutcome struct {
	kind  authKind
	code  int32
	name  string
	delay time.Duration
	stage int // 0 = at saslStart, 1 = at the first saslContinue
}

type authKind int

const (
	authSucceed authKind = iota
	authError
	authBadSig
	authReset
	authDelay
)

var (
	// Succeed runs a genuine SCRAM-SHA-256 conversation (a wrong password
	// still fails with code 18, as on a real server).
	Succeed = AuthOutcome{kind: authSucceed}
	// Fail18 answers AuthenticationFailed (code 18).
	Fail18 = AuthOutcome{kind: authError, code: 18, name: "AuthenticationFailed"}
	// Fail334 answers MechanismUnavailable (code 334).
	Fail334 = AuthOutcome{kind: authError, code: 334, name: "MechanismUnavailable"}
	// BadServerSignature completes the exchange with a wrong "v=" value.
	BadServerSignature = AuthOutcome{kind: authBadSig}
	// AuthReset closes the connection with a TCP reset.
	AuthReset = AuthOutcome{kind: authReset}
)

// ErrorCode answers {ok: 0, code, codeName} (for example 10107, 91, 11600).
func ErrorCode(code int32, codeName string) AuthOutcome {
	return AuthOutcome{kind: authError, code: code, name: codeName}
}

// AuthDelay waits d before running a genuine conversation.
func AuthDelay(d time.Duration) AuthOutcome { return AuthOutcome{kind: authDelay, delay: d} }

// AtContinue applies the outcome at the first saslContinue instead of at
// saslStart (Fail18, Fail334, ErrorCode, AuthReset, AuthDelay). BadServerSignature
// always applies at the final step.
func (o AuthOutcome) AtContinue() AuthOutcome { o.stage = 1; return o }

const scramIterations = 15000

type scramConv struct {
	user         string
	mechanism    string
	clientFirstB string // client-first-message-bare
	serverFirst  string
	nonce        string
	skipEmpty    bool
	outcome      AuthOutcome
	salted       []byte
	step         int // 1 after saslStart, 2 after client-final (waiting for empty exchange)
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func userSalt(user string) []byte {
	h := sha256.Sum256([]byte("mongosim-synthetic-salt:" + user))
	return h[:16]
}

func saltedPassword(pw, user string) []byte {
	k, err := pbkdf2.Key(sha256.New, pw, userSalt(user), scramIterations, 32)
	if err != nil {
		panic(err)
	}
	return k
}

// parseSCRAM splits "a=1,b=2" attribute lists (values may contain '=').
func parseSCRAM(s string) map[string]string {
	m := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		if len(part) >= 2 && part[1] == '=' {
			m[part[:1]] = part[2:]
		}
	}
	return m
}

func unescapeSCRAMName(s string) string {
	s = strings.ReplaceAll(s, "=2C", ",")
	return strings.ReplaceAll(s, "=3D", "=")
}

// scramStart handles the client-first message. It returns the server-first.
func (s *Server) scramStart(conv *scramConv, payload string) (string, error) {
	// gs2 header "n,," then client-first-message-bare.
	if !strings.HasPrefix(payload, "n,,") {
		return "", fmt.Errorf("unsupported gs2 header")
	}
	bare := payload[3:]
	attrs := parseSCRAM(bare)
	user := unescapeSCRAMName(attrs["n"])
	pw, ok := s.opts.Users[user]
	if !ok || attrs["r"] == "" {
		return "", fmt.Errorf("unknown user")
	}
	conv.user = user
	conv.clientFirstB = bare
	s.mu.Lock()
	s.nonceSeq++
	seq := s.nonceSeq
	s.mu.Unlock()
	conv.nonce = attrs["r"] + base64.StdEncoding.EncodeToString([]byte("mongosim-nonce-"+strconv.FormatInt(seq, 10)))
	conv.serverFirst = fmt.Sprintf("r=%s,s=%s,i=%d", conv.nonce, base64.StdEncoding.EncodeToString(userSalt(user)), scramIterations)
	conv.salted = saltedPassword(pw, user)
	return conv.serverFirst, nil
}

// scramFinish verifies the client-final message and returns the server-final.
func (s *Server) scramFinish(conv *scramConv, payload string) (string, error) {
	i := strings.LastIndex(payload, ",p=")
	if i < 0 {
		return "", fmt.Errorf("no proof")
	}
	withoutProof := payload[:i]
	attrs := parseSCRAM(payload)
	if attrs["r"] != conv.nonce || attrs["c"] != "biws" {
		return "", fmt.Errorf("nonce mismatch")
	}
	proof, err := base64.StdEncoding.DecodeString(attrs["p"])
	if err != nil || len(proof) != sha256.Size {
		return "", fmt.Errorf("bad proof")
	}
	authMsg := conv.clientFirstB + "," + conv.serverFirst + "," + withoutProof
	clientKey := hmacSHA256(conv.salted, []byte("Client Key"))
	stored := sha256.Sum256(clientKey)
	sig := hmacSHA256(stored[:], []byte(authMsg))
	rec := make([]byte, len(proof))
	for j := range proof {
		rec[j] = proof[j] ^ sig[j]
	}
	got := sha256.Sum256(rec)
	if subtle.ConstantTimeCompare(got[:], stored[:]) != 1 {
		return "", fmt.Errorf("bad proof")
	}
	serverKey := hmacSHA256(conv.salted, []byte("Server Key"))
	ss := hmacSHA256(serverKey, []byte(authMsg))
	if conv.outcome.kind == authBadSig {
		ss[0] ^= 0xff
	}
	return "v=" + base64.StdEncoding.EncodeToString(ss), nil
}

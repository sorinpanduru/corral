// Package scram is a thin adapter around github.com/xdg-go/scram for
// SCRAM-SHA-256 client authentication (SPEC-002 FR-22, ADR-0019).
//
// The library does SASLprep, username escaping, nonces and the derived-key
// cache. This package adds shape checks on server messages and maps every
// failure to one of four constant sentinels.
//
// Compliance (FR-24): library errors are never wrapped, logged or returned,
// because they could echo parts of a message. Credentials are held only inside
// the library client and are never formatted.
package scram

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"

	xscram "github.com/xdg-go/scram"
	"github.com/xdg-go/stringprep"
)

// Sentinels. Constant text only.
var (
	// ErrServerSignature means the server's signature did not verify.
	ErrServerSignature = errors.New("scram: server signature mismatch")
	// ErrProtocol means a malformed or unexpected message, or a server error.
	ErrProtocol = errors.New("scram: protocol error")
	// ErrSASLprep means the username or password cannot be prepared.
	ErrSASLprep = errors.New("scram: credential rejected by SASLprep")
	// ErrIterations means the server asked for fewer than 4096 or more than 1,000,000 iterations.
	ErrIterations = errors.New("scram: iteration count outside the accepted range")
)

const (
	minIterations = 4096
	// maxIterations caps the server-requested PBKDF2 work. A hostile or
	// compromised upstream could otherwise burn CPU with a huge count.
	// MongoDB defaults are 15000 (SHA-256); 1,000,000 leaves ample headroom.
	maxIterations = 1_000_000
)

// Client holds one library client (and its derived-key cache) per credential.
type Client struct {
	c *xscram.Client
}

// NewClient prepares a client. Like the official MongoDB drivers, only the
// password is SASLprepped; the username is used as given (MongoDB stores it
// unprepared). A SASLprep failure gives ErrSASLprep.
func NewClient(username, password []byte) (*Client, error) {
	pw, err := stringprep.SASLprep.Prepare(string(password))
	if err != nil {
		return nil, ErrSASLprep
	}
	c, err := xscram.SHA256.NewClientUnprepped(string(username), pw, "")
	if err != nil {
		return nil, ErrSASLprep
	}
	return &Client{c: c.WithMinIterations(minIterations)}, nil
}

// NewConversation starts one authentication attempt.
func (c *Client) NewConversation() *Conversation {
	return &Conversation{cc: c.c.NewConversation()}
}

// Conversation is one authentication attempt. It is not safe for concurrent
// use.
type Conversation struct {
	cc    *xscram.ClientConversation
	state int // 0 new, 1 sent client-first, 2 sent client-final, 3 finished
	nonce string
}

// First returns the client-first message.
func (c *Conversation) First() ([]byte, error) {
	if c.state != 0 {
		return nil, ErrProtocol
	}
	msg, err := c.cc.Step("")
	if err != nil {
		return nil, ErrProtocol
	}
	for _, part := range strings.Split(msg, ",") {
		if strings.HasPrefix(part, "r=") {
			c.nonce = part[2:]
		}
	}
	if c.nonce == "" {
		return nil, ErrProtocol
	}
	c.state = 1
	return []byte(msg), nil
}

// Step consumes a server message. It returns the reply to send, and done once
// the server's signature has verified.
func (c *Conversation) Step(server []byte) (reply []byte, done bool, err error) {
	switch c.state {
	case 1:
		return c.onServerFirst(string(server))
	case 2:
		return c.onServerFinal(string(server))
	}
	return nil, false, ErrProtocol
}

func (c *Conversation) onServerFirst(msg string) ([]byte, bool, error) {
	parts := strings.Split(msg, ",")
	if len(parts) < 3 || !strings.HasPrefix(parts[0], "r=") ||
		!strings.HasPrefix(parts[1], "s=") || !strings.HasPrefix(parts[2], "i=") {
		return nil, false, ErrProtocol
	}
	nonce := parts[0][2:]
	if len(nonce) <= len(c.nonce) || !strings.HasPrefix(nonce, c.nonce) {
		return nil, false, ErrProtocol
	}
	if salt, err := base64.StdEncoding.DecodeString(parts[1][2:]); err != nil || len(salt) == 0 {
		return nil, false, ErrProtocol
	}
	iters, err := strconv.Atoi(parts[2][2:])
	if err != nil {
		return nil, false, ErrProtocol
	}
	if iters < minIterations || iters > maxIterations {
		return nil, false, ErrIterations
	}
	out, err := c.cc.Step(msg)
	if err != nil {
		return nil, false, ErrProtocol
	}
	c.state = 2
	return []byte(out), false, nil
}

func (c *Conversation) onServerFinal(msg string) ([]byte, bool, error) {
	if !strings.HasPrefix(msg, "v=") || strings.Contains(msg, ",") {
		return nil, false, ErrProtocol
	}
	if sig, err := base64.StdEncoding.DecodeString(msg[2:]); err != nil || len(sig) == 0 {
		return nil, false, ErrProtocol
	}
	c.state = 3
	if _, err := c.cc.Step(msg); err != nil || !c.cc.Valid() {
		return nil, false, ErrServerSignature
	}
	return nil, true, nil
}

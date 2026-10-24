package scram_test

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	xscram "github.com/xdg-go/scram"
	"github.com/xdg-go/stringprep"

	"github.com/sorinpanduru/corral/internal/infra/upstreamconn/scram"
)

// Test-side SCRAM server built with the library's server API and synthetic
// credentials. No real credential is used anywhere in this file.

type creds struct {
	name, password string
	salt           []byte
	iters          int
}

func newCreds(t testing.TB, name, password string, iters int) creds {
	t.Helper()
	salt := make([]byte, 24)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	return creds{name, password, salt, iters}
}

// server returns a library SCRAM-SHA-256 server that knows c. The stored
// credentials are derived by the library client from the (SASLprepped)
// password, as MongoDB does.
func (c creds) server(t testing.TB) *xscram.Server {
	t.Helper()
	// Driver semantics (MongoDB): only the password is SASLprepped; the
	// username is used exactly as given. So the stored credentials are derived
	// from the prepped password with an unprepped client.
	prepped, err := stringprep.SASLprep.Prepare(c.password)
	if err != nil {
		t.Fatal(err)
	}
	lc, err := xscram.SHA256.NewClientUnprepped(c.name, prepped, "")
	if err != nil {
		t.Fatal(err)
	}
	stored := lc.GetStoredCredentials(xscram.KeyFactors{Salt: string(c.salt), Iters: c.iters})
	srv, err := xscram.SHA256.NewServer(func(user string) (xscram.StoredCredentials, error) {
		// The library server passes the wire form of the name (RFC 5802
		// escaping: "=2C" for ',' and "=3D" for '='). Decode it strictly, as
		// MongoDB does, so a wrongly escaped username fails the lookup.
		if decoded, ok := unescapeName(user); !ok || decoded != c.name {
			return xscram.StoredCredentials{}, errors.New("unknown user")
		}
		return stored, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// unescapeName reverses RFC 5802 saslname escaping. A bare '=' not followed
// by 2C or 3D is invalid.
func unescapeName(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '=' {
			b.WriteByte(s[i])
			continue
		}
		if len(s)-i < 3 {
			return "", false
		}
		switch s[i+1 : i+3] {
		case "2C":
			b.WriteByte(',')
		case "3D":
			b.WriteByte('=')
		default:
			return "", false
		}
		i += 2
	}
	return b.String(), true
}

var allSentinels = []error{scram.ErrServerSignature, scram.ErrProtocol, scram.ErrSASLprep, scram.ErrIterations}

func isSentinel(err error) bool {
	for _, s := range allSentinels {
		if err == s {
			return true
		}
	}
	return false
}

// run drives a full conversation against srv and returns the last error and
// the done flag.
func run(t *testing.T, conv *scram.Conversation, srv *xscram.Server) (done bool, err error) {
	t.Helper()
	sc := srv.NewConversation()
	first, err := conv.First()
	if err != nil {
		return false, err
	}
	var msg = first
	for i := 0; i < 6; i++ {
		resp, serr := sc.Step(string(msg))
		if serr != nil {
			return false, fmt.Errorf("test server rejected the exchange")
		}
		reply, d, err := conv.Step([]byte(resp))
		if err != nil {
			return d, err
		}
		if d {
			return true, nil
		}
		msg = reply
	}
	return false, errors.New("conversation did not finish")
}

// AC-11, FR-22: a conversation against the library server succeeds.
func TestConversationSucceeds_FR22(t *testing.T) {
	c := newCreds(t, "svc-user", "synthetic-pass-1", 4096)
	cl, err := scram.NewClient([]byte(c.name), []byte(c.password))
	if err != nil {
		t.Fatal(err)
	}
	done, err := run(t, cl.NewConversation(), c.server(t))
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
}

// A wrong password fails with the server's rejection, not a library error:
// the test server returns an error at the proof step.
func TestConversationWrongPassword_FR22(t *testing.T) {
	c := newCreds(t, "svc-user", "synthetic-pass-1", 4096)
	cl, _ := scram.NewClient([]byte(c.name), []byte("another-synthetic-pass"))
	_, err := run(t, cl.NewConversation(), c.server(t))
	if err == nil {
		t.Fatal("wrong password authenticated")
	}
}

// AC-25 (unit): a tampered v= gives ErrServerSignature.
func TestTamperedServerSignature_AC25(t *testing.T) {
	c := newCreds(t, "svc-user", "synthetic-pass-1", 4096)
	cl, _ := scram.NewClient([]byte(c.name), []byte(c.password))
	conv := cl.NewConversation()
	sc := c.server(t).NewConversation()

	first, err := conv.First()
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := sc.Step(string(first))
	c2, done, err := conv.Step([]byte(s1))
	if err != nil || done {
		t.Fatalf("step 2: done=%v err=%v", done, err)
	}
	s2, err := sc.Step(string(c2))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s2, "v=") {
		t.Fatalf("unexpected server-final shape")
	}
	bad := make([]byte, 32)
	for i := range bad {
		bad[i] = byte(i)
	}
	tampered := "v=" + base64.StdEncoding.EncodeToString(bad)
	_, _, err = conv.Step([]byte(tampered))
	if err != scram.ErrServerSignature {
		t.Fatalf("tampered v=: got %v, want ErrServerSignature", err)
	}
}

// A signature that is the right length but one flipped bit also fails.
func TestFlippedSignatureBit_AC25(t *testing.T) {
	c := newCreds(t, "svc-user", "synthetic-pass-1", 4096)
	cl, _ := scram.NewClient([]byte(c.name), []byte(c.password))
	conv := cl.NewConversation()
	sc := c.server(t).NewConversation()
	first, _ := conv.First()
	s1, _ := sc.Step(string(first))
	c2, _, _ := conv.Step([]byte(s1))
	s2, _ := sc.Step(string(c2))
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s2, "v="))
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 1
	if _, _, err := conv.Step([]byte("v=" + base64.StdEncoding.EncodeToString(raw))); err != scram.ErrServerSignature {
		t.Fatalf("got %v, want ErrServerSignature", err)
	}
}

// helper: start a conversation and return the client nonce and conversation.
func started(t *testing.T) (*scram.Conversation, string) {
	t.Helper()
	cl, err := scram.NewClient([]byte("svc-user"), []byte("synthetic-pass-1"))
	if err != nil {
		t.Fatal(err)
	}
	conv := cl.NewConversation()
	first, err := conv.First()
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(first), "r=")
	if i < 0 {
		t.Fatal("client-first has no nonce")
	}
	return conv, string(first)[i+2:]
}

func b64(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }

// FR-22: an e= server-final is ErrProtocol, never ErrServerSignature.
func TestServerErrorIsProtocol_FR22(t *testing.T) {
	c := newCreds(t, "svc-user", "synthetic-pass-1", 4096)
	for _, final := range []string{"e=invalid-proof", "e=other-error", "e=", "e=invalid-proof,v=AAAA"} {
		t.Run(final, func(t *testing.T) {
			cl, _ := scram.NewClient([]byte(c.name), []byte(c.password))
			conv := cl.NewConversation()
			sc := c.server(t).NewConversation()
			first, _ := conv.First()
			s1, _ := sc.Step(string(first))
			if _, _, err := conv.Step([]byte(s1)); err != nil {
				t.Fatal(err)
			}
			_, _, err := conv.Step([]byte(final))
			if err != scram.ErrProtocol {
				t.Fatalf("got %v, want ErrProtocol", err)
			}
		})
	}
}

// FR-22: malformed server-first messages are ErrProtocol. Each message is
// built from the conversation's own client nonce.
func TestMalformedServerFirst_FR22(t *testing.T) {
	salt := b64(16)
	bad := map[string]func(n string) string{
		"empty":                    func(n string) string { return "" },
		"garbage":                  func(n string) string { return "hello" },
		"no salt":                  func(n string) string { return "r=" + n + "x,i=4096" },
		"no iterations":            func(n string) string { return "r=" + n + "x,s=" + salt },
		"no nonce":                 func(n string) string { return "s=" + salt + ",i=4096" },
		"iterations not a number":  func(n string) string { return "r=" + n + "x,s=" + salt + ",i=abc" },
		"salt not base64":          func(n string) string { return "r=" + n + "x,s=!!!,i=4096" },
		"server error":             func(n string) string { return "e=something" },
		"nonce does not extend":    func(n string) string { return "r=zzzz" + n + ",s=" + salt + ",i=4096" },
		"nonce replaced":           func(n string) string { return "r=completely-different,s=" + salt + ",i=4096" },
		"nonce is a strict prefix": func(n string) string { return "r=" + n[:len(n)/2] + ",s=" + salt + ",i=4096" },
	}
	for name, build := range bad {
		t.Run(name, func(t *testing.T) {
			conv, nonce := started(t)
			_, _, err := conv.Step([]byte(build(nonce)))
			if err != scram.ErrProtocol {
				t.Fatalf("got %v, want ErrProtocol", err)
			}
		})
	}
}

// FR-22: malformed server-final messages are ErrProtocol.
func TestMalformedServerFinal_FR22(t *testing.T) {
	c := newCreds(t, "svc-user", "synthetic-pass-1", 4096)
	for _, final := range []string{"", "garbage", "v=", "v=!!!not-base64", "x=1", "v=AAAA,extra=1"} {
		t.Run(final, func(t *testing.T) {
			cl, _ := scram.NewClient([]byte(c.name), []byte(c.password))
			conv := cl.NewConversation()
			sc := c.server(t).NewConversation()
			first, _ := conv.First()
			s1, _ := sc.Step(string(first))
			conv.Step([]byte(s1))
			_, _, err := conv.Step([]byte(final))
			if err != scram.ErrProtocol && err != scram.ErrServerSignature {
				t.Fatalf("got %v, want a package sentinel", err)
			}
			if final != "v=AAAA,extra=1" && err != scram.ErrProtocol {
				// Structurally broken messages are protocol errors; only a
				// well-formed v= with a wrong value is a signature failure.
				t.Fatalf("got %v, want ErrProtocol", err)
			}
		})
	}
}

// FR-22: an iteration count below 4096 gives ErrIterations; 4096 is fine.
func TestIterations_FR22(t *testing.T) {
	for _, n := range []int{4095, 4000, 1, 0} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			c := newCreds(t, "svc-user", "synthetic-pass-1", n)
			cl, _ := scram.NewClient([]byte(c.name), []byte(c.password))
			_, err := run(t, cl.NewConversation(), c.server(t))
			if err != scram.ErrIterations {
				t.Fatalf("iterations %d: got %v, want ErrIterations", n, err)
			}
		})
	}
	c := newCreds(t, "svc-user", "synthetic-pass-1", 4096)
	cl, _ := scram.NewClient([]byte(c.name), []byte(c.password))
	if _, err := run(t, cl.NewConversation(), c.server(t)); err != nil {
		t.Fatalf("4096 rejected: %v", err)
	}
}

// Crafted server-first with a low count is rejected before any key derivation
// and with ErrIterations (not ErrProtocol).
func TestIterationsCrafted_FR22(t *testing.T) {
	conv, nonce := started(t)
	_, _, err := conv.Step([]byte("r=" + nonce + "srv,s=" + b64(16) + ",i=4095"))
	if err != scram.ErrIterations {
		t.Fatalf("got %v, want ErrIterations", err)
	}
}

// FR-22: usernames containing ',' and '=' are escaped and authenticate.
func TestUsernameEscaping_FR22(t *testing.T) {
	for _, name := range []string{"a,b", "a=b", "x=2Cy,z==", "plain"} {
		t.Run(name, func(t *testing.T) {
			c := newCreds(t, name, "synthetic-pass-1", 4096)
			cl, err := scram.NewClient([]byte(name), []byte(c.password))
			if err != nil {
				t.Fatal(err)
			}
			if done, err := run(t, cl.NewConversation(), c.server(t)); err != nil || !done {
				t.Fatalf("done=%v err=%v", done, err)
			}
		})
	}
}

// FR-22: SASLprep. A no-break space in the password maps to an ordinary space,
// so it authenticates against stored credentials made from the prepped form.
func TestSASLprep_FR22(t *testing.T) {
	c := newCreds(t, "svc-user", "pass word", 4096)
	cl, err := scram.NewClient([]byte(c.name), []byte("pass word"))
	if err != nil {
		t.Fatal(err)
	}
	if done, err := run(t, cl.NewConversation(), c.server(t)); err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	// A prohibited character (control U+0007) is ErrSASLprep from NewClient.
	if _, err := scram.NewClient([]byte("svc-user"), []byte("bad\x07pass")); err != scram.ErrSASLprep {
		t.Fatalf("prohibited character: got %v, want ErrSASLprep", err)
	}
	// Invalid UTF-8 is also unusable.
	if _, err := scram.NewClient([]byte("svc-user"), []byte{0xff, 0xfe}); err != scram.ErrSASLprep {
		t.Fatalf("invalid UTF-8: got %v, want ErrSASLprep", err)
	}
}

// A second conversation from the same Client (same salt and iteration count)
// works. That it reuses the derived keys instead of repeating PBKDF2 is a
// review item: the library exposes no counter, and a timing assertion would
// be flaky.
func TestSecondConversationSameClient_FR22(t *testing.T) {
	c := newCreds(t, "svc-user", "synthetic-pass-1", 4096)
	cl, _ := scram.NewClient([]byte(c.name), []byte(c.password))
	srv := c.server(t)
	for i := 0; i < 3; i++ {
		if done, err := run(t, cl.NewConversation(), srv); err != nil || !done {
			t.Fatalf("conversation %d: done=%v err=%v", i, done, err)
		}
	}
}

// Conversations are independent: two concurrent conversations from one Client
// both complete (run under -race).
func TestConcurrentConversations_FR22(t *testing.T) {
	c := newCreds(t, "svc-user", "synthetic-pass-1", 4096)
	cl, _ := scram.NewClient([]byte(c.name), []byte(c.password))
	srv := c.server(t)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			sc := srv.NewConversation()
			conv := cl.NewConversation()
			msg, err := conv.First()
			for k := 0; err == nil && k < 4; k++ {
				var resp string
				resp, err = sc.Step(string(msg))
				if err != nil {
					break
				}
				var done bool
				msg, done, err = conv.Step([]byte(resp))
				if done {
					break
				}
			}
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent conversation: %v", err)
		}
	}
}

// FR-24: only the four constant sentinels leave the package, and neither the
// username nor the password marker appears in any error text.
func TestErrorsAreConstantAndMarkerFree_FR24(t *testing.T) {
	um, pm := "USER-MARKER-3b7e", "PASS-MARKER-9d41"
	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			return
		}
		if !isSentinel(err) {
			t.Errorf("%s: error is not one of the package sentinels (%T)", name, err)
		}
		txt := fmt.Sprint(err)
		if strings.Contains(txt, um) || strings.Contains(txt, pm) || fmt.Sprintf("%v %+v %#v", err, err, err) == "" {
			t.Errorf("%s: error text contains a credential marker", name)
		}
	}
	for _, e := range allSentinels {
		if e.Error() == "" || strings.Contains(e.Error(), um) {
			t.Errorf("sentinel text unusable")
		}
	}
	c := newCreds(t, um, pm, 4096)
	cl, err := scram.NewClient([]byte(um), []byte(pm))
	if err != nil {
		t.Fatal(err)
	}
	// Failure scenarios with the marker credentials.
	_, err = run(t, cl.NewConversation(), c.server(t))
	check("success path", err)
	conv := cl.NewConversation()
	conv.First()
	_, _, err = conv.Step([]byte("e=" + um + pm))
	check("e= with markers", err)
	conv = cl.NewConversation()
	conv.First()
	_, _, err = conv.Step([]byte("r=" + um + ",s=" + pm + ",i=4095"))
	check("crafted first", err)
	_, err = scram.NewClient([]byte(um), []byte(pm+"\x07"))
	check("saslprep", err)
	if err == nil {
		t.Error("prohibited character accepted")
	}
}

// Step before First, or after completion, must not panic and must return an
// error (not a library error value).
func TestMisuse_NoPanic(t *testing.T) {
	cl, _ := scram.NewClient([]byte("svc-user"), []byte("synthetic-pass-1"))
	_, _, err := cl.NewConversation().Step([]byte("r=x,s=AA==,i=4096"))
	if err == nil || !isSentinel(err) {
		t.Fatalf("Step before First: %v", err)
	}
	c := newCreds(t, "svc-user", "synthetic-pass-1", 4096)
	conv := cl.NewConversation()
	if done, err := run(t, conv, c.server(t)); err != nil || !done {
		t.Fatal(err)
	}
	if _, _, err := conv.Step([]byte("v=AAAA")); err == nil || !isSentinel(err) {
		t.Fatalf("Step after done: %v", err)
	}
}

// Driver semantics (FR-22): the username is NOT SASLprepped. A name with a
// no-break space or a soft hyphen (which SASLprep would map to a space or to
// nothing) authenticates exactly as given.
func TestUsernameIsNotPrepped_FR22(t *testing.T) {
	for _, name := range []string{"a\u00a0b", "soft\u00adhyphen", "\u00a0lead", "trail\u00ad", "ctl\x07name", "\u2003em-space"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			c := newCreds(t, name, "synthetic-pass-1", 4096)
			cl, err := scram.NewClient([]byte(name), []byte(c.password))
			if err != nil {
				t.Fatalf("NewClient refused a username that must be used as given: %v", err)
			}
			if done, err := run(t, cl.NewConversation(), c.server(t)); err != nil || !done {
				t.Fatalf("done=%v err=%v", done, err)
			}
		})
	}
	// The prepped spelling is a different user: the server (exact lookup)
	// rejects it, proving the client did not alter the name.
	c := newCreds(t, "a\u00a0b", "synthetic-pass-1", 4096)
	cl, _ := scram.NewClient([]byte("a b"), []byte(c.password))
	if _, err := run(t, cl.NewConversation(), c.server(t)); err == nil {
		t.Fatal("a username equal to the prepped form authenticated as the unprepped one")
	}
}

// The password is prepped: NBSP maps to a space, a soft hyphen to nothing.
func TestPasswordIsPrepped_FR22(t *testing.T) {
	for in, stored := range map[string]string{
		"pass\u00a0word":   "pass word",
		"pass\u00adword":   "password",
		"\u2003lead\u00a0": " lead ",
	} {
		c := newCreds(t, "svc-user", stored, 4096)
		cl, err := scram.NewClient([]byte(c.name), []byte(in))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if done, err := run(t, cl.NewConversation(), c.server(t)); err != nil || !done {
			t.Fatalf("%q: done=%v err=%v", in, done, err)
		}
	}
}

// Only the password can fail SASLprep now; a prohibited character in the
// username is not an error.
func TestSASLprepAppliesToPasswordOnly_FR22(t *testing.T) {
	if _, err := scram.NewClient([]byte("user\x07name"), []byte("ok-pass")); err != nil {
		t.Fatalf("username with a control character refused: %v", err)
	}
	if _, err := scram.NewClient([]byte("svc-user"), []byte("bad\x07pass")); err != scram.ErrSASLprep {
		t.Fatalf("prohibited password character: %v, want ErrSASLprep", err)
	}
}

// An upper cap on the server's iteration count: absurd values are refused
// with ErrIterations, before any key derivation (so the test is instant). The
// exact cap is the implementer's constant; these values are far above any
// sensible one.
func TestIterationsAboveCap_FR22(t *testing.T) {
	for _, n := range []int{1_000_001, 100_000_000, 1 << 30, 2147483647} { // 1,000,001: just above the implementer's 1,000,000 cap
		conv, nonce := started(t)
		_, _, err := conv.Step([]byte(fmt.Sprintf("r=%ssrv,s=%s,i=%d", nonce, b64(16), n)))
		if err != scram.ErrIterations {
			t.Fatalf("iterations %d: got %v, want ErrIterations", n, err)
		}
	}
	// A realistic count (MongoDB's default for SCRAM-SHA-256 is 15000) is fine.
	c := newCreds(t, "svc-user", "synthetic-pass-1", 15000)
	cl, _ := scram.NewClient([]byte(c.name), []byte(c.password))
	if done, err := run(t, cl.NewConversation(), c.server(t)); err != nil || !done {
		t.Fatalf("15000 iterations: done=%v err=%v", done, err)
	}
}

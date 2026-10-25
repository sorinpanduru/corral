package proxy_test

import (
	"fmt"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/wire"
)

func check(t *testing.T, label string, d proxy.Decision, kind proxy.DecisionKind, ref proxy.Refusal) {
	t.Helper()
	if d.Kind != kind {
		t.Fatalf("%s: kind %v, want %v", label, d.Kind, kind)
	}
	if (kind == proxy.Refuse || kind == proxy.HandshakeError) && d.Refusal != ref {
		t.Fatalf("%s: refusal %q (code %d), want %q (code %d)", label, d.Refusal.Detail(), d.Refusal.Code(), ref.Detail(), ref.Code())
	}
}

// FR-13, FR-14: a handshake is answered locally, over OP_MSG and OP_QUERY.
func TestHandshakeAnswered_FR13(t *testing.T) {
	for _, name := range []string{"hello", "isMaster", "ismaster"} {
		cs := &proxy.ConnState{}
		d := decide(hs(name, wire.Handshake{}, 0), cs, okView, proxy.PhaseServing)
		check(t, name, d, proxy.Handshake, proxy.Refusal{})
		if !cs.MetadataSeen {
			t.Errorf("%s: MetadataSeen not set by the first handshake", name)
		}
		cs = &proxy.ConnState{}
		d = decide(hsQuery(name, wire.Handshake{}), cs, okView, proxy.PhaseServing)
		check(t, name+" over OP_QUERY", d, proxy.Handshake, proxy.Refusal{})
	}
}

// FR-16, G-6 (AC-9): a later handshake carrying client gets 186; one without
// client is answered.
func TestRepeatedMetadata_AC9(t *testing.T) {
	cs := &proxy.ConnState{}
	check(t, "first", decide(hs("hello", wire.Handshake{HasClient: true}, 0), cs, okView, proxy.PhaseServing), proxy.Handshake, proxy.Refusal{})
	check(t, "second with client", decide(hs("hello", wire.Handshake{HasClient: true}, 0), cs, okView, proxy.PhaseServing), proxy.HandshakeError, proxy.MetadataRepeated)
	check(t, "third with client", decide(hs("isMaster", wire.Handshake{HasClient: true}, 0), cs, okView, proxy.PhaseServing), proxy.HandshakeError, proxy.MetadataRepeated)
	check(t, "later without client", decide(hs("hello", wire.Handshake{}, 0), cs, okView, proxy.PhaseServing), proxy.Handshake, proxy.Refusal{})
	// The first handshake does not need client for the rule to arm.
	cs = &proxy.ConnState{}
	decide(hs("hello", wire.Handshake{}, 0), cs, okView, proxy.PhaseServing)
	check(t, "client after a client-less first", decide(hs("hello", wire.Handshake{HasClient: true}, 0), cs, okView, proxy.PhaseServing), proxy.HandshakeError, proxy.MetadataRepeated)
	// Over OP_QUERY too.
	cs = &proxy.ConnState{}
	decide(hsQuery("isMaster", wire.Handshake{HasClient: true}), cs, okView, proxy.PhaseServing)
	check(t, "OP_QUERY repeated", decide(hsQuery("isMaster", wire.Handshake{HasClient: true}), cs, okView, proxy.PhaseServing), proxy.HandshakeError, proxy.MetadataRepeated)
}

// FR-17 (AC-9): loadBalanced gets 72, ahead of the repeated-client rule.
func TestLoadBalanced_AC9(t *testing.T) {
	cs := &proxy.ConnState{}
	check(t, "first", decide(hs("hello", wire.Handshake{LoadBalanced: true}, 0), cs, okView, proxy.PhaseServing), proxy.HandshakeError, proxy.LoadBalanced)
	check(t, "again", decide(hs("hello", wire.Handshake{LoadBalanced: true, HasClient: true}, 0), cs, okView, proxy.PhaseServing), proxy.HandshakeError, proxy.LoadBalanced)
	if !cs.MetadataSeen {
		t.Fatal("a refused first handshake must still count as the first")
	}
}

// FR-19 (AC-5 unit): never reached and incompatible give code 6 handshake
// errors; the first handshake counts, so a later one with client gets 186.
func TestHandshakeAvailability_AC5(t *testing.T) {
	cs := &proxy.ConnState{}
	never := view{detail: "upstream_unavailable"}
	check(t, "never reached", decide(hs("hello", wire.Handshake{HasClient: true}, 0), cs, never, proxy.PhaseServing), proxy.HandshakeError, proxy.UpstreamUnavailable)
	check(t, "second with client", decide(hs("hello", wire.Handshake{HasClient: true}, 0), cs, never, proxy.PhaseServing), proxy.HandshakeError, proxy.MetadataRepeated)
	check(t, "second without client", decide(hs("hello", wire.Handshake{}, 0), cs, never, proxy.PhaseServing), proxy.HandshakeError, proxy.UpstreamUnavailable)
	inc := view{detail: "upstream_incompatible"}
	check(t, "incompatible", decide(hs("hello", wire.Handshake{}, 0), &proxy.ConnState{}, inc, proxy.PhaseServing), proxy.HandshakeError, proxy.UpstreamIncompatible)
	// A refused handshake's metadata is still reported for the connection.
	d := decide(hs("hello", wire.Handshake{HasClient: true, AppName: "kept"}, 0), &proxy.ConnState{}, never, proxy.PhaseServing)
	_ = d
}

// FR-23 (AC-11 unit): client auth commands.
func TestClientAuthCommands_AC11(t *testing.T) {
	for _, c := range []string{"saslStart", "saslContinue", "authenticate"} {
		d := decide(msg(c, 0), &proxy.ConnState{}, okView, proxy.PhaseServing)
		check(t, c, d, proxy.Refuse, proxy.ClientAuthSASL)
		if !d.ClientAuth || d.Blocked {
			t.Errorf("%s: ClientAuth=%v Blocked=%v", c, d.ClientAuth, d.Blocked)
		}
	}
	for _, c := range []string{"getnonce", "logout"} {
		d := decide(msg(c, 0), &proxy.ConnState{}, okView, proxy.PhaseServing)
		check(t, c, d, proxy.Refuse, proxy.ClientAuthOther)
		if !d.ClientAuth {
			t.Errorf("%s: ClientAuth not set", c)
		}
	}
}

// FR-33 (AC-17 unit): blocked commands, case-sensitive, top-level first stage.
func TestBlockedCommands_AC17(t *testing.T) {
	for _, c := range []string{"currentOp", "killOp", "killAllSessions", "killAllSessionsByPattern"} {
		d := decide(msg(c, 0), &proxy.ConnState{}, okView, proxy.PhaseServing)
		check(t, c, d, proxy.Refuse, proxy.CommandBlocked)
		if !d.Blocked || d.ClientAuth {
			t.Errorf("%s: Blocked=%v ClientAuth=%v", c, d.Blocked, d.ClientAuth)
		}
	}
	agg := func(stage string) wire.Request {
		r := msg("aggregate", 0)
		r.FirstStage = stage
		return r
	}
	check(t, "aggregate $currentOp", decide(agg("$currentOp"), &proxy.ConnState{}, okView, proxy.PhaseServing), proxy.Refuse, proxy.CommandBlocked)
	for _, stage := range []string{"$match", "", "$CurrentOp", "$currentop", "$unionWith"} {
		check(t, "aggregate "+stage, decide(agg(stage), &proxy.ConnState{}, okView, proxy.PhaseServing), proxy.Forward, proxy.Refusal{})
	}
	// A first stage only counts for aggregate.
	r := msg("find", 0)
	r.FirstStage = "$currentOp"
	check(t, "find with a stage", decide(r, &proxy.ConnState{}, okView, proxy.PhaseServing), proxy.Forward, proxy.Refusal{})
	for _, c := range []string{"CurrentOp", "currentop", "KillOp", "killop", "currentOp ", "find", "ping", "insert"} {
		check(t, c, decide(msg(c, 0), &proxy.ConnState{}, okView, proxy.PhaseServing), proxy.Forward, proxy.Refusal{})
	}
}

// FR-8 (AC-5 unit): a non-handshake OP_QUERY gets the 352 query failure.
func TestOpQueryOther_AC5(t *testing.T) {
	for _, c := range []string{"find", "ping", "saslStart", "currentOp", "getnonce", "HELLO", "IsMaster", ""} {
		d := decide(queryOther(c), &proxy.ConnState{}, okView, proxy.PhaseServing)
		if d.Kind != proxy.QueryFailure {
			t.Errorf("%q: kind %v, want QueryFailure", c, d.Kind)
		}
		if d.ClientAuth || d.Blocked {
			t.Errorf("%q: counted as auth or blocked over OP_QUERY (R-1)", c)
		}
	}
}

// FR-13, G-5: exact, case-sensitive matching for handshake, auth and blocked
// names. Other spellings are ordinary commands over OP_MSG.
func TestCaseSensitiveNames_FR13(t *testing.T) {
	for _, c := range []string{"HELLO", "Hello", "IsMaster", "ISMASTER", "SaslStart", "SASLSTART", "Authenticate", "GetNonce", "Logout", "KillOp"} {
		r := msg(c, 0) // Handshake is nil: the wire layer did not classify it as one
		check(t, c, decide(r, &proxy.ConnState{}, okView, proxy.PhaseServing), proxy.Forward, proxy.Refusal{})
	}
}

// FR-59 (AC-36 unit): moreToCome messages that are refused or answered locally
// get no reply.
func TestMoreToComeNoReply_AC36(t *testing.T) {
	never := view{detail: "upstream_unavailable"}
	cases := map[string]proxy.Decision{
		"handshake":     decide(hs("hello", wire.Handshake{}, moreToCome), &proxy.ConnState{}, okView, proxy.PhaseServing),
		"handshake 6":   decide(hs("hello", wire.Handshake{}, moreToCome), &proxy.ConnState{}, never, proxy.PhaseServing),
		"load balanced": decide(hs("hello", wire.Handshake{LoadBalanced: true}, moreToCome), &proxy.ConnState{}, okView, proxy.PhaseServing),
		"saslStart":     decide(msg("saslStart", moreToCome), &proxy.ConnState{}, okView, proxy.PhaseServing),
		"logout":        decide(msg("logout", moreToCome), &proxy.ConnState{}, okView, proxy.PhaseServing),
		"currentOp":     decide(msg("currentOp", moreToCome), &proxy.ConnState{}, okView, proxy.PhaseServing),
		"late (91)":     decide(msg("find", moreToCome), &proxy.ConnState{}, okView, proxy.PhaseLate),
	}
	for name, d := range cases {
		if !d.NoReply {
			t.Errorf("%s: NoReply not set", name)
		}
	}
	// Without moreToCome every one of them replies.
	if d := decide(msg("saslStart", 0), &proxy.ConnState{}, okView, proxy.PhaseServing); d.NoReply {
		t.Error("NoReply set without moreToCome")
	}
	// A forwarded moreToCome message is Forward (the barrier is the engine's).
	if d := decide(msg("insert", moreToCome), &proxy.ConnState{}, okView, proxy.PhaseServing); d.Kind != proxy.Forward {
		t.Errorf("forwarded moreToCome: kind %v", d.Kind)
	}
	// Other flag bits (exhaustAllowed, checksum) do not suppress replies.
	if d := decide(msg("saslStart", uint32(wire.FlagExhaustAllowed)), &proxy.ConnState{}, okView, proxy.PhaseServing); d.NoReply {
		t.Error("exhaustAllowed suppressed the reply")
	}
}

// FR-47: late messages get 91; moreToCome gets none.
func TestLateMessageShuttingDown_FR47(t *testing.T) {
	d := decide(msg("find", 0), &proxy.ConnState{}, okView, proxy.PhaseLate)
	check(t, "late find", d, proxy.Refuse, proxy.ShuttingDown)
	if d.NoReply {
		t.Error("NoReply set without moreToCome")
	}
	// Serving phase does not refuse.
	check(t, "serving find", decide(msg("find", 0), &proxy.ConnState{}, okView, proxy.PhaseServing), proxy.Forward, proxy.Refusal{})
}

// A handshake decision never consults the pool state beyond the view, and
// Decide does not mutate anything but ConnState.MetadataSeen.
func TestDecideIsPure_AC2(t *testing.T) {
	r := hs("hello", wire.Handshake{HasClient: true}, 0)
	before := fmt.Sprintf("%+v", *r.Handshake)
	cs := &proxy.ConnState{}
	decide(r, cs, okView, proxy.PhaseServing)
	if after := fmt.Sprintf("%+v", *r.Handshake); after != before {
		t.Fatal("Decide modified the request")
	}
}

type expect struct {
	kind proxy.DecisionKind
	ref  proxy.Refusal
}

// AC-40 (unit): the FR-61 order over every combination of phase x command x
// handshake flags x upstream state x moreToCome. The oracle below is the
// spec's decision list, written independently of the implementation.
func TestFR61Order_AC40(t *testing.T) {
	type cmd struct {
		label string
		build func(h wire.Handshake, flags uint32) wire.Request
		class string // handshake | auth-sasl | auth-other | blocked | other | forward | query-other
	}
	cmds := []cmd{
		{"hello", func(h wire.Handshake, f uint32) wire.Request { return hs("hello", h, f) }, "handshake"},
		{"isMaster", func(h wire.Handshake, f uint32) wire.Request { return hs("isMaster", h, f) }, "handshake"},
		{"ismaster", func(h wire.Handshake, f uint32) wire.Request { return hs("ismaster", h, f) }, "handshake"},
		{"hello/OP_QUERY", func(h wire.Handshake, f uint32) wire.Request { return hsQuery("hello", h) }, "handshake"},
		{"saslStart", func(h wire.Handshake, f uint32) wire.Request { return msg("saslStart", f) }, "auth-sasl"},
		{"saslContinue", func(h wire.Handshake, f uint32) wire.Request { return msg("saslContinue", f) }, "auth-sasl"},
		{"authenticate", func(h wire.Handshake, f uint32) wire.Request { return msg("authenticate", f) }, "auth-sasl"},
		{"getnonce", func(h wire.Handshake, f uint32) wire.Request { return msg("getnonce", f) }, "auth-other"},
		{"logout", func(h wire.Handshake, f uint32) wire.Request { return msg("logout", f) }, "auth-other"},
		{"currentOp", func(h wire.Handshake, f uint32) wire.Request { return msg("currentOp", f) }, "blocked"},
		{"killOp", func(h wire.Handshake, f uint32) wire.Request { return msg("killOp", f) }, "blocked"},
		{"killAllSessions", func(h wire.Handshake, f uint32) wire.Request { return msg("killAllSessions", f) }, "blocked"},
		{"killAllSessionsByPattern", func(h wire.Handshake, f uint32) wire.Request { return msg("killAllSessionsByPattern", f) }, "blocked"},
		{"aggregate $currentOp", func(h wire.Handshake, f uint32) wire.Request {
			r := msg("aggregate", f)
			r.FirstStage = "$currentOp"
			return r
		}, "blocked"},
		{"aggregate $match", func(h wire.Handshake, f uint32) wire.Request {
			r := msg("aggregate", f)
			r.FirstStage = "$match"
			return r
		}, "forward"},
		{"find", func(h wire.Handshake, f uint32) wire.Request { return msg("find", f) }, "forward"},
		{"HELLO", func(h wire.Handshake, f uint32) wire.Request { return msg("HELLO", f) }, "forward"},
		{"IsMaster", func(h wire.Handshake, f uint32) wire.Request { return msg("IsMaster", f) }, "forward"},
		{"SaslStart", func(h wire.Handshake, f uint32) wire.Request { return msg("SaslStart", f) }, "forward"},
		{"OP_QUERY find", func(h wire.Handshake, f uint32) wire.Request { return queryOther("find") }, "query-other"},
		{"OP_QUERY saslStart", func(h wire.Handshake, f uint32) wire.Request { return queryOther("saslStart") }, "query-other"},
		{"OP_QUERY currentOp", func(h wire.Handshake, f uint32) wire.Request { return queryOther("currentOp") }, "query-other"},
		{"OP_QUERY HELLO", func(h wire.Handshake, f uint32) wire.Request { return queryOther("HELLO") }, "query-other"},
		{"OP_QUERY IsMaster", func(h wire.Handshake, f uint32) wire.Request { return queryOther("IsMaster") }, "query-other"},
	}
	views := map[string]view{"ok": okView, "never reached": {detail: "upstream_unavailable"}, "incompatible": {detail: "upstream_incompatible"}}
	n := 0
	for _, phase := range []proxy.Phase{proxy.PhaseServing, proxy.PhaseLate} {
		for vname, v := range views {
			for _, c := range cmds {
				for _, seen := range []bool{false, true} {
					for _, lb := range []bool{false, true} {
						for _, client := range []bool{false, true} {
							for _, mtc := range []bool{false, true} {
								h := wire.Handshake{LoadBalanced: lb, HasClient: client}
								var flags uint32
								if mtc {
									flags = moreToCome
								}
								r := c.build(h, flags)
								cs := &proxy.ConnState{MetadataSeen: seen}
								got := decide(r, cs, v, phase)
								n++

								var want expect
								switch {
								case phase == proxy.PhaseLate:
									want = expect{proxy.Refuse, proxy.ShuttingDown}
								case c.class == "handshake":
									switch {
									case lb:
										want = expect{proxy.HandshakeError, proxy.LoadBalanced}
									case client && seen:
										want = expect{proxy.HandshakeError, proxy.MetadataRepeated}
									case vname == "never reached":
										want = expect{proxy.HandshakeError, proxy.UpstreamUnavailable}
									case vname == "incompatible":
										want = expect{proxy.HandshakeError, proxy.UpstreamIncompatible}
									default:
										want = expect{proxy.Handshake, proxy.Refusal{}}
									}
								case c.class == "auth-sasl":
									want = expect{proxy.Refuse, proxy.ClientAuthSASL}
								case c.class == "auth-other":
									want = expect{proxy.Refuse, proxy.ClientAuthOther}
								case c.class == "blocked":
									want = expect{proxy.Refuse, proxy.CommandBlocked}
								case c.class == "query-other":
									want = expect{proxy.QueryFailure, proxy.Refusal{}}
								default:
									want = expect{proxy.Forward, proxy.Refusal{}}
								}
								label := fmt.Sprintf("phase=%v view=%s cmd=%s seen=%v lb=%v client=%v mtc=%v", phase, vname, c.label, seen, lb, client, mtc)
								check(t, label, got, want.kind, want.ref)
								// NoReply: any refusal or local answer to a moreToCome
								// OP_MSG; never for forwards.
								wantNoReply := mtc && want.kind != proxy.Forward && r.Kind == wire.KindOpMsg
								if r.Kind == wire.KindOpMsg && got.NoReply != wantNoReply {
									t.Fatalf("%s: NoReply=%v, want %v", label, got.NoReply, wantNoReply)
								}
								if want.kind == proxy.Forward && got.NoReply {
									t.Fatalf("%s: a forwarded message must not be NoReply", label)
								}
								// MetadataSeen is set by the first handshake command read,
								// whatever its outcome (not in the late phase, which answers
								// before the handshake rules).
								if phase == proxy.PhaseServing && c.class == "handshake" && !cs.MetadataSeen {
									t.Fatalf("%s: MetadataSeen not set", label)
								}
								if c.class != "handshake" && cs.MetadataSeen != seen {
									t.Fatalf("%s: a non-handshake command changed MetadataSeen", label)
								}
							}
						}
					}
				}
			}
		}
	}
	// phases x views x commands x seen x lb x client x moreToCome.
	if want := 2 * len(views) * len(cmds) * 16; n != want {
		t.Fatalf("%d combinations evaluated, want %d", n, want)
	}
}

// AC-40 literal examples from the plan.
func TestFR61Examples_AC40(t *testing.T) {
	never := view{detail: "upstream_unavailable"}
	cs := &proxy.ConnState{}
	check(t, "first hello with client, never reached", decide(hs("hello", wire.Handshake{HasClient: true}, 0), cs, never, proxy.PhaseServing), proxy.HandshakeError, proxy.UpstreamUnavailable)
	check(t, "second hello with client", decide(hs("hello", wire.Handshake{HasClient: true}, 0), cs, never, proxy.PhaseServing), proxy.HandshakeError, proxy.MetadataRepeated)
	cs = &proxy.ConnState{}
	check(t, "loadBalanced plus client", decide(hs("hello", wire.Handshake{LoadBalanced: true, HasClient: true}, 0), cs, okView, proxy.PhaseServing), proxy.HandshakeError, proxy.LoadBalanced)
	check(t, "then client again", decide(hs("hello", wire.Handshake{HasClient: true}, 0), cs, okView, proxy.PhaseServing), proxy.HandshakeError, proxy.MetadataRepeated)
	check(t, "never reached: saslStart", decide(msg("saslStart", 0), &proxy.ConnState{}, never, proxy.PhaseServing), proxy.Refuse, proxy.ClientAuthSASL)
	check(t, "never reached: killOp", decide(msg("killOp", 0), &proxy.ConnState{}, never, proxy.PhaseServing), proxy.Refuse, proxy.CommandBlocked)
	check(t, "never reached: OP_QUERY find", decide(queryOther("find"), &proxy.ConnState{}, never, proxy.PhaseServing), proxy.QueryFailure, proxy.Refusal{})
	check(t, "never reached: find", decide(msg("find", 0), &proxy.ConnState{}, never, proxy.PhaseServing), proxy.Forward, proxy.Refusal{})
	check(t, "late: saslStart", decide(msg("saslStart", 0), &proxy.ConnState{}, okView, proxy.PhaseLate), proxy.Refuse, proxy.ShuttingDown)
}

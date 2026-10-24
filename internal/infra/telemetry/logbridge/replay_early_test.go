package logbridge_test

import (
	"context"
	"log"
	"log/slog"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/earlyslog"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
)

// seedEarly logs through the process default handler as a package initialiser
// would before Install. The default at test start is the earlyslog counting
// handler. Any stale counts are drained first.
func seedEarly(t *testing.T, m string, levels ...slog.Level) {
	t.Helper()
	earlyslog.Drain()
	for _, l := range levels {
		//constmsg:allow simulated third-party init-time caller
		slog.Log(context.Background(), l, m, "k"+m, m)
	}
}

var allLevels = []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}

// FR-28, Amendment 1: one thirdparty record per non-zero level, fixed message,
// original level, no original text, even when many records were seen.
func TestInstallReplaysEarlyOncePerLevel(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	seedEarly(t, m, slog.LevelDebug, slog.LevelWarn, slog.LevelWarn, slog.LevelWarn, slog.LevelError)

	logbridge.Install(e.f, e.lv, e.clk.Now)

	rs := e.records()
	if len(rs) != 3 {
		t.Fatalf("records = %d, want 3 (debug, warn, error): %+v", len(rs), rs)
	}
	for i, want := range []slog.Level{slog.LevelDebug, slog.LevelWarn, slog.LevelError} {
		expect(t, rs[i], want, "thirdparty", 0)
		if rs[i].Msg != logbridge.MsgThirdPartySlogDflt {
			t.Errorf("record %d message = %q, want %q", i, rs[i].Msg, logbridge.MsgThirdPartySlogDflt)
		}
	}
	e.noMarker(m)
}

// FR-28: all four levels are replayed, each once.
func TestInstallReplaysEarlyAllLevels(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	seedEarly(t, m, allLevels...)

	logbridge.Install(e.f, e.lv, e.clk.Now)

	rs := e.records()
	if len(rs) != 4 {
		t.Fatalf("records = %d, want 4", len(rs))
	}
	for i, want := range allLevels {
		expect(t, rs[i], want, "thirdparty", 0)
	}
	e.noMarker(m)
}

// FR-28: zero counts produce no records.
func TestInstallNoEarlyRecordsEmitsNothing(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelDebug)
	earlyslog.Drain()

	logbridge.Install(e.f, e.lv, e.clk.Now)

	if n := e.count(); n != 0 {
		t.Fatalf("records = %d, want 0: %+v", n, e.records())
	}
}

// FR-28: the replay honours the Install level (debug replay filtered at info).
func TestInstallReplayHonoursLevel(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelInfo)
	m := marker(t)
	seedEarly(t, m, slog.LevelDebug, slog.LevelInfo)

	logbridge.Install(e.f, e.lv, e.clk.Now)

	r := e.only()
	expect(t, r, slog.LevelInfo, "thirdparty", 0)
	e.noMarker(m)
}

// FR-28: counts are drained by Install, so a second Install replays nothing.
func TestInstallDrainsEarlyCounts(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	seedEarly(t, m, slog.LevelWarn, slog.LevelError)

	logbridge.Install(e.f, e.lv, e.clk.Now)
	if n := e.count(); n != 2 {
		t.Fatalf("first Install records = %d, want 2", n)
	}
	if got := earlyslog.Drain(); got != (earlyslog.Counts{}) {
		t.Errorf("counts after Install = %+v, want zero", got)
	}

	logbridge.Install(e.f, e.lv, e.clk.Now)
	if n := e.count(); n != 2 {
		t.Fatalf("records after second Install = %d, want still 2", n)
	}
}

// Amendment 1: replayed records go through the per-level rate limiter of the
// slog.Default bridge. A record logged right after the replay is suppressed
// and reported in suppressed_count once the window passes. Other levels are
// unaffected.
func TestInstallReplayRespectsRateLimiter(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	seedEarly(t, m, slog.LevelWarn)

	logbridge.Install(e.f, e.lv, e.clk.Now)
	if n := e.count(); n != 1 {
		t.Fatalf("records after Install = %d, want 1", n)
	}

	//constmsg:allow simulated third-party caller
	slog.Warn(m) // same source+level inside the window: suppressed
	if n := e.count(); n != 1 {
		t.Fatalf("warn within window emitted: records = %d, want 1", n)
	}
	//constmsg:allow simulated third-party caller
	slog.Error(m) // different level: own limiter
	if n := e.count(); n != 2 {
		t.Fatalf("error record missing: records = %d, want 2", n)
	}

	e.clk.Advance(win)
	//constmsg:allow simulated third-party caller
	slog.Warn(m)
	rs := e.records()
	if len(rs) != 3 {
		t.Fatalf("records = %d, want 3", len(rs))
	}
	expect(t, rs[2], slog.LevelWarn, "thirdparty", 1)
	e.noMarker(m)
}

// FR-28, AC-26: an early std log.Print is replayed as one info record with the
// std log fixed message (not the slog.Default one), component=thirdparty, and
// none of the original text.
func TestInstallReplaysEarlyStdLog(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	earlyslog.Drain()
	log.Print(m)

	logbridge.Install(e.f, e.lv, e.clk.Now)

	r := e.only()
	expect(t, r, slog.LevelInfo, "thirdparty", 0)
	if r.Msg != logbridge.MsgThirdPartyStdLog {
		t.Errorf("message = %q, want %q", r.Msg, logbridge.MsgThirdPartyStdLog)
	}
	if r.Msg == logbridge.MsgThirdPartySlogDflt {
		t.Error("std log record carries the slog.Default message")
	}
	e.noMarker(m)
}

// AC-26: limiters are separate per source. An early std log burst does not
// suppress an early slog.Default info record, and the reverse.
func TestInstallReplayStdLogAndSlogLimitersIndependent(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	seedEarly(t, m, slog.LevelInfo, slog.LevelInfo)
	for i := 0; i < 3; i++ {
		log.Print(m)
	}

	logbridge.Install(e.f, e.lv, e.clk.Now)

	rs := e.records()
	if len(rs) != 2 {
		t.Fatalf("records = %d, want 2 (one per source): %+v", len(rs), rs)
	}
	msgs := map[string]bool{rs[0].Msg: true, rs[1].Msg: true}
	if !msgs[logbridge.MsgThirdPartySlogDflt] || !msgs[logbridge.MsgThirdPartyStdLog] {
		t.Errorf("messages = %q, %q; want one of each source", rs[0].Msg, rs[1].Msg)
	}

	e.clk.Advance(win)
	//constmsg:allow simulated third-party caller
	slog.Info(m)
	log.Print(m)
	rs = e.records()
	if len(rs) != 4 {
		t.Fatalf("records = %d, want 4", len(rs))
	}
	// slog.Default: 2 early, 1 emitted, 1 suppressed. std log: 3 early, 2 suppressed.
	for _, r := range rs[2:] {
		want := int64(1)
		if r.Msg == logbridge.MsgThirdPartyStdLog {
			want = 2
		}
		if got := r.suppressed(t); got != want {
			t.Errorf("%q suppressed_count = %d, want %d", r.Msg, got, want)
		}
	}
	e.noMarker(m)
}

// AC-26, FR-33: n early warns produce one record; after the window the next
// record reports suppressed_count n-1 (as for 10 slog.Default warns: 9).
func TestInstallReplayManyEarlyWarnsSuppressedCount(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	const n = 10
	levels := make([]slog.Level, n)
	for i := range levels {
		levels[i] = slog.LevelWarn
	}
	seedEarly(t, m, levels...)

	logbridge.Install(e.f, e.lv, e.clk.Now)
	expect(t, e.only(), slog.LevelWarn, "thirdparty", 0)

	e.clk.Advance(win)
	//constmsg:allow simulated third-party caller
	slog.Warn(m)
	rs := e.records()
	if len(rs) != 2 {
		t.Fatalf("records = %d, want 2", len(rs))
	}
	expect(t, rs[1], slog.LevelWarn, "thirdparty", n-1)
	e.noMarker(m)
}

// FR-28: a *slog.Logger captured during init (before Install) and used after
// Install produces a fixed-message thirdparty record, no marker text.
func TestInstallForwardsLoggerCapturedBeforeInstall(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	earlyslog.Drain()
	captured := slog.Default().With("k"+m, m).WithGroup("g" + m)

	logbridge.Install(e.f, e.lv, e.clk.Now)
	if n := e.count(); n != 0 {
		t.Fatalf("records after Install = %d, want 0", n)
	}
	//constmsg:allow simulated third-party caller
	captured.Warn(m, "a"+m, m)

	r := e.only()
	expect(t, r, slog.LevelWarn, "thirdparty", 0)
	if r.Msg != logbridge.MsgThirdPartySlogDflt {
		t.Errorf("message = %q, want %q", r.Msg, logbridge.MsgThirdPartySlogDflt)
	}
	e.noMarker(m)
}

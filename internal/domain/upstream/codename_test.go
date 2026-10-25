package upstream_test

import (
	"context"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/upstream"
)

// AC-62 (FR-35, FR-9, FR-29, FR-49; Amendment 6), unit: the code name in the
// "target marked not primary" event comes from the fixed table, whatever the
// reply carries, for every form (ok:0, ok:1 writeConcernError, barrier ping).

var notPrimaryNames = map[int32]string{
	10107: "NotWritablePrimary",
	13435: "NotPrimaryNoSecondaryOk",
	13436: "NotPrimaryOrSecondary",
	11602: "InterruptedDueToReplStateChange",
	11600: "InterruptedAtShutdown",
	189:   "PrimarySteppedDown",
	91:    "ShutdownInProgress",
}

func TestNotPrimaryCodeNameFromTable_AC62(t *testing.T) {
	ctx := context.Background()
	forms := map[string]struct {
		src upstream.ReplySource
		f   func(code int32, name string) upstream.ReplyCodeInfo
	}{
		"ok:0 with a different codeName": {upstream.Forwarded, func(c int32, n string) upstream.ReplyCodeInfo {
			return upstream.ReplyCodeInfo{OKZero: true, HasTop: true, Top: c, TopName: n}
		}},
		"ok:0 with no codeName": {upstream.Forwarded, func(c int32, _ string) upstream.ReplyCodeInfo {
			return upstream.ReplyCodeInfo{OKZero: true, HasTop: true, Top: c}
		}},
		"ok:1 writeConcernError (no name is ever read)": {upstream.Forwarded, func(c int32, _ string) upstream.ReplyCodeInfo {
			return upstream.ReplyCodeInfo{HasWCE: true, WCE: c}
		}},
		"barrier ping with a different codeName": {upstream.Barrier, func(c int32, n string) upstream.ReplyCodeInfo {
			return upstream.ReplyCodeInfo{OKZero: true, HasTop: true, Top: c, TopName: n}
		}},
	}
	for code, want := range notPrimaryNames {
		for name, form := range forms {
			t.Run(name+"/"+want, func(t *testing.T) {
				h := rsHarness(t)
				if !h.m.ReplyCodes(ctx, A, form.src, form.f(code, "SynthMarkerName")) {
					t.Fatal("not marked")
				}
				got := h.rec.snapshotMarked()
				if len(got) != 1 || got[0].Code != code || got[0].CodeName != want {
					t.Fatalf("marked %+v, want code %d name %q", got, code, want)
				}
			})
		}
	}
}

// 10107 with the legacy name NotMaster is reported as NotWritablePrimary.
func TestLegacyNotMasterNameMapped_AC62(t *testing.T) {
	h := rsHarness(t)
	h.m.ReplyCodes(context.Background(), A, upstream.Forwarded, upstream.ReplyCodeInfo{OKZero: true, HasTop: true, Top: 10107, TopName: "NotMaster"})
	got := h.rec.snapshotMarked()
	if len(got) != 1 || got[0].CodeName != "NotWritablePrimary" {
		t.Fatalf("%+v", got)
	}
}

// The table maps exactly the seven codes: no other code marks a host, in any form.
func TestOnlyTheSevenCodesMark_AC62(t *testing.T) {
	h := rsHarness(t)
	ctx := context.Background()
	for code := int32(-5); code <= 20000; code++ {
		if _, ok := notPrimaryNames[code]; ok {
			continue
		}
		for _, f := range []upstream.ReplyCodeInfo{
			{OKZero: true, HasTop: true, Top: code, TopName: "NotWritablePrimary"},
			{HasWCE: true, WCE: code},
		} {
			if h.m.ReplyCodes(ctx, A, upstream.Forwarded, f) {
				t.Fatalf("code %d marked the host", code)
			}
		}
	}
	if got := h.rec.snapshotMarked(); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

// Single-mongos mode: the two codes that mark a mongos use the table name too.
func TestMongosMarkNamesFromTable_AC62(t *testing.T) {
	for code, want := range map[int32]string{91: "ShutdownInProgress", 11600: "InterruptedAtShutdown"} {
		h := mongosHarness(t)
		if !h.m.ReplyCodes(context.Background(), A, upstream.Forwarded, upstream.ReplyCodeInfo{OKZero: true, HasTop: true, Top: code, TopName: "SynthMarkerName"}) {
			t.Fatalf("%d did not mark", code)
		}
		if got := h.rec.snapshotMarked(); len(got) != 1 || got[0].CodeName != want {
			t.Fatalf("%d: %+v, want name %q", code, got, want)
		}
	}
}

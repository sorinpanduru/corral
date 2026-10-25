package proxy_test

import (
	"math"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
)

func TestRequestBufferLimit_FR38(t *testing.T) {
	if proxy.RequestBufferLimit != 65536 {
		t.Fatalf("RequestBufferLimit = %d", proxy.RequestBufferLimit)
	}
}

// FR-3, AC-2 (unit): IDs are positive, increasing, and wrap to 1 after
// 2,147,483,647.
func TestConnIDsIncreaseAndWrap_FR3(t *testing.T) {
	ids := proxy.NewConnIDsAt(math.MaxInt32 - 1)
	var got []int32
	for i := 0; i < 4; i++ {
		id, ok := ids.Acquire()
		if !ok {
			t.Fatal("Acquire failed")
		}
		got = append(got, id)
	}
	want := []int32{math.MaxInt32 - 1, math.MaxInt32, 1, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

// The default allocator starts at 1 and increases.
func TestConnIDsStartAtOne_FR3(t *testing.T) {
	ids := proxy.NewConnIDs()
	for want := int32(1); want <= 5; want++ {
		if id, ok := ids.Acquire(); !ok || id != want {
			t.Fatalf("Acquire = %d (%v), want %d", id, ok, want)
		}
	}
}

// AC-2 (unit): no duplicates among 10,000 open IDs, across the wrap, and after
// releasing half of them no new ID collides with one still open.
func TestConnIDsNoDuplicatesAmongOpen_AC2(t *testing.T) {
	ids := proxy.NewConnIDsAt(math.MaxInt32 - 5000)
	open := map[int32]bool{}
	for i := 0; i < 10000; i++ {
		id, ok := ids.Acquire()
		if !ok {
			t.Fatalf("Acquire failed at %d", i)
		}
		if id <= 0 {
			t.Fatalf("non-positive ID %d", id)
		}
		if open[id] {
			t.Fatalf("ID %d handed out twice while open", id)
		}
		open[id] = true
	}
	n := 0
	for id := range open {
		if n%2 == 0 {
			ids.Release(id)
			delete(open, id)
		}
		n++
	}
	for i := 0; i < 5000; i++ {
		id, ok := ids.Acquire()
		if !ok {
			t.Fatalf("Acquire failed at %d", i)
		}
		if open[id] {
			t.Fatalf("ID %d reused while open", id)
		}
		open[id] = true
	}
}

// Releasing an ID that is not open is harmless.
func TestConnIDsReleaseUnknown(t *testing.T) {
	ids := proxy.NewConnIDs()
	ids.Release(12345)
	ids.Release(-1)
	if id, ok := ids.Acquire(); !ok || id != 1 {
		t.Fatalf("Acquire after bogus releases = %d (%v)", id, ok)
	}
}

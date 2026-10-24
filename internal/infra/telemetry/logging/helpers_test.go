package logging_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
)

// fanout sends every record to both handlers, so the suite's real output
// handler is exercised unchanged while logcheck validates the record.
type fanout struct{ out, check slog.Handler }

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool { return f.out.Enabled(ctx, l) }
func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	if err := f.check.Handle(ctx, r.Clone()); err != nil {
		return err
	}
	return f.out.Handle(ctx, r)
}
func (f fanout) WithAttrs(as []slog.Attr) slog.Handler {
	return fanout{out: f.out.WithAttrs(as), check: f.check.WithAttrs(as)}
}
func (f fanout) WithGroup(n string) slog.Handler {
	return fanout{out: f.out.WithGroup(n), check: f.check.WithGroup(n)}
}

// checked wires the AC-5 logcheck handler beside out. Set behindCorrelation
// when the result is wrapped by logging.WithTraceCorrelation.
func checked(t testing.TB, out slog.Handler, behindCorrelation bool) slog.Handler {
	t.Helper()
	var opts []logcheck.Option
	if behindCorrelation {
		opts = append(opts, logcheck.BehindCorrelation())
	}
	return fanout{out: out, check: logcheck.New(t, opts...)}
}

// syncBuffer is a goroutine-safe buffer used as a log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Lines() []string {
	s := strings.TrimRight(b.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// jsonLines parses every output line as a JSON object, failing on any line
// that is not valid JSON.
func jsonLines(t *testing.T, b *syncBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range b.Lines() {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line is not valid JSON: %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

// marker returns a random synthetic marker used to prove rejected input is
// never echoed in an error message (FR-27).
func marker(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(b)
}

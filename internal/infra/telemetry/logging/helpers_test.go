package logging_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

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

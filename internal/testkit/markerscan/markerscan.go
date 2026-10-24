// Package markerscan is the AC-33 leak scanner. Tests plant synthetic markers
// (values that must never be emitted, FR-24), feed the scanner everything the
// process emitted, and expect no findings.
//
// Each marker is searched in raw form, hex (lower and upper case) and base64
// (standard and URL-safe alphabets) at all three alignments, with the unstable
// edge characters dropped from each base64 pattern.
//
// Allowed and banned markers must not share any 12-byte substring, because
// raw-partial matches any 12-byte window of the joined stream.
//
// All data handled here must be synthetic. Findings never contain the matched
// data, only the marker as the test supplied it, the source and the encoding
// name.
package markerscan

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// Source identifies where scanned data came from.
type Source int

// The scanned sources.
const (
	SourceStdout Source = iota + 1
	SourceStderr
	SourceMetrics
	SourceReadyz
	SourceSpans
)

// AllSources lists every source.
func AllSources() []Source {
	return []Source{SourceStdout, SourceStderr, SourceMetrics, SourceReadyz, SourceSpans}
}

func (s Source) String() string {
	switch s {
	case SourceStdout:
		return "stdout"
	case SourceStderr:
		return "stderr"
	case SourceMetrics:
		return "metrics"
	case SourceReadyz:
		return "readyz"
	case SourceSpans:
		return "spans"
	}
	return fmt.Sprintf("source(%d)", int(s))
}

// DefaultMaxSourceBytes bounds the data retained per source. Data beyond it is
// discarded and reported by Truncated, so a huge input cannot exhaust memory.
const DefaultMaxSourceBytes = 64 << 20

var maxSourceBytes atomic.Int64

func init() { maxSourceBytes.Store(DefaultMaxSourceBytes) }

func maxBytes() int { return int(maxSourceBytes.Load()) }

// SetMaxSourceBytesForTest lowers (or raises) the per-source cap, which also
// bounds captured streams and the rendered collector text. It returns a func
// that restores the previous value. The cap is process-wide.
func SetMaxSourceBytesForTest(n int) (restore func()) {
	old := maxSourceBytes.Swap(int64(n))
	return func() { maxSourceBytes.Store(old) }
}

// Marker constraints (AC-33): long enough to avoid accidental matches and
// restricted to a safe alphabet so every encoding is well defined.
const (
	minMarkerLen = 16
	partialLen   = 12
)

func validMarker(m string) error {
	if len(m) < minMarkerLen {
		return fmt.Errorf("markerscan: marker shorter than %d bytes", minMarkerLen)
	}
	for i := 0; i < len(m); i++ {
		c := m[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return fmt.Errorf("markerscan: marker byte %d outside [A-Za-z0-9_-]", i)
		}
	}
	return nil
}

// Pattern is one searchable encoding of a marker.
type Pattern struct {
	Name  string // raw, raw-partial, raw-casefold, hex-lower, hex-upper, base64-std/N, base64-url/N
	Bytes []byte
	Fold  bool // match ASCII case-insensitively (Bytes is lower case)
}

// Finding reports that a marker was found in a source.
type Finding struct {
	Marker   string // the marker as given to AddMarker
	Source   Source
	Encoding string // name of the matching pattern
}

func (f Finding) String() string {
	return fmt.Sprintf("marker %q found in %s as %s", f.Marker, f.Source, f.Encoding)
}

// Patterns returns every search pattern for marker: raw, every 12-byte window
// (raw-partial), case-folded raw, hex lower, hex upper and base64 (standard,
// URL-safe) at prefix lengths 0, 1 and 2. The count depends only on the marker
// length. A pattern may be empty for a marker too
// short to leave stable base64 characters; the scanner skips empty patterns.
func Patterns(marker string) []Pattern {
	raw := []byte(marker)
	hx := hex.EncodeToString(raw)
	ps := []Pattern{
		{Name: "raw", Bytes: raw},
		{Name: "raw-casefold", Bytes: asciiLower(raw), Fold: true},
		{Name: "hex-lower", Bytes: []byte(strings.ToLower(hx))},
		{Name: "hex-upper", Bytes: []byte(strings.ToUpper(hx))},
	}
	for i := 0; i+partialLen <= len(raw); i++ {
		ps = append(ps, Pattern{Name: "raw-partial", Bytes: raw[i : i+partialLen]})
	}
	encs := []struct {
		name string
		enc  *base64.Encoding
	}{
		{"base64-std", base64.RawStdEncoding},
		{"base64-url", base64.RawURLEncoding},
	}
	for _, e := range encs {
		for k := 0; k <= 2; k++ {
			ps = append(ps, Pattern{Name: fmt.Sprintf("%s/%d", e.name, k), Bytes: b64Stable(e.enc, raw, k)})
		}
	}
	return ps
}

// b64Stable encodes marker preceded by k arbitrary bytes and keeps only the
// characters fully determined by the marker: leading characters that mix in
// prefix bits and a trailing partial character (which depends on the byte
// after the marker) are dropped.
func b64Stable(enc *base64.Encoding, marker []byte, k int) []byte {
	buf := make([]byte, k+len(marker))
	copy(buf[k:], marker)
	s := enc.EncodeToString(buf) // unpadded
	lead := (k*8 + 5) / 6        // chars touched by prefix bits: 0, 2, 3
	trail := 0
	if (k+len(marker))%3 != 0 {
		trail = 1
	}
	if lead+trail >= len(s) {
		return nil
	}
	return []byte(s[lead : len(s)-trail])
}

func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

type markerEntry struct {
	marker   string
	sources  map[Source]bool // nil means all
	patterns []Pattern
}

// Scanner accumulates markers and data. It is safe for concurrent use.
type Scanner struct {
	mu         sync.Mutex
	markers    []markerEntry
	data       map[Source][]byte
	truncated  map[Source]bool
	collectors []*otlpcollector.Collector
}

// New returns an empty scanner.
func New() *Scanner {
	return &Scanner{data: map[Source][]byte{}, truncated: map[Source]bool{}}
}

// AddMarker registers a marker. With no sources it is scanned in all of them.
// A marker must be at least 16 bytes and use only [A-Za-z0-9_-]; otherwise it
// calls tb.Fatalf with the reason (never the marker itself).
func (s *Scanner) AddMarker(tb testing.TB, m string, sources ...Source) {
	tb.Helper()
	if err := validMarker(m); err != nil {
		tb.Fatalf("markerscan: invalid marker: %v", err)
		return
	}
	e := markerEntry{marker: m, patterns: Patterns(m)}
	if len(sources) > 0 {
		e.sources = map[Source]bool{}
		for _, src := range sources {
			e.sources[src] = true
		}
	}
	s.mu.Lock()
	s.markers = append(s.markers, e)
	s.mu.Unlock()
}

// Add appends data from a source. Successive calls for one source form a
// stream (a marker split across calls is still found). Order relative to
// AddMarker does not matter. Data beyond the per-source cap is dropped and
// reported by Truncated.
func (s *Scanner) Add(src Source, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	room := maxBytes() - len(s.data[src])
	if room < 0 {
		room = 0
	}
	if len(data) > room {
		data = data[:room]
		s.truncated[src] = true
	}
	if len(data) > 0 {
		s.data[src] = append(s.data[src], data...)
	}
}

// AddCollector registers a collector whose recorded spans and resources are
// scanned as SourceSpans at each Findings call. Request headers are never
// scanned.
func (s *Scanner) AddCollector(c *otlpcollector.Collector) {
	s.mu.Lock()
	s.collectors = append(s.collectors, c)
	s.mu.Unlock()
}

// view is a consistent snapshot of the scannable data.
type view struct {
	markers   []markerEntry
	data      map[Source][]byte
	truncated map[Source]bool
}

func (s *Scanner) snapshot() view {
	s.mu.Lock()
	v := view{
		markers:   append([]markerEntry(nil), s.markers...),
		data:      make(map[Source][]byte, len(s.data)+1),
		truncated: make(map[Source]bool, len(s.truncated)+1),
	}
	for k, d := range s.data {
		v.data[k] = d[:len(d):len(d)] // append-only buffer: this view stays valid
	}
	for k, t := range s.truncated {
		v.truncated[k] = t
	}
	collectors := append([]*otlpcollector.Collector(nil), s.collectors...)
	s.mu.Unlock()

	if len(collectors) > 0 {
		base := v.data[SourceSpans]
		var b bytes.Buffer
		b.Write(base)
		room := maxBytes() - len(base)
		for _, c := range collectors {
			if room <= 0 {
				v.truncated[SourceSpans] = true
				break
			}
			text, trunc := renderCollector(c, room)
			b.WriteByte('\n')
			b.WriteString(text)
			room -= len(text) + 1
			if trunc {
				v.truncated[SourceSpans] = true
			}
		}
		v.data[SourceSpans] = b.Bytes()
	}
	return v
}

// Truncated reports whether src exceeded the per-source cap, so that its scan
// is incomplete.
func (s *Scanner) Truncated(src Source) bool { return s.snapshot().truncated[src] }

// AssertClean fails tb if there is any finding or any truncated source, or if
// no marker was registered (a scan without markers proves nothing).
func (s *Scanner) AssertClean(tb testing.TB) {
	tb.Helper()
	v := s.snapshot()
	if len(v.markers) == 0 {
		tb.Errorf("markerscan: no markers registered; nothing was scanned")
	}
	for _, f := range v.findings() {
		tb.Errorf("markerscan: %s", f)
	}
	for _, src := range AllSources() {
		if v.truncated[src] {
			tb.Errorf("markerscan: %s data exceeded the %d byte cap; scan incomplete", src, maxBytes())
		}
	}
}

// Findings scans everything added so far, in deterministic order.
func (s *Scanner) Findings() []Finding { return s.snapshot().findings() }

func (v view) findings() []Finding {
	var out []Finding
	for _, m := range v.markers {
		for _, src := range AllSources() {
			if m.sources != nil && !m.sources[src] {
				continue
			}
			d := v.data[src]
			if len(d) == 0 {
				continue
			}
			seen := map[string]bool{}
			for _, p := range m.patterns {
				if len(p.Bytes) == 0 || seen[p.Name] {
					continue
				}
				if contains(d, p) {
					seen[p.Name] = true
					out = append(out, Finding{Marker: m.marker, Source: src, Encoding: p.Name})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Marker < out[j].Marker
	})
	return out
}

const foldChunk = 1 << 20

func contains(d []byte, p Pattern) bool {
	if !p.Fold {
		return bytes.Contains(d, p.Bytes)
	}
	// ASCII case-insensitive search in bounded chunks (no full-size copy).
	buf := make([]byte, 0, foldChunk+len(p.Bytes))
	for off := 0; off < len(d); off += foldChunk {
		end := off + foldChunk + len(p.Bytes) - 1
		if end > len(d) {
			end = len(d)
		}
		buf = buf[:0]
		for _, c := range d[off:end] {
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			buf = append(buf, c)
		}
		if bytes.Contains(buf, p.Bytes) {
			return true
		}
	}
	return false
}

// RenderCollector renders the scannable text of a collector's payload, bounded
// by the per-source cap. See renderCollector.
func RenderCollector(c *otlpcollector.Collector) string {
	t, _ := renderCollector(c, maxBytes())
	return t
}

// renderCollector renders span names, attribute keys and values, event names
// and attributes, status messages and resource attributes, one item per line,
// stopping at limit bytes (truncated reports that). Request headers are
// excluded. Bytes attributes are already padded standard base64 (T13
// rendering) and are found by the base64 patterns. Values that parse as JSON
// arrays or objects (the T13 rendering of arrays and kvlists) additionally
// contribute their decoded leaves (object keys and string values), so JSON
// escaping cannot hide a marker.
func renderCollector(c *otlpcollector.Collector, limit int) (text string, truncated bool) {
	var b strings.Builder
	full := false
	emit := func(v string) {
		if full {
			return
		}
		if b.Len()+len(v)+1 > limit {
			room := limit - b.Len()
			if room > 0 {
				b.WriteString(v[:min(room, len(v))])
			}
			full, truncated = true, true
			return
		}
		b.WriteString(v)
		b.WriteByte('\n')
	}
	line := func(v string) {
		emit(v)
		if len(v) > 0 && (v[0] == '[' || v[0] == '{') && !full {
			var x any
			if json.Unmarshal([]byte(v), &x) == nil {
				walkLeaves(x, emit)
			}
		}
	}
	attrs := func(m map[string]string) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			line(k)
			line(m[k])
		}
	}
	for _, r := range c.Resources() {
		attrs(r)
	}
	for _, sp := range c.Spans() {
		line(sp.Name)
		attrs(sp.Attributes)
		for _, e := range sp.Events {
			line(e.Name)
			attrs(e.Attributes)
		}
		line(sp.StatusMessage)
	}
	return b.String(), truncated
}

func walkLeaves(x any, emit func(string)) {
	switch v := x.(type) {
	case string:
		emit(v)
	case []any:
		for _, e := range v {
			walkLeaves(e, emit)
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			emit(k)
			walkLeaves(v[k], emit)
		}
	}
}

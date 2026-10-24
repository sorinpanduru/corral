// Package markerscan is the AC-33 leak scanner. Tests plant synthetic markers
// (values that must never be emitted, FR-24), feed the scanner everything the
// process emitted, and expect no findings.
//
// Each marker is searched in raw form, hex (lower and upper case) and base64
// (standard and URL-safe alphabets) at all three alignments, with the unstable
// edge characters dropped from each base64 pattern.
//
// All data handled here must be synthetic. Findings never contain the matched
// data, only the marker as the test supplied it, the source and the encoding
// name.
package markerscan

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"

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

// MaxSourceBytes bounds the data retained per source. Data beyond it is
// discarded and reported by Truncated, so a huge input cannot exhaust memory.
const MaxSourceBytes = 64 << 20

// Pattern is one searchable encoding of a marker.
type Pattern struct {
	Name  string // e.g. "raw", "hex-lower", "base64-std/1"
	Bytes []byte
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

// Patterns returns every search pattern for marker: raw, hex lower, hex upper
// and base64 (standard, URL-safe) at prefix lengths 0, 1 and 2. The count
// depends only on the marker length. A pattern may be empty for a marker too
// short to leave stable base64 characters; the scanner skips empty patterns.
func Patterns(marker string) []Pattern {
	raw := []byte(marker)
	hx := hex.EncodeToString(raw)
	ps := []Pattern{
		{"raw", raw},
		{"hex-lower", []byte(strings.ToLower(hx))},
		{"hex-upper", []byte(strings.ToUpper(hx))},
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
			ps = append(ps, Pattern{fmt.Sprintf("%s/%d", e.name, k), b64Stable(e.enc, raw, k)})
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
// Empty markers are ignored.
func (s *Scanner) AddMarker(m string, sources ...Source) {
	if m == "" {
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
// AddMarker does not matter.
func (s *Scanner) Add(src Source, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	room := MaxSourceBytes - len(s.data[src])
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

// Truncated lists sources whose data exceeded MaxSourceBytes; their scan is
// incomplete.
func (s *Scanner) Truncated() []Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Source
	for _, src := range AllSources() {
		if s.truncated[src] {
			out = append(out, src)
		}
	}
	return out
}

// Findings scans everything added so far, in deterministic order.
func (s *Scanner) Findings() []Finding {
	s.mu.Lock()
	markers := append([]markerEntry(nil), s.markers...)
	collectors := append([]*otlpcollector.Collector(nil), s.collectors...)
	data := make(map[Source][]byte, len(s.data)+1)
	for k, v := range s.data {
		data[k] = v[:len(v):len(v)] // append-only buffer: this view stays valid
	}
	s.mu.Unlock()

	if len(collectors) > 0 {
		var b bytes.Buffer
		b.Write(data[SourceSpans])
		for _, c := range collectors {
			b.WriteByte('\n')
			b.WriteString(RenderCollector(c))
		}
		data[SourceSpans] = b.Bytes()
	}

	var out []Finding
	for _, m := range markers {
		for _, src := range AllSources() {
			if m.sources != nil && !m.sources[src] {
				continue
			}
			d := data[src]
			if len(d) == 0 {
				continue
			}
			for _, p := range m.patterns {
				if len(p.Bytes) > 0 && bytes.Contains(d, p.Bytes) {
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

// jsonUnescape undoes encoding/json's HTML escaping, which the collector's
// compact JSON rendering of arrays and kvlists applies.
var jsonUnescape = strings.NewReplacer(`<`, "<", `>`, ">", `&`, "&")

// RenderCollector renders the scannable text of a collector's payload: span
// names, attribute keys and values, event names and attributes, status
// messages and resource attributes, one item per line. Request headers are
// excluded. Bytes attributes are already padded standard base64 and
// arrays/kvlists sorted compact JSON (T13 rendering).
func RenderCollector(c *otlpcollector.Collector) string {
	var b strings.Builder
	line := func(v string) {
		b.WriteString(v)
		b.WriteByte('\n')
		if u := jsonUnescape.Replace(v); u != v {
			b.WriteString(u)
			b.WriteByte('\n')
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
	return b.String()
}

package markerscan_test

// Self-test for the marker-scan harness (plan task T14, SPEC-001 AC-33).
// All data is synthetic; markers are random per test.
//
// The test builds its own encodings of the marker (independent of the
// harness) and plants them in every scanned source, then expects detection.
//
// AC-33 mapping:
//   TestPlantedEncodingsDetectedInEverySource  raw, hex lower/upper, base64
//       std and URL-safe (padded and unpadded), marker at byte offsets 0, 1, 2
//       (and varied trailing bytes, so unstable edges are dropped) in each of
//       stdout, stderr, metrics, readyz, spans (via Add).
//   TestSpanPayloadFieldsScanned               collector payload: span name,
//       attribute key and value, event name and attributes, status description,
//       resource attributes, bytes attribute; each at offsets 0..2.
//   TestTransportHeadersNotScanned             collector request headers excluded.
//   TestSourceSubsetSelection                  per-marker source subset; none = all.
//   TestNegativeControl                        no marker, near-miss and other markers.
//   TestAddOrderIndependent                    AddMarker before or after Add.
//   TestCaptureStdio                           fd redirection of stdout and stderr.
//   TestPatterns                               pattern set is non-trivial.
//
// Round 2 additions: safe marker alphabet, Encoding names, raw-partial and
// raw-casefold, truncation caps, AssertClean, split-across-Add, JSON leaves,
// RunCapture; fd-level capture lives in capture_unix_test.go.
//
// API expected from T14 (identifiers not fixed by the plan are marked *):
//   New() *Scanner
//   (*Scanner).AddMarker(tb testing.TB, m string, sources ...Source)   -- tb.Fatalf when m is
//     shorter than 16 bytes or has a byte outside [A-Za-z0-9_-]; no sources = all
//   (*Scanner).Add(src Source, data []byte)   -- may be called repeatedly; data
//     of one source is concatenated, so a marker split across calls is found
//   (*Scanner).AddCollector(c *otlpcollector.Collector)   -- spans, resources;
//     JSON array/kvlist renderings are parsed to leaves; headers never scanned
//   (*Scanner).Findings() []Finding
//   (*Scanner).Truncated(src Source) bool   *  -- source exceeded the cap
//   AssertClean(tb testing.TB) on *Scanner: (*Scanner).AssertClean(tb)   *
//     -- fails tb on any finding, any truncated source, or no registered marker
//   type Source; SourceStdout, SourceStderr, SourceMetrics, SourceReadyz, SourceSpans
//   type Finding struct { Marker string; Source Source; Encoding string }
//     Encoding is one of: "raw", "raw-partial" (any 12-byte window of the
//     marker), "raw-casefold", "hex-lower", "hex-upper", "base64-std/N",
//     "base64-url/N" (N = alignment 0, 1 or 2 = number of prefix bytes)
//   Patterns(marker string) []Pattern
//   CaptureStdio(tb testing.TB) (stop func() (stdout, stderr []byte))
//     -- fd-level (dup2 of fds 1 and 2); NOT parallel-safe; when a stream
//     exceeds the cap, stop reports it through tb (Error/Fatal) and the
//     returned slice is at most the cap; the writer is never blocked for long
//   RunCapture(tb testing.TB, cmd *exec.Cmd) (stdout, stderr []byte, truncated bool) *
//     -- runs cmd with pipes, drains beyond the cap, fails tb on start error
//   SetMaxSourceBytesForTest(n int) (restore func())   *
//     -- lowers the per-source cap (also the rendered collector text cap)

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

var textSources = []markerscan.Source{
	markerscan.SourceStdout,
	markerscan.SourceStderr,
	markerscan.SourceMetrics,
	markerscan.SourceReadyz,
	markerscan.SourceSpans,
}

func srcName(s markerscan.Source) string { return fmt.Sprintf("source#%v", s) }

const safeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"

// newMarker returns a random synthetic marker, 24 bytes of the safe alphabet.
// Safe markers have 6-bit groups <= 61, so standard and URL-safe base64
// coincide; alphabet coverage is proven on Patterns directly (see
// TestEncodingFixtureIsMeaningful).
func newMarker(t testing.TB) string {
	t.Helper()
	b := make([]byte, 24)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(safeAlphabet))))
		if err != nil {
			t.Fatal(err)
		}
		b[i] = safeAlphabet[n.Int64()]
	}
	return string(b)
}

func mustAdd(t testing.TB, s *markerscan.Scanner, m string, src ...markerscan.Source) {
	t.Helper()
	s.AddMarker(t, m, src...)
}

// fakeTB records failures without stopping the test.
type fakeTB struct {
	testing.TB
	failed bool
}

func (f *fakeTB) Errorf(string, ...any) { f.failed = true }
func (f *fakeTB) Error(...any)          { f.failed = true }
func (f *fakeTB) Fatalf(string, ...any) { f.failed = true }
func (f *fakeTB) Fatal(...any)          { f.failed = true }
func (f *fakeTB) Fail()                 { f.failed = true }
func (f *fakeTB) FailNow()              { f.failed = true }

var fillers = []string{"", "Q", "\xfe\x7f"}  // 0, 1, 2 prefix bytes
var trailers = []string{"", "Z", "\x00\xff"} // 0, 1, 2 following bytes

type plant struct {
	name string
	text string
	enc  string // expected Finding.Encoding
}

// encodings returns the planted texts for marker m with k prefix bytes and
// the given trailing bytes. Computed independently of the harness.
func encodings(m string, k int, trail string) []plant {
	raw := []byte(fillers[k] + m + trail)
	std := base64.StdEncoding.EncodeToString(raw)
	url := base64.URLEncoding.EncodeToString(raw)
	return []plant{
		{"raw", fillers[k] + m + trail, "raw"},
		{"hex-lower", hex.EncodeToString(raw), "hex-lower"},
		{"hex-upper", strings.ToUpper(hex.EncodeToString(raw)), "hex-upper"},
		{"b64-std", std, fmt.Sprintf("base64-std/%d", k)},
		{"b64-std-unpadded", strings.TrimRight(std, "="), fmt.Sprintf("base64-std/%d", k)},
		{"b64-url", url, fmt.Sprintf("base64-url/%d", k)},
		{"b64-url-unpadded", strings.TrimRight(url, "="), fmt.Sprintf("base64-url/%d", k)},
	}
}

func foundEnc(fs []markerscan.Finding, m string, src markerscan.Source, enc string) bool {
	for _, f := range fs {
		if f.Marker == m && f.Source == src && f.Encoding == enc {
			return true
		}
	}
	return false
}

// foundEncAlt is foundEnc, but for base64 encodings either alphabet's name
// is accepted (they coincide for safe-alphabet markers); the /N suffix must match.
func foundEncAlt(fs []markerscan.Finding, m string, src markerscan.Source, enc string) bool {
	if i := strings.Index(enc, "/"); strings.HasPrefix(enc, "base64-") && i > 0 {
		n := enc[i:]
		return foundEnc(fs, m, src, "base64-std"+n) || foundEnc(fs, m, src, "base64-url"+n)
	}
	return foundEnc(fs, m, src, enc)
}

func found(fs []markerscan.Finding, m string, src markerscan.Source) bool {
	for _, f := range fs {
		if f.Marker == m && f.Source == src {
			return true
		}
	}
	return false
}

// TestEncodingFixtureIsMeaningful proves both base64 alphabets are produced
// at all 3 alignments, using Patterns directly with bytes outside the safe
// alphabet (all-ones 6-bit groups, so every stable char is '/' or '_').
func TestEncodingFixtureIsMeaningful(t *testing.T) { // AC-33
	m := strings.Repeat("\xff", 24)
	pats := markerscan.Patterns(m)
	byName := map[string][]byte{}
	for _, p := range pats {
		byName[p.Name] = p.Bytes
	}
	for k := range fillers {
		std, url := byName[fmt.Sprintf("base64-std/%d", k)], byName[fmt.Sprintf("base64-url/%d", k)]
		if len(std) == 0 || len(url) == 0 {
			t.Fatalf("alignment %d: missing base64 pattern (std=%q url=%q)", k, std, url)
		}
		if !bytes.ContainsAny(std, "+/") || !bytes.ContainsAny(url, "-_") {
			t.Errorf("alignment %d: std %q / url %q lack the alphabet-specific characters", k, std, url)
		}
		if bytes.Equal(std, url) {
			t.Errorf("alignment %d: std and url patterns are equal", k)
		}
		raw := []byte(fillers[k] + m)
		stdEnc := []byte(base64.RawStdEncoding.EncodeToString(raw))
		urlEnc := []byte(base64.RawURLEncoding.EncodeToString(raw))
		if !bytes.Contains(stdEnc, std) {
			t.Errorf("alignment %d: std pattern %q not inside encoding %q", k, std, stdEnc)
		}
		if !bytes.Contains(urlEnc, url) {
			t.Errorf("alignment %d: url pattern %q not inside encoding %q", k, url, urlEnc)
		}
	}
}

func TestAddMarkerValidation(t *testing.T) { // AC-33, D4
	s := markerscan.New()
	for _, bad := range []string{"", "short_marker", "0123456789abcde", "has space 0123456789abc", "plus+0123456789abcdef", "slash/0123456789abcdef", "angle<0123456789abcdef", "nl0123456789abcdef\n", "uni\u00e9_0123456789abcdef"} {
		f := &fakeTB{TB: t}
		s.AddMarker(f, bad)
		if !f.failed {
			t.Errorf("AddMarker(%q) did not fail tb for an unsafe marker", bad)
		}
	}
	for _, good := range []string{"Abc_def-0123456789", "0123456789abcdef"} {
		f := &fakeTB{TB: t}
		s.AddMarker(f, good)
		if f.failed {
			t.Errorf("safe marker %q rejected", good)
		}
	}
}

func TestAssertCleanFailsWithoutMarkers(t *testing.T) { // AC-33
	s := markerscan.New()
	s.Add(markerscan.SourceStdout, []byte("nothing"))
	f := &fakeTB{TB: t}
	s.AssertClean(f)
	if !f.failed {
		t.Error("AssertClean passed although no marker was registered")
	}
}

func TestPlantedEncodingsDetectedInEverySource(t *testing.T) { // AC-33
	for _, src := range textSources {
		for k := range fillers {
			for ti, trail := range trailers {
				m := newMarker(t)
				for _, p := range encodings(m, k, trail) {
					name := fmt.Sprintf("%s/offset%d/trail%d/%s", srcName(src), k, ti, p.name)
					t.Run(name, func(t *testing.T) {
						s := markerscan.New()
						mustAdd(t, s, m)
						s.Add(src, []byte("ts=1 noise "+p.text+" tail\n"))
						fs := s.Findings()
						if !foundEncAlt(fs, m, src, p.enc) {
							t.Fatalf("marker not detected as %q in %s; findings=%v", p.enc, p.name, fs)
						}
					})
				}
			}
		}
	}
}

func TestRawAtTextOffsets0To2(t *testing.T) { // AC-33
	for _, src := range textSources {
		for k := 0; k <= 2; k++ {
			m := newMarker(t)
			s := markerscan.New()
			mustAdd(t, s, m)
			s.Add(src, []byte(strings.Repeat("-", k)+m))
			if !found(s.Findings(), m, src) {
				t.Errorf("%s offset %d: raw marker at end of data not detected", srcName(src), k)
			}
		}
	}
}

func TestAddOrderIndependent(t *testing.T) { // AC-33
	m := newMarker(t)
	s := markerscan.New()
	s.Add(markerscan.SourceStdout, []byte("x "+m))
	mustAdd(t, s, m)
	if !found(s.Findings(), m, markerscan.SourceStdout) {
		t.Fatal("marker added after data not detected")
	}
}

func TestSourceSubsetSelection(t *testing.T) { // AC-33
	m := newMarker(t)
	s := markerscan.New()
	mustAdd(t, s, m, markerscan.SourceStdout, markerscan.SourceMetrics)
	for _, src := range textSources {
		s.Add(src, []byte("v "+m))
	}
	fs := s.Findings()
	for _, src := range textSources {
		want := src == markerscan.SourceStdout || src == markerscan.SourceMetrics
		if got := found(fs, m, src); got != want {
			t.Errorf("%s: found=%v want %v", srcName(src), got, want)
		}
	}

	// A marker with no sources is scanned in all of them.
	all := newMarker(t)
	s2 := markerscan.New()
	mustAdd(t, s2, all)
	for _, src := range textSources {
		s2.Add(src, []byte("v "+all))
	}
	for _, src := range textSources {
		if !found(s2.Findings(), all, src) {
			t.Errorf("%s: marker without source list not scanned", srcName(src))
		}
	}

	// Subset also applies to encodings and to the collector.
	m3 := newMarker(t)
	s3 := markerscan.New()
	mustAdd(t, s3, m3, markerscan.SourceStdout)
	s3.Add(markerscan.SourceStderr, []byte(base64.StdEncoding.EncodeToString([]byte("Q"+m3))))
	s3.Add(markerscan.SourceStderr, []byte(hex.EncodeToString([]byte(m3))))
	c := otlpcollector.Start(t)
	postSpan(t, c, spanSpec{name: m3}, nil)
	s3.AddCollector(c)
	if fs := s3.Findings(); len(fs) != 0 {
		t.Errorf("findings outside the chosen subset: %v", fs)
	}
}

func TestNegativeControl(t *testing.T) { // AC-33
	m := newMarker(t)
	other := newMarker(t)
	s := markerscan.New()
	mustAdd(t, s, m)
	for _, src := range textSources {
		s.Add(src, []byte("level=info msg=started\n# TYPE corral_up gauge\nok\n"))
		// Add joins calls into one stream: end every plant with a newline so
		// fragments cannot join into a window of the marker.
		s.Add(src, []byte(other+"\n"))                             // a different marker
		s.Add(src, []byte(m[:11]+"\n"))                            // shorter than a 12-byte window
		s.Add(src, []byte(m[13:]+"\n"))                            // 11 bytes
		s.Add(src, []byte(hex.EncodeToString([]byte(other))+"\n")) // other marker, hex
		s.Add(src, []byte(base64.StdEncoding.EncodeToString([]byte("Q"+other))+"\n"))
		s.Add(src, nil)
	}
	if fs := s.Findings(); len(fs) != 0 {
		t.Fatalf("false positives: %v", fs)
	}
	s.AssertClean(t)
	if fs := markerscan.New().Findings(); len(fs) != 0 {
		t.Fatalf("empty scanner has findings: %v", fs)
	}
}

func TestRawPartialAndCasefold(t *testing.T) { // AC-33, D5
	m := newMarker(t)
	for _, src := range textSources {
		s := markerscan.New()
		mustAdd(t, s, m)
		s.Add(src, []byte("x "+m[6:18]+" y")) // 12-byte window
		if !foundEnc(s.Findings(), m, src, "raw-partial") {
			t.Errorf("%s: 12-byte window not reported as raw-partial: %v", srcName(src), s.Findings())
		}
		s2 := markerscan.New()
		mustAdd(t, s2, m)
		s2.Add(src, []byte("x "+strings.ToUpper(m)+" y"))
		s2.Add(src, []byte("x "+strings.ToLower(m)+" y"))
		if !foundEnc(s2.Findings(), m, src, "raw-casefold") {
			t.Errorf("%s: case-folded marker not reported as raw-casefold: %v", srcName(src), s2.Findings())
		}
	}
}

func TestSplitAcrossAddCalls(t *testing.T) { // AC-33, D8
	m := newMarker(t)
	cases := []struct {
		name, enc string
		text      string
	}{
		{"raw", "raw", m},
		{"base64-std", "base64-std/1", base64.StdEncoding.EncodeToString([]byte("Q" + m))},
		{"base64-url", "base64-url/1", base64.URLEncoding.EncodeToString([]byte("Q" + m))},
		{"hex", "hex-lower", hex.EncodeToString([]byte(m))},
	}
	for _, c := range cases {
		for cut := 1; cut < len(c.text); cut += 3 {
			s := markerscan.New()
			mustAdd(t, s, m)
			s.Add(markerscan.SourceStdout, []byte("pre "+c.text[:cut]))
			s.Add(markerscan.SourceStdout, []byte(c.text[cut:]+" post"))
			if !foundEncAlt(s.Findings(), m, markerscan.SourceStdout, c.enc) {
				t.Fatalf("%s cut at %d: split marker not detected: %v", c.name, cut, s.Findings())
			}
		}
	}
}

func TestTruncationAndAssertClean(t *testing.T) { // AC-33, D2, D6, D7
	t.Run("assert clean passes", func(t *testing.T) {
		s := markerscan.New()
		mustAdd(t, s, newMarker(t))
		s.Add(markerscan.SourceStdout, []byte("nothing here"))
		f := &fakeTB{TB: t}
		s.AssertClean(f)
		if f.failed {
			t.Error("AssertClean failed a clean, untruncated scanner")
		}
		if s.Truncated(markerscan.SourceStdout) {
			t.Error("small source reported truncated")
		}
	})
	t.Run("assert clean fails on finding", func(t *testing.T) {
		m := newMarker(t)
		s := markerscan.New()
		mustAdd(t, s, m)
		s.Add(markerscan.SourceMetrics, []byte(m))
		f := &fakeTB{TB: t}
		s.AssertClean(f)
		if !f.failed {
			t.Error("AssertClean passed despite a finding")
		}
	})
	t.Run("add over cap", func(t *testing.T) {
		defer markerscan.SetMaxSourceBytesForTest(64)()
		s := markerscan.New()
		mustAdd(t, s, newMarker(t))
		s.Add(markerscan.SourceStderr, bytes.Repeat([]byte("a"), 1000))
		s.Add(markerscan.SourceStdout, []byte("ok"))
		if !s.Truncated(markerscan.SourceStderr) {
			t.Error("over-cap source not reported truncated")
		}
		if s.Truncated(markerscan.SourceStdout) {
			t.Error("under-cap source reported truncated")
		}
		f := &fakeTB{TB: t}
		s.AssertClean(f)
		if !f.failed {
			t.Error("AssertClean passed despite a truncated source")
		}
	})
	t.Run("collector text over cap", func(t *testing.T) {
		defer markerscan.SetMaxSourceBytesForTest(256)()
		c := otlpcollector.Start(t)
		postSpan(t, c, spanSpec{name: strings.Repeat("a", 4096)}, nil)
		s := markerscan.New()
		mustAdd(t, s, newMarker(t))
		s.AddCollector(c)
		if !s.Truncated(markerscan.SourceSpans) {
			t.Error("oversized collector text not reported truncated")
		}
		f := &fakeTB{TB: t}
		s.AssertClean(f)
		if !f.failed {
			t.Error("AssertClean passed despite truncated spans")
		}
	})
}

func TestPatterns(t *testing.T) { // AC-33
	m := newMarker(t)
	// raw, hex lower, hex upper, and at least one base64 alphabet at 3 alignments.
	if n := len(markerscan.Patterns(m)); n < 6 {
		t.Fatalf("Patterns returned %d entries, want at least 6", n)
	}
	if len(markerscan.Patterns(newMarker(t))) != len(markerscan.Patterns(m)) {
		t.Error("pattern count should not depend on marker content of equal length")
	}
}

// ---- collector payload ----

type spanSpec struct {
	name        string
	attrKey     string
	attrVal     string
	attrBytes   []byte
	attrArray   []string
	attrKV      map[string]string
	eventName   string
	eventKey    string
	eventVal    string
	status      string
	resourceKey string
	resourceVal string
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// postSpan sends one synthetic span to the collector over OTLP/HTTP.
func postSpan(t *testing.T, c *otlpcollector.Collector, sp spanSpec, hdr map[string]string) {
	t.Helper()
	name := sp.name
	if name == "" {
		name = "op"
	}
	span := &tracev1.Span{
		TraceId: bytes.Repeat([]byte{1}, 16),
		SpanId:  bytes.Repeat([]byte{2}, 8),
		Name:    name,
	}
	if sp.attrKey != "" || sp.attrVal != "" {
		k := sp.attrKey
		if k == "" {
			k = "k"
		}
		span.Attributes = append(span.Attributes, str(k, sp.attrVal))
	}
	if sp.attrBytes != nil {
		span.Attributes = append(span.Attributes, &commonpb.KeyValue{Key: "b",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: sp.attrBytes}}})
	}
	if sp.attrArray != nil {
		var vals []*commonpb.AnyValue
		for _, v := range sp.attrArray {
			vals = append(vals, &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}})
		}
		span.Attributes = append(span.Attributes, &commonpb.KeyValue{Key: "arr",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vals}}}})
	}
	if sp.attrKV != nil {
		var kvs []*commonpb.KeyValue
		for k, v := range sp.attrKV {
			kvs = append(kvs, str(k, v))
		}
		span.Attributes = append(span.Attributes, &commonpb.KeyValue{Key: "kv",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: kvs}}}})
	}
	if sp.eventName != "" || sp.eventKey != "" || sp.eventVal != "" {
		ev := &tracev1.Span_Event{Name: sp.eventName}
		if ev.Name == "" {
			ev.Name = "ev"
		}
		if sp.eventKey != "" || sp.eventVal != "" {
			k := sp.eventKey
			if k == "" {
				k = "ek"
			}
			ev.Attributes = []*commonpb.KeyValue{str(k, sp.eventVal)}
		}
		span.Events = append(span.Events, ev)
	}
	if sp.status != "" {
		span.Status = &tracev1.Status{Code: tracev1.Status_STATUS_CODE_ERROR, Message: sp.status}
	}
	res := &resourcepb.Resource{}
	if sp.resourceKey != "" || sp.resourceVal != "" {
		k := sp.resourceKey
		if k == "" {
			k = "rk"
		}
		res.Attributes = []*commonpb.KeyValue{str(k, sp.resourceVal)}
	}
	req := &collectorpb.ExportTraceServiceRequest{ResourceSpans: []*tracev1.ResourceSpans{{
		Resource:   res,
		ScopeSpans: []*tracev1.ScopeSpans{{Spans: []*tracev1.Span{span}}},
	}}}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	hr, err := http.NewRequest(http.MethodPost, "http://"+strings.TrimPrefix(c.HTTPEndpoint(), "http://")+"/v1/traces", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	hr.Header.Set("Content-Type", "application/x-protobuf")
	for k, v := range hdr {
		hr.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("collector status %d", resp.StatusCode)
	}
}

func TestSpanPayloadFieldsScanned(t *testing.T) { // AC-33
	fields := []struct {
		name string
		spec func(v string) spanSpec
	}{
		{"span-name", func(v string) spanSpec { return spanSpec{name: v} }},
		{"attr-key", func(v string) spanSpec { return spanSpec{attrKey: v, attrVal: "x"} }},
		{"attr-value", func(v string) spanSpec { return spanSpec{attrVal: v} }},
		{"event-name", func(v string) spanSpec { return spanSpec{eventName: v} }},
		{"event-attr-key", func(v string) spanSpec { return spanSpec{eventKey: v, eventVal: "x"} }},
		{"event-attr-value", func(v string) spanSpec { return spanSpec{eventVal: v} }},
		{"status-description", func(v string) spanSpec { return spanSpec{status: v} }},
		{"resource-attr-key", func(v string) spanSpec { return spanSpec{resourceKey: v, resourceVal: "x"} }},
		{"resource-attr-value", func(v string) spanSpec { return spanSpec{resourceVal: v} }},
	}
	for _, f := range fields {
		for k := range fillers {
			m := newMarker(t)
			for _, p := range encodings(m, k, "") {
				// Attribute keys and names are strings; the encodings are all printable
				// except the filler bytes, which are valid in protobuf strings only if
				// UTF-8, so skip non-UTF-8 plants for string fields.
				if !isUTF8Safe(p.text) {
					continue
				}
				t.Run(fmt.Sprintf("%s/offset%d/%s", f.name, k, p.name), func(t *testing.T) {
					c := otlpcollector.Start(t)
					postSpan(t, c, f.spec(p.text), nil)
					s := markerscan.New()
					mustAdd(t, s, m)
					s.AddCollector(c)
					if !found(s.Findings(), m, markerscan.SourceSpans) {
						t.Fatalf("not detected in %s", f.name)
					}
				})
			}
		}
	}
}

func isUTF8Safe(s string) bool { return strings.ToValidUTF8(s, "") == s }

func TestJSONRenderedValuesScanned(t *testing.T) { // AC-33, D3
	// Arrays and kvlists are rendered by the collector as compact JSON; the
	// scanner must see the decoded leaves, including JSON-escaped ones.
	m := newMarker(t)
	std := base64.StdEncoding.EncodeToString([]byte("Q" + m))
	leaves := []string{
		m,
		"a<b>&" + m + "\"q\\", // json escapes < > & " \ in this leaf
		"<" + std + ">",       // base64 with escaped neighbours
		"<" + hex.EncodeToString([]byte(m)) + ">",
	}
	for i, leaf := range leaves {
		for _, kind := range []string{"array", "kvlist-value", "kvlist-key"} {
			c := otlpcollector.Start(t)
			var sp spanSpec
			switch kind {
			case "array":
				sp.attrArray = []string{"x", leaf}
			case "kvlist-value":
				sp.attrKV = map[string]string{"k": leaf}
			case "kvlist-key":
				sp.attrKV = map[string]string{leaf: "v"}
			}
			postSpan(t, c, sp, nil)
			s := markerscan.New()
			mustAdd(t, s, m)
			s.AddCollector(c)
			if !found(s.Findings(), m, markerscan.SourceSpans) {
				t.Errorf("leaf %d in %s not detected", i, kind)
			}
		}
	}
}

func TestBytesAttributeScanned(t *testing.T) { // AC-33
	// The collector renders bytes attributes as padded standard base64, so a
	// marker inside bytes at offsets 0..2 must be found via base64 scanning.
	for k := range fillers {
		for _, trail := range trailers {
			m := newMarker(t)
			c := otlpcollector.Start(t)
			postSpan(t, c, spanSpec{attrBytes: []byte(fillers[k] + m + trail)}, nil)
			s := markerscan.New()
			mustAdd(t, s, m)
			s.AddCollector(c)
			if !found(s.Findings(), m, markerscan.SourceSpans) {
				t.Errorf("offset %d trail %q: marker in bytes attribute not detected", k, trail)
			}
		}
	}
}

func TestTransportHeadersNotScanned(t *testing.T) { // AC-33
	m := newMarker(t)
	c := otlpcollector.Start(t)
	postSpan(t, c, spanSpec{name: "clean"}, map[string]string{"Authorization": m, "X-Marker": m})
	s := markerscan.New()
	mustAdd(t, s, m)
	s.AddCollector(c)
	if fs := s.Findings(); len(fs) != 0 {
		t.Fatalf("transport headers were scanned: %v", fs)
	}
}

// ---- capture ----

func TestCaptureStdio(t *testing.T) { // AC-33
	m := newMarker(t)
	defer markerscan.SetMaxSourceBytesForTest(8 << 20)()
	stop := markerscan.CaptureStdio(t)
	fmt.Fprintln(os.Stdout, "out line "+m)
	fmt.Fprintln(os.Stderr, "err line "+hex.EncodeToString([]byte(m)))
	big := strings.Repeat("0123456789abcdef", 16<<10) // 256 KiB, beyond a pipe buffer
	fmt.Fprint(os.Stdout, big)
	out, errb := stop()
	if !bytes.Contains(out, []byte("out line "+m)) || !bytes.Contains(out, []byte(big)) {
		t.Errorf("stdout not captured fully (%d bytes)", len(out))
	}
	if bytes.Contains(out, []byte("err line")) || !bytes.Contains(errb, []byte("err line")) {
		t.Errorf("stdout and stderr were mixed up")
	}

	s := markerscan.New()
	mustAdd(t, s, m)
	s.Add(markerscan.SourceStdout, out)
	s.Add(markerscan.SourceStderr, errb)
	fs := s.Findings()
	if !found(fs, m, markerscan.SourceStdout) || !found(fs, m, markerscan.SourceStderr) {
		t.Errorf("captured output not detected: %v", fs)
	}
}

// ---- capture caps: must fail, not hang ----

func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not complete within %v (hang)", what, d)
	}
}

func TestCaptureStdioOverCapReportsAndCompletes(t *testing.T) { // AC-33, D2
	const limit = 4096
	defer markerscan.SetMaxSourceBytesForTest(limit)()
	f := &fakeTB{TB: t}
	var out, errb []byte
	within(t, 20*time.Second, "in-process capture over cap", func() {
		stop := markerscan.CaptureStdio(f)
		chunk := strings.Repeat("x", 1024)
		for i := 0; i < 1024; i++ { // 1 MiB per stream
			fmt.Fprint(os.Stdout, chunk)
			fmt.Fprint(os.Stderr, chunk)
		}
		out, errb = stop()
	})
	if !f.failed {
		t.Error("truncation of captured stdio was not reported through tb")
	}
	if len(out) > limit || len(errb) > limit {
		t.Errorf("capture exceeded cap: %d / %d", len(out), len(errb))
	}
}

// TestChildHelper is the child process body for RunCapture tests.
func TestChildHelper(t *testing.T) {
	if os.Getenv("CORRAL_MARKERSCAN_CHILD") != "1" {
		t.Skip("child process helper")
	}
	fmt.Fprintln(os.Stdout, "child-out "+os.Getenv("CORRAL_MARKERSCAN_MARKER"))
	fmt.Fprintln(os.Stderr, "child-err")
	if os.Getenv("CORRAL_MARKERSCAN_FLOOD") == "1" {
		chunk := strings.Repeat("y", 1024)
		for i := 0; i < 2048; i++ {
			fmt.Fprint(os.Stdout, chunk)
			fmt.Fprint(os.Stderr, chunk)
		}
	}
}

func childCmd(m string, flood bool) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildHelper$", "-test.v")
	fl := "0"
	if flood {
		fl = "1"
	}
	cmd.Env = append(os.Environ(), "CORRAL_MARKERSCAN_CHILD=1", "CORRAL_MARKERSCAN_FLOOD="+fl, "CORRAL_MARKERSCAN_MARKER="+m)
	return cmd
}

func TestRunCapture(t *testing.T) { // AC-33
	m := newMarker(t)
	out, errb, trunc := markerscan.RunCapture(t, childCmd(m, false))
	if trunc {
		t.Error("small child output reported truncated")
	}
	if !bytes.Contains(out, []byte("child-out "+m)) || !bytes.Contains(errb, []byte("child-err")) {
		t.Fatalf("child output not captured: %q / %q", out, errb)
	}
	s := markerscan.New()
	mustAdd(t, s, m)
	s.Add(markerscan.SourceStdout, out)
	s.Add(markerscan.SourceStderr, errb)
	if !found(s.Findings(), m, markerscan.SourceStdout) {
		t.Error("marker in child stdout not detected")
	}
}

func TestRunCaptureOverCapReportsAndCompletes(t *testing.T) { // AC-33, D2
	const limit = 4096
	defer markerscan.SetMaxSourceBytesForTest(limit)()
	m := newMarker(t)
	var out, errb []byte
	var trunc bool
	within(t, 30*time.Second, "child capture over cap", func() {
		out, errb, trunc = markerscan.RunCapture(t, childCmd(m, true))
	})
	if !trunc {
		t.Error("truncation of child output was not reported")
	}
	if len(out) > limit || len(errb) > limit {
		t.Errorf("capture exceeded cap: %d / %d", len(out), len(errb))
	}
	if !bytes.Contains(out, []byte("child-out "+m)) {
		t.Error("data within the cap (the head of the stream) was lost")
	}
}

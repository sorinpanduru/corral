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
// Assumed (not fixed by the plan) identifiers, to be matched by T14:
//   type Source, constants SourceStdout, SourceStderr, SourceMetrics,
//     SourceReadyz, SourceSpans (all distinct);
//   type Finding struct { Marker string; Source Source; ... } (Marker is the
//     marker as given to AddMarker);
//   type Pattern (opaque here); Patterns returns at least one entry per marker;
//   AddMarker with no sources means all sources; Findings is computed from
//     everything added so far, in either call order;
//   AddCollector scans the collector's recorded spans and resources as source
//     SourceSpans, rendered per the T13 collector (bytes attributes as padded
//     standard base64) and never its request headers;
//   CaptureStdio replaces os.Stdout/os.Stderr and the stop func restores them.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

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

// newMarker returns a random synthetic marker. The ">?>?" run makes the
// standard and URL-safe base64 alphabets differ at every alignment.
func newMarker(t testing.TB) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "mk-" + hex.EncodeToString(b) + ">?>?>?~~"
}

var fillers = []string{"", "Q", "\xfe\x7f"}  // 0, 1, 2 prefix bytes
var trailers = []string{"", "Z", "\x00\xff"} // 0, 1, 2 following bytes

type plant struct {
	name string
	text string
}

// encodings returns the planted texts for marker m with k prefix bytes and
// the given trailing bytes. Computed independently of the harness.
func encodings(m string, k int, trail string) []plant {
	raw := []byte(fillers[k] + m + trail)
	std := base64.StdEncoding.EncodeToString(raw)
	url := base64.URLEncoding.EncodeToString(raw)
	return []plant{
		{"raw", fillers[k] + m + trail},
		{"hex-lower", hex.EncodeToString(raw)},
		{"hex-upper", strings.ToUpper(hex.EncodeToString(raw))},
		{"b64-std", std},
		{"b64-std-unpadded", strings.TrimRight(std, "=")},
		{"b64-url", url},
		{"b64-url-unpadded", strings.TrimRight(url, "=")},
	}
}

func found(fs []markerscan.Finding, m string, src markerscan.Source) bool {
	for _, f := range fs {
		if f.Marker == m && f.Source == src {
			return true
		}
	}
	return false
}

func TestEncodingFixtureIsMeaningful(t *testing.T) {
	m := newMarker(t)
	for k := range fillers {
		e := encodings(m, k, "")
		std, url := e[3].text, e[5].text
		if !strings.ContainsAny(std, "+/") || !strings.ContainsAny(url, "-_") {
			t.Fatalf("offset %d: fixture does not exercise both base64 alphabets", k)
		}
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
						s.AddMarker(m)
						s.Add(src, []byte("ts=1 noise "+p.text+" tail\n"))
						if !found(s.Findings(), m, src) {
							t.Fatalf("marker not detected in %s; findings=%v", p.name, s.Findings())
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
			s.AddMarker(m)
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
	s.AddMarker(m)
	if !found(s.Findings(), m, markerscan.SourceStdout) {
		t.Fatal("marker added after data not detected")
	}
}

func TestSourceSubsetSelection(t *testing.T) { // AC-33
	m := newMarker(t)
	s := markerscan.New()
	s.AddMarker(m, markerscan.SourceStdout, markerscan.SourceMetrics)
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
	s2.AddMarker(all)
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
	s3.AddMarker(m3, markerscan.SourceStdout)
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
	s.AddMarker(m)
	for _, src := range textSources {
		s.Add(src, []byte("level=info msg=started\n# TYPE corral_up gauge\nok\n"))
		s.Add(src, []byte(other))                             // a different marker
		s.Add(src, []byte(m[:len(m)-3]))                      // truncated marker
		s.Add(src, []byte(strings.ToUpper(m)))                // case-changed raw
		s.Add(src, []byte(hex.EncodeToString([]byte(other)))) // other marker, hex
		s.Add(src, []byte(base64.StdEncoding.EncodeToString([]byte("Q"+other))))
		s.Add(src, nil)
	}
	if fs := s.Findings(); len(fs) != 0 {
		t.Fatalf("false positives: %v", fs)
	}
	if fs := markerscan.New().Findings(); len(fs) != 0 {
		t.Fatalf("empty scanner has findings: %v", fs)
	}
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
					s.AddMarker(m)
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

func TestBytesAttributeScanned(t *testing.T) { // AC-33
	// The collector renders bytes attributes as padded standard base64, so a
	// marker inside bytes at offsets 0..2 must be found via base64 scanning.
	for k := range fillers {
		for _, trail := range trailers {
			m := newMarker(t)
			c := otlpcollector.Start(t)
			postSpan(t, c, spanSpec{attrBytes: []byte(fillers[k] + m + trail)}, nil)
			s := markerscan.New()
			s.AddMarker(m)
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
	s.AddMarker(m)
	s.AddCollector(c)
	if fs := s.Findings(); len(fs) != 0 {
		t.Fatalf("transport headers were scanned: %v", fs)
	}
}

// ---- capture ----

func TestCaptureStdio(t *testing.T) { // AC-33
	m := newMarker(t)
	origOut, origErr := os.Stdout, os.Stderr
	stop := markerscan.CaptureStdio(t)
	fmt.Fprintln(os.Stdout, "out line "+m)
	fmt.Fprintln(os.Stderr, "err line "+hex.EncodeToString([]byte(m)))
	big := strings.Repeat("0123456789abcdef", 16<<10) // 256 KiB, beyond a pipe buffer
	fmt.Fprint(os.Stdout, big)
	out, errb := stop()
	if os.Stdout != origOut || os.Stderr != origErr {
		t.Error("stop did not restore os.Stdout / os.Stderr")
	}
	if !bytes.Contains(out, []byte("out line "+m)) || !bytes.Contains(out, []byte(big)) {
		t.Errorf("stdout not captured fully (%d bytes)", len(out))
	}
	if bytes.Contains(out, []byte("err line")) || !bytes.Contains(errb, []byte("err line")) {
		t.Errorf("stdout and stderr were mixed up")
	}

	s := markerscan.New()
	s.AddMarker(m)
	s.Add(markerscan.SourceStdout, out)
	s.Add(markerscan.SourceStderr, errb)
	fs := s.Findings()
	if !found(fs, m, markerscan.SourceStdout) || !found(fs, m, markerscan.SourceStderr) {
		t.Errorf("captured output not detected: %v", fs)
	}
}

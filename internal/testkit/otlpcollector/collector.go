// Package otlpcollector is an in-process fake OTLP trace collector for tests
// (ADR-0005). It serves OTLP/HTTP (POST /v1/traces, protobuf, gzip aware) and
// OTLP/gRPC (TraceService/Export, gzip aware) on separate ephemeral loopback
// listeners, and records decoded spans, resources, transport headers, content
// encoding and the number of accepted TCP connections. All data is synthetic.
package otlpcollector

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip" // registers the gzip compressor for gRPC requests
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// MaxBodyBytes caps an OTLP/HTTP request body, both on the wire and after
// gzip decompression. Larger requests get 413 and record nothing.
const MaxBodyBytes = 4 << 20

// Mode selects how the collector answers export requests.
type Mode int

const (
	// ModeAccept decodes, records and acknowledges requests.
	ModeAccept Mode = iota
	// ModeReject answers with the configured HTTP status / gRPC code and
	// records no spans.
	ModeReject
	// ModeUnresponsive holds each request until Close; no spans are recorded.
	ModeUnresponsive
)

// Protocol names reported in RequestInfo.Protocol.
const (
	ProtocolHTTP = "http"
	ProtocolGRPC = "grpc"
)

// RecordedEvent is a span event.
type RecordedEvent struct {
	Name       string
	Attributes map[string]string
}

// RecordedSpan is a decoded span.
type RecordedSpan struct {
	Name          string
	Attributes    map[string]string
	Events        []RecordedEvent
	StatusCode    int32
	StatusMessage string
}

// RequestInfo describes one received export request.
type RequestInfo struct {
	Protocol        string // "http" or "grpc"
	Headers         map[string][]string
	ContentEncoding string // "gzip" or "" (identity)
}

// Option configures Start.
type Option func(*config)

type config struct {
	mode       Mode
	httpStatus int
	grpcCode   codes.Code
	httpBody   string
	grpcMsg    string
}

const defaultRejectText = "rejected by fake collector"

// WithMode sets the initial mode.
func WithMode(m Mode) Option { return func(c *config) { c.mode = m } }

// WithHTTPRejectStatus sets the HTTP status returned in ModeReject.
func WithHTTPRejectStatus(s int) Option { return func(c *config) { c.httpStatus = s } }

// WithGRPCRejectCode sets the gRPC code returned in ModeReject.
func WithGRPCRejectCode(code codes.Code) Option { return func(c *config) { c.grpcCode = code } }

// WithHTTPRejectBody sets the HTTP response body returned in ModeReject
// (default "rejected by fake collector"). The body is truncated to
// MaxBodyBytes. Test-only: it lets a test make the fake collector echo text
// back to the code under test; that text is synthetic and must never contain
// real credentials or customer data.
func WithHTTPRejectBody(body string) Option {
	return func(c *config) {
		if len(body) > MaxBodyBytes {
			body = body[:MaxBodyBytes]
		}
		c.httpBody = body
	}
}

// WithGRPCRejectMessage sets the gRPC status message returned in ModeReject
// (default "rejected by fake collector"). The message is truncated to
// MaxBodyBytes. Test-only: the echoed text is synthetic and must never
// contain real credentials or customer data.
func WithGRPCRejectMessage(msg string) Option {
	return func(c *config) {
		if len(msg) > MaxBodyBytes {
			msg = msg[:MaxBodyBytes]
		}
		c.grpcMsg = msg
	}
}

// Collector is the fake collector. Create it with Start.
type Collector struct {
	cfg   config
	mode  atomic.Int32
	conns atomic.Int64

	httpLn  net.Listener
	grpcLn  net.Listener
	httpSrv *http.Server
	grpcSrv *grpc.Server

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup // serve loops

	inflightMu sync.Mutex
	closed     bool
	inflight   sync.WaitGroup // request handlers

	mu        sync.Mutex
	spans     []RecordedSpan
	resources []map[string]string
	requests  []RequestInfo
}

// Start launches the collector and registers Close with tb.Cleanup.
func Start(tb testing.TB, opts ...Option) *Collector {
	tb.Helper()
	c := &Collector{
		cfg: config{mode: ModeAccept, httpStatus: http.StatusServiceUnavailable, grpcCode: codes.Unavailable,
			httpBody: defaultRejectText, grpcMsg: defaultRejectText},
		done: make(chan struct{}),
	}
	for _, o := range opts {
		o(&c.cfg)
	}
	c.mode.Store(int32(c.cfg.mode))

	var err error
	if c.httpLn, err = listen(&c.conns); err != nil {
		tb.Fatalf("otlpcollector: http listen: %v", err)
	}
	if c.grpcLn, err = listen(&c.conns); err != nil {
		_ = c.httpLn.Close()
		tb.Fatalf("otlpcollector: grpc listen: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", c.handleHTTP)
	c.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	c.grpcSrv = grpc.NewServer(grpc.StatsHandler(encodingStats{}))
	collectortrace.RegisterTraceServiceServer(c.grpcSrv, grpcService{c: c})

	c.wg.Add(2)
	go func() { defer c.wg.Done(); _ = c.httpSrv.Serve(c.httpLn) }()
	go func() { defer c.wg.Done(); _ = c.grpcSrv.Serve(c.grpcLn) }()

	tb.Cleanup(c.Close)
	return c
}

// HTTPEndpoint returns the OTLP/HTTP host:port (127.0.0.1:<port>).
func (c *Collector) HTTPEndpoint() string { return c.httpLn.Addr().String() }

// GRPCEndpoint returns the OTLP/gRPC host:port (127.0.0.1:<port>).
func (c *Collector) GRPCEndpoint() string { return c.grpcLn.Addr().String() }

// SetMode switches the mode for subsequent requests.
func (c *Collector) SetMode(m Mode) { c.mode.Store(int32(m)) }

// ConnCount returns the number of TCP connections accepted on either listener.
// Read it while the collector is open, after the code under test has shut
// down and before Close, so that late or closing connections are not missed.
func (c *Collector) ConnCount() int { return int(c.conns.Load()) }

// Spans returns a deep copy of all recorded spans.
func (c *Collector) Spans() []RecordedSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RecordedSpan, len(c.spans))
	for i, sp := range c.spans {
		out[i] = sp
		out[i].Attributes = copyMap(sp.Attributes)
		if sp.Events != nil {
			out[i].Events = make([]RecordedEvent, len(sp.Events))
			for j, e := range sp.Events {
				out[i].Events[j] = RecordedEvent{Name: e.Name, Attributes: copyMap(e.Attributes)}
			}
		}
	}
	return out
}

// Resources returns deep copies, one attribute map per received ResourceSpans.
func (c *Collector) Resources() []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]string, len(c.resources))
	for i, r := range c.resources {
		out[i] = copyMap(r)
	}
	return out
}

// Requests returns a deep copy of the received request descriptions. Recorded
// headers may include credentials (for example Authorization), so the
// collector must only be used with synthetic data.
func (c *Collector) Requests() []RequestInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RequestInfo, len(c.requests))
	for i, r := range c.requests {
		out[i] = r
		out[i].Headers = make(map[string][]string, len(r.Headers))
		for k, v := range r.Headers {
			out[i].Headers[k] = append([]string(nil), v...)
		}
	}
	return out
}

// Close stops both listeners, releases held requests and waits for the
// servers to stop. It is safe to call repeatedly; recorded data stays readable.
func (c *Collector) Close() {
	c.closeOnce.Do(func() {
		c.inflightMu.Lock()
		c.closed = true
		c.inflightMu.Unlock()
		close(c.done)
		c.grpcSrv.Stop()
		_ = c.httpSrv.Close()
	})
	c.wg.Wait()
	c.inflight.Wait()
}

// UnreachableEndpoint reserves a loopback port, releases it and returns the
// now-closed host:port, so that dialling it is refused.
func UnreachableEndpoint(tb testing.TB) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("otlpcollector: reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		tb.Fatalf("otlpcollector: release port: %v", err)
	}
	return addr
}

// countingListener counts accepted TCP connections.
type countingListener struct {
	net.Listener
	n *atomic.Int64
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return conn, err
}

func listen(n *atomic.Int64) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return countingListener{Listener: ln, n: n}, nil
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// begin registers an in-flight handler; it reports false once Close has begun.
func (c *Collector) begin() bool {
	c.inflightMu.Lock()
	defer c.inflightMu.Unlock()
	if c.closed {
		return false
	}
	c.inflight.Add(1)
	return true
}

func (c *Collector) record(info RequestInfo) {
	c.mu.Lock()
	c.requests = append(c.requests, info)
	c.mu.Unlock()
}

func (c *Collector) recordTraces(req *collectortrace.ExportTraceServiceRequest) {
	var spans []RecordedSpan
	var resources []map[string]string
	for _, rs := range req.GetResourceSpans() {
		resources = append(resources, attrMap(rs.GetResource().GetAttributes()))
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				r := RecordedSpan{
					Name:          s.GetName(),
					Attributes:    attrMap(s.GetAttributes()),
					StatusCode:    int32(s.GetStatus().GetCode()),
					StatusMessage: s.GetStatus().GetMessage(),
				}
				for _, e := range s.GetEvents() {
					r.Events = append(r.Events, RecordedEvent{Name: e.GetName(), Attributes: attrMap(e.GetAttributes())})
				}
				spans = append(spans, r)
			}
		}
	}
	c.mu.Lock()
	c.spans = append(c.spans, spans...)
	c.resources = append(c.resources, resources...)
	c.mu.Unlock()
}

func attrMap(kvs []*commonpb.KeyValue) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.GetKey()] = anyString(kv.GetValue())
	}
	return m
}

// anyString renders an attribute value: strings as is, int decimal, bool
// true/false, double via FormatFloat 'g', bytes as padded standard base64,
// arrays and kvlists as compact JSON with sorted keys.
func anyString(v *commonpb.AnyValue) string {
	if s, ok := v.GetValue().(*commonpb.AnyValue_StringValue); ok {
		return s.StringValue
	}
	switch x := anyNatural(v).(type) {
	case string:
		return x
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		b, err := json.Marshal(x) // map keys are sorted by encoding/json
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func anyNatural(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		out := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			out = append(out, anyNatural(e))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		out := make(map[string]any, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			out[kv.GetKey()] = anyNatural(kv.GetValue())
		}
		return out
	}
	return nil
}

// handleHTTP serves POST /v1/traces.
func (c *Collector) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if !c.begin() {
		http.Error(w, "collector closed", http.StatusServiceUnavailable)
		return
	}
	defer c.inflight.Done()

	enc := ""
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		enc = "gzip"
	}
	c.record(RequestInfo{Protocol: ProtocolHTTP, Headers: r.Header.Clone(), ContentEncoding: enc})

	switch Mode(c.mode.Load()) {
	case ModeUnresponsive:
		select {
		case <-c.done:
		case <-r.Context().Done():
		}
		// Never acknowledge a held request: abort the connection so the
		// client sees a failure rather than an implicit 200.
		panic(http.ErrAbortHandler)
	case ModeReject:
		// Reject mode answers regardless of method or body, so the
		// configured status always reaches the client.
		http.Error(w, c.cfg.httpBody, c.cfg.httpStatus)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	if enc == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			bodyError(w, err)
			return
		}
		defer zr.Close()
		body = zr
	}
	// Read one byte past the cap (also bounds decompressed gzip output).
	// A fixed buffer (no append growth) keeps memory use bounded at about
	// MaxBodyBytes regardless of the input.
	buf := make([]byte, MaxBodyBytes+1)
	n, err := io.ReadFull(body, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		bodyError(w, err)
		return
	}
	raw := buf[:n]
	if len(raw) > MaxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req collectortrace.ExportTraceServiceRequest
	if err := proto.Unmarshal(raw, &req); err != nil {
		http.Error(w, "bad protobuf body", http.StatusBadRequest)
		return
	}
	c.recordTraces(&req)

	out, _ := proto.Marshal(&collectortrace.ExportTraceServiceResponse{})
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

func bodyError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "bad request body", http.StatusBadRequest)
}

type encodingKey struct{}

// encodingStats captures the request compression of each gRPC call, which is
// not exposed in the incoming metadata.
type encodingStats struct{}

type encodingBox struct{ enc atomic.Value }

func (encodingStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return context.WithValue(ctx, encodingKey{}, &encodingBox{})
}

func (encodingStats) HandleRPC(ctx context.Context, s stats.RPCStats) {
	if h, ok := s.(*stats.InHeader); ok {
		if b, ok := ctx.Value(encodingKey{}).(*encodingBox); ok {
			b.enc.Store(h.Compression)
		}
	}
}

func (encodingStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (encodingStats) HandleConn(context.Context, stats.ConnStats)                       {}

type grpcService struct {
	collectortrace.UnimplementedTraceServiceServer
	c *Collector
}

func (g grpcService) Export(ctx context.Context, req *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	c := g.c
	if !c.begin() {
		return nil, status.Error(codes.Unavailable, "collector closed")
	}
	defer c.inflight.Done()
	enc := ""
	if b, ok := ctx.Value(encodingKey{}).(*encodingBox); ok {
		if v, _ := b.enc.Load().(string); v == "gzip" {
			enc = "gzip"
		}
	}
	headers := map[string][]string{}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		for k, v := range md {
			headers[k] = append([]string(nil), v...)
		}
	}
	c.record(RequestInfo{Protocol: ProtocolGRPC, Headers: headers, ContentEncoding: enc})

	switch Mode(c.mode.Load()) {
	case ModeUnresponsive:
		select {
		case <-c.done:
		case <-ctx.Done():
		}
		return nil, status.Error(codes.Unavailable, "collector closed")
	case ModeReject:
		return nil, status.Error(c.cfg.grpcCode, c.cfg.grpcMsg)
	}
	c.recordTraces(req)
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

var _ collectortrace.TraceServiceServer = grpcService{}

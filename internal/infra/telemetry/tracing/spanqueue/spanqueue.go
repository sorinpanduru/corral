// Package spanqueue implements corral's bounded-queue span processor
// (SPEC-001 FR-37, FR-34; ADR-0007). It replaces the stock SDK
// BatchSpanProcessor so that queue-full drops can be counted.
//
// Compliance: spans are handed to the exporter unchanged; this package reads
// no span attributes, logs nothing (export errors go to otel.Handle, which the
// logging bridge routes) and holds no customer data beyond the queued spans.
package spanqueue

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// DropCounter counts spans dropped because the queue was full.
type DropCounter interface{ Add(float64) }

// Config holds the queue and export settings (OTEL_BSP_* mapping, FR-37).
// Values are expected to be validated upstream (FR-27, T10); the clamping and
// fallbacks in New are defence in depth only.
type Config struct {
	MaxQueueSize       int
	MaxExportBatchSize int
	ScheduleDelay      time.Duration
	ExportTimeout      time.Duration
}

// DefaultConfig returns the OpenTelemetry specification defaults.
func DefaultConfig() Config {
	return Config{
		MaxQueueSize:       2048,
		MaxExportBatchSize: 512,
		ScheduleDelay:      5 * time.Second,
		ExportTimeout:      30 * time.Second,
	}
}

// NewTicker creates the schedule ticker: a channel and a stop function.
type NewTicker func(d time.Duration) (<-chan time.Time, func())

// Option customises a Processor.
type Option func(*Processor)

// WithTicker injects the schedule-delay clock.
func WithTicker(f NewTicker) Option {
	return func(p *Processor) {
		if f != nil {
			p.newTicker = f
		}
	}
}

func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

type flushReq struct {
	ctx   context.Context
	limit int // max spans to export: the queue length when ForceFlush was called
	reply chan error
}

// Processor is a sdktrace.SpanProcessor with a fixed-capacity queue and a
// single export worker.
type Processor struct {
	exp       sdktrace.SpanExporter
	cfg       Config
	dropped   DropCounter
	newTicker NewTicker

	mu sync.Mutex // guards queue and stopped
	// queue is a mutex-guarded slice (not a channel) so Shutdown can clear it atomically.
	queue   []sdktrace.ReadOnlySpan
	stopped bool

	wake    chan struct{}
	flushCh chan flushReq
	stop    chan struct{}
	done    chan struct{} // closed when the worker has exited
	closed  chan struct{} // closed when Shutdown has finished

	baseCtx    context.Context
	baseCancel context.CancelFunc
}

var _ sdktrace.SpanProcessor = (*Processor)(nil)

// New starts the worker and returns the processor. Non-positive config values
// fall back to the defaults.
func New(exp sdktrace.SpanExporter, cfg Config, dropped DropCounter, opts ...Option) *Processor {
	def := DefaultConfig()
	if cfg.MaxQueueSize <= 0 {
		cfg.MaxQueueSize = def.MaxQueueSize
	}
	if cfg.MaxExportBatchSize <= 0 {
		cfg.MaxExportBatchSize = def.MaxExportBatchSize
	}
	if cfg.MaxExportBatchSize > cfg.MaxQueueSize {
		cfg.MaxExportBatchSize = cfg.MaxQueueSize
	}
	if cfg.ScheduleDelay <= 0 {
		cfg.ScheduleDelay = def.ScheduleDelay
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = def.ExportTimeout
	}
	p := &Processor{
		exp:       exp,
		cfg:       cfg,
		dropped:   dropped,
		newTicker: realTicker,
		queue:     make([]sdktrace.ReadOnlySpan, 0, cfg.MaxQueueSize),
		wake:      make(chan struct{}, 1),
		flushCh:   make(chan flushReq),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		closed:    make(chan struct{}),
	}
	for _, o := range opts {
		o(p)
	}
	p.baseCtx, p.baseCancel = context.WithCancel(context.Background())
	go p.run()
	return p
}

// OnStart is a no-op.
func (p *Processor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// OnEnd enqueues a sampled span without ever blocking. When the queue is
// full the span is dropped and the drop counter is incremented. After
// Shutdown, spans are refused without being counted.
func (p *Processor) OnEnd(s sdktrace.ReadOnlySpan) {
	if s == nil || !s.SpanContext().IsSampled() {
		return
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	if len(p.queue) >= p.cfg.MaxQueueSize {
		p.mu.Unlock()
		if p.dropped != nil {
			p.dropped.Add(1)
		}
		return
	}
	p.queue = append(p.queue, s)
	full := len(p.queue) >= p.cfg.MaxExportBatchSize
	p.mu.Unlock()
	if full {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// ForceFlush exports everything queued, honouring ctx.
func (p *Processor) ForceFlush(ctx context.Context) error {
	return p.flush(ctx)
}

func (p *Processor) flush(ctx context.Context) error {
	req := flushReq{ctx: ctx, limit: p.queueLen(), reply: make(chan error, 1)}
	select {
	case p.flushCh <- req:
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown flushes within ctx's deadline, drops what is left (not counted),
// shuts the exporter down once and refuses further spans. Later calls wait
// for the first to finish, or for ctx.
func (p *Processor) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		select {
		case <-p.closed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.stopped = true
	p.mu.Unlock()
	defer close(p.closed)

	err := p.flush(ctx)

	// Whatever remains is dropped; abort any export still in flight.
	p.mu.Lock()
	clear(p.queue)
	p.queue = nil
	p.mu.Unlock()
	if err != nil {
		// Abort the in-flight export before waiting for the worker to exit.
		p.baseCancel()
	}
	close(p.stop)
	select {
	case <-p.done:
	case <-ctx.Done():
	}
	p.baseCancel()

	if serr := p.exp.Shutdown(ctx); serr != nil && err == nil {
		err = serr
	}
	return err
}

func (p *Processor) run() {
	defer close(p.done)
	tick, stopTick := p.newTicker(p.cfg.ScheduleDelay)
	defer stopTick()
	for {
		select {
		case <-p.stop:
			return
		case <-tick:
			p.drain(nil, 1, p.queueLen())
		case <-p.wake:
			p.drain(nil, p.cfg.MaxExportBatchSize, p.queueLen())
			// The drain is bounded; re-arm if a full batch is still queued.
			if p.queueLen() >= p.cfg.MaxExportBatchSize {
				select {
				case p.wake <- struct{}{}:
				default:
				}
			}
		case req := <-p.flushCh:
			req.reply <- p.drain(req.ctx, 1, req.limit)
		}
	}
}

// drain exports batches while at least min spans are queued. If ctx is
// non-nil it is checked between batches. If limit >= 0, at most limit spans
// are exported in total.
func (p *Processor) drain(ctx context.Context, min, limit int) error {
	for limit != 0 {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		select {
		case <-p.stop:
			return nil
		default:
		}
		batch := p.take(min, limit)
		if batch == nil {
			return nil
		}
		p.export(batch)
		if limit >= 0 {
			limit -= len(batch)
		}
	}
	return nil
}

func (p *Processor) take(min, limit int) []sdktrace.ReadOnlySpan {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.queue)
	if n == 0 || n < min {
		return nil
	}
	if n > p.cfg.MaxExportBatchSize {
		n = p.cfg.MaxExportBatchSize
	}
	if limit >= 0 && n > limit {
		if limit < min {
			return nil // never export a partial batch on a size trigger
		}
		n = limit
	}
	batch := make([]sdktrace.ReadOnlySpan, n)
	copy(batch, p.queue[:n])
	rest := copy(p.queue, p.queue[n:])
	clear(p.queue[rest:])
	p.queue = p.queue[:rest]
	return batch
}

func (p *Processor) export(batch []sdktrace.ReadOnlySpan) {
	ctx, cancel := context.WithTimeout(p.baseCtx, p.cfg.ExportTimeout)
	defer cancel()
	if err := p.exp.ExportSpans(ctx, batch); err != nil {
		otel.Handle(err)
	}
}

// queueLen returns the current queue length. Timer and wake drains are bounded
// by it so the worker returns to its select loop (and serves flushes) under
// continuous load.
func (p *Processor) queueLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

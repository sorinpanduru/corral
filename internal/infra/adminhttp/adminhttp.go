// Package adminhttp is the admin HTTP listener: /healthz, /readyz and
// /metrics on a dedicated mux (FR-15..FR-18, FR-35).
//
// Security: the listener is unauthenticated. Every response body is a fixed
// string; nothing from the request (path, query, headers) and no
// caller-supplied readiness text is ever echoed.
package adminhttp

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/infra/config"
)

// DefaultAddr is the FR-16 default admin bind address.
const DefaultAddr = "127.0.0.1:9187"

const (
	contentTypePlain  = "text/plain; charset=utf-8"
	readHeaderTimeout = 5 * time.Second
)

// Options configures a Server.
type Options struct {
	Addr      string
	Gatherer  prometheus.Gatherer
	Readiness health.Readiness
	ErrorLog  *log.Logger
}

// Server is the admin HTTP server.
type Server struct {
	opts     Options
	srv      *http.Server
	handler  http.Handler
	ln       atomic.Pointer[net.Listener]
	started  atomic.Bool
	stopping atomic.Bool
}

// New builds a Server. It does not bind.
func New(opts Options) *Server {
	if opts.Addr == "" {
		opts.Addr = DefaultAddr
	}
	s := &Server{opts: opts}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /metrics", s.metricsHandler())

	// Exact paths only. The pre-filter also prevents the mux's path-cleaning
	// redirects, which would echo the request path in a Location header.
	s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz", "/metrics":
			mux.ServeHTTP(w, r)
		default:
			writePlain(w, http.StatusNotFound, "not found\n")
		}
	})

	s.srv = &http.Server{
		Addr:              opts.Addr,
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          opts.ErrorLog,
	}
	return s
}

func (s *Server) metricsHandler() http.Handler {
	g := s.opts.Gatherer
	if g == nil {
		g = prometheus.NewRegistry()
	}
	// EnableOpenMetrics stays false so exemplars are never exposed.
	return promhttp.HandlerFor(g, promhttp.HandlerOpts{
		EnableOpenMetrics: false,
		ErrorLog:          s.opts.ErrorLog, // may be nil; promhttp tolerates nil
		ErrorHandling:     promhttp.HTTPErrorOnError,
	})
}

func writePlain(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", contentTypePlain)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body)) // HEAD discards the body in net/http
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writePlain(w, http.StatusOK, "ok\n")
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	reason := s.readiness(r.Context())
	if reason == "" {
		writePlain(w, http.StatusOK, "ok\n")
		return
	}
	// reason is always one of health.Reasons(): a fixed string.
	writePlain(w, http.StatusServiceUnavailable, string(reason)+"\n")
}

// readiness returns "" when ready, else an in-set reason.
func (s *Server) readiness(ctx context.Context) health.Reason {
	if s.stopping.Load() {
		return health.ReasonShuttingDown
	}
	if !s.started.Load() {
		return health.ReasonStarting
	}
	if s.opts.Readiness == nil {
		return ""
	}
	st := s.opts.Readiness.Readiness(ctx)
	if st.Ready {
		return ""
	}
	for _, r := range health.Reasons() {
		if st.Reason == r {
			return r
		}
	}
	return health.ReasonUnknown
}

// Listen binds the admin address. Failure is a config error for
// admin.listen that wraps the net error (errors.Is works) and carries no
// address text.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return config.NewError("admin.listen", err)
	}
	s.ln.Store(&ln)
	return nil
}

// Serve serves on the bound listener until Shutdown.
func (s *Server) Serve() error {
	lp := s.ln.Load()
	if lp == nil {
		return config.NewError("admin.listen", nil)
	}
	return s.srv.Serve(*lp)
}

// Addr returns the bound address, or nil before Listen.
func (s *Server) Addr() net.Addr {
	if lp := s.ln.Load(); lp != nil {
		return (*lp).Addr()
	}
	return nil
}

// MarkStarted ends the "starting" phase (ADR-0006 step 9).
func (s *Server) MarkStarted() { s.started.Store(true) }

// MarkStopping makes /readyz return shutting_down; effective on return.
func (s *Server) MarkStopping() { s.stopping.Store(true) }

// Handler returns the admin handler.
func (s *Server) Handler() http.Handler { return s.handler }

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

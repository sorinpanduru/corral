package upstreamconn

import (
	"context"
	"io"
	"sync"

	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/domain/wire"
)

// Checker runs the monitoring checks (FR-37). It keeps one monitoring
// connection per host (appName corral-monitor), authenticates only when a
// request asks for it, and opens a fresh connection when asked.
type Checker struct {
	cfg  Config
	obs  upstream.Observer
	auth *authenticator

	mu    sync.Mutex
	hosts map[string]*monitorConn
}

type monitorConn struct {
	mu sync.Mutex // serialises checks of one host
	c  *Conn
}

// NewChecker returns the monitor's Checker. The concrete type also implements
// io.Closer, which closes every monitoring connection.
func NewChecker(cfg Config, obs upstream.Observer) upstream.Checker {
	if obs == nil {
		obs = upstream.Noop{}
	}
	return &Checker{cfg: cfg, obs: obs, auth: newAuthenticator(cfg), hosts: map[string]*monitorConn{}}
}

var _ io.Closer = (*Checker)(nil)

// Close closes all monitoring connections, reporting each as closed with
// reason shutdown.
func (k *Checker) Close() error {
	k.mu.Lock()
	hosts := make(map[string]*monitorConn, len(k.hosts))
	for h, m := range k.hosts {
		hosts[h] = m
	}
	k.mu.Unlock()
	for h, m := range hosts {
		m.mu.Lock()
		m.drop(k, context.Background(), h, pool.CloseShutdown)
		m.mu.Unlock()
	}
	return nil
}

// CloseHost closes the monitoring connection to a host the monitor no longer
// tracks (an arbiter, for example), with the given reason (idle). A member that
// reports a different replica-set name is still re-checked by the monitor, so
// its connection is kept and this is not called for it.
func (k *Checker) CloseHost(host string, reason pool.CloseReason) {
	k.mu.Lock()
	m := k.hosts[host]
	k.mu.Unlock()
	if m == nil {
		return
	}
	m.mu.Lock()
	m.drop(k, context.Background(), host, reason)
	m.mu.Unlock()
}

func (k *Checker) monitor(host string) *monitorConn {
	k.mu.Lock()
	defer k.mu.Unlock()
	m := k.hosts[host]
	if m == nil {
		m = &monitorConn{}
		k.hosts[host] = m
	}
	return m
}

// drop closes the monitoring connection and reports it. The reason is a
// pool.CloseReason, so only values of the closed enum can be reported.
func (m *monitorConn) drop(k *Checker, ctx context.Context, host string, reason pool.CloseReason) {
	if m.c != nil {
		m.c.Close(reason)
		m.c = nil
		k.obs.MonitorConnClosed(ctx, host, string(reason))
	}
}

// Check runs one check of req.Host within ConnectTimeout.
func (k *Checker) Check(ctx context.Context, req upstream.CheckRequest) upstream.Report {
	host := req.Host
	rep := upstream.Report{Host: host}
	ctx, cancel := context.WithTimeout(ctx, k.cfg.ConnectTimeout)
	defer cancel()

	m := k.monitor(host)
	m.mu.Lock()
	defer m.mu.Unlock()

	if req.Fresh {
		m.drop(k, ctx, host, pool.CloseError) // a fresh check replaces the old connection
	}
	var hr wire.HelloReply
	var err error
	if m.c == nil {
		var c *Conn
		c, err = connect(ctx, k.cfg, host)
		if err == nil {
			hr, err = c.hello(ctx, k.cfg, "corral-monitor")
			if err != nil {
				c.Close(pool.CloseError)
			}
		}
		if err != nil {
			k.obs.MonitorConnFailed(ctx, host, err)
			rep.Err = err
			return rep
		}
		m.c = c
		k.obs.MonitorConnOpened(ctx, host, c.id)
	} else {
		hr, err = m.c.hello(ctx, k.cfg, "corral-monitor")
		if err != nil {
			m.drop(k, ctx, host, pool.CloseError)
			k.obs.MonitorConnFailed(ctx, host, err)
			rep.Err = err
			return rep
		}
	}
	fillReport(&rep, hr)

	if req.Authenticate && k.auth.enabled() && !m.c.authed {
		if aerr := k.auth.authenticate(ctx, m.c); aerr != nil {
			rep.AuthErr = aerr
			if upstream.IsAuthFailed(aerr) {
				k.obs.AuthFailed(ctx, "monitor", host, aerr)
			} else {
				m.drop(k, ctx, host, pool.CloseError)
			}
		} else {
			k.obs.AuthSucceeded(ctx, "monitor", host)
		}
	}
	return rep
}

func fillReport(r *upstream.Report, h wire.HelloReply) {
	r.IsWritablePrimary = h.IsWritablePrimary
	r.Secondary = h.Secondary
	r.ArbiterOnly = h.ArbiterOnly
	r.IsMongos = h.IsMongos
	r.SetName, r.HasSetName = h.SetName, h.SetName != ""
	r.Hosts, r.Passives, r.Primary = h.Hosts, h.Passives, h.Primary
	r.ElectionID, r.HasElectionID = h.ElectionID, h.HasElectionID
	r.SetVersion, r.HasSetVersion = int64(h.SetVersion), h.HasSetVersion
	r.Params = upstream.Params{
		MaxBsonObjectSize: h.MaxBsonObjectSize, MaxMessageSizeBytes: h.MaxMessageSizeBytes,
		MaxWriteBatchSize: h.MaxWriteBatchSize, LogicalSessionTimeoutMinutes: h.LogicalSessionTimeoutMinutes,
		HasLSTM: h.HasLSTM, MinWire: h.MinWire, MaxWire: h.MaxWire,
	}
}

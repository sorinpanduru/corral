package faultproxy

import (
	"net"
	"sync"
	"time"
)

type pendingReq struct {
	command string
	db      string
	fault   *Fault // reply-side fault to apply
}

type pconn struct {
	p      *Proxy
	idx    int
	client net.Conn
	up     net.Conn
	done   chan struct{}
	once   sync.Once

	// guarded by p.mu
	appName    string
	open       bool
	black      bool
	blackholed bool
	pending    map[int32]pendingReq
}

func (c *pconn) close(reset bool) {
	c.once.Do(func() {
		if tc, ok := c.client.(*net.TCPConn); ok && reset {
			_ = tc.SetLinger(0)
		}
		_ = c.client.Close()
		c.p.mu.Lock()
		up := c.up
		c.open = false
		c.p.mu.Unlock()
		if up != nil {
			if tc, ok := up.(*net.TCPConn); ok && reset {
				_ = tc.SetLinger(0)
			}
			_ = up.Close()
		}
		close(c.done)
	})
}

func (c *pconn) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-c.done:
		return false
	}
}

func (c *pconn) isBlack() bool { c.p.mu.Lock(); defer c.p.mu.Unlock(); return c.black }

func (c *pconn) run(black bool) {
	if black {
		c.p.mu.Lock()
		c.black = true
		c.p.mu.Unlock()
		// Read and discard until the client goes away.
		for {
			if _, err := readFrame(c.client); err != nil {
				c.close(false)
				return
			}
		}
	}
	up, err := net.DialTimeout("tcp", c.p.upstream, 5*time.Second)
	if err != nil {
		c.close(true)
		return
	}
	c.p.mu.Lock()
	c.up = up
	closed := !c.open
	c.p.mu.Unlock()
	if closed {
		_ = up.Close()
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.pumpRequests() }()
	go func() { defer wg.Done(); c.pumpReplies() }()
	wg.Wait()
	c.close(false)
}

func (c *pconn) pumpRequests() {
	for {
		raw, err := readFrame(c.client)
		if err != nil {
			c.close(false)
			return
		}
		in := parseFrame(raw, true)
		c.p.mu.Lock()
		if isHello(in.command) && c.appName == "" && in.parsed {
			if v, err := in.body.LookupErr("client", "application", "name"); err == nil {
				c.appName, _ = v.StringValueOK()
			}
		}
		app := c.appName
		if in.flags&flagMoreToCome == 0 || in.opcode != opMsg {
			// moreToCome requests get no reply: nothing to correlate.
			c.pending[in.requestID] = pendingReq{command: in.command, db: in.db}
		}
		c.p.mu.Unlock()
		if in.parsed {
			c.p.record(Record{
				ConnIndex: c.idx, AppName: app, Direction: ToUpstream, Command: in.command, DB: in.db,
				LsidID: in.lsid, HasTxnNumber: in.hasTxn, TxnNumber: in.txn, StartTxn: in.startTxn,
				RequestID: in.requestID, Flags: in.flags, Time: time.Now(),
			})
		}
		if c.isBlack() {
			continue
		}
		if !c.p.waitResumed(c.done) {
			return
		}
		f := c.p.matchFault(in.command, app, c.idx)
		resetAfter := false
		if f != nil {
			switch f.kind {
			case fkResetAfterRequest:
				resetAfter = true
			case fkDelay:
				if !c.sleep(f.d) {
					return
				}
			case fkDelayReply:
				c.p.mu.Lock()
				pr := c.pending[in.requestID]
				pr.fault = f
				c.pending[in.requestID] = pr
				c.p.mu.Unlock()
			}
		}
		if _, err := c.up.Write(raw); err != nil {
			c.close(true)
			return
		}
		if resetAfter {
			c.close(true)
			return
		}
	}
}

func (c *pconn) pumpReplies() {
	for {
		raw, err := readFrame(c.up)
		if err != nil {
			c.close(false)
			return
		}
		in := parseFrame(raw, false)
		c.p.mu.Lock()
		pr := c.pending[in.responseTo]
		if in.parsed && in.flags&flagMoreToCome != 0 {
			c.pending[in.requestID] = pr // exhaust stream: next reply answers this one
		}
		delete(c.pending, in.responseTo)
		app := c.appName
		c.p.mu.Unlock()
		if in.parsed {
			ok := false
			if v, e := in.body.LookupErr("ok"); e == nil {
				f, _ := v.AsFloat64OK()
				ok = f == 1
			}
			c.p.record(Record{
				ConnIndex: c.idx, AppName: app, Direction: ToClient, Command: pr.command, DB: pr.db,
				RequestID: in.requestID, ResponseTo: in.responseTo, Flags: in.flags, OK: ok, Time: time.Now(),
			})
		}
		if c.isBlack() {
			continue
		}
		if !c.p.waitResumed(c.done) {
			return
		}
		if pr.fault != nil && pr.fault.kind == fkDelayReply {
			if !c.sleep(pr.fault.d) {
				return
			}
		}
		if in.parsed && isHello(pr.command) {
			raw = c.p.rewriteHello(raw, in, c.p.addr)
		}
		if _, err := c.client.Write(raw); err != nil {
			c.close(false)
			return
		}
	}
}

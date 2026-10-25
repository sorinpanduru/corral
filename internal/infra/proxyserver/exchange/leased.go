package exchange

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/domain/wire"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/spans"
	"github.com/sorinpanduru/corral/internal/infra/upstreamnet"
)

// maxBarrierReply bounds the barrier ping reply.
const maxBarrierReply = 1 << 20

func serverAddr(host string) netip.AddrPort {
	ap, _ := netip.ParseAddrPort(host)
	return ap
}

// watch watches the client connection while the exchange waits for the
// upstream (FR-41). stop ends it and reports whether the client went away; if
// it did, the upstream read is interrupted.
func (x *msg) watch(nc net.Conn) (stop func() bool) {
	stopCh := make(chan struct{})
	done := make(chan struct{})
	var gone atomic.Bool
	closed := upstreamnet.WatchClose(x.s.cc.Conn, stopCh)
	go func() {
		defer close(done)
		select {
		case <-closed:
			gone.Store(true)
			_ = nc.SetReadDeadline(time.Unix(1, 0))
		case <-x.s.ctx.Done(): // the drain ended: the exchange is cut (FR-47)
			gone.Store(true)
			_ = nc.SetReadDeadline(time.Unix(1, 0))
		case <-stopCh:
		}
	}()
	return func() bool {
		close(stopCh)
		<-done
		if !gone.Load() {
			_ = nc.SetReadDeadline(time.Time{})
		}
		return gone.Load()
	}
}

// clientGone ends the exchange for a client that disconnected while its
// request was in flight (FR-41): the upstream connection is discarded.
func (x *msg) clientGone(lease Lease, rt spans.Step, ex spans.Exchange) (proxy.CloseReason, bool) {
	lease.Discard(pool.CloseClientAbandoned)
	err := io.EOF
	rt.End(err, nil)
	ex.End(err, nil)
	x.complete(proxy.OutcomeProxyError, err, false)
	return proxy.CloseClientClosed, false
}

// lateFail is a validation failure after part of the message went upstream
// (FR-38): the held-back byte is never written, the upstream connection is
// discarded and the client is closed.
func (x *msg) lateFail(lease Lease, rt spans.Step, ex spans.Exchange, err error) (proxy.CloseReason, bool) {
	lease.Discard(pool.CloseClientAbandoned)
	rt.End(err, nil)
	ex.End(err, nil)
	r, more := x.protocolFail(err)
	return r, more
}

// clientReadFailUp is a client read failure while streaming the request: the
// upstream holds a partial message and is discarded.
func (x *msg) clientReadFailUp(lease Lease, rt spans.Step, ex spans.Exchange, err error) (proxy.CloseReason, bool) {
	lease.Discard(pool.CloseClientAbandoned)
	rt.End(err, nil)
	ex.End(err, nil)
	return x.clientIOFail(err)
}

func (x *msg) leased(ctx context.Context, ex spans.Exchange, lease Lease) (proxy.CloseReason, bool) {
	s := x.s
	up := lease.Upstream()
	nc := up.NetConn()
	rt := ex.RoundTrip(ctx, serverAddr(up.Host()), up.ID())

	flags := x.req.Flags
	reqID := up.Correlator().Next()
	if flags&wire.FlagMoreToCome != 0 {
		// No reply will ever match this ID: do not leave it outstanding.
		up.Correlator().Forget(reqID)
	}
	hdr, _ := wire.RewriteRequestHeader(x.prefix[:20], reqID)
	forwardEnd := int64(x.h.Length)
	if flags&wire.FlagChecksumPresent != 0 {
		forwardEnd -= 4 // the checksum is stripped, never forwarded
	}
	heldOff := forwardEnd - 1
	var held byte
	pos := int64(20)
	// emit writes the part of data (message bytes from offset pos) that may go
	// upstream now: everything before the held-back last byte.
	emit := func(data []byte) error {
		end := pos + int64(len(data))
		if heldOff >= pos && heldOff < end {
			held = data[heldOff-pos]
		}
		w := min(end, heldOff) - pos
		pos = end
		if w > 0 {
			_, err := nc.Write(data[:w])
			return err
		}
		return nil
	}

	if _, err := nc.Write(hdr[:]); err != nil {
		return x.upstreamFail(lease, rt, ex, errorf("write to the upstream failed", err))
	}
	if err := emit(x.prefix[20:]); err != nil {
		return x.upstreamFail(lease, rt, ex, errorf("write to the upstream failed", err))
	}
	for x.remaining > 0 {
		n := int64(len(s.buf))
		if n > ForwardChunk {
			n = ForwardChunk
		}
		if n > x.remaining {
			n = x.remaining
		}
		chunk := s.buf[:n]
		if err := s.readBudget(chunk); err != nil {
			return x.clientReadFailUp(lease, rt, ex, err)
		}
		x.remaining -= n
		if err := x.v.Write(chunk); err != nil {
			return x.lateFail(lease, rt, ex, err)
		}
		if err := emit(chunk); err != nil {
			return x.upstreamFail(lease, rt, ex, errorf("write to the upstream failed", err))
		}
	}
	if x.v != nil {
		if err := x.v.Close(); err != nil || !x.v.Done() {
			if err == nil {
				err = wire.ErrMalformed()
			}
			return x.lateFail(lease, rt, ex, err)
		}
	}
	if _, err := nc.Write([]byte{held}); err != nil {
		return x.upstreamFail(lease, rt, ex, errorf("write to the upstream failed", err))
	}

	if flags&wire.FlagMoreToCome != 0 {
		rt.End(nil, nil)
		ex.End(nil, nil)
		x.barrier(lease, up)
		x.complete(proxy.OutcomeOK, nil, false)
		return "", true
	}
	return x.reply(lease, up, rt, ex)
}

// barrier is the FR-29 ping after a moreToCome request. It has no timeout of
// its own and does not watch the client (Amendment 1, G-12): only an abort of
// the lease interrupts it. On any failure the upstream connection is discarded
// and the client connection stays open.
func (x *msg) barrier(lease Lease, up Upstream) {
	d := &x.s.e.d
	nc := up.NetConn()
	done := make(chan struct{})
	var aborted atomic.Bool
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-lease.Aborted():
			aborted.Store(true)
			_ = nc.SetReadDeadline(time.Unix(1, 0))
		case <-done:
		}
	}()
	end := func() { close(done); <-finished }

	id := up.Correlator().Next()
	_ = nc.SetReadDeadline(time.Time{})
	fail := func(err error) {
		lease.Discard(pool.CloseError)
		x.networkError(up.Host(), err)
		d.Obs.UpstreamBarrierFailed(x.s.ctx, up.Host(), err)
		if isProtocolErr(err) {
			d.Obs.UpstreamProtocolError(x.s.ctx, up.Host(), err)
		}
	}
	if _, err := nc.Write(wire.BuildMsg(id, 0, 0, wire.PingBody())); err != nil {
		end()
		fail(errorf("barrier write to the upstream failed", err))
		return
	}
	h, scanner, err := readOwnReply(nc, up)
	end()
	if err != nil || aborted.Load() {
		if err == nil {
			err = errors.New("barrier interrupted by a lease abort")
		}
		fail(err)
		return
	}
	_ = h
	f := scanner.Fields()
	marked := false
	if f.HasCode && d.ReplyCodes != nil {
		marked = d.ReplyCodes(x.s.ctx, up.Host(), upstream.Barrier, upstream.ReplyCodeInfo{
			HasTop: true, OKZero: f.IsError(), Top: f.Code, TopName: f.CodeName})
	}
	if marked || f.IsError() {
		fail(errors.New("barrier ping reply was an error"))
		return
	}
	_ = nc.SetReadDeadline(time.Time{})
	lease.Return()
}

// readOwnReply reads and validates a small reply (the barrier ping).
func readOwnReply(nc net.Conn, up Upstream) (wire.Header, *wire.ReplyScanner, error) {
	var hb [16]byte
	if _, err := io.ReadFull(nc, hb[:]); err != nil {
		return wire.Header{}, nil, err
	}
	h := wire.ParseHeader(hb[:])
	limit := up.MaxMessageSize()
	if limit > maxBarrierReply {
		limit = maxBarrierReply
	}
	if err := wire.CheckReplyHeader(h, limit); err != nil {
		return h, nil, err
	}
	if err := up.Correlator().Match(h.ResponseTo); err != nil {
		return h, nil, err
	}
	body := make([]byte, int(h.Length)-16)
	if _, err := io.ReadFull(nc, body); err != nil {
		return h, nil, err
	}
	sc := wire.NewReplyScanner(h)
	if err := sc.Write(body); err != nil {
		return h, nil, err
	}
	if err := sc.Close(); err != nil {
		return h, nil, err
	}
	return h, sc, nil
}

// reply streams the upstream reply to the client (FR-58, FR-57).
func (x *msg) reply(lease Lease, up Upstream, rt spans.Step, ex spans.Exchange) (proxy.CloseReason, bool) {
	s := x.s
	d := &s.e.d
	nc := up.NetConn()
	stopWatch := x.watch(nc)
	var head [20]byte
	_, err := io.ReadFull(nc, head[:])
	gone := stopWatch()
	if err != nil {
		if gone {
			return x.clientGone(lease, rt, ex)
		}
		return x.upstreamFail(lease, rt, ex, errorf("read from the upstream failed", err))
	}
	h := wire.ParseHeader(head[:16])
	if err := wire.CheckReplyHeader(h, up.MaxMessageSize()); err != nil {
		return x.upstreamFail(lease, rt, ex, err)
	}
	if err := up.Correlator().Match(h.ResponseTo); err != nil {
		return x.upstreamFail(lease, rt, ex, err)
	}
	scanner := wire.NewReplyScanner(h)
	if err := scanner.Write(head[16:]); err != nil { // the flags: moreToCome and unknown bits
		return x.upstreamFail(lease, rt, ex, err)
	}
	rew := wire.RewriteReplyHeader(head[:], s.ids.Next(), x.req.RequestID)
	strip := 0
	if uint32(head[16])&wire.FlagChecksumPresent != 0 {
		strip = 4
	}

	// Each validated chunk goes straight to the client from rb. Only the last
	// chunk is held until the whole reply has validated, so a reply with a bad
	// tail never reaches the client complete (FR-58). The chunk sizes keep at
	// least four bytes in the last chunk so a stripped checksum never straddles.
	out := &replyOut{s: s, head: rew[:], budget: proxy.NewIOBudget(d.IOTimeout, d.Clock)}
	rb := make([]byte, ReplyChunk)
	total := int64(h.Length) - 20
	var last []byte
	for total > 0 {
		n := int64(len(rb))
		if n > total {
			n = total
		}
		if rem := total - n; strip > 0 && rem > 0 && rem < int64(strip)+1 {
			n = total - int64(strip) - 1 // the last chunk keeps a body byte with the checksum
		}
		if _, err := io.ReadFull(nc, rb[:n]); err != nil {
			return x.upstreamFail(lease, rt, ex, errorf("read from the upstream failed", err))
		}
		if err := scanner.Write(rb[:n]); err != nil {
			return x.upstreamFail(lease, rt, ex, err)
		}
		total -= n
		if d.Probe != nil {
			d.Probe.ReplyHeld(s.cc.ID, int(n))
		}
		if total == 0 {
			last = rb[:n]
			break
		}
		if err := out.write(rb[:n]); err != nil {
			return x.replyWriteFail(lease, rt, ex, err)
		}
	}
	if err := scanner.Close(); err != nil {
		return x.upstreamFail(lease, rt, ex, err)
	}
	if len(last) < strip {
		return x.upstreamFail(lease, rt, ex, wire.ErrMalformed())
	}
	if err := out.write(last[:len(last)-strip]); err != nil {
		return x.replyWriteFail(lease, rt, ex, err)
	}
	f := scanner.Fields()
	lease.Return()
	if d.ReplyCodes != nil && (f.HasCode || f.HasWCECode) {
		d.ReplyCodes(s.ctx, up.Host(), upstream.Forwarded, upstream.ReplyCodeInfo{
			HasTop: f.HasCode, OKZero: f.IsError(), Top: f.Code, TopName: f.CodeName,
			HasWCE: f.HasWCECode, WCE: f.WCECode})
	}
	rt.End(nil, &f)
	ex.End(nil, &f)
	x.complete(proxy.OutcomeForReply(f), nil, false)
	return "", true
}

// replyWriteFail: writing the reply to the client failed. The upstream reply is
// only partly read, so its connection is discarded.
func (x *msg) replyWriteFail(lease Lease, rt spans.Step, ex spans.Exchange, err error) (proxy.CloseReason, bool) {
	lease.Discard(pool.CloseClientAbandoned)
	rt.End(err, nil)
	ex.End(err, nil)
	return x.clientIOFail(err)
}

// replyOut writes a reply to the client. The 20-byte header goes out with the
// first write. The whole reply shares one write budget (FR-57): only time spent
// inside a client write is charged, so waiting for the upstream is free but a
// slow-reading client cannot hold the lease for longer than the I/O timeout in
// total.
type replyOut struct {
	s      *session
	head   []byte // pending header, nil once written
	budget *proxy.IOBudget
}

func (o *replyOut) write(b []byte) error {
	if o.head == nil && len(b) == 0 {
		return nil
	}
	_ = o.s.cc.Conn.SetWriteDeadline(o.budget.BeginRead())
	defer o.budget.EndRead()
	if o.head != nil {
		bufs := net.Buffers{o.head, b}
		o.head = nil
		_, err := bufs.WriteTo(o.s.cc.Conn)
		return err
	}
	_, err := o.s.cc.Conn.Write(b)
	return err
}

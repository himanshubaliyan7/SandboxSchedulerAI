package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
)

const (
	// relayBufSize is the per-direction tunnel buffer.
	relayBufSize = 32 << 10
	// minReadBuf lets a full TLS record (the ClientHello) be peeked even
	// when MaxHeaderBytes is configured smaller.
	minReadBuf = 18 << 10
	// errorWriteTimeout bounds writing a refusal to the client.
	errorWriteTimeout = 5 * time.Second
	// maxDialAttempts caps how many resolved addresses are tried.
	maxDialAttempts = 4
	// maxResolved caps how many resolver results are considered.
	maxResolved = 16
)

var relayBufs = sync.Pool{New: func() any { b := make([]byte, relayBufSize); return &b }}

// activity is the last time bytes moved on either side of a flow.
type activity struct{ last atomic.Int64 }

func (a *activity) touch()               { a.last.Store(time.Now().UnixNano()) }
func (a *activity) since() time.Duration { return time.Duration(time.Now().UnixNano() - a.last.Load()) }

// idleConn applies an idle timeout shared between the two sides of a flow
// and counts bytes. While auto is false it leaves deadlines to the caller.
type idleConn struct {
	net.Conn
	idle time.Duration
	act  *activity
	auto atomic.Bool
	rd   *atomic.Int64 // bytes read, may be nil
	wr   *atomic.Int64 // bytes written, may be nil
}

func (c *idleConn) Read(p []byte) (int, error) {
	for {
		auto := c.auto.Load()
		if auto {
			_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
		}
		n, err := c.Conn.Read(p)
		if n > 0 {
			c.act.touch()
			if c.rd != nil {
				c.rd.Add(int64(n))
			}
		}
		// This side was quiet, but the flow is idle only if the other
		// side was too.
		if n == 0 && auto && isTimeout(err) && c.act.since() < c.idle {
			continue
		}
		return n, err
	}
}

func (c *idleConn) Write(p []byte) (int, error) {
	if c.auto.Load() {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	}
	// Count before the write and correct afterwards, so the counter can
	// never lag behind bytes the peer has already answered.
	if c.wr != nil {
		c.wr.Add(int64(len(p)))
	}
	n, err := c.Conn.Write(p)
	if c.wr != nil && n < len(p) {
		c.wr.Add(int64(n - len(p)))
	}
	if n > 0 {
		c.act.touch()
	}
	return n, err
}

func (c *idleConn) closeWrite() {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// conn is one client connection from a sandbox.
type conn struct {
	sb  *sandbox
	m   *Manager
	raw *net.UnixConn
	ic  *idleConn
	br  *bufio.Reader
}

func newConn(sb *sandbox, uc *net.UnixConn) *conn {
	act := &activity{}
	act.touch()
	return &conn{
		sb:  sb,
		m:   sb.m,
		raw: uc,
		ic:  &idleConn{Conn: uc, idle: sb.m.cfg.IdleTimeout, act: act},
	}
}

// rejectOverLimit runs on the accept goroutine: a short write to a fresh
// socket does not block, and no goroutine is spent on a flood.
func (c *conn) rejectOverLimit() {
	_ = c.raw.SetWriteDeadline(time.Now().Add(time.Second))
	c.sb.record(Decision{Verdict: VerdictDenied, Reason: ReasonConnLimit, Detail: "too many concurrent connections"})
	_, _ = io.WriteString(c.raw, httpErrorText(503, VerdictDenied, ReasonConnLimit, "too many concurrent connections"))
	c.raw.Close()
}

func (c *conn) serve() {
	defer c.sb.wg.Done()
	defer c.sb.removeConn(c)
	defer c.raw.Close()

	size := c.m.cfg.MaxHeaderBytes
	if size < minReadBuf {
		size = minReadBuf
	}
	c.br = bufio.NewReaderSize(c.ic, size)

	start := time.Now()
	_ = c.raw.SetReadDeadline(start.Add(c.m.cfg.HeaderTimeout))
	first, err := c.br.Peek(1)
	if err != nil {
		if isTimeout(err) {
			c.sb.record(Decision{Verdict: VerdictError, Reason: ReasonHeaderTimeout, Duration: time.Since(start)})
		}
		return
	}
	if first[0] == socksVersion {
		c.serveSOCKS(start)
		return
	}
	c.serveHTTP(start)
}

// refusal is why a request did not get an upstream connection, and how to
// tell the client.
type refusal struct {
	verdict Verdict
	reason  Reason
	detail  string
	ip      netip.Addr
	status  int  // HTTP status
	socks   byte // SOCKS5 reply code
}

func denied(reason Reason, detail string) *refusal {
	return &refusal{verdict: VerdictDenied, reason: reason, detail: detail, status: 403, socks: socksNotAllowed}
}

func (r *refusal) decision(proto string, t target, start time.Time) Decision {
	return Decision{
		Protocol: proto, Host: t.host, Port: t.port, ResolvedIP: r.ip,
		Verdict: r.verdict, Reason: r.reason, Detail: r.detail, Duration: time.Since(start),
	}
}

// flow is one established upstream connection: a tunnel or a single
// plain-HTTP exchange.
type flow struct {
	c        *conn
	proto    string
	t        target
	ip       netip.Addr
	start    time.Time
	upstream *idleConn
	ctx      context.Context
	cancel   context.CancelFunc
	lifetime *time.Timer
	up, down atomic.Int64
	end      atomic.Pointer[Reason]
}

// setEnd records why the flow ended; the first reason wins.
func (f *flow) setEnd(r Reason) { f.end.CompareAndSwap(nil, &r) }

// kill ends the flow from any goroutine by closing both of its sockets.
func (f *flow) kill(r Reason) {
	f.setEnd(r)
	f.cancel()
	f.upstream.Close()
	f.c.raw.Close()
}

func (f *flow) endReason() Reason {
	if r := f.end.Load(); r != nil {
		return *r
	}
	return ReasonClosed
}

// finish releases the flow and emits its decision.
func (f *flow) finish() {
	f.lifetime.Stop()
	f.cancel()
	f.upstream.Close()
	f.c.sb.removeFlow(f)
	d := Decision{
		Protocol: f.proto, Host: f.t.host, Port: f.t.port, ResolvedIP: f.ip,
		Verdict: VerdictAllowed, Reason: f.endReason(),
		BytesUp: f.up.Load(), BytesDown: f.down.Load(), Duration: time.Since(f.start),
	}
	switch d.Reason {
	case ReasonSNIMismatch, ReasonTLSMalformed:
		d.Verdict = VerdictDenied
	case ReasonUpstreamFailed:
		d.Verdict = VerdictError
	}
	f.c.sb.emit(d, true)
}

// authorize runs pipeline steps 1 to 4 for an already parsed target and
// returns the addresses that may be dialled, in resolver order.
func (m *Manager) authorize(ctx context.Context, pol *policy, t target) ([]netip.Addr, *refusal) {
	// Step 1.
	if pol.mode == core.NetworkNone {
		return nil, denied(ReasonModeNone, "network mode is none")
	}
	// Step 2 (the rest of it; parsing happened in the protocol handler).
	if m.ports != nil {
		if _, ok := m.ports[t.port]; !ok {
			return nil, denied(ReasonPortNotAllowed, fmt.Sprintf("port %d is not allowed", t.port))
		}
	}
	// Step 3.
	if reason, ok := pol.permitsTarget(t); !ok {
		detail := t.host + " is not in the allowlist"
		if reason == ReasonIPNotAllowed {
			detail = t.host + " is not in the allowed CIDRs"
		}
		return nil, denied(reason, detail)
	}
	// Step 4.
	var resolved []netip.Addr
	if t.isIP() {
		resolved = []netip.Addr{t.ip}
	} else {
		rctx, cancel := context.WithTimeout(ctx, m.cfg.ResolveTimeout)
		addrs, err := m.cfg.Resolver.LookupNetIP(rctx, "ip", t.host)
		cancel()
		if err != nil || len(addrs) == 0 {
			r := &refusal{verdict: VerdictError, reason: ReasonResolveFailed, detail: "cannot resolve " + t.host, status: 502, socks: socksHostUnreachable}
			if isTimeout(err) {
				r.status = 504
			}
			return nil, r
		}
		if len(addrs) > maxResolved {
			addrs = addrs[:maxResolved]
		}
		resolved = addrs
	}
	var (
		ok        []netip.Addr
		firstCat  string
		firstAddr netip.Addr
	)
	for _, a := range resolved {
		a = normalizeAddr(a)
		cat, bad := m.deny.check(a)
		if !bad && pol.deniesAddr(a) {
			cat, bad = catPolicy, true
		}
		if bad {
			if firstCat == "" {
				firstCat, firstAddr = cat, a
			}
			continue
		}
		dup := false
		for _, seen := range ok {
			if seen == a {
				dup = true
				break
			}
		}
		if !dup {
			ok = append(ok, a)
		}
	}
	if len(ok) == 0 {
		r := denied(ReasonDeniedAddress, fmt.Sprintf("%s resolves only to denied addresses (%s)", t.host, firstCat))
		if t.isIP() {
			r.detail = fmt.Sprintf("%s is a denied address (%s)", t.host, firstCat)
		}
		r.ip = firstAddr
		return nil, r
	}
	return ok, nil
}

// errDialDenied is returned by the dial Control hook.
type errDialDenied struct {
	addr string
	cat  string
}

func (e *errDialDenied) Error() string {
	return fmt.Sprintf("egress: refusing to connect to %s (%s)", e.addr, e.cat)
}

// dialValidated is pipeline step 5 for one address. addr must have passed
// authorize. The Control hook sees the address of the socket about to be
// connected and refuses anything that is not exactly addr or that is in
// the deny set.
func (m *Manager) dialValidated(ctx context.Context, pol *policy, addr netip.AddrPort, timeout time.Duration) (net.Conn, error) {
	addr = netip.AddrPortFrom(normalizeAddr(addr.Addr()), addr.Port())
	want, rewritten := addr, false
	if m.dialRewrite != nil {
		to, ok := m.dialRewrite(addr)
		if !ok {
			return nil, fmt.Errorf("egress: no test route for %s", addr)
		}
		want, rewritten = to, true
	}
	d := net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			got, err := netip.ParseAddrPort(address)
			if err != nil {
				return &errDialDenied{address, catInvalid}
			}
			got = netip.AddrPortFrom(normalizeAddr(got.Addr()), got.Port())
			if got != want {
				return &errDialDenied{address, "not the validated address"}
			}
			if rewritten {
				return nil
			}
			if cat, bad := m.deny.check(got.Addr()); bad {
				return &errDialDenied{address, cat}
			}
			if pol.deniesAddr(got.Addr()) {
				return &errDialDenied{address, catPolicy}
			}
			return nil
		},
	}
	network := "tcp4"
	if want.Addr().Is6() {
		network = "tcp6"
	}
	return d.DialContext(ctx, network, want.String())
}

// open runs the whole pipeline for t and, on success, returns a registered
// flow with a live upstream connection.
func (c *conn) open(proto string, t target, start time.Time) (*flow, *refusal) {
	pol := c.sb.policy.Load()
	addrs, ref := c.m.authorize(c.sb.ctx, pol, t)
	if ref != nil {
		return nil, ref
	}
	if len(addrs) > maxDialAttempts {
		addrs = addrs[:maxDialAttempts]
	}
	var (
		up      net.Conn
		ip      netip.Addr
		err     error
		dialEnd = time.Now().Add(c.m.cfg.DialTimeout)
	)
	for i, a := range addrs {
		left := time.Until(dialEnd)
		if left <= 0 {
			if err == nil {
				err = context.DeadlineExceeded
			}
			break
		}
		ip = a
		up, err = c.m.dialValidated(c.sb.ctx, pol, netip.AddrPortFrom(a, t.port), left/time.Duration(len(addrs)-i))
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, dialRefusal(err, t, ip)
	}
	ctx, cancel := context.WithCancel(c.sb.ctx)
	f := &flow{c: c, proto: proto, t: t, ip: ip, start: start, ctx: ctx, cancel: cancel}
	f.upstream = &idleConn{Conn: up, idle: c.m.cfg.IdleTimeout, act: c.ic.act, rd: &f.down, wr: &f.up}
	f.upstream.auto.Store(true)
	if reason, ok := c.sb.addFlow(f, pol); !ok {
		cancel()
		up.Close()
		r := denied(reason, "policy changed while connecting")
		r.ip = ip
		return nil, r
	}
	f.lifetime = time.AfterFunc(c.m.cfg.MaxTunnelLifetime, func() { f.kill(ReasonMaxLifetime) })
	return f, nil
}

func dialRefusal(err error, t target, ip netip.Addr) *refusal {
	var dd *errDialDenied
	if errors.As(err, &dd) {
		r := denied(ReasonDeniedAddress, fmt.Sprintf("%s is a denied address (%s)", ip, dd.cat))
		r.ip = ip
		return r
	}
	r := &refusal{verdict: VerdictError, reason: ReasonDialFailed, ip: ip, status: 502, socks: socksGeneralFailure,
		detail: "cannot connect to " + t.String()}
	switch {
	case isTimeout(err):
		r.reason, r.status, r.socks = ReasonDialTimeout, 504, socksTTLExpired
		r.detail = "timed out connecting to " + t.String()
	case errors.Is(err, syscall.ECONNREFUSED):
		r.socks = socksConnRefused
		r.detail = "connection refused by " + t.String()
	case errors.Is(err, syscall.ENETUNREACH):
		r.socks = socksNetUnreachable
	case errors.Is(err, syscall.EHOSTUNREACH):
		r.socks = socksHostUnreachable
	}
	return r
}

// tunnel relays bytes between the client and f.upstream until both
// directions are done, then finishes the flow.
func (c *conn) tunnel(f *flow) {
	defer f.finish()
	c.ic.act.touch()
	c.ic.auto.Store(true)

	down := make(chan struct{})
	go func() {
		defer close(down)
		if err := relay(c.ic, f.upstream); err != nil {
			f.kill(relayEnd(err))
			return
		}
		c.ic.closeWrite()
	}()

	if !c.m.cfg.DisableSNICheck && !f.t.isIP() {
		if reason, alert := c.checkSNI(f.t.host); reason != "" {
			_, _ = c.ic.Write(alert)
			f.kill(reason)
			<-down
			return
		}
	}
	if err := relay(f.upstream, c.br); err != nil {
		f.kill(relayEnd(err))
	} else {
		f.upstream.closeWrite()
	}
	<-down
}

// relay copies src to dst through a fixed buffer. It returns nil on a
// clean end of stream.
func relay(dst io.Writer, src io.Reader) error {
	bp := relayBufs.Get().(*[]byte)
	defer relayBufs.Put(bp)
	buf := *bp
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}

func relayEnd(err error) Reason {
	if isTimeout(err) {
		return ReasonIdleTimeout
	}
	return ReasonIOError
}

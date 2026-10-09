package egress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxResponseHeaderBytes = 256 << 10

var (
	errHeaderTooLarge = errors.New("request header too large")
	errObsFold        = errors.New("folded header line")
)

// httpErrorText renders a complete proxy error response.
func httpErrorText(status int, v Verdict, reason Reason, detail string) string {
	body := fmt.Sprintf("egress proxy: %s: %s: %s\n", v, reason, detail)
	return fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\nX-Egress-Verdict: %s\r\nX-Egress-Reason: %s\r\n\r\n%s",
		status, http.StatusText(status), len(body), v, reason, body)
}

func (c *conn) httpError(status int, v Verdict, reason Reason, detail string) {
	_ = c.raw.SetWriteDeadline(time.Now().Add(errorWriteTimeout))
	_, _ = io.WriteString(c.raw, httpErrorText(status, v, reason, detail))
}

// headerEnd returns the length of the header block at the front of b,
// including its terminating blank line, or -1. It uses the same notion of
// a line as net/http: LF, optionally preceded by CR.
func headerEnd(b []byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' {
			continue
		}
		if i+1 < len(b) && b[i+1] == '\n' {
			return i + 2
		}
		if i+2 < len(b) && b[i+1] == '\r' && b[i+2] == '\n' {
			return i + 3
		}
	}
	return -1
}

// peekHead waits until a whole header block is buffered and returns a
// copy of it without consuming it. The caller's read deadline bounds the
// wait; MaxHeaderBytes bounds the size.
func (c *conn) peekHead() ([]byte, error) {
	limit := c.m.cfg.MaxHeaderBytes
	scanned := 0
	for {
		n := c.br.Buffered()
		if n == 0 {
			if _, err := c.br.Peek(1); err != nil {
				return nil, err
			}
			n = c.br.Buffered()
		}
		b, _ := c.br.Peek(n)
		from := scanned - 2
		if from < 0 {
			from = 0
		}
		if i := headerEnd(b[from:]); i >= 0 {
			if from+i > limit {
				return nil, errHeaderTooLarge
			}
			return append([]byte(nil), b[:from+i]...), nil
		}
		scanned = n
		if n >= limit {
			return nil, errHeaderTooLarge
		}
		if _, err := c.br.Peek(n + 1); err != nil {
			return nil, err
		}
	}
}

// scanHostHeaders returns the values of the Host header lines in a raw
// header block. net/http hides the Host header of an absolute-form
// request, so it is read here.
func scanHostHeaders(head []byte) ([]string, error) {
	var hosts []string
	lines := bytes.Split(head, []byte("\n"))
	for i, line := range lines {
		if i == 0 {
			continue
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			return nil, errObsFold
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		if strings.EqualFold(string(line[:colon]), "host") {
			hosts = append(hosts, strings.Trim(string(line[colon+1:]), " \t"))
		}
	}
	return hosts, nil
}

// hostHeaderAgrees reports whether a Host header value names the same
// authority as t. defaultPort is the port implied when the header has
// none; zero means an absent port agrees with anything.
func hostHeaderAgrees(value string, t target, defaultPort uint16) bool {
	ht, hasPort, err := parseAuthority(value, defaultPort)
	if err != nil || ht.host != t.host {
		return false
	}
	if !hasPort && defaultPort == 0 {
		return true
	}
	return ht.port == t.port
}

func (c *conn) serveHTTP(start time.Time) {
	for first := true; ; first = false {
		if !first {
			// Between keep-alive requests: wait under the idle
			// timeout, then give the header its own deadline.
			c.ic.auto.Store(false)
			_ = c.raw.SetReadDeadline(time.Now().Add(c.m.cfg.IdleTimeout))
			if _, err := c.br.Peek(1); err != nil {
				return
			}
			start = time.Now()
			_ = c.raw.SetReadDeadline(start.Add(c.m.cfg.HeaderTimeout))
		}
		fail := func(status int, reason Reason, detail string) {
			c.httpError(status, VerdictError, reason, detail)
			c.sb.record(Decision{Verdict: VerdictError, Reason: reason, Detail: detail, Duration: time.Since(start)})
		}
		head, err := c.peekHead()
		switch {
		case err == nil:
		case errors.Is(err, errHeaderTooLarge):
			fail(431, ReasonHeaderTooLarge, "request header too large")
			return
		case isTimeout(err):
			fail(408, ReasonHeaderTimeout, "request header not received in time")
			return
		default:
			return // client went away
		}
		hosts, err := scanHostHeaders(head)
		if err != nil || len(hosts) > 1 {
			fail(400, ReasonBadRequest, "malformed or repeated Host header")
			return
		}
		req, err := http.ReadRequest(c.br)
		if err != nil {
			fail(400, ReasonBadRequest, "malformed request")
			return
		}
		_ = c.raw.SetReadDeadline(time.Time{})
		if req.Method == http.MethodConnect {
			c.handleConnect(req, hosts, start)
			return
		}
		if !c.handleRequest(req, hosts, start) {
			return
		}
	}
}

func (c *conn) handleConnect(req *http.Request, hosts []string, start time.Time) {
	const proto = "connect"
	t, err := parseTarget(req.RequestURI, 0)
	if err != nil {
		c.httpError(400, VerdictDenied, ReasonBadTarget, "invalid CONNECT target")
		c.sb.record(Decision{Protocol: proto, Verdict: VerdictDenied, Reason: ReasonBadTarget, Detail: err.Error(), Duration: time.Since(start)})
		return
	}
	if req.ContentLength != 0 || len(req.TransferEncoding) > 0 {
		c.httpError(400, VerdictError, ReasonBadRequest, "CONNECT must not have a body")
		c.sb.record(Decision{Protocol: proto, Host: t.host, Port: t.port, Verdict: VerdictError, Reason: ReasonBadRequest, Detail: "CONNECT with a body", Duration: time.Since(start)})
		return
	}
	if len(hosts) == 1 && !hostHeaderAgrees(hosts[0], t, 0) {
		ref := denied(ReasonHostMismatch, "Host header does not match the CONNECT target")
		c.httpError(ref.status, ref.verdict, ref.reason, ref.detail)
		c.sb.record(ref.decision(proto, t, start))
		return
	}
	f, ref := c.open(proto, t, start)
	if ref != nil {
		c.httpError(ref.status, ref.verdict, ref.reason, ref.detail)
		c.sb.record(ref.decision(proto, t, start))
		return
	}
	_ = c.raw.SetWriteDeadline(time.Now().Add(errorWriteTimeout))
	if _, err := io.WriteString(c.raw, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		f.setEnd(ReasonIOError)
		f.finish()
		return
	}
	c.tunnel(f)
}

// hopHeaders are removed from forwarded requests and responses, together
// with anything named by Connection and every Proxy-* header.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopHeaders(h http.Header) {
	for _, v := range h["Connection"] {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
	for name := range h {
		if len(name) >= 6 && strings.EqualFold(name[:6], "proxy-") {
			delete(h, name)
		}
	}
}

func hasToken(values []string, token string) bool {
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// continueGate makes sure "100 Continue" is written to the client at most
// once and never after the real response has started.
type continueGate struct {
	mu   sync.Mutex
	done bool
	w    io.Writer
}

func (g *continueGate) send() {
	g.mu.Lock()
	if !g.done {
		g.done = true
		_, _ = io.WriteString(g.w, "HTTP/1.1 100 Continue\r\n\r\n")
	}
	g.mu.Unlock()
}

func (g *continueGate) block() {
	g.mu.Lock()
	g.done = true
	g.mu.Unlock()
}

// clientBody is the request body as handed to the upstream transport. Its
// Close never drains the client (net/http's would read an unbounded body
// to its end); whether the body was fully read decides if the client
// connection can be reused.
type clientBody struct {
	rc     io.Reader
	gate   *continueGate // nil unless the client expects 100 Continue
	eof    atomic.Bool
	closed atomic.Bool
}

func (b *clientBody) Read(p []byte) (int, error) {
	if b.closed.Load() {
		return 0, http.ErrBodyReadAfterClose
	}
	if b.gate != nil {
		b.gate.send()
	}
	n, err := b.rc.Read(p)
	if err == io.EOF {
		b.eof.Store(true)
	}
	return n, err
}

func (b *clientBody) Close() error {
	b.closed.Store(true)
	return nil
}

// handleRequest serves one plain-HTTP absolute-form request. It reports
// whether the client connection may carry another request.
func (c *conn) handleRequest(req *http.Request, hosts []string, start time.Time) (keep bool) {
	const proto = "http"
	bad := func(detail string) bool {
		c.httpError(400, VerdictError, ReasonBadRequest, detail)
		c.sb.record(Decision{Protocol: proto, Verdict: VerdictError, Reason: ReasonBadRequest, Detail: detail, Duration: time.Since(start)})
		return false
	}
	u := req.URL
	switch {
	case !u.IsAbs() || u.Host == "":
		return bad("absolute-form request URI required (this is a proxy)")
	case u.Scheme != "http":
		return bad("only http:// can be proxied in plain form; use CONNECT")
	case u.User != nil:
		return bad("userinfo in request URI")
	}
	t, err := parseTarget(u.Host, 80)
	if err != nil {
		c.httpError(400, VerdictDenied, ReasonBadTarget, "invalid target host or port")
		c.sb.record(Decision{Protocol: proto, Verdict: VerdictDenied, Reason: ReasonBadTarget, Detail: err.Error(), Duration: time.Since(start)})
		return false
	}
	if len(hosts) == 1 && !hostHeaderAgrees(hosts[0], t, 80) {
		ref := denied(ReasonHostMismatch, "Host header does not match the request URI")
		c.httpError(ref.status, ref.verdict, ref.reason, ref.detail)
		c.sb.record(ref.decision(proto, t, start))
		return false
	}
	f, ref := c.open(proto, t, start)
	if ref != nil {
		c.httpError(ref.status, ref.verdict, ref.reason, ref.detail)
		c.sb.record(ref.decision(proto, t, start))
		return false
	}
	defer f.finish()

	out := req.Clone(f.ctx)
	out.RequestURI = ""
	out.Close = false
	gate := &continueGate{w: c.ic}
	body := &clientBody{rc: req.Body}
	if hasToken(req.Header["Expect"], "100-continue") {
		body.gate = gate
	}
	stripHopHeaders(out.Header)
	out.Header.Del("Expect")
	if _, ok := out.Header["User-Agent"]; !ok {
		out.Header.Set("User-Agent", "") // stop the transport adding its own
	}
	if req.ContentLength == 0 {
		out.Body = http.NoBody
		body.eof.Store(true)
	} else {
		out.Body = body
	}

	// The transport is used for one exchange on the connection that was
	// already validated and dialled; it can never dial anything itself.
	var used atomic.Bool
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if used.Swap(true) {
				return nil, errors.New("egress: upstream connection already used")
			}
			return f.upstream, nil
		},
		DisableKeepAlives:      true,
		DisableCompression:     true,
		ResponseHeaderTimeout:  c.m.cfg.ResponseHeaderTimeout,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
	defer tr.CloseIdleConnections()

	c.ic.act.touch()
	c.ic.auto.Store(true)
	resp, err := tr.RoundTrip(out)
	gate.block()
	if err == nil && resp.StatusCode == http.StatusSwitchingProtocols {
		resp.Body.Close()
		err = errors.New("protocol upgrade is not supported over plain HTTP")
	}
	if err != nil {
		if f.end.Load() == nil {
			status, detail := 502, "upstream request failed"
			if isTimeout(err) {
				status, detail = 504, "upstream timed out"
			}
			f.setEnd(ReasonUpstreamFailed)
			c.httpError(status, VerdictError, ReasonUpstreamFailed, detail)
		}
		return false
	}
	defer resp.Body.Close()

	stripHopHeaders(resp.Header)
	chunked := len(resp.TransferEncoding) > 0 && resp.TransferEncoding[0] == "chunked"
	http11 := req.ProtoAtLeast(1, 1)
	keep = !req.Close && http11 && (resp.ContentLength >= 0 || chunked)
	if !http11 && chunked {
		resp.TransferEncoding = nil
		resp.ContentLength = -1
	}
	resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1
	resp.Close = !keep
	if err := resp.Write(c.ic); err != nil {
		f.setEnd(relayEnd(err))
		return false
	}
	// A request body that was not read to its end leaves the client
	// connection in an unknown position.
	return keep && body.eof.Load()
}

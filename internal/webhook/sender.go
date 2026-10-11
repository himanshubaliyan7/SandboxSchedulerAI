package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"syscall"
	"time"
)

// Defaults of an HTTPSender.
const (
	DefaultRequestTimeout = 10 * time.Second
	// maxResponseBytes is how much of a response body is read (and
	// thrown away) before the connection is closed.
	maxResponseBytes = 64 << 10
	// maxResponseHeaderBytes bounds the response header block.
	maxResponseHeaderBytes = 32 << 10

	userAgent = "Warmbox-Webhooks/1"

	// HeaderEventType and HeaderAttempt accompany the Standard Webhooks
	// headers. They are conveniences and are not signed.
	HeaderEventType = "Warmbox-Event-Type"
	HeaderAttempt   = "Warmbox-Delivery-Attempt"
)

// Request is one attempt to deliver a signed body.
type Request struct {
	URL    string
	Header http.Header
	Body   []byte
}

// Result is the outcome of one attempt.
type Result struct {
	// Status is the HTTP status, or zero when no response arrived.
	Status   int
	Duration time.Duration
	// Code is empty for a 2xx response and otherwise one of the Code*
	// constants; Error is a description that is safe to show the tenant.
	Code  string
	Error string
	// Detail is for the server's log only.
	Detail string
	// RetryAfter is the endpoint's Retry-After, when it sent one.
	RetryAfter time.Duration
}

// OK reports whether the endpoint accepted the event.
func (r Result) OK() bool { return r.Code == "" }

// Sender delivers one request. The worker and the test route use an
// HTTPSender; tests substitute their own.
type Sender interface {
	Send(ctx context.Context, req Request) Result
}

// HTTPSender is the Sender that talks to the network. Every Send
// validates the URL, resolves the host once, and connects only to an
// address that passed the destination rules (see url.go). Nothing is
// reused between two sends: no connection, no DNS answer.
type HTTPSender struct {
	dest    *Destinations
	timeout time.Duration
	// rootCAs replaces the system roots; tests set it.
	rootCAs *x509.CertPool
	// dialRewrite, when set, redirects the TCP connection for a validated
	// address to another address. It exists for tests, which have no
	// public address to listen on: the address is still validated first,
	// and the hook is not reachable from any configuration.
	dialRewrite func(netip.AddrPort) (netip.AddrPort, bool)
	// onDial, when set, is told every address a connection is opened to.
	onDial func(netip.AddrPort)
}

// NewHTTPSender returns a sender that enforces dest. timeout bounds one
// attempt from the lookup to the end of the response; zero means
// DefaultRequestTimeout.
func NewHTTPSender(dest *Destinations, timeout time.Duration) *HTTPSender {
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	return &HTTPSender{dest: dest, timeout: timeout}
}

// errDialRefused is returned by the dial hook.
type errDialRefused struct {
	addr, why string
}

func (e *errDialRefused) Error() string {
	return fmt.Sprintf("webhook: refusing to connect to %s (%s)", e.addr, e.why)
}

// dial connects to the first reachable of the validated addresses. The
// address the transport asks for (the URL's host and port) is ignored.
func (s *HTTPSender) dial(ctx context.Context, addrs []netip.Addr, port uint16) (net.Conn, error) {
	if len(addrs) > maxDialAttempts {
		addrs = addrs[:maxDialAttempts]
	}
	var lastErr error
	for _, a := range addrs {
		validated := netip.AddrPortFrom(a.Unmap(), port)
		want, rewritten := validated, false
		if s.dialRewrite != nil {
			to, ok := s.dialRewrite(validated)
			if !ok {
				lastErr = fmt.Errorf("webhook: no test route for %s", validated)
				continue
			}
			want, rewritten = to, true
		}
		d := net.Dialer{
			// The socket is about to connect: refuse anything but the
			// address that was validated, and validate it once more.
			Control: func(_, address string, _ syscall.RawConn) error {
				got, err := netip.ParseAddrPort(address)
				if err != nil {
					return &errDialRefused{address, "unparsable"}
				}
				got = netip.AddrPortFrom(got.Addr().Unmap(), got.Port())
				if got != want {
					return &errDialRefused{address, "not the validated address"}
				}
				if rewritten {
					return nil
				}
				if cat, denied := s.dest.check(got.Addr()); denied {
					return &errDialRefused{address, cat}
				}
				return nil
			},
		}
		network := "tcp4"
		if want.Addr().Is6() {
			network = "tcp6"
		}
		if s.onDial != nil {
			s.onDial(validated)
		}
		conn, err := d.DialContext(ctx, network, want.String())
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// Send makes one attempt. It never follows a redirect and never returns
// an error: whatever went wrong is in the Result.
func (s *HTTPSender) Send(ctx context.Context, req Request) Result {
	start := time.Now()
	done := func(r Result) Result {
		r.Duration = time.Since(start)
		return r
	}
	fail := func(err error) Result {
		if de, ok := asDestinationError(err); ok {
			return done(Result{Code: de.Code, Error: de.Message, Detail: de.Detail})
		}
		return done(classifyError(err))
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	target, err := s.dest.Parse(req.URL)
	if err != nil {
		return fail(err)
	}
	addrs, err := s.dest.Resolve(ctx, target)
	if err != nil {
		return fail(err)
	}
	tr := &http.Transport{
		Proxy: nil, // never an environment proxy: it would do the connecting
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return s.dial(ctx, addrs, target.Port)
		},
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: s.rootCAs},
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		// A redirect is an answer, not an instruction: its target was
		// never validated.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL.String(), bytes.NewReader(req.Body))
	if err != nil {
		return fail(invalidURL("the url cannot be requested"))
	}
	for k, vs := range req.Header {
		hreq.Header[k] = vs
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(hreq)
	if err != nil {
		return fail(err)
	}
	// Drain a bounded part so that a well-behaved endpoint sees its
	// response read, then drop the connection whatever is left.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()

	res := Result{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		res.Code = CodeRedirect
		res.Error = fmt.Sprintf("the endpoint answered %d; redirects are not followed, so give the final url", resp.StatusCode)
	default:
		res.Code = CodeHTTPStatus
		res.Error = fmt.Sprintf("the endpoint answered %d", resp.StatusCode)
	}
	return done(res)
}

// classifyError turns a transport error into a Result without a status.
// The message never contains the error text of the resolver or the
// dialer: only which kind of failure it was.
func classifyError(err error) Result {
	var (
		refused  *errDialRefused
		certErr  *tls.CertificateVerificationError
		recErr   tls.RecordHeaderError
		alertErr tls.AlertError
		netErr   net.Error
	)
	// The detail goes to the server log. net/http wraps a failure in a
	// *url.Error whose text repeats the request URL, path and query
	// included, and those are often the receiver's credential; only the
	// cause is kept.
	detail := err.Error()
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		detail = uerr.Op + ": " + uerr.Err.Error()
	}
	switch {
	case errors.As(err, &refused):
		return Result{Code: CodeDestinationNotAllowed, Error: "the destination address is not allowed", Detail: detail}
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return Result{Code: CodeTimeout, Error: "the endpoint did not answer in time", Detail: detail}
	case errors.As(err, &certErr):
		return Result{Code: CodeTLSError, Error: "the endpoint's TLS certificate could not be verified: " + certErr.Err.Error(), Detail: detail}
	case errors.As(err, &recErr), errors.As(err, &alertErr):
		return Result{Code: CodeTLSError, Error: "the TLS handshake with the endpoint failed", Detail: detail}
	case errors.Is(err, syscall.ECONNREFUSED):
		return Result{Code: CodeConnectionFailed, Error: "the endpoint refused the connection", Detail: detail}
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return Result{Code: CodeConnectionFailed, Error: "the endpoint closed the connection", Detail: detail}
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return Result{Code: CodeConnectionFailed, Error: "the endpoint is unreachable", Detail: detail}
	case errors.Is(err, context.Canceled):
		return Result{Code: CodeConnectionFailed, Error: "the attempt was cancelled", Detail: detail}
	}
	return Result{Code: CodeConnectionFailed, Error: "the connection to the endpoint failed", Detail: detail}
}

// parseRetryAfter reads a Retry-After header: seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		if n > int64(24*time.Hour/time.Second) {
			n = int64(24 * time.Hour / time.Second)
		}
		return time.Duration(n) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

package webhook

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// route is a test network: validated "public" addresses are connected to
// local test servers instead. Validation happens first, on the public
// address; only the TCP connection is redirected.
type route struct {
	mu     sync.Mutex
	to     map[netip.Addr]netip.AddrPort
	dialed []netip.AddrPort // validated addresses a connection was opened to
}

func (r *route) rewrite(a netip.AddrPort) (netip.AddrPort, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	to, ok := r.to[a.Addr()]
	return to, ok
}

func (r *route) dials() []netip.AddrPort {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]netip.AddrPort(nil), r.dialed...)
}

type senderEnv struct {
	t      *testing.T
	sender *HTTPSender
	res    *fakeResolver
	route  *route
}

// newSenderEnv returns a sender whose only "public" hosts are the ones
// the test adds with serve.
func newSenderEnv(t *testing.T, cfg DestinationConfig, timeout time.Duration) *senderEnv {
	t.Helper()
	dest, res := testDest(t, cfg)
	rt := &route{to: map[netip.Addr]netip.AddrPort{}}
	s := NewHTTPSender(dest, timeout)
	s.dialRewrite = rt.rewrite
	s.onDial = func(a netip.AddrPort) { rt.mu.Lock(); rt.dialed = append(rt.dialed, a); rt.mu.Unlock() }
	return &senderEnv{t: t, sender: s, res: res, route: rt}
}

// serve makes host resolve to addr and routes addr to srv. It returns
// the webhook URL of the server under that name.
func (e *senderEnv) serve(host, addr string, srv *httptest.Server) string {
	e.t.Helper()
	listen := netip.MustParseAddrPort(srv.Listener.Addr().String())
	e.res.set(host, []string{addr})
	e.route.mu.Lock()
	e.route.to[netip.MustParseAddr(addr)] = listen
	e.route.mu.Unlock()
	scheme := "http"
	if srv.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + host + ":" + strconv.Itoa(int(listen.Port())) + "/hook"
}

type received struct {
	mu   sync.Mutex
	reqs []*http.Request
	body [][]byte
}

func (r *received) handler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, req)
		r.body = append(r.body, b)
		r.mu.Unlock()
		w.WriteHeader(status)
	}
}

func (r *received) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func TestSenderDelivers(t *testing.T) {
	e := newSenderEnv(t, DestinationConfig{AllowHTTP: true}, 0)
	var got received
	srv := httptest.NewServer(got.handler(http.StatusNoContent))
	defer srv.Close()
	url := e.serve("hooks.example.com", publicAddr, srv)

	// Environment proxies must play no part: they would do the connecting.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")

	h := http.Header{}
	h.Set(HeaderID, "evt_1")
	h.Set(HeaderSignature, "v1,abc")
	res := e.sender.Send(context.Background(), Request{URL: url + "?a=b", Header: h, Body: []byte(`{"id":"evt_1"}`)})
	if !res.OK() || res.Status != http.StatusNoContent || res.Error != "" || res.Duration <= 0 {
		t.Fatalf("result: %+v", res)
	}
	if got.count() != 1 {
		t.Fatalf("%d requests arrived", got.count())
	}
	req := got.reqs[0]
	if req.Method != "POST" || req.URL.Path != "/hook" || req.URL.RawQuery != "a=b" || string(got.body[0]) != `{"id":"evt_1"}` ||
		req.Header.Get("Content-Type") != "application/json" || req.Header.Get("User-Agent") != userAgent ||
		req.Header.Get(HeaderID) != "evt_1" || req.Header.Get(HeaderSignature) != "v1,abc" ||
		!strings.HasPrefix(req.Host, "hooks.example.com:") {
		t.Errorf("request: %s %s host=%q headers=%v body=%s", req.Method, req.URL, req.Host, req.Header, got.body[0])
	}
	if d := e.route.dials(); len(d) != 1 || d[0].Addr() != netip.MustParseAddr(publicAddr) {
		t.Errorf("dialed %v", d)
	}
	if n := e.res.lookups("hooks.example.com"); n != 1 {
		t.Errorf("%d lookups for one delivery", n)
	}
}

func TestSenderStatuses(t *testing.T) {
	e := newSenderEnv(t, DestinationConfig{AllowHTTP: true}, 0)
	for _, c := range []struct {
		status int
		code   string
		retry  string
		want   time.Duration
	}{
		{200, "", "", 0}, {201, "", "", 0}, {202, "", "", 0}, {204, "", "", 0},
		{400, CodeHTTPStatus, "", 0}, {401, CodeHTTPStatus, "", 0}, {404, CodeHTTPStatus, "", 0}, {410, CodeHTTPStatus, "", 0},
		{429, CodeHTTPStatus, "120", 2 * time.Minute},
		{500, CodeHTTPStatus, "", 0}, {502, CodeHTTPStatus, "", 0},
		{503, CodeHTTPStatus, "7", 7 * time.Second},
		{503, CodeHTTPStatus, "-3", 0}, {503, CodeHTTPStatus, "soon", 0},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if c.retry != "" {
				w.Header().Set("Retry-After", c.retry)
			}
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, "an answer nobody reads")
		}))
		url := e.serve("hooks.example.com", publicAddr, srv)
		res := e.sender.Send(context.Background(), Request{URL: url, Body: []byte("{}")})
		srv.Close()
		if res.Status != c.status || res.Code != c.code || res.OK() != (c.code == "") || res.RetryAfter != c.want {
			t.Errorf("status %d (Retry-After %q): %+v", c.status, c.retry, res)
		}
		if c.code != "" && !strings.Contains(res.Error, strconv.Itoa(c.status)) {
			t.Errorf("status %d: message %q", c.status, res.Error)
		}
	}
	if d := parseRetryAfter(time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat), time.Now()); d < 80*time.Second || d > 91*time.Second {
		t.Errorf("Retry-After as a date: %s", d)
	}
}

// A redirect is reported, never followed: neither to a private address
// nor anywhere else.
func TestSenderDoesNotFollowRedirects(t *testing.T) {
	e := newSenderEnv(t, DestinationConfig{AllowHTTP: true}, 0)
	var internal received
	inner := httptest.NewServer(internal.handler(200)) // listens on loopback: "the admin panel"
	defer inner.Close()
	for _, location := range []string{inner.URL + "/admin", "http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/", "http://other.example.com/"} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			hits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				http.Redirect(w, r, location, status)
			}))
			url := e.serve("hooks.example.com", publicAddr, srv)
			before := len(e.route.dials())
			res := e.sender.Send(context.Background(), Request{URL: url, Body: []byte("{}")})
			srv.Close()
			if res.OK() || res.Code != CodeRedirect || res.Status != status || hits != 1 {
				t.Errorf("%d to %s: %+v after %d requests", status, location, res, hits)
			}
			if d := e.route.dials()[before:]; len(d) != 1 || d[0].Addr() != netip.MustParseAddr(publicAddr) {
				t.Errorf("%d to %s: connections were opened to %v", status, location, d)
			}
		}
	}
	if internal.count() != 0 {
		t.Fatalf("the redirect target received %d requests", internal.count())
	}
	if n := e.res.lookups("other.example.com"); n != 0 {
		t.Errorf("the redirect target's name was resolved %d times", n)
	}
}

// DNS rebinding: a name that answers with a public address when it is
// checked and with a private one when asked again. The name is asked
// once per delivery, and the connection goes to the address that was
// checked.
func TestSenderResolvesOncePerDelivery(t *testing.T) {
	e := newSenderEnv(t, DestinationConfig{AllowHTTP: true}, 0)
	var got, internal received
	srv := httptest.NewServer(got.handler(200))
	defer srv.Close()
	inner := httptest.NewServer(internal.handler(200))
	defer inner.Close()
	url := e.serve("rebind.example.com", publicAddr, srv)
	e.res.set("rebind.example.com", []string{publicAddr}, []string{"127.0.0.1"})

	res := e.sender.Send(context.Background(), Request{URL: url, Body: []byte("{}")})
	if !res.OK() {
		t.Fatalf("first delivery: %+v", res)
	}
	if n := e.res.lookups("rebind.example.com"); n != 1 {
		t.Fatalf("the name was resolved %d times during one delivery; the second answer is the private one", n)
	}
	if d := e.route.dials(); len(d) != 1 || d[0].Addr() != netip.MustParseAddr(publicAddr) {
		t.Fatalf("dialed %v", d)
	}

	// The next delivery validates afresh; the name now points inside, so
	// nothing is connected at all.
	res = e.sender.Send(context.Background(), Request{URL: url, Body: []byte("{}")})
	if res.OK() || res.Code != CodeDestinationNotAllowed || res.Status != 0 {
		t.Fatalf("second delivery: %+v", res)
	}
	if n := e.res.lookups("rebind.example.com"); n != 2 {
		t.Errorf("%d lookups after two deliveries", n)
	}
	if d := e.route.dials(); len(d) != 1 {
		t.Errorf("a connection was opened for the refused delivery: %v", d)
	}
	if got.count() != 1 || internal.count() != 0 {
		t.Errorf("requests: %d at the endpoint, %d at the internal server", got.count(), internal.count())
	}
}

func TestSenderRefusesDeniedDestinations(t *testing.T) {
	e := newSenderEnv(t, DestinationConfig{AllowHTTP: true}, 0)
	var internal received
	inner := httptest.NewServer(internal.handler(200))
	defer inner.Close()
	port := strconv.Itoa(int(netip.MustParseAddrPort(inner.Listener.Addr().String()).Port()))
	for _, a := range deniedAddrs {
		// Through DNS ...
		e.res.set("evil.example.com", []string{a})
		res := e.sender.Send(context.Background(), Request{URL: "http://evil.example.com:" + port + "/", Body: []byte("{}")})
		if res.Code != CodeDestinationNotAllowed || res.Status != 0 {
			t.Errorf("name resolving to %s: %+v", a, res)
		}
		// ... and as a literal.
		host := a
		if strings.Contains(a, ":") {
			host = "[" + a + "]"
		}
		res = e.sender.Send(context.Background(), Request{URL: "http://" + host + ":" + port + "/", Body: []byte("{}")})
		if res.Code != CodeDestinationNotAllowed || res.Status != 0 {
			t.Errorf("literal %s: %+v", a, res)
		}
	}
	// URLs that are not acceptable at all never reach the resolver.
	for _, u := range []string{"http://user:pw@evil.example.com/", "ftp://evil.example.com/", "http://127.1:" + port + "/", "http://evil.example.com/#x", ""} {
		before := e.res.lookups("evil.example.com")
		if res := e.sender.Send(context.Background(), Request{URL: u}); res.Code != CodeInvalidURL {
			t.Errorf("Send(%q): %+v", u, res)
		}
		if e.res.lookups("evil.example.com") != before {
			t.Errorf("Send(%q) resolved the host", u)
		}
	}
	if len(e.route.dials()) != 0 || internal.count() != 0 {
		t.Fatalf("connections %v, requests %d", e.route.dials(), internal.count())
	}

	// https is required unless the operator said otherwise.
	strict := newSenderEnv(t, DestinationConfig{}, 0)
	strict.res.set("hooks.example.com", []string{publicAddr})
	if res := strict.sender.Send(context.Background(), Request{URL: "http://hooks.example.com/"}); res.Code != CodeInvalidURL {
		t.Errorf("http without AllowHTTP: %+v", res)
	}
}

// flippingGuard allows an address the first `allow` times it is asked and
// denies it afterwards: an address that turns out to be the host's own
// between the check and the connection.
type flippingGuard struct {
	allow int32
	asked atomic.Int32
}

func (g *flippingGuard) Check(netip.Addr) (string, bool) {
	if g.asked.Add(1) <= g.allow {
		return "", false
	}
	return "host_interface", true
}

// The socket hook is a second, independent check: without the test
// route, the address about to be connected is validated again.
func TestSenderChecksTheSocketAddress(t *testing.T) {
	var internal received
	inner := httptest.NewServer(internal.handler(200))
	defer inner.Close()
	dest, _ := testDest(t, DestinationConfig{AllowHTTP: true})
	guard := &flippingGuard{allow: 2} // the URL's literal, then the resolved address
	dest.guard = guard
	s := NewHTTPSender(dest, 0)
	res := s.Send(context.Background(), Request{URL: inner.URL, Body: []byte("{}")})
	if res.Code != CodeDestinationNotAllowed || internal.count() != 0 {
		t.Fatalf("result %+v, %d requests", res, internal.count())
	}
	if guard.asked.Load() != 3 || !strings.Contains(res.Detail, "host_interface") {
		t.Errorf("the guard was asked %d times (want 3: url, resolve, socket); detail %q", guard.asked.Load(), res.Detail)
	}

	// With loopback allowed (development) the same request goes through,
	// to the real address, with no test route in between.
	lo, _ := testDest(t, DestinationConfig{AllowHTTP: true, AllowLoopback: true})
	if res := NewHTTPSender(lo, 0).Send(context.Background(), Request{URL: inner.URL, Body: []byte("{}")}); !res.OK() {
		t.Fatalf("loopback with AllowLoopback: %+v", res)
	}
	if internal.count() != 1 {
		t.Fatalf("%d requests", internal.count())
	}
}

func TestSenderTriesTheNextAddress(t *testing.T) {
	e := newSenderEnv(t, DestinationConfig{AllowHTTP: true}, 0)
	var got received
	srv := httptest.NewServer(got.handler(200))
	defer srv.Close()
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := netip.MustParseAddrPort(dead.Addr().String())
	dead.Close()
	url := e.serve("multi.example.com", publicAddr, srv)
	e.route.mu.Lock()
	e.route.to[netip.MustParseAddr("93.184.216.35")] = deadAddr
	e.route.mu.Unlock()
	e.res.set("multi.example.com", []string{"10.0.0.1", "93.184.216.35", publicAddr})
	res := e.sender.Send(context.Background(), Request{URL: url, Body: []byte("{}")})
	if !res.OK() || got.count() != 1 {
		t.Fatalf("result %+v", res)
	}
	d := e.route.dials()
	if len(d) != 2 || d[0].Addr().String() != "93.184.216.35" || d[1].Addr().String() != publicAddr {
		t.Errorf("dialed %v; the private answer must be skipped and the dead address followed by the live one", d)
	}

	// Nothing listens: a connection failure, described without the address.
	e.res.set("multi.example.com", []string{"93.184.216.35"})
	res = e.sender.Send(context.Background(), Request{URL: url, Body: []byte("{}")})
	if res.Code != CodeConnectionFailed || res.Status != 0 || strings.Contains(res.Error, "127.0.0.1") || strings.Contains(res.Error, "93.184") {
		t.Errorf("refused connection: %+v", res)
	}
}

func TestSenderTimeout(t *testing.T) {
	e := newSenderEnv(t, DestinationConfig{AllowHTTP: true}, 100*time.Millisecond)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	url := e.serve("slow.example.com", publicAddr, srv)
	start := time.Now()
	res := e.sender.Send(context.Background(), Request{URL: url, Body: []byte("{}")})
	if res.Code != CodeTimeout || res.Status != 0 || time.Since(start) > 3*time.Second {
		t.Fatalf("result %+v after %s", res, time.Since(start))
	}
}

// An endpoint that answers with an endless body cannot keep the sender,
// or fill its memory: a bounded part is read and the connection dropped.
func TestSenderCapsTheResponse(t *testing.T) {
	e := newSenderEnv(t, DestinationConfig{AllowHTTP: true}, 5*time.Second)
	var written atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		chunk := make([]byte, 32<<10)
		for r.Context().Err() == nil {
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	url := e.serve("flood.example.com", publicAddr, srv)
	start := time.Now()
	res := e.sender.Send(context.Background(), Request{URL: url, Body: []byte("{}")})
	if !res.OK() || time.Since(start) > 2*time.Second {
		t.Fatalf("result %+v after %s", res, time.Since(start))
	}
	time.Sleep(50 * time.Millisecond)
	// What the server managed to push before the reset is bounded by
	// socket buffers, not by how long it is willing to write.
	if n := written.Load(); n > 16<<20 {
		t.Errorf("the server wrote %d bytes to a sender that reads %d", n, maxResponseBytes)
	}

	// Oversized response headers are refused as well.
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 64; i++ {
			w.Header().Set("X-Pad-"+strconv.Itoa(i), strings.Repeat("x", 4096))
		}
		w.WriteHeader(200)
	}))
	defer big.Close()
	url = e.serve("bigheader.example.com", publicAddr, big)
	if res := e.sender.Send(context.Background(), Request{URL: url, Body: []byte("{}")}); res.OK() {
		t.Errorf("a %d KiB response header was accepted: %+v", 64*4, res)
	}
}

func TestSenderTLS(t *testing.T) {
	var got received
	srv := httptest.NewTLSServer(got.handler(200))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	// httptest's certificate is valid for "example.com".
	e := newSenderEnv(t, DestinationConfig{}, 0)
	e.sender.rootCAs = pool
	url := e.serve("example.com", publicAddr, srv)
	if res := e.sender.Send(context.Background(), Request{URL: url, Body: []byte("{}")}); !res.OK() {
		t.Fatalf("https delivery: %+v", res)
	}
	if got.count() != 1 || got.reqs[0].TLS == nil || got.reqs[0].TLS.ServerName != "example.com" {
		t.Fatalf("the request did not arrive over TLS with the host's name as SNI")
	}

	// The certificate is checked against the name in the URL, not against
	// whatever address was connected to ...
	other := e.serve("not-in-the-cert.example.org", publicAddr, srv)
	if res := e.sender.Send(context.Background(), Request{URL: other, Body: []byte("{}")}); res.Code != CodeTLSError {
		t.Errorf("wrong name: %+v", res)
	}
	// ... and against the system's roots unless told otherwise.
	e.sender.rootCAs = nil
	if res := e.sender.Send(context.Background(), Request{URL: url, Body: []byte("{}")}); res.Code != CodeTLSError || res.Status != 0 {
		t.Errorf("untrusted certificate: %+v", res)
	}
	if got.count() != 1 {
		t.Errorf("%d requests arrived", got.count())
	}
}

// The detail of a failed attempt is logged; it must not repeat the
// request URL, whose path and query are often the receiver's credential.
func TestClassifyErrorDetailHasNoURL(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "https://hooks.example.com/s3cr3t-path?token=s3cr3t-token", Err: errors.New("EOF")}
	res := classifyError(err)
	if strings.Contains(res.Detail, "s3cr3t") || strings.Contains(res.Error, "s3cr3t") || !strings.Contains(res.Detail, "EOF") {
		t.Errorf("classified %v as %+v", err, res)
	}
}

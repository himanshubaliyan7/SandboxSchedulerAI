package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
)

const testWait = 5 * time.Second

// Public addresses used as resolver answers. Nothing is ever dialled at
// them: the test dial hook maps them to loopback test servers and refuses
// everything else.
var (
	pubA = netip.MustParseAddr("93.184.216.34")
	pubB = netip.MustParseAddr("151.101.1.69")
	pub6 = netip.MustParseAddr("2606:2800:220:1:248:1893:25c8:1946")
)

type fakeResolver struct {
	mu    sync.Mutex
	hosts map[string][]netip.Addr
	seq   map[string][][]netip.Addr // per-call answers; the last one repeats
	calls map[string]int
}

func (r *fakeResolver) set(host string, addrs ...netip.Addr) {
	r.mu.Lock()
	r.hosts[host] = addrs
	r.mu.Unlock()
}

func (r *fakeResolver) count(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

func (r *fakeResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.calls[host]
	r.calls[host] = n + 1
	if seq, ok := r.seq[host]; ok {
		if n >= len(seq) {
			n = len(seq) - 1
		}
		return seq[n], nil
	}
	if addrs, ok := r.hosts[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

type env struct {
	t   *testing.T
	m   *Manager
	res *fakeResolver
	dir string

	mu        sync.Mutex
	routes    map[netip.Addr]bool
	dialled   []netip.AddrPort
	decisions []Decision
}

func newEnv(t *testing.T, cfg Config) *env {
	t.Helper()
	dir, err := os.MkdirTemp("", "egr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	e := &env{
		t:      t,
		dir:    dir,
		res:    &fakeResolver{hosts: map[string][]netip.Addr{}, seq: map[string][][]netip.Addr{}, calls: map[string]int{}},
		routes: map[netip.Addr]bool{pubA: true, pubB: true, pub6: true},
	}
	cfg.Resolver = e.res
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.OnDecision = func(d Decision) {
		e.mu.Lock()
		e.decisions = append(e.decisions, d)
		e.mu.Unlock()
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	setDialRewrite(m, func(ap netip.AddrPort) (netip.AddrPort, bool) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.dialled = append(e.dialled, ap)
		if !e.routes[ap.Addr()] {
			return netip.AddrPort{}, false
		}
		return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), ap.Port()), true
	})
	e.m = m
	t.Cleanup(m.Close)
	return e
}

func (e *env) attach(id string, p core.NetworkPolicy) string {
	e.t.Helper()
	path := filepath.Join(e.dir, id+".sock")
	if err := e.m.Attach(id, path, p); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e *env) dials() []netip.AddrPort {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]netip.AddrPort(nil), e.dialled...)
}

func (e *env) waitDecision(match func(Decision) bool) Decision {
	e.t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		e.mu.Lock()
		for _, d := range e.decisions {
			if match(d) {
				e.mu.Unlock()
				return d
			}
		}
		n := len(e.decisions)
		e.mu.Unlock()
		if time.Now().After(deadline) {
			e.t.Fatalf("no matching decision among %d", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func allowlist(domains ...string) core.NetworkPolicy {
	return core.NetworkPolicy{Mode: core.NetworkAllowlist, AllowDomains: domains}
}

// echoServer echoes every connection. It returns its loopback port.
func echoServer(t *testing.T) (port string, accepted func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	n := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			n++
			mu.Unlock()
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

func dialSock(t *testing.T, path string) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(testWait))
	return c
}

// connect sends a CONNECT and returns the response.
func connect(t *testing.T, c net.Conn, br *bufio.Reader, target string) *http.Response {
	t.Helper()
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("CONNECT %s: %v", target, err)
	}
	return resp
}

// connectStatus opens a fresh connection, sends a CONNECT and returns the
// status, the refusal reason and the connection.
func connectStatus(t *testing.T, path, target string) (int, string, net.Conn, *bufio.Reader) {
	t.Helper()
	c := dialSock(t, path)
	br := bufio.NewReader(c)
	resp := connect(t, c, br, target)
	if resp.StatusCode != 200 {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode, resp.Header.Get("X-Egress-Reason"), c, br
}

func assertEcho(t *testing.T, c net.Conn, r io.Reader, msg string) {
	t.Helper()
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != msg {
		t.Fatalf("echo: got %q, %v", buf, err)
	}
}

// proxyClient is an HTTP client that uses the sandbox socket as its proxy.
func proxyClient(t *testing.T, path string) *http.Client {
	t.Helper()
	tr := &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "egress.invalid:3128"}),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: testWait, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func get(t *testing.T, cl *http.Client, rawURL string) (int, string, http.Header) {
	t.Helper()
	resp, err := cl.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// socksConnect performs a SOCKS5 handshake and returns the reply code.
func socksConnect(t *testing.T, c net.Conn, atyp byte, addr []byte, port int) byte {
	t.Helper()
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	sel := make([]byte, 2)
	if _, err := io.ReadFull(c, sel); err != nil || sel[0] != 5 || sel[1] != 0 {
		t.Fatalf("socks method selection: %v %v", sel, err)
	}
	req := append([]byte{5, 1, 0, atyp}, addr...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	rep := make([]byte, 10)
	if _, err := io.ReadFull(c, rep); err != nil || rep[0] != 5 {
		t.Fatalf("socks reply: %v %v", rep, err)
	}
	return rep[1]
}

func socksDomain(name string) []byte { return append([]byte{byte(len(name))}, name...) }

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func expectClosed(t *testing.T, c net.Conn, r io.Reader) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(testWait))
	buf := make([]byte, 64)
	for {
		_, err := r.Read(buf)
		if err == nil {
			continue
		}
		if isTimeout(err) {
			t.Fatal("connection still open")
		}
		return
	}
}

func TestConnectTunnel(t *testing.T) {
	e := newEnv(t, Config{})
	port, _ := echoServer(t)
	e.res.set("api.test", pubA)
	path := e.attach("sb1", allowlist("api.test"))

	status, _, c, br := connectStatus(t, path, "API.Test.:"+port)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	assertEcho(t, c, br, "hello through the tunnel")
	assertEcho(t, c, br, strings.Repeat("x", 200_000)[:70_000])
	if st, ok := e.m.Stats("sb1"); !ok || st.BytesUp == 0 || st.BytesDown == 0 || st.ActiveConns != 1 {
		t.Errorf("live stats: %+v %v", st, ok)
	}
	c.Close()
	d := e.waitDecision(func(d Decision) bool { return d.Verdict == VerdictAllowed })
	want := int64(len("hello through the tunnel") + 70_000)
	if d.SandboxID != "sb1" || d.Protocol != "connect" || d.Host != "api.test" || strconv.Itoa(int(d.Port)) != port ||
		d.ResolvedIP != pubA || d.Reason != ReasonClosed || d.BytesUp != want || d.BytesDown != want || d.Duration <= 0 {
		t.Errorf("decision: %+v", d)
	}
	st, _ := e.m.Stats("sb1")
	if st.Allowed != 1 || st.BytesUp != want || st.BytesDown != want || len(st.Denied) != 0 {
		t.Errorf("stats: %+v", st)
	}
	if got := e.dials(); len(got) != 1 || got[0].Addr() != pubA {
		t.Errorf("dialled %v", got)
	}
}

func TestPlainHTTP(t *testing.T) {
	e := newEnv(t, Config{})
	var seen *http.Request
	var seenBody string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen, seenBody = r.Clone(context.Background()), string(b)
		mu.Unlock()
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		w.Header().Set("Proxy-Authenticate", "Basic")
		w.Header().Set("X-Upstream", "yes")
		fmt.Fprintf(w, "path=%s", r.URL.RequestURI())
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)
	e.res.set("web.test", pubA)
	path := e.attach("sb1", allowlist("web.test"))
	cl := proxyClient(t, path)

	req, _ := http.NewRequest("GET", "http://web.test:"+port+"/some/path?q=1", nil)
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	req.Header.Set("Proxy-Custom", "x")
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "drop me")
	req.Header.Set("X-Keep", "keep me")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "path=/some/path?q=1" || resp.Header.Get("X-Upstream") != "yes" || resp.Header.Get("Proxy-Authenticate") != "" {
		t.Fatalf("response: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	mu.Lock()
	if seen.Host != "web.test:"+port {
		t.Errorf("upstream Host = %q", seen.Host)
	}
	for _, h := range []string{"Proxy-Authorization", "Proxy-Custom", "Proxy-Connection", "X-Hop", "Keep-Alive", "X-Forwarded-For", "Via"} {
		if v := seen.Header.Get(h); v != "" {
			t.Errorf("upstream saw %s: %q", h, v)
		}
	}
	// The proxy uses one upstream connection per request and says so.
	if v := seen.Header.Get("Connection"); v != "close" {
		t.Errorf("upstream Connection = %q", v)
	}
	if seen.Header.Get("X-Keep") != "keep me" {
		t.Error("end-to-end header lost")
	}
	mu.Unlock()

	// POST with a body, on the same keep-alive connection.
	resp, err = cl.Post("http://web.test:"+port+"/post", "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	mu.Lock()
	if seenBody != "payload" || seen.Method != "POST" {
		t.Errorf("upstream got %s %q", seen.Method, seenBody)
	}
	mu.Unlock()

	// A redirect is handed back, never followed by the proxy.
	status, _, hdr := get(t, cl, "http://web.test:"+port+"/redirect")
	if status != 302 || !strings.Contains(hdr.Get("Location"), "169.254.169.254") {
		t.Errorf("redirect: %d %v", status, hdr)
	}
	for _, ap := range e.dials() {
		if ap.Addr() != pubA {
			t.Errorf("proxy dialled %v", ap)
		}
	}
	d := e.waitDecision(func(d Decision) bool { return d.Protocol == "http" && d.Verdict == VerdictAllowed })
	if d.Host != "web.test" || d.ResolvedIP != pubA || d.BytesUp == 0 || d.BytesDown == 0 {
		t.Errorf("decision: %+v", d)
	}
}

func TestDeniedHost(t *testing.T) {
	e := newEnv(t, Config{})
	port, accepted := echoServer(t)
	for _, h := range []string{"api.test", "evilapi.test", "api.test.evil.test", "sub.api.test", "wild.test", "evilwild.test", "other.test"} {
		e.res.set(h, pubA)
	}
	path := e.attach("sb1", allowlist("api.test", "*.wild.test"))
	cl := proxyClient(t, path)
	for _, host := range []string{"evilapi.test", "api.test.evil.test", "sub.api.test", "wild.test", "evilwild.test", "other.test"} {
		status, reason, _, _ := connectStatus(t, path, host+":"+port)
		if status != 403 || reason != string(ReasonHostNotAllowed) {
			t.Errorf("CONNECT %s: %d %q", host, status, reason)
		}
		status, body, hdr := get(t, cl, "http://"+host+":"+port+"/")
		if status != 403 || hdr.Get("X-Egress-Reason") != string(ReasonHostNotAllowed) || hdr.Get("X-Egress-Verdict") != "denied" || !strings.Contains(body, "allowlist") {
			t.Errorf("GET %s: %d %q %v", host, status, body, hdr)
		}
		if n := e.res.count(host); n != 0 {
			t.Errorf("%s was resolved %d times although not allowlisted", host, n)
		}
	}
	if status, _, _, _ := connectStatus(t, path, "deep.er.wild.test:"+port); status != 502 {
		// allowed by the wildcard, but the fake resolver has no record
		t.Errorf("wildcard subdomain: status %d, want 502 (resolve failure)", status)
	}
	if len(e.dials()) != 0 || accepted() != 0 {
		t.Errorf("something was dialled: %v", e.dials())
	}
	st, _ := e.m.Stats("sb1")
	if st.Denied[ReasonHostNotAllowed] != 12 || st.Errors[ReasonResolveFailed] != 1 || st.Allowed != 0 {
		t.Errorf("stats: %+v", st)
	}
}

func TestRebindingRefused(t *testing.T) {
	e := newEnv(t, Config{})
	port, accepted := echoServer(t)
	bad := map[string]string{
		"loop.test":       "127.0.0.1",
		"loop6.test":      "::1",
		"mapped.test":     "::ffff:127.0.0.1",
		"private.test":    "10.0.0.5",
		"private2.test":   "192.168.1.1",
		"private3.test":   "172.20.0.1",
		"meta.test":       "169.254.169.254",
		"meta2.test":      "100.100.100.200",
		"meta3.test":      "168.63.129.16",
		"meta6.test":      "fd00:ec2::254",
		"cgnat.test":      "100.64.0.1",
		"ula.test":        "fd00::1",
		"linklocal.test":  "fe80::1",
		"zero.test":       "0.0.0.0",
		"nat64.test":      "64:ff9b::7f00:1",
		"sixtofour.test":  "2002:a00:1::1",
		"mappedmeta.test": "::ffff:169.254.169.254",
	}
	var names []string
	for name, addr := range bad {
		e.res.set(name, netip.MustParseAddr(addr))
		names = append(names, name)
	}
	e.res.set("multi.test", netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("127.0.0.1"))
	names = append(names, "multi.test")
	path := e.attach("sb1", allowlist(names...))
	full := e.attach("sbfull", core.NetworkPolicy{Mode: core.NetworkFull})
	cl := proxyClient(t, path)
	for _, name := range names {
		for _, p := range []string{path, full} {
			status, reason, _, _ := connectStatus(t, p, name+":"+port)
			if status != 403 || reason != string(ReasonDeniedAddress) {
				t.Errorf("CONNECT %s: %d %q", name, status, reason)
			}
		}
		status, _, hdr := get(t, cl, "http://"+name+":"+port+"/")
		if status != 403 || hdr.Get("X-Egress-Reason") != string(ReasonDeniedAddress) {
			t.Errorf("GET %s: %d", name, status)
		}
		c := dialSock(t, path)
		if code := socksConnect(t, c, socksAtypDomain, socksDomain(name), atoi(t, port)); code != socksNotAllowed {
			t.Errorf("SOCKS %s: reply %#x", name, code)
		}
	}
	if len(e.dials()) != 0 || accepted() != 0 {
		t.Fatalf("a denied address was dialled: %v", e.dials())
	}
	d := e.waitDecision(func(d Decision) bool { return d.Host == "meta.test" })
	if d.Verdict != VerdictDenied || d.ResolvedIP != netip.MustParseAddr("169.254.169.254") || !strings.Contains(d.Detail, catMetadata) {
		t.Errorf("decision: %+v", d)
	}

	// A mixed answer: the denied address is dropped, the public one used.
	e.res.set("mixed.test", netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("::ffff:127.0.0.1"), pubA)
	if err := e.m.Update("sb1", allowlist("mixed.test")); err != nil {
		t.Fatal(err)
	}
	status, _, c, br := connectStatus(t, path, "mixed.test:"+port)
	if status != 200 {
		t.Fatalf("mixed: %d", status)
	}
	assertEcho(t, c, br, "ok")
	if got := e.dials(); len(got) != 1 || got[0].Addr() != pubA {
		t.Errorf("dialled %v", got)
	}
}

func TestResolvedOnceNeverRebound(t *testing.T) {
	e := newEnv(t, Config{})
	port, _ := echoServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer srv.Close()
	private := netip.MustParseAddr("127.0.0.1")
	// First answer public, every later answer private: the classic
	// time-of-check/time-of-use rebinding attempt.
	e.res.seq["rebind.test"] = [][]netip.Addr{{pubA}, {private}}
	e.res.seq["rebind-http.test"] = [][]netip.Addr{{pubA}, {private}}
	path := e.attach("sb1", allowlist("rebind.test", "rebind-http.test"))

	status, _, c, br := connectStatus(t, path, "rebind.test:"+port)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	assertEcho(t, c, br, "pinned")
	if n := e.res.count("rebind.test"); n != 1 {
		t.Errorf("resolver called %d times for one CONNECT", n)
	}
	if got := e.dials(); len(got) != 1 || got[0].Addr() != pubA {
		t.Errorf("dialled %v, want only %v", got, pubA)
	}

	status, _, _ = get(t, proxyClient(t, path), "http://rebind-http.test:"+portOf(t, srv.URL)+"/")
	if status != 200 {
		t.Fatalf("http status %d", status)
	}
	if n := e.res.count("rebind-http.test"); n != 1 {
		t.Errorf("resolver called %d times for one GET", n)
	}
	// The next request resolves again, gets the private answer, and is
	// refused: one lookup per request, each one filtered.
	if status, reason, _, _ := connectStatus(t, path, "rebind.test:"+port); status != 403 || reason != string(ReasonDeniedAddress) {
		t.Errorf("second CONNECT: %d %q", status, reason)
	}
	if n := e.res.count("rebind.test"); n != 2 {
		t.Errorf("resolver called %d times for two CONNECTs", n)
	}
	for _, ap := range e.dials() {
		if ap.Addr() != pubA {
			t.Errorf("dialled %v", ap)
		}
	}
}

func TestIPLiteralDeniedInEveryMode(t *testing.T) {
	e := newEnv(t, Config{})
	port, accepted := echoServer(t)
	paths := map[string]string{
		"none":      e.attach("none", core.NetworkPolicy{Mode: core.NetworkNone}),
		"allowlist": e.attach("allow", core.NetworkPolicy{Mode: core.NetworkAllowlist, AllowCIDRs: []string{"0.0.0.0/0", "::/0", "169.254.169.254/32", "127.0.0.0/8"}}),
		"full":      e.attach("full", core.NetworkPolicy{Mode: core.NetworkFull}),
	}
	literals := []string{
		"169.254.169.254", "[::ffff:169.254.169.254]", "[::ffff:a9fe:a9fe]", "[fd00:ec2::254]", "100.100.100.200", "168.63.129.16",
		"127.0.0.1", "[::1]", "[::ffff:127.0.0.1]", "10.0.0.1", "192.168.0.1", "172.16.0.1", "0.0.0.0", "[::]",
		"[64:ff9b::a9fe:a9fe]", "[2002:a9fe:a9fe::1]", "[fe80::1]", "100.64.0.1",
	}
	for mode, path := range paths {
		cl := proxyClient(t, path)
		for _, lit := range literals {
			status, reason, _, _ := connectStatus(t, path, lit+":"+port)
			wantReason := string(ReasonDeniedAddress)
			if mode == "none" {
				wantReason = string(ReasonModeNone)
			}
			if status != 403 || reason != wantReason {
				t.Errorf("%s: CONNECT %s: %d %q", mode, lit, status, reason)
			}
			if status, _, _ := get(t, cl, "http://"+lit+":"+port+"/latest/meta-data/"); status != 403 {
				t.Errorf("%s: GET %s: %d", mode, lit, status)
			}
			a := netip.MustParseAddr(strings.Trim(lit, "[]"))
			atyp, raw := byte(socksAtypIPv6), a.AsSlice()
			if a.Is4() {
				atyp = socksAtypIPv4
			}
			if code := socksConnect(t, dialSock(t, path), atyp, raw, atoi(t, port)); code != socksNotAllowed {
				t.Errorf("%s: SOCKS %s: %#x", mode, lit, code)
			}
			// The same literal passed as a SOCKS domain name.
			if code := socksConnect(t, dialSock(t, path), socksAtypDomain, socksDomain(strings.Trim(lit, "[]")), atoi(t, port)); code != socksNotAllowed {
				t.Errorf("%s: SOCKS domain %s: %#x", mode, lit, code)
			}
		}
		// Numeric spellings of loopback are not targets at all.
		for _, odd := range []string{"127.1", "0x7f.0.0.1", "2130706433", "017700000001"} {
			if status, _, _, _ := connectStatus(t, path, odd+":"+port); status != 400 && status != 403 {
				t.Errorf("%s: CONNECT %s: %d", mode, odd, status)
			}
		}
	}
	if len(e.dials()) != 0 || accepted() != 0 {
		t.Fatalf("dialled %v", e.dials())
	}
}

func TestFullMode(t *testing.T) {
	e := newEnv(t, Config{})
	port, _ := echoServer(t)
	e.res.set("anything.example", pubB)
	e.res.set("v6.example", pub6)
	path := e.attach("sb1", core.NetworkPolicy{Mode: core.NetworkFull, DenyCIDRs: []string{"151.101.0.0/16"}})

	// DenyCIDRs applies in full mode.
	if status, reason, _, _ := connectStatus(t, path, "anything.example:"+port); status != 403 || reason != string(ReasonDeniedAddress) {
		t.Errorf("policy deny CIDR: %d %q", status, reason)
	}
	if status, _, _, _ := connectStatus(t, path, pubB.String()+":"+port); status != 403 {
		t.Errorf("policy deny CIDR literal: %d", status)
	}
	if err := e.m.Update("sb1", core.NetworkPolicy{Mode: core.NetworkFull}); err != nil {
		t.Fatal(err)
	}
	status, _, c, br := connectStatus(t, path, "anything.example:"+port)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	assertEcho(t, c, br, "full")
	// A public IP literal is fine in full mode.
	status, _, c, br = connectStatus(t, path, pubA.String()+":"+port)
	if status != 200 {
		t.Fatalf("literal status %d", status)
	}
	assertEcho(t, c, br, "literal")
	// IPv6 answers are validated and pinned the same way.
	if status, _, _, _ = connectStatus(t, path, "v6.example:"+port); status != 200 {
		t.Fatalf("v6 status %d", status)
	}
	got := e.dials()
	if got[len(got)-1].Addr() != pub6 {
		t.Errorf("dialled %v", got)
	}
}

func TestAllowCIDRs(t *testing.T) {
	e := newEnv(t, Config{})
	port, _ := echoServer(t)
	e.res.set("named.test", pubA)
	path := e.attach("sb1", core.NetworkPolicy{Mode: core.NetworkAllowlist, AllowCIDRs: []string{"93.184.216.0/24"}})
	status, _, c, br := connectStatus(t, path, pubA.String()+":"+port)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	assertEcho(t, c, br, "cidr")
	if status, reason, _, _ := connectStatus(t, path, pubB.String()+":"+port); status != 403 || reason != string(ReasonIPNotAllowed) {
		t.Errorf("outside CIDR: %d %q", status, reason)
	}
	// AllowCIDRs does not make a hostname acceptable.
	if status, reason, _, _ := connectStatus(t, path, "named.test:"+port); status != 403 || reason != string(ReasonHostNotAllowed) {
		t.Errorf("hostname with only AllowCIDRs: %d %q", status, reason)
	}
}

func TestKeepAliveReevaluated(t *testing.T) {
	e := newEnv(t, Config{})
	mk := func(name string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, name) }))
		t.Cleanup(srv.Close)
		return portOf(t, srv.URL)
	}
	portA, portB := mk("server-a"), mk("server-b")
	e.res.set("a.test", pubA)
	e.res.set("b.test", pubB)
	e.res.set("forbidden.test", pubB)
	e.res.set("internal.test", netip.MustParseAddr("10.1.2.3"))
	path := e.attach("sb1", allowlist("a.test", "b.test", "internal.test"))

	c := dialSock(t, path)
	br := bufio.NewReader(c)
	do := func(host, port string) (int, string) {
		t.Helper()
		fmt.Fprintf(c, "GET http://%s:%s/ HTTP/1.1\r\nHost: %s:%s\r\n\r\n", host, port, host, port)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("GET %s: %v", host, err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if s, b := do("a.test", portA); s != 200 || b != "server-a" {
		t.Fatalf("first: %d %q", s, b)
	}
	// Same connection, different allowed host: must reach the other
	// upstream, not reuse the first one.
	if s, b := do("b.test", portB); s != 200 || b != "server-b" {
		t.Fatalf("second: %d %q", s, b)
	}
	if s, b := do("a.test", portA); s != 200 || b != "server-a" {
		t.Fatalf("third: %d %q", s, b)
	}
	if e.res.count("a.test") != 2 || e.res.count("b.test") != 1 {
		t.Errorf("resolver calls: a=%d b=%d", e.res.count("a.test"), e.res.count("b.test"))
	}
	// Same connection, a host the policy forbids.
	if s, _ := do("forbidden.test", portB); s != 403 {
		t.Fatalf("forbidden host on a keep-alive connection: %d", s)
	}
	expectClosed(t, c, br)

	// And an allowlisted name that resolves privately, second on a
	// connection that began with a permitted request.
	c = dialSock(t, path)
	br = bufio.NewReader(c)
	if s, _ := do("a.test", portA); s != 200 {
		t.Fatal("first on new connection")
	}
	if s, _ := do("internal.test", portA); s != 403 {
		t.Fatalf("private target on a keep-alive connection: %d", s)
	}
	for _, ap := range e.dials() {
		if ap.Addr() != pubA && ap.Addr() != pubB {
			t.Errorf("dialled %v", ap)
		}
	}
}

func TestHostHeaderMismatch(t *testing.T) {
	e := newEnv(t, Config{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.Host) }))
	defer srv.Close()
	port := portOf(t, srv.URL)
	e.res.set("a.test", pubA)
	path := e.attach("sb1", allowlist("a.test"))
	send := func(raw string) (int, string, string) {
		t.Helper()
		c := dialSock(t, path)
		io.WriteString(c, raw)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("X-Egress-Reason"), string(b)
	}
	uri := "http://a.test:" + port + "/"
	if s, _, b := send("GET " + uri + " HTTP/1.1\r\nHost: A.Test:" + port + "\r\n\r\n"); s != 200 || b != "a.test:"+port {
		t.Errorf("matching Host: %d %q", s, b)
	}
	if s, _, b := send("GET " + uri + " HTTP/1.0\r\n\r\n"); s != 200 || b != "a.test:"+port {
		t.Errorf("no Host header: %d %q", s, b)
	}
	for _, h := range []string{"evil.test:" + port, "evil.test", "a.test", "a.test:1", "169.254.169.254", ""} {
		if s, r, _ := send("GET " + uri + " HTTP/1.1\r\nHost: " + h + "\r\n\r\n"); s != 403 || r != string(ReasonHostMismatch) {
			t.Errorf("Host %q: %d %q", h, s, r)
		}
	}
	if s, r, _ := send("CONNECT a.test:" + port + " HTTP/1.1\r\nHost: evil.test:" + port + "\r\n\r\n"); s != 403 || r != string(ReasonHostMismatch) {
		t.Errorf("CONNECT with other Host: %d %q", s, r)
	}
	for name, raw := range map[string]string{
		"two Host headers": "GET " + uri + " HTTP/1.1\r\nHost: a.test:" + port + "\r\nHost: evil.test\r\n\r\n",
		"origin form":      "GET / HTTP/1.1\r\nHost: a.test:" + port + "\r\n\r\n",
		"https scheme":     "GET https://a.test:" + port + "/ HTTP/1.1\r\nHost: a.test:" + port + "\r\n\r\n",
		"userinfo":         "GET http://u:p@a.test:" + port + "/ HTTP/1.1\r\nHost: a.test:" + port + "\r\n\r\n",
		"garbage":          "NOT A REQUEST\r\n\r\n",
		"http2 preface":    "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n",
		"connect no port":  "CONNECT a.test HTTP/1.1\r\nHost: a.test\r\n\r\n",
		"connect bad port": "CONNECT a.test:0 HTTP/1.1\r\n\r\n",
		"folded header":    "GET " + uri + " HTTP/1.1\r\nX: 1\r\n Host: evil\r\n\r\n",
	} {
		if s, _, _ := send(raw); s != 400 {
			t.Errorf("%s: status %d, want 400", name, s)
		}
	}
}

func TestSNICheck(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secret") }))
	// Refused tunnels end the upstream handshake early; that is expected.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	port := portOf(t, srv.URL)
	handshake := func(e *env, path, target, sni string) error {
		t.Helper()
		status, _, c, br := connectStatus(t, path, target+":"+port)
		if status != 200 {
			t.Fatalf("CONNECT %s: %d", target, status)
		}
		if br.Buffered() != 0 {
			t.Fatal("unexpected buffered bytes")
		}
		tc := tls.Client(c, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
		if err := tc.Handshake(); err != nil {
			return err
		}
		fmt.Fprintf(tc, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target)
		resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(resp.Body)
		if string(b) != "secret" {
			return fmt.Errorf("body %q", b)
		}
		return nil
	}

	e := newEnv(t, Config{})
	e.res.set("front.test", pubA)
	path := e.attach("sb1", allowlist("front.test"))
	if err := handshake(e, path, "front.test", "front.test"); err != nil {
		t.Errorf("matching SNI: %v", err)
	}
	if err := handshake(e, path, "front.test", "FRONT.test"); err != nil {
		t.Errorf("matching SNI, other case: %v", err)
	}
	if err := handshake(e, path, "front.test", ""); err != nil {
		t.Errorf("no SNI: %v", err)
	}
	err := handshake(e, path, "front.test", "hidden.test")
	if err == nil {
		t.Fatal("mismatched SNI was allowed")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("mismatch error %q does not carry the TLS access_denied alert", err)
	}
	d := e.waitDecision(func(d Decision) bool { return d.Reason == ReasonSNIMismatch })
	if d.Verdict != VerdictDenied || d.Host != "front.test" || d.BytesUp != 0 {
		t.Errorf("decision: %+v (no client byte may reach the upstream)", d)
	}
	if st, _ := e.m.Stats("sb1"); st.Denied[ReasonSNIMismatch] != 1 || st.Allowed != 3 {
		t.Errorf("stats: %+v", st)
	}

	// A ClientHello split into tiny records cannot hide its SNI.
	hello := refragment(clientHello(t, &tls.Config{ServerName: "hidden.test", InsecureSkipVerify: true}), 7)
	status, _, c, br := connectStatus(t, path, "front.test:"+port)
	if status != 200 {
		t.Fatal(status)
	}
	c.Write(hello)
	alert := make([]byte, 7)
	if _, err := io.ReadFull(br, alert); err != nil || !bytes.Equal(alert, tlsAlert(tlsAlertAccessDeny)) {
		t.Errorf("fragmented mismatched hello: got %x, %v", alert, err)
	}
	expectClosed(t, c, br)

	// Something that starts like TLS but is not a ClientHello is refused.
	status, _, c, br = connectStatus(t, path, "front.test:"+port)
	if status != 200 {
		t.Fatal(status)
	}
	c.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x04, 0x02, 0x00, 0x00, 0x00})
	expectClosed(t, c, br)
	e.waitDecision(func(d Decision) bool { return d.Reason == ReasonTLSMalformed && d.Verdict == VerdictDenied })

	// SOCKS5 tunnels get the same check.
	sc := dialSock(t, path)
	if code := socksConnect(t, sc, socksAtypDomain, socksDomain("front.test"), atoi(t, port)); code != socksSucceeded {
		t.Fatalf("socks: %#x", code)
	}
	if err := tls.Client(sc, &tls.Config{ServerName: "hidden.test", InsecureSkipVerify: true}).Handshake(); err == nil {
		t.Error("mismatched SNI over SOCKS5 was allowed")
	}
	sc = dialSock(t, path)
	if code := socksConnect(t, sc, socksAtypDomain, socksDomain("front.test"), atoi(t, port)); code != socksSucceeded {
		t.Fatalf("socks: %#x", code)
	}
	if err := tls.Client(sc, &tls.Config{ServerName: "front.test", InsecureSkipVerify: true}).Handshake(); err != nil {
		t.Errorf("matching SNI over SOCKS5: %v", err)
	}

	// With the check disabled the mismatch passes.
	e2 := newEnv(t, Config{DisableSNICheck: true})
	e2.res.set("front.test", pubA)
	path2 := e2.attach("sb1", allowlist("front.test"))
	if err := handshake(e2, path2, "front.test", "hidden.test"); err != nil {
		t.Errorf("DisableSNICheck: %v", err)
	}
}

func TestServerSpeaksFirst(t *testing.T) {
	// The SNI peek must not hold back a protocol where the server talks
	// first and the client stays silent until it has heard the banner.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.WriteString(c, "220 banner\r\n")
				line, _ := bufio.NewReader(c).ReadString('\n')
				io.WriteString(c, "250 got "+line)
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	e := newEnv(t, Config{})
	e.res.set("smtp.test", pubA)
	path := e.attach("sb1", allowlist("smtp.test"))
	status, _, c, br := connectStatus(t, path, "smtp.test:"+port)
	if status != 200 {
		t.Fatal(status)
	}
	if line, err := br.ReadString('\n'); err != nil || line != "220 banner\r\n" {
		t.Fatalf("banner: %q %v", line, err)
	}
	io.WriteString(c, "EHLO sandbox\r\n")
	if line, err := br.ReadString('\n'); err != nil || line != "250 got EHLO sandbox\r\n" {
		t.Fatalf("reply: %q %v", line, err)
	}
}

func TestSOCKS5(t *testing.T) {
	e := newEnv(t, Config{})
	port, _ := echoServer(t)
	p := atoi(t, port)
	e.res.set("db.test", pubA)
	e.res.set("other.test", pubB)
	path := e.attach("sb1", core.NetworkPolicy{Mode: core.NetworkAllowlist, AllowDomains: []string{"db.test", "nx.test"}, AllowCIDRs: []string{"93.184.216.34/32", "2606:2800:220::/48"}})

	c := dialSock(t, path)
	if code := socksConnect(t, c, socksAtypDomain, socksDomain("DB.test."), p); code != socksSucceeded {
		t.Fatalf("domain: %#x", code)
	}
	assertEcho(t, c, c, "socks domain")
	c.Close()
	d := e.waitDecision(func(d Decision) bool { return d.Protocol == "socks5" && d.Verdict == VerdictAllowed })
	if d.Host != "db.test" || d.ResolvedIP != pubA || d.BytesUp != 12 || d.BytesDown != 12 {
		t.Errorf("decision: %+v", d)
	}

	c = dialSock(t, path)
	if code := socksConnect(t, c, socksAtypIPv4, pubA.AsSlice(), p); code != socksSucceeded {
		t.Fatalf("ipv4: %#x", code)
	}
	assertEcho(t, c, c, "socks v4")

	c = dialSock(t, path)
	if code := socksConnect(t, c, socksAtypIPv6, pub6.AsSlice(), p); code != socksSucceeded {
		t.Fatalf("ipv6: %#x", code)
	}
	assertEcho(t, c, c, "socks v6")

	// Client pipelines its first payload right behind the request.
	c = dialSock(t, path)
	msg := append([]byte{5, 1, 0, 5, 1, 0, socksAtypDomain}, socksDomain("db.test")...)
	msg = append(msg, byte(p>>8), byte(p))
	msg = append(msg, "early"...)
	c.Write(msg)
	buf := make([]byte, 2+10+5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf[12:]) != "early" || buf[3] != socksSucceeded {
		t.Fatalf("pipelined: %x %v", buf, err)
	}

	denied := []struct {
		name string
		atyp byte
		addr []byte
		port int
		want byte
	}{
		{"domain not allowlisted", socksAtypDomain, socksDomain("other.test"), p, socksNotAllowed},
		{"lookalike", socksAtypDomain, socksDomain("evildb.test"), p, socksNotAllowed},
		{"ip not in allow CIDRs", socksAtypIPv4, pubB.AsSlice(), p, socksNotAllowed},
		{"ipv4-mapped loopback", socksAtypIPv6, netip.MustParseAddr("::ffff:127.0.0.1").AsSlice(), p, socksNotAllowed},
		{"bad hostname", socksAtypDomain, socksDomain("db test"), p, socksNotAllowed},
		{"port zero", socksAtypDomain, socksDomain("db.test"), 0, socksNotAllowed},
		{"unresolvable", socksAtypDomain, socksDomain("nx.test"), p, socksHostUnreachable},
		{"bad address type", 0x02, []byte{1, 2, 3, 4}, p, socksAtypNotSupported},
	}
	for _, tc := range denied {
		if code := socksConnect(t, dialSock(t, path), tc.atyp, tc.addr, tc.port); code != tc.want {
			t.Errorf("%s: reply %#x, want %#x", tc.name, code, tc.want)
		}
	}
	// Connection refused upstream is reported as such, not as policy.
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	_, deadPort, _ := net.SplitHostPort(closed.Addr().String())
	closed.Close()
	if code := socksConnect(t, dialSock(t, path), socksAtypDomain, socksDomain("db.test"), atoi(t, deadPort)); code != socksConnRefused {
		t.Errorf("refused upstream: reply %#x", code)
	}

	raw := func(b []byte) []byte {
		c := dialSock(t, path)
		c.Write(b)
		out, _ := io.ReadAll(c)
		return out
	}
	if out := raw([]byte{5, 1, 2}); !bytes.Equal(out, []byte{5, 0xff}) {
		t.Errorf("no acceptable auth method: %x", out)
	}
	if out := raw([]byte{5, 1, 0, 5, 2, 0, 1, 1, 2, 3, 4, 0, 80}); len(out) != 12 || out[3] != socksCmdNotSupported {
		t.Errorf("BIND: %x", out)
	}
	if out := raw([]byte{5, 1, 0, 5, 3, 0, 1, 1, 2, 3, 4, 0, 80}); len(out) != 12 || out[3] != socksCmdNotSupported {
		t.Errorf("UDP ASSOCIATE: %x", out)
	}
	if out := raw([]byte{5, 1, 0, 5, 1, 0, 3, 0, 0, 80}); len(out) != 12 || out[3] != socksGeneralFailure {
		t.Errorf("empty domain: %x", out)
	}
	for _, ap := range e.dials() {
		if ap.Addr() != pubA && ap.Addr() != pub6 {
			t.Errorf("dialled %v", ap)
		}
	}

	// Mode none over SOCKS.
	none := e.attach("none", core.NetworkPolicy{Mode: core.NetworkNone})
	if code := socksConnect(t, dialSock(t, none), socksAtypDomain, socksDomain("db.test"), p); code != socksNotAllowed {
		t.Errorf("mode none: %#x", code)
	}
}

func TestUpdateStricterClosesTunnels(t *testing.T) {
	e := newEnv(t, Config{})
	port, _ := echoServer(t)
	e.res.set("keep.test", pubA)
	e.res.set("drop.test", pubB)
	path := e.attach("sb1", allowlist("keep.test", "drop.test"))

	_, _, keep, keepBr := connectStatus(t, path, "keep.test:"+port)
	_, _, drop, dropBr := connectStatus(t, path, "drop.test:"+port)
	sdrop := dialSock(t, path)
	if code := socksConnect(t, sdrop, socksAtypDomain, socksDomain("drop.test"), atoi(t, port)); code != socksSucceeded {
		t.Fatal(code)
	}
	assertEcho(t, keep, keepBr, "one")
	assertEcho(t, drop, dropBr, "two")
	assertEcho(t, sdrop, sdrop, "three")

	if err := e.m.Update("sb1", allowlist("keep.test")); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, drop, dropBr)
	expectClosed(t, sdrop, sdrop)
	assertEcho(t, keep, keepBr, "still here")
	if status, reason, _, _ := connectStatus(t, path, "drop.test:"+port); status != 403 || reason != string(ReasonHostNotAllowed) {
		t.Errorf("new tunnel after update: %d %q", status, reason)
	}
	d := e.waitDecision(func(d Decision) bool {
		return d.Protocol == "connect" && d.Host == "drop.test" && d.Reason == ReasonPolicyUpdated
	})
	if d.Verdict != VerdictAllowed || d.BytesUp != 3 {
		t.Errorf("decision: %+v", d)
	}

	// A DenyCIDR that covers the pinned address closes the tunnel too.
	if err := e.m.Update("sb1", core.NetworkPolicy{Mode: core.NetworkAllowlist, AllowDomains: []string{"keep.test"}, DenyCIDRs: []string{"93.184.216.0/24"}}); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, keep, keepBr)

	// Loosen again, then none: everything goes.
	if err := e.m.Update("sb1", allowlist("keep.test")); err != nil {
		t.Fatal(err)
	}
	_, _, keep, keepBr = connectStatus(t, path, "keep.test:"+port)
	assertEcho(t, keep, keepBr, "back")
	if err := e.m.Update("sb1", core.NetworkPolicy{Mode: core.NetworkNone}); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, keep, keepBr)
	if status, reason, _, _ := connectStatus(t, path, "keep.test:"+port); status != 403 || reason != string(ReasonModeNone) {
		t.Errorf("mode none: %d %q", status, reason)
	}

	if err := e.m.Update("missing", allowlist()); !errors.Is(err, ErrNotAttached) {
		t.Errorf("Update of unknown sandbox: %v", err)
	}
	if err := e.m.Update("sb1", core.NetworkPolicy{Mode: "bogus"}); err == nil {
		t.Error("invalid policy accepted")
	}
	// The invalid update changed nothing.
	if status, reason, _, _ := connectStatus(t, path, "keep.test:"+port); status != 403 || reason != string(ReasonModeNone) {
		t.Errorf("after invalid update: %d %q", status, reason)
	}
}

func TestUpdateClosesStreamingHTTP(t *testing.T) {
	e := newEnv(t, Config{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first chunk\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	e.res.set("stream.test", pubA)
	path := e.attach("sb1", allowlist("stream.test"))
	c := dialSock(t, path)
	target := "stream.test:" + portOf(t, srv.URL)
	fmt.Fprintf(c, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "first chunk\n" {
		t.Fatalf("streamed chunk: %q %v", line, err)
	}
	if err := e.m.Update("sb1", allowlist()); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c, br)
	e.waitDecision(func(d Decision) bool { return d.Protocol == "http" && d.Reason == ReasonPolicyUpdated })
}

func TestDetach(t *testing.T) {
	e := newEnv(t, Config{})
	port, _ := echoServer(t)
	e.res.set("api.test", pubA)
	path := e.attach("sb1", allowlist("api.test"))
	if fi, err := os.Stat(path); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket: %v %v", fi, err)
	}
	_, _, tun, tunBr := connectStatus(t, path, "api.test:"+port)
	assertEcho(t, tun, tunBr, "up")
	idle := dialSock(t, path) // a connection that never sent anything
	half := dialSock(t, path) // a connection stuck mid-header
	io.WriteString(half, "GET http://api.test/ HTTP/1.1\r\n")
	time.Sleep(20 * time.Millisecond)

	if err := e.m.Detach("sb1"); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, tun, tunBr)
	expectClosed(t, idle, idle)
	expectClosed(t, half, half)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("socket still exists: %v", err)
	}
	if _, err := net.Dial("unix", path); err == nil {
		t.Error("can still connect after Detach")
	}
	if _, ok := e.m.Stats("sb1"); ok {
		t.Error("stats still available after Detach")
	}
	e.waitDecision(func(d Decision) bool { return d.Host == "api.test" && d.Reason == ReasonDetached })
	if err := e.m.Detach("sb1"); err != nil {
		t.Errorf("second Detach: %v", err)
	}
	// The ID and path can be attached again.
	if err := e.m.Attach("sb1", path, allowlist("api.test")); err != nil {
		t.Fatal(err)
	}
	if status, _, _, _ := connectStatus(t, path, "api.test:"+port); status != 200 {
		t.Errorf("after re-attach: %d", status)
	}
}

func TestAttachErrorsAndSocketMode(t *testing.T) {
	e := newEnv(t, Config{})
	path := e.attach("sb1", allowlist())
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", fi.Mode(), err)
	}
	if err := e.m.Attach("sb1", filepath.Join(e.dir, "x.sock"), allowlist()); !errors.Is(err, ErrAlreadyAttached) {
		t.Errorf("duplicate ID: %v", err)
	}
	if err := e.m.Attach("sb2", path, allowlist()); !errors.Is(err, ErrAlreadyAttached) {
		t.Errorf("duplicate path: %v", err)
	}
	if err := e.m.Attach("sb3", filepath.Join(e.dir, "y.sock"), core.NetworkPolicy{Mode: "bogus"}); err == nil {
		t.Error("invalid policy accepted")
	}
	if err := e.m.Attach("", path, allowlist()); err == nil {
		t.Error("empty ID accepted")
	}
	if err := e.m.Attach("sb4", filepath.Join(e.dir, "missing", "z.sock"), allowlist()); err == nil {
		t.Error("missing directory accepted")
	}
	// A regular file at the path is not clobbered.
	file := filepath.Join(e.dir, "file.sock")
	os.WriteFile(file, []byte("precious"), 0o644)
	if err := e.m.Attach("sb5", file, allowlist()); err == nil {
		t.Error("attached over a regular file")
	}
	if b, _ := os.ReadFile(file); string(b) != "precious" {
		t.Error("regular file was modified")
	}
	// Neither is the target of a symlink planted at the path.
	link := filepath.Join(e.dir, "link.sock")
	os.Symlink(file, link)
	if err := e.m.Attach("sb6", link, allowlist()); err == nil {
		t.Error("attached over a symlink")
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o644 {
		t.Errorf("symlink target mode changed to %v", fi.Mode())
	}
	// A stale socket is replaced.
	stale := filepath.Join(e.dir, "stale.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	if err := e.m.Attach("sb7", stale, allowlist()); err != nil {
		t.Errorf("stale socket: %v", err)
	}

	// A wider mode is honoured exactly, whatever the umask.
	e2 := newEnv(t, Config{SocketMode: 0o666})
	p2 := e2.attach("wide", allowlist())
	if fi, err := os.Stat(p2); err != nil || fi.Mode().Perm() != 0o666 {
		t.Errorf("0666 socket mode: %v %v", fi.Mode(), err)
	}

	e.m.Close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Error("Close left a socket behind")
	}
	if err := e.m.Attach("late", filepath.Join(e.dir, "late.sock"), allowlist()); !errors.Is(err, ErrClosed) {
		t.Errorf("Attach after Close: %v", err)
	}
	if err := e.m.Update("sb1", allowlist()); !errors.Is(err, ErrClosed) {
		t.Errorf("Update after Close: %v", err)
	}
}

func TestChmodSocketRefusesSymlink(t *testing.T) {
	dir, _ := os.MkdirTemp("", "egr")
	defer os.RemoveAll(dir)
	victim := filepath.Join(dir, "victim")
	os.WriteFile(victim, nil, 0o600)
	link := filepath.Join(dir, "s.sock")
	os.Symlink(victim, link)
	if err := chmodSocket(link, 0o666); err == nil {
		t.Error("chmodSocket followed a symlink")
	}
	if fi, _ := os.Stat(victim); fi.Mode().Perm() != 0o600 {
		t.Errorf("victim mode is now %v", fi.Mode())
	}
}

func TestConnectionCap(t *testing.T) {
	e := newEnv(t, Config{MaxConnsPerSandbox: 3})
	port, _ := echoServer(t)
	e.res.set("api.test", pubA)
	path := e.attach("sb1", allowlist("api.test"))
	other := e.attach("sb2", allowlist("api.test"))

	var held []net.Conn
	for i := 0; i < 3; i++ {
		status, _, c, br := connectStatus(t, path, "api.test:"+port)
		if status != 200 {
			t.Fatalf("conn %d: %d", i, status)
		}
		assertEcho(t, c, br, "x")
		held = append(held, c)
	}
	for i := 0; i < 5; i++ {
		c := dialSock(t, path)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil || resp.StatusCode != 503 || resp.Header.Get("X-Egress-Reason") != string(ReasonConnLimit) {
			t.Fatalf("over the cap: %v %v", resp, err)
		}
		expectClosed(t, c, c)
	}
	// Another sandbox is unaffected by this one's cap.
	if status, _, _, _ := connectStatus(t, other, "api.test:"+port); status != 200 {
		t.Errorf("other sandbox: %d", status)
	}
	if st, _ := e.m.Stats("sb1"); st.ActiveConns != 3 || st.Denied[ReasonConnLimit] != 5 {
		t.Errorf("stats: %+v", st)
	}
	// Freeing a slot lets a new connection in.
	held[0].Close()
	deadline := time.Now().Add(testWait)
	for {
		if st, _ := e.m.Stats("sb1"); st.ActiveConns < 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slot never freed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if status, _, _, _ := connectStatus(t, path, "api.test:"+port); status != 200 {
		t.Errorf("after freeing a slot: %d", status)
	}
}

func TestSlowHeaderCutOff(t *testing.T) {
	e := newEnv(t, Config{HeaderTimeout: 300 * time.Millisecond})
	e.res.set("api.test", pubA)
	path := e.attach("sb1", allowlist("api.test"))

	// Trickle a header one byte at a time; each byte arrives well within
	// the timeout, so only an absolute deadline stops it.
	c := dialSock(t, path)
	stop := make(chan struct{})
	defer close(stop)
	go func(c net.Conn) {
		for _, b := range []byte("GET http://api.test/ HTTP/1.1\r\nX-Slow: " + strings.Repeat("a", 500)) {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			if _, err := c.Write([]byte{b}); err != nil {
				return
			}
		}
	}(c)
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != 408 {
		t.Fatalf("slow header: %v %v", resp, err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("cut off after %v", el)
	}
	expectClosed(t, c, c)

	// A client that sends nothing at all.
	c = dialSock(t, path)
	start = time.Now()
	expectClosed(t, c, c)
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("silent client cut off after %v", el)
	}
	// A stalled SOCKS handshake.
	c = dialSock(t, path)
	c.Write([]byte{5, 1, 0, 5, 1, 0, 3, 50, 'a'})
	start = time.Now()
	sel := make([]byte, 2)
	io.ReadFull(c, sel)
	expectClosed(t, c, c)
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("stalled SOCKS handshake cut off after %v", el)
	}
	st, _ := e.m.Stats("sb1")
	if st.Errors[ReasonHeaderTimeout] != 3 {
		t.Errorf("stats: %+v", st)
	}
	if len(e.dials()) != 0 {
		t.Error("something was dialled")
	}
}

func TestHeaderTooLarge(t *testing.T) {
	e := newEnv(t, Config{MaxHeaderBytes: 2048})
	e.res.set("api.test", pubA)
	path := e.attach("sb1", allowlist("api.test"))
	c := dialSock(t, path)
	go io.WriteString(c, "GET http://api.test/ HTTP/1.1\r\nX-Big: "+strings.Repeat("a", 64<<10)+"\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != 431 {
		t.Fatalf("oversized header: %v %v", resp, err)
	}
	// Just under the limit is fine (and refused only by the resolver stub).
	c = dialSock(t, path)
	io.WriteString(c, "GET http://nx.api.test/ HTTP/1.1\r\nX-Big: "+strings.Repeat("a", 1900)+"\r\n\r\n")
	resp, err = http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("header under the limit: %v %v", resp, err)
	}
}

func TestIdleAndLifetime(t *testing.T) {
	port, _ := echoServer(t)
	t.Run("idle", func(t *testing.T) {
		e := newEnv(t, Config{IdleTimeout: 300 * time.Millisecond})
		e.res.set("api.test", pubA)
		path := e.attach("sb1", allowlist("api.test"))
		_, _, c, br := connectStatus(t, path, "api.test:"+port)
		// Activity keeps the tunnel open past the idle timeout.
		for i := 0; i < 6; i++ {
			assertEcho(t, c, br, "tick")
			time.Sleep(100 * time.Millisecond)
		}
		start := time.Now()
		expectClosed(t, c, br)
		if el := time.Since(start); el > 2*time.Second {
			t.Errorf("idle tunnel closed after %v", el)
		}
		e.waitDecision(func(d Decision) bool { return d.Reason == ReasonIdleTimeout && d.Verdict == VerdictAllowed })
	})
	t.Run("lifetime", func(t *testing.T) {
		e := newEnv(t, Config{MaxTunnelLifetime: 400 * time.Millisecond})
		e.res.set("api.test", pubA)
		path := e.attach("sb1", allowlist("api.test"))
		_, _, c, br := connectStatus(t, path, "api.test:"+port)
		start := time.Now()
		for {
			_ = c.SetDeadline(time.Now().Add(testWait))
			if _, err := io.WriteString(c, "busy"); err != nil {
				break
			}
			if _, err := io.ReadFull(br, make([]byte, 4)); err != nil {
				break
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("busy tunnel outlived its maximum lifetime")
			}
			time.Sleep(10 * time.Millisecond)
		}
		e.waitDecision(func(d Decision) bool { return d.Reason == ReasonMaxLifetime })
	})
}

func TestUpstreamDownIsNotPolicy(t *testing.T) {
	e := newEnv(t, Config{DialTimeout: time.Second})
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	_, deadPort, _ := net.SplitHostPort(closed.Addr().String())
	closed.Close()
	e.res.set("down.test", pubA)
	path := e.attach("sb1", allowlist("down.test", "nx.test"))
	if status, reason, _, _ := connectStatus(t, path, "down.test:"+deadPort); status != 502 || reason != string(ReasonDialFailed) {
		t.Errorf("refused upstream: %d %q", status, reason)
	}
	status, body, hdr := get(t, proxyClient(t, path), "http://down.test:"+deadPort+"/")
	if status != 502 || hdr.Get("X-Egress-Verdict") != "error" || !strings.Contains(body, "refused") {
		t.Errorf("refused upstream over HTTP: %d %q", status, body)
	}
	if status, reason, _, _ := connectStatus(t, path, "nx.test:80"); status != 502 || reason != string(ReasonResolveFailed) {
		t.Errorf("unresolvable: %d %q", status, reason)
	}
	st, _ := e.m.Stats("sb1")
	if st.Errors[ReasonDialFailed] != 2 || st.Errors[ReasonResolveFailed] != 1 || len(st.Denied) != 0 {
		t.Errorf("stats: %+v", st)
	}
}

func TestAllowedPorts(t *testing.T) {
	port, _ := echoServer(t)
	e := newEnv(t, Config{AllowedPorts: []uint16{443, uint16(atoi(t, port))}})
	e.res.set("api.test", pubA)
	path := e.attach("sb1", core.NetworkPolicy{Mode: core.NetworkFull})
	if status, _, _, _ := connectStatus(t, path, "api.test:"+port); status != 200 {
		t.Errorf("allowed port: %d", status)
	}
	if status, reason, _, _ := connectStatus(t, path, "api.test:22"); status != 403 || reason != string(ReasonPortNotAllowed) {
		t.Errorf("port 22: %d %q", status, reason)
	}
	if status, _, _ := get(t, proxyClient(t, path), "http://api.test/"); status != 403 {
		t.Errorf("default port 80: %d", status)
	}
	if e.res.count("api.test") != 1 {
		t.Errorf("resolver called for a refused port")
	}
}

func TestSandboxesIsolated(t *testing.T) {
	e := newEnv(t, Config{})
	port, _ := echoServer(t)
	e.res.set("a.test", pubA)
	e.res.set("b.test", pubB)
	pa := e.attach("sbA", allowlist("a.test"))
	pb := e.attach("sbB", allowlist("b.test"))
	pn := e.attach("sbN", core.NetworkPolicy{})

	sa, _, ca, bra := connectStatus(t, pa, "a.test:"+port)
	sb, _, cb, brb := connectStatus(t, pb, "b.test:"+port)
	if sa != 200 || sb != 200 {
		t.Fatalf("own hosts: %d %d", sa, sb)
	}
	if s, _, _, _ := connectStatus(t, pa, "b.test:"+port); s != 403 {
		t.Errorf("A reached B's host: %d", s)
	}
	if s, _, _, _ := connectStatus(t, pb, "a.test:"+port); s != 403 {
		t.Errorf("B reached A's host: %d", s)
	}
	for _, h := range []string{"a.test", "b.test"} {
		if s, reason, _, _ := connectStatus(t, pn, h+":"+port); s != 403 || reason != string(ReasonModeNone) {
			t.Errorf("default (none) sandbox reached %s: %d %q", h, s, reason)
		}
	}
	// Tightening A leaves B's live tunnel and policy alone.
	if err := e.m.Update("sbA", core.NetworkPolicy{Mode: core.NetworkNone}); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, ca, bra)
	assertEcho(t, cb, brb, "b lives")
	// Detaching A leaves B serving.
	if err := e.m.Detach("sbA"); err != nil {
		t.Fatal(err)
	}
	assertEcho(t, cb, brb, "b still lives")
	if s, _, _, _ := connectStatus(t, pb, "b.test:"+port); s != 200 {
		t.Errorf("B after A detached: %d", s)
	}
	stB, _ := e.m.Stats("sbB")
	if stB.Allowed != 2 || stB.Denied[ReasonHostNotAllowed] != 1 || stB.ActiveConns != 2 {
		t.Errorf("B stats: %+v", stB)
	}
	e.mu.Lock()
	for _, d := range e.decisions {
		if (d.SandboxID == "sbA" && d.Host == "b.test" || d.SandboxID == "sbB" && d.Host == "a.test") && d.Verdict == VerdictAllowed {
			t.Errorf("cross-sandbox allow: %+v", d)
		}
	}
	e.mu.Unlock()
}

// TestDialControlLastLine exercises the production dialer (no test hook):
// even if a denied address somehow got past the filter, the Control hook
// refuses it before any packet leaves.
func TestDialControlLastLine(t *testing.T) {
	m, err := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	port, accepted := echoServer(t)
	p := uint16(atoi(t, port))
	full, _ := compilePolicy(core.NetworkPolicy{Mode: core.NetworkFull})
	for _, a := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "169.254.169.254", "10.0.0.1", "0.0.0.0"} {
		c, err := m.dialValidated(context.Background(), full, netip.AddrPortFrom(netip.MustParseAddr(a), p), time.Second)
		var dd *errDialDenied
		if err == nil {
			c.Close()
			t.Errorf("dial %s succeeded", a)
		} else if !errors.As(err, &dd) {
			t.Errorf("dial %s: %v, want a deny error from Control", a, err)
		}
	}
	// A policy DenyCIDR is enforced there too.
	denyPol, _ := compilePolicy(core.NetworkPolicy{Mode: core.NetworkFull, DenyCIDRs: []string{"93.184.216.0/24"}})
	_, err = m.dialValidated(context.Background(), denyPol, netip.AddrPortFrom(pubA, p), time.Second)
	var dd *errDialDenied
	if !errors.As(err, &dd) {
		t.Errorf("policy-denied dial: %v", err)
	}
	if accepted() != 0 {
		t.Error("loopback server was reached")
	}
	// End to end through a production-configured manager: loopback is
	// unreachable whatever the resolver says.
	res := &fakeResolver{hosts: map[string][]netip.Addr{"local.test": {netip.MustParseAddr("127.0.0.1")}}, seq: map[string][][]netip.Addr{}, calls: map[string]int{}}
	m2, err := New(Config{Resolver: res, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	dir, _ := os.MkdirTemp("", "egr")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "p.sock")
	if err := m2.Attach("prod", path, core.NetworkPolicy{Mode: core.NetworkFull}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"local.test:" + port, "127.0.0.1:" + port, "localhost:" + port} {
		if status, _, _, _ := connectStatus(t, path, target); status == 200 {
			t.Errorf("production manager tunnelled to %s", target)
		}
	}
	if accepted() != 0 {
		t.Error("loopback server was reached through the proxy")
	}
}

func TestExpectContinueAndLargeBody(t *testing.T) {
	e := newEnv(t, Config{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, "got %d", n)
	}))
	defer srv.Close()
	e.res.set("up.test", pubA)
	path := e.attach("sb1", allowlist("up.test"))
	target := "up.test:" + portOf(t, srv.URL)
	c := dialSock(t, path)
	br := bufio.NewReader(c)
	fmt.Fprintf(c, "POST http://%s/ HTTP/1.1\r\nHost: %s\r\nContent-Length: 300000\r\nExpect: 100-continue\r\n\r\n", target, target)
	line, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 100") {
		t.Fatalf("expected 100 Continue, got %q %v", line, err)
	}
	br.ReadString('\n')
	go c.Write(bytes.Repeat([]byte("z"), 300000))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "got 300000" {
		t.Errorf("body %q", b)
	}
	// The connection is reusable afterwards.
	fmt.Fprintf(c, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	if resp, err = http.ReadResponse(br, nil); err != nil || resp.StatusCode != 200 {
		t.Fatalf("reuse: %v", err)
	}
}

func TestConcurrentChurn(t *testing.T) {
	e := newEnv(t, Config{})
	port, _ := echoServer(t)
	e.res.set("a.test", pubA)
	e.res.set("b.test", pubB)
	path := e.attach("sb1", allowlist("a.test", "b.test"))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			host := []string{"a.test", "b.test"}[i%2]
			for {
				select {
				case <-stop:
					return
				default:
				}
				c, err := net.Dial("unix", path)
				if err != nil {
					return
				}
				_ = c.SetDeadline(time.Now().Add(testWait))
				fmt.Fprintf(c, "CONNECT %s:%s HTTP/1.1\r\n\r\n", host, port)
				br := bufio.NewReader(c)
				if resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"}); err == nil && resp.StatusCode == 200 {
					io.WriteString(c, "data")
					io.ReadFull(br, make([]byte, 4))
				}
				c.Close()
			}
		}(i)
	}
	policies := []core.NetworkPolicy{allowlist("a.test"), allowlist("b.test"), {Mode: core.NetworkNone}, {Mode: core.NetworkFull}, allowlist("a.test", "b.test")}
	for i := 0; i < 200; i++ {
		if err := e.m.Update("sb1", policies[i%len(policies)]); err != nil {
			t.Fatal(err)
		}
		e.m.Stats("sb1")
		time.Sleep(time.Millisecond)
	}
	if err := e.m.Detach("sb1"); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
}

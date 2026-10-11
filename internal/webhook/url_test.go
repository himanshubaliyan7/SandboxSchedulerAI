package webhook

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

// fakeResolver answers from a script: each lookup of a host consumes the
// next answer, and the last one repeats. It counts lookups.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]string
	calls   map[string]int
	err     error
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{answers: map[string][][]string{}, calls: map[string]int{}}
}

func (r *fakeResolver) set(host string, answers ...[]string) {
	r.mu.Lock()
	r.answers[host] = answers
	r.mu.Unlock()
}

func (r *fakeResolver) lookups(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

func (r *fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if network != "ip" {
		return nil, errors.New("unexpected network " + network)
	}
	n := r.calls[host]
	r.calls[host]++
	if r.err != nil {
		return nil, r.err
	}
	as := r.answers[host]
	if len(as) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	if n >= len(as) {
		n = len(as) - 1
	}
	out := make([]netip.Addr, 0, len(as[n]))
	for _, s := range as[n] {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

func testDest(t *testing.T, cfg DestinationConfig) (*Destinations, *fakeResolver) {
	t.Helper()
	r := newFakeResolver()
	if cfg.Resolver == nil {
		cfg.Resolver = r
	}
	d, err := NewDestinations(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d, r
}

// publicAddr is an address no rule denies (TEST-NET would be denied as
// documentation space).
const publicAddr = "93.184.216.34"

// deniedAddrs is one address from every range a webhook must not reach.
var deniedAddrs = []string{
	"127.0.0.1", "127.255.255.254", "::1", // loopback
	"10.0.0.1", "10.255.255.255", "172.16.0.1", "172.31.255.255", "192.168.0.1", "192.168.255.255", // RFC 1918
	"100.64.0.1", "100.127.255.255", // CGNAT
	"169.254.0.1", "169.254.255.255", "fe80::1", // link-local
	"fc00::1", "fd00::1", "fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", // ULA
	"169.254.169.254", "100.100.100.200", "168.63.129.16", "fd00:ec2::254", // cloud metadata
	"0.0.0.0", "0.1.2.3", "::", // unspecified
	"224.0.0.1", "ff02::1", "255.255.255.255", // multicast, broadcast
	"240.0.0.1", "192.0.0.1", "198.18.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1", "2001:db8::1", // reserved, benchmark, documentation
	"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::ffff:192.168.1.1", // IPv4-mapped IPv6
	"64:ff9b::7f00:1", "64:ff9b::a00:1", "64:ff9b::a9fe:a9fe", // NAT64 around loopback, private, metadata
	"2002:7f00:1::1", "2002:a00:1::1", // 6to4 around loopback and private
	"2001:0:a00:1::1",                        // Teredo with a private server address
	"::7f00:1", "::ffff:0:7f00:1", "fec0::1", // IPv4-compatible, IPv4-translated, site-local
}

func TestParseURL(t *testing.T) {
	d, _ := testDest(t, DestinationConfig{})
	for _, c := range []struct {
		in, want string
	}{
		{"https://hooks.example.com/sbx", "https://hooks.example.com/sbx"},
		{"HTTPS://Hooks.Example.COM./sbx?x=1&y=%20", "https://hooks.example.com/sbx?x=1&y=%20"},
		{"https://hooks.example.com", "https://hooks.example.com"},
		{"https://hooks.example.com:443/a", "https://hooks.example.com/a"},
		{"https://hooks.example.com:8443/a/b%2Fc", "https://hooks.example.com:8443/a/b%2Fc"},
		{"https://" + publicAddr + "/h", "https://" + publicAddr + "/h"},
		{"https://[2606:2800:220:1:248:1893:25c8:1946]:8443/h", "https://[2606:2800:220:1:248:1893:25c8:1946]:8443/h"},
	} {
		got, err := d.Parse(c.in)
		if err != nil || got.URL.String() != c.want {
			t.Errorf("Parse(%q) = %v, %v; want %q", c.in, got.URL, err, c.want)
		}
	}
	for _, c := range []struct{ in, why string }{
		{"", "required"},
		{"http://hooks.example.com/", "https"},
		{"ftp://hooks.example.com/", "https"},
		{"//hooks.example.com/", "https"},
		{"hooks.example.com/path", "https"},
		{"https:hooks.example.com", "host"},
		{"https:///path", "host"},
		{"https://user@hooks.example.com/", "user name"},
		{"https://user:pw@hooks.example.com/", "user name"},
		{"https://hooks.example.com@127.0.0.1/", "user name"},
		{"https://hooks.example.com/#frag", "fragment"},
		{"https://hooks.example.com/#", "fragment"},
		{"https://hooks.example.com:0/", "port"},
		{"https://hooks.example.com:/", "port"},
		{"https://hooks.example.com:99999/", ""},
		{"https://hooks.example.com:80a/", ""},
		{"https://hooks example.com/", "space"},
		{"https://hooks.example.com/a b", "space"},
		{"https://hooks.example.com/\n", "control"},
		{"https://hooks.example.com\\@127.0.0.1/", "backslash"},
		{"https://127.1/", "host"},
		{"https://0x7f.0.0.1/", "host"},
		{"https://2130706433/", "host"},
		{"https://0177.0.0.1/", "host"},
		{"https://[fe80::1%25eth0]/", "host"},
		{"https://exa_mple..com/", "host"},
		{"https://bücher.example/", "host"},
		{"https://" + strings.Repeat("a", 2100) + ".example.com/", "longer"},
	} {
		_, err := d.Parse(c.in)
		de, ok := asDestinationError(err)
		if !ok || (de.Code != CodeInvalidURL && de.Code != CodeDestinationNotAllowed) {
			t.Errorf("Parse(%q) = %v; want a refusal", c.in, err)
			continue
		}
		if c.why != "" && !strings.Contains(de.Message, c.why) {
			t.Errorf("Parse(%q): message %q does not mention %q", c.in, de.Message, c.why)
		}
	}
	// An IP literal in any denied range is refused before anything is sent.
	for _, a := range deniedAddrs {
		host := a
		if strings.Contains(a, ":") {
			host = "[" + a + "]"
		}
		_, err := d.Parse("https://" + host + "/hook")
		if de, ok := asDestinationError(err); !ok || de.Code != CodeDestinationNotAllowed {
			t.Errorf("Parse(https://%s/hook) = %v; want %s", host, err, CodeDestinationNotAllowed)
		}
	}

	// http is a development switch.
	dev, _ := testDest(t, DestinationConfig{AllowHTTP: true})
	if got, err := dev.Parse("http://hooks.example.com:80/x"); err != nil || got.URL.String() != "http://hooks.example.com/x" || got.Port != 80 {
		t.Errorf("http with AllowHTTP: %v %v", got.URL, err)
	}
	if _, err := dev.Parse("http://127.0.0.1/x"); err == nil {
		t.Error("AllowHTTP opened loopback")
	}
}

func TestResolveRefusesEveryDeniedRange(t *testing.T) {
	d, r := testDest(t, DestinationConfig{})
	ctx := context.Background()
	for _, a := range deniedAddrs {
		r.set("hooks.example.com", []string{a})
		target, err := d.Parse("https://hooks.example.com/h")
		if err != nil {
			t.Fatal(err)
		}
		addrs, err := d.Resolve(ctx, target)
		de, ok := asDestinationError(err)
		if !ok || de.Code != CodeDestinationNotAllowed || len(addrs) != 0 {
			t.Errorf("a name resolving to %s: %v, %v; want %s", a, addrs, err, CodeDestinationNotAllowed)
			continue
		}
		// The tenant is not told what the name resolved to, nor which rule
		// refused it; the log is.
		if strings.Contains(de.Message, a) || de.Detail == "" {
			t.Errorf("%s: message %q, detail %q", a, de.Message, de.Detail)
		}
	}

	// The same message whether the name does not exist, the lookup fails
	// or the name points somewhere private.
	r.set("hooks.example.com", []string{"10.0.0.1"})
	target, _ := d.Parse("https://hooks.example.com/h")
	_, errPrivate := d.Resolve(ctx, target)
	r.set("hooks.example.com")
	_, errMissing := d.Resolve(ctx, target)
	r.err = errors.New("server misbehaving")
	_, errBroken := d.Resolve(ctx, target)
	r.err = nil
	if errPrivate.Error() != errMissing.Error() || errPrivate.Error() != errBroken.Error() {
		t.Errorf("the three refusals differ:\n %v\n %v\n %v", errPrivate, errMissing, errBroken)
	}

	// Mixed answers: only the public addresses survive, in order, once.
	r.set("hooks.example.com", []string{"10.0.0.1", publicAddr, "::ffff:" + publicAddr, "::1", "2606:2800:220:1:248:1893:25c8:1946"})
	addrs, err := d.Resolve(ctx, target)
	if err != nil || len(addrs) != 2 || addrs[0] != netip.MustParseAddr(publicAddr) || !addrs[1].Is6() {
		t.Errorf("mixed answers: %v, %v", addrs, err)
	}

	// The host's own addresses, whatever range they are in.
	ifaces, _ := net.InterfaceAddrs()
	for _, ia := range ifaces {
		ipn, ok := ia.(*net.IPNet)
		if !ok {
			continue
		}
		a, ok := netip.AddrFromSlice(ipn.IP)
		if !ok || a.Unmap().IsLinkLocalUnicast() {
			continue
		}
		r.set("self.example.com", []string{a.Unmap().String()})
		self, _ := d.Parse("https://self.example.com/h")
		if _, err := d.Resolve(ctx, self); err == nil {
			t.Errorf("the host's own address %s was accepted", a)
		}
	}
}

func TestOperatorExceptions(t *testing.T) {
	ctx := context.Background()
	d, r := testDest(t, DestinationConfig{PermitPrivateCIDRs: []string{"10.20.0.0/16", "fd12:3456::/32"}})
	r.set("onprem.example.com", []string{"10.20.3.4"})
	target, _ := d.Parse("https://onprem.example.com/h")
	if addrs, err := d.Resolve(ctx, target); err != nil || len(addrs) != 1 {
		t.Errorf("permitted private range: %v %v", addrs, err)
	}
	if _, err := d.Parse("https://10.20.3.4/h"); err != nil {
		t.Errorf("permitted private literal: %v", err)
	}
	if _, err := d.Parse("https://[fd12:3456::1]/h"); err != nil {
		t.Errorf("permitted ULA literal: %v", err)
	}
	for _, a := range []string{"10.21.0.1", "192.168.0.1", "127.0.0.1", "169.254.169.254", "fd00:ec2::254"} {
		r.set("onprem.example.com", []string{a})
		if _, err := d.Resolve(ctx, target); err == nil {
			t.Errorf("%s was opened by an exception for another range", a)
		}
	}
	// Only private space can be excepted.
	for _, bad := range []string{"127.0.0.0/8", "169.254.0.0/16", "8.8.8.0/24", "0.0.0.0/0", "nonsense"} {
		if _, err := NewDestinations(DestinationConfig{PermitPrivateCIDRs: []string{bad}}); err == nil {
			t.Errorf("PermitPrivateCIDRs %q accepted", bad)
		}
	}

	// The loopback switch opens loopback and nothing else.
	lo, r2 := testDest(t, DestinationConfig{AllowLoopback: true})
	for _, a := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1"} {
		r2.set("localhost.example.com", []string{a})
		lt, _ := lo.Parse("https://localhost.example.com/h")
		if _, err := lo.Resolve(ctx, lt); err != nil {
			t.Errorf("AllowLoopback: %s refused: %v", a, err)
		}
	}
	for _, a := range []string{"10.0.0.1", "169.254.169.254", "192.168.1.1", "64:ff9b::7f00:1"} {
		r2.set("localhost.example.com", []string{a})
		lt, _ := lo.Parse("https://localhost.example.com/h")
		if _, err := lo.Resolve(ctx, lt); err == nil {
			t.Errorf("AllowLoopback opened %s", a)
		}
	}
}

func TestCheckForStoring(t *testing.T) {
	d, r := testDest(t, DestinationConfig{})
	r.set("hooks.example.com", []string{publicAddr})
	got, err := d.Check(context.Background(), "HTTPS://hooks.example.com:443/in")
	if err != nil || got != "https://hooks.example.com/in" || r.lookups("hooks.example.com") != 1 {
		t.Errorf("Check = %q, %v after %d lookups", got, err, r.lookups("hooks.example.com"))
	}
	r.set("internal.example.com", []string{"192.168.1.10"})
	if _, err := d.Check(context.Background(), "https://internal.example.com/in"); err == nil {
		t.Error("a name that resolves to a private address was accepted for storing")
	}
	if _, err := d.Check(context.Background(), "https://nowhere.example.com/in"); err == nil {
		t.Error("a name that does not resolve was accepted for storing")
	}
	// A literal needs no lookup.
	if _, err := d.Check(context.Background(), "https://"+publicAddr+"/in"); err != nil {
		t.Errorf("public literal: %v", err)
	}
}

func TestLogURLDropsPathAndQuery(t *testing.T) {
	for in, want := range map[string]string{
		"https://hooks.example.com/services/T000/B000/s3cr3tPath?token=abc": "https://hooks.example.com/...",
		"https://example.com:8443": "https://example.com:8443/...",
		"::not a url":              "(unparsable)",
	} {
		if got := LogURL(in); got != want {
			t.Errorf("LogURL(%q) = %q, want %q", in, got, want)
		}
	}
}

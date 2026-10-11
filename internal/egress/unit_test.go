package egress

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
)

func TestDomainMatch(t *testing.T) {
	ds, err := compileDomains([]string{"example.com", "*.wild.org", "API.Mixed.Net.", "*.Deep.Example.io", "a.b.c.test"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host string
		want bool
	}{
		// exact
		{"example.com", true},
		{"EXAMPLE.com", true},
		{"example.com.", true},
		{"www.example.com", false}, // exact entry does not cover subdomains
		{"evilexample.com", false},
		{"example.com.evil.net", false},
		{"xample.com", false},
		{"example.co", false},
		{"com", false},
		// wildcard: any depth, never the apex
		{"wild.org", false},
		{"a.wild.org", true},
		{"a.b.c.wild.org", true},
		{"A.B.WILD.ORG.", true},
		{"evilwild.org", false},
		{"a.evilwild.org", false},
		{"wild.org.evil.net", false},
		{"a.wild.org.evil.net", false},
		{"wild.orgx", false},
		{"org", false},
		// case and trailing dot in the entry
		{"api.mixed.net", true},
		{"x.api.mixed.net", false},
		{"deep.example.io", false},
		{"x.deep.example.io", true},
		{"x.y.deep.example.io", true},
		{"example.io", false},
		{"xdeep.example.io", false},
		// multi-label exact
		{"a.b.c.test", true},
		{"b.c.test", false},
		{"z.a.b.c.test", false},
	}
	for _, c := range cases {
		host, ip, err := normalizeHost(c.host)
		if err != nil || ip.IsValid() {
			t.Errorf("normalizeHost(%q) = %q, %v, %v", c.host, host, ip, err)
			continue
		}
		if got := ds.match(host); got != c.want {
			t.Errorf("match(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestCompileDomainsRejects(t *testing.T) {
	for _, e := range []string{"", "*", "*.", "*.*.example.com", "foo.*.com", "ex*mple.com", "1.2.3.4", "*.1.2.3.4", "::1", "a..b", "-a.com", "exa mple.com", "http://example.com", "example.com:443", "bücher.de"} {
		if _, err := compileDomains([]string{e}); err == nil {
			t.Errorf("compileDomains(%q) accepted", e)
		}
	}
}

func TestParseTarget(t *testing.T) {
	good := []struct {
		in   string
		def  uint16
		host string
		port uint16
		isIP bool
	}{
		{"example.com:443", 0, "example.com", 443, false},
		{"EXAMPLE.Com.:8080", 0, "example.com", 8080, false},
		{"example.com", 80, "example.com", 80, false},
		{"a_b.example.com:1", 0, "a_b.example.com", 1, false},
		{"xn--bcher-kva.de:443", 0, "xn--bcher-kva.de", 443, false},
		{"localhost:65535", 0, "localhost", 65535, false},
		{"1.2.3.4:80", 0, "1.2.3.4", 80, true},
		{"[2001:db8::1]:443", 0, "2001:db8::1", 443, true},
		{"[2001:DB8:0:0::1]", 80, "2001:db8::1", 80, true},
		{"[::ffff:1.2.3.4]:80", 0, "1.2.3.4", 80, true},
		{"3com.example:80", 0, "3com.example", 80, false},
	}
	for _, c := range good {
		got, err := parseTarget(c.in, c.def)
		if err != nil {
			t.Errorf("parseTarget(%q): %v", c.in, err)
			continue
		}
		if got.host != c.host || got.port != c.port || got.isIP() != c.isIP {
			t.Errorf("parseTarget(%q) = %+v", c.in, got)
		}
	}
	bad := []string{
		"", ":80", "example.com", "example.com:", "example.com:0", "example.com:65536", "example.com:99999",
		"example.com:080", "example.com:+80", "example.com:-1", "example.com: 80", "example.com:8o", "example.com:80:80",
		"example.com:443/", "exam ple.com:80", "example.com\r\n:80", "exa\x00mple.com:80", "bücher.de:80",
		"user@example.com:80", "example..com:80", ".example.com:80", "-example.com:80", "example-.com:80",
		strings.Repeat("a", 64) + ".com:80", strings.Repeat("a.", 127) + "com:80",
		"..:80", ".:80",
		// numeric forms that are not canonical IP literals
		"127.1:80", "0x7f.0.0.1:80", "0x7f000001:80", "2130706433:80", "017700000001:80", "010.0.0.1:80",
		"1.2.3:80", "1.2.3.4.:80", "1.2.3.4.5:80", "example.123:80", "1.2.3.256:80",
		// IPv6 syntax
		"::1:80", "2001:db8::1", "[::1", "[::1]x:80", "[::1]80", "[fe80::1%eth0]:80", "[fe80::1%25eth0]:80",
		"[1.2.3.4]:80", "[example.com]:80", "[]:80", "[[::1]]:80", "[::1]]:80", "[:::1]:80",
		"fe80::1%eth0", "1.2.3.4%eth0:80",
	}
	for _, in := range bad {
		if got, err := parseTarget(in, 0); err == nil {
			t.Errorf("parseTarget(%q) accepted: %+v", in, got)
		}
	}
}

func testDenySet(t *testing.T, permit []string, ifaces []string) *denySet {
	t.Helper()
	d, err := newDenySet(permit, time.Hour, func() ([]net.Addr, error) {
		var out []net.Addr
		for _, s := range ifaces {
			ip, n, err := net.ParseCIDR(s)
			if err != nil {
				return nil, err
			}
			out = append(out, &net.IPNet{IP: ip, Mask: n.Mask})
		}
		return out, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDenySet(t *testing.T) {
	d := testDenySet(t, nil, []string{"203.0.114.7/24", "2a00:1450::99/64"})
	cases := []struct {
		addr string
		cat  string // "" = allowed
	}{
		// loopback
		{"127.0.0.1", catLoopback}, {"127.255.255.254", catLoopback}, {"::1", catLoopback},
		// unspecified
		{"0.0.0.0", catUnspecified}, {"0.1.2.3", catUnspecified}, {"::", catUnspecified},
		// link-local
		{"169.254.0.1", catLinkLocal}, {"169.254.255.255", catLinkLocal}, {"fe80::1", catLinkLocal}, {"febf::1", catLinkLocal},
		// metadata
		{"169.254.169.254", catMetadata}, {"100.100.100.200", catMetadata}, {"168.63.129.16", catMetadata}, {"fd00:ec2::254", catMetadata},
		// RFC 1918
		{"10.0.0.1", catPrivate}, {"10.255.255.255", catPrivate}, {"172.16.0.1", catPrivate}, {"172.31.255.255", catPrivate},
		{"192.168.0.1", catPrivate}, {"192.168.255.255", catPrivate},
		// CGNAT
		{"100.64.0.1", catCGNAT}, {"100.127.255.255", catCGNAT},
		// ULA
		{"fc00::1", catULA}, {"fd12:3456:789a::1", catULA}, {"fdff:ffff::1", catULA},
		// multicast, broadcast, reserved
		{"224.0.0.1", catMulticast}, {"239.255.255.255", catMulticast}, {"ff02::1", catMulticast}, {"ff0e::1", catMulticast},
		{"255.255.255.255", catBroadcast}, {"240.0.0.1", catReserved}, {"254.1.2.3", catReserved},
		// documentation, benchmark, other reserved
		{"192.0.2.1", catDocumentation}, {"198.51.100.1", catDocumentation}, {"203.0.113.1", catDocumentation},
		{"2001:db8::1", catDocumentation}, {"3fff::1", catDocumentation},
		{"198.18.0.1", catBenchmark}, {"198.19.255.255", catBenchmark}, {"2001:2::1", catBenchmark},
		{"192.0.0.1", catReserved}, {"192.88.99.1", catReserved}, {"100::1", catReserved}, {"fec0::1", catReserved},
		{"64:ff9b:1::1", catReserved}, {"2001:20::1", catReserved}, {"2001:1::1", catReserved},
		{"4000::1", catReserved}, {"::2", catReserved}, {"1::1", catReserved}, {"e000::1", catReserved},
		// IPv4-mapped IPv6 is judged as the IPv4 address
		{"::ffff:127.0.0.1", catLoopback}, {"::ffff:10.0.0.1", catPrivate}, {"::ffff:169.254.169.254", catMetadata},
		{"::ffff:192.168.1.1", catPrivate}, {"::ffff:100.64.0.1", catCGNAT}, {"::ffff:0.0.0.0", catUnspecified},
		// IPv4-compatible and IPv4-translated forms
		{"::127.0.0.1", catReserved}, {"::10.0.0.1", catReserved}, {"::8.8.8.8", catReserved},
		{"::ffff:0:127.0.0.1", catReserved}, {"::ffff:0:8.8.8.8", catReserved},
		// NAT64
		{"64:ff9b::7f00:1", "embedded_" + catLoopback}, {"64:ff9b::a00:1", "embedded_" + catPrivate},
		{"64:ff9b::a9fe:a9fe", "embedded_" + catMetadata}, {"64:ff9b::c0a8:101", "embedded_" + catPrivate},
		{"64:ff9b::6464:64c8", "embedded_" + catMetadata}, {"64:ff9b::", "embedded_" + catUnspecified},
		// 6to4
		{"2002:7f00:1::1", "embedded_" + catLoopback}, {"2002:a00:1::", "embedded_" + catPrivate},
		{"2002:a9fe:a9fe::1", "embedded_" + catMetadata}, {"2002:c0a8:101::1", "embedded_" + catPrivate},
		{"2002:6440:1::1", "embedded_" + catCGNAT},
		// Teredo: server in clear, client inverted (10.0.0.1 -> f5ff:fffe)
		{"2001:0:7f00:1::1", "embedded_" + catLoopback}, {"2001:0:a9fe:a9fe::1", "embedded_" + catMetadata},
		{"2001:0:808:808:0:0:f5ff:fffe", "embedded_" + catPrivate}, {"2001:0:808:808:0:0:80ff:fffe", "embedded_" + catLoopback},
		// host interface addresses, also via mapping and embedding
		{"203.0.114.7", catHostInterface}, {"2a00:1450::99", catHostInterface}, {"::ffff:203.0.114.7", catHostInterface},
		{"64:ff9b::cb00:7207", "embedded_" + catHostInterface}, {"2002:cb00:7207::1", "embedded_" + catHostInterface},
		// zones
		{"fe80::1%eth0", catInvalid}, {"2606:4700::1%eth0", catInvalid},

		// allowed: public addresses next to the denied ranges
		{"8.8.8.8", ""}, {"1.1.1.1", ""}, {"93.184.216.34", ""}, {"9.255.255.255", ""}, {"11.0.0.0", ""},
		{"172.15.255.255", ""}, {"172.32.0.0", ""}, {"192.167.255.255", ""}, {"192.169.0.0", ""},
		{"100.63.255.255", ""}, {"100.128.0.0", ""}, {"169.253.255.255", ""}, {"169.255.0.0", ""},
		{"126.255.255.255", ""}, {"128.0.0.0", ""}, {"223.255.255.255", ""}, {"198.17.255.255", ""}, {"198.20.0.0", ""},
		{"168.63.129.15", ""}, {"203.0.114.8", ""}, {"192.0.1.1", ""},
		{"2606:4700:4700::1111", ""}, {"2a00:1450::98", ""}, {"2001:4860:4860::8888", ""}, {"::ffff:8.8.8.8", ""},
		{"64:ff9b::808:808", ""}, {"2002:808:808::1", ""}, {"2001:0:808:808:0:0:f7f7:f7f7", ""},
	}
	for _, c := range cases {
		a, err := netip.ParseAddr(c.addr)
		if err != nil {
			t.Fatalf("bad test address %q: %v", c.addr, err)
		}
		cat, denied := d.check(a)
		if denied != (c.cat != "") || cat != c.cat {
			t.Errorf("check(%s) = %q, %v; want %q", c.addr, cat, denied, c.cat)
		}
	}
	if _, denied := d.check(netip.Addr{}); !denied {
		t.Error("the zero Addr must be denied")
	}
}

func TestPermitPrivateCIDRs(t *testing.T) {
	for _, bad := range []string{"0.0.0.0/0", "127.0.0.0/8", "169.254.0.0/16", "169.254.169.254", "8.8.8.0/24", "10.0.0.0/7", "::/0", "fe80::/10", "::1", "garbage", "172.0.0.0/8"} {
		if _, err := newDenySet([]string{bad}, time.Hour, func() ([]net.Addr, error) { return nil, nil }); err == nil {
			t.Errorf("PermitPrivateCIDRs %q accepted", bad)
		}
	}
	d := testDenySet(t, []string{"10.20.0.0/16", "100.64.0.0/10", "fd00::/8"}, []string{"10.20.0.1/16"})
	cases := []struct {
		addr   string
		denied bool
	}{
		{"10.20.5.5", false},
		{"::ffff:10.20.5.5", false},
		{"10.21.0.1", true},
		{"10.20.0.1", true},         // host interface inside the permitted range
		{"100.100.100.200", true},   // metadata inside permitted CGNAT
		{"100.64.1.1", false},       //
		{"fd00:ec2::254", true},     // metadata inside permitted ULA
		{"fd12::1", false},          //
		{"fc00::1", true},           // outside fd00::/8
		{"127.0.0.1", true},         //
		{"169.254.169.254", true},   //
		{"192.168.1.1", true},       //
		{"64:ff9b::a14:505", false}, // NAT64 of a permitted address
		{"64:ff9b::a15:1", true},
	}
	for _, c := range cases {
		if _, denied := d.check(netip.MustParseAddr(c.addr)); denied != c.denied {
			t.Errorf("check(%s) denied = %v, want %v", c.addr, denied, c.denied)
		}
	}
}

func TestHostAddrsRefresh(t *testing.T) {
	cur := "198.41.0.4/24"
	d, err := newDenySet(nil, time.Nanosecond, func() ([]net.Addr, error) {
		ip, n, _ := net.ParseCIDR(cur)
		return []net.Addr{&net.IPNet{IP: ip, Mask: n.Mask}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, denied := d.check(netip.MustParseAddr("198.41.0.4")); !denied {
		t.Fatal("host address not denied")
	}
	cur = "199.9.14.201/24"
	time.Sleep(time.Millisecond)
	if _, denied := d.check(netip.MustParseAddr("199.9.14.201")); !denied {
		t.Fatal("new host address not denied after refresh")
	}
	if _, denied := d.check(netip.MustParseAddr("198.41.0.4")); denied {
		t.Fatal("old host address still denied after refresh")
	}
}

func TestRealHostInterfacesDenied(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip(err)
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		na, _ := netip.AddrFromSlice(ipn.IP)
		if cat, denied := m.deny.check(na); !denied {
			t.Errorf("host address %s not denied", na)
		} else {
			t.Logf("%s -> %s", na.Unmap(), cat)
		}
	}
}

func TestCompilePolicy(t *testing.T) {
	for _, p := range []core.NetworkPolicy{
		{Mode: "wide-open"},
		{Mode: core.NetworkAllowlist, AllowDomains: []string{"*"}},
		{Mode: core.NetworkAllowlist, AllowCIDRs: []string{"example.com"}},
		{Mode: core.NetworkFull, DenyCIDRs: []string{"1.2.3.4/33"}},
		{Mode: core.NetworkFull, DenyCIDRs: []string{"fe80::1%eth0"}},
	} {
		if _, err := compilePolicy(p); err == nil {
			t.Errorf("compilePolicy(%+v) accepted", p)
		}
	}
	p, err := compilePolicy(core.NetworkPolicy{})
	if err != nil || p.mode != core.NetworkNone {
		t.Fatalf("empty mode: %+v, %v", p, err)
	}
	p, err = compilePolicy(core.NetworkPolicy{Mode: core.NetworkFull, DenyCIDRs: []string{"::ffff:8.8.8.0/120", "9.9.9.9", "2606:4700::/32"}})
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "::ffff:8.8.8.8": true, "64:ff9b::808:808": true, "2002:808:808::1": true,
		"9.9.9.9": true, "9.9.9.10": false, "2606:4700::1": true, "8.8.4.4": false,
	} {
		if got := p.deniesAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("deniesAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

// clientHello returns the first flight a Go TLS client sends.
func clientHello(tb testing.TB, cfg *tls.Config) []byte {
	tb.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _ = tls.Client(a, cfg).Handshake() }()
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 8192)
	n, err := b.Read(buf)
	if err != nil {
		tb.Fatal(err)
	}
	return buf[:n]
}

// refragment re-splits a single TLS record into records of at most size
// payload bytes.
func refragment(rec []byte, size int) []byte {
	payload := rec[5:]
	var out []byte
	for len(payload) > 0 {
		n := size
		if n > len(payload) {
			n = len(payload)
		}
		out = append(out, rec[0], rec[1], rec[2], byte(n>>8), byte(n))
		out = append(out, payload[:n]...)
		payload = payload[n:]
	}
	return out
}

func TestClientHelloSNI(t *testing.T) {
	hello := clientHello(t, &tls.Config{ServerName: "Secure.Example.com", InsecureSkipVerify: true})
	noSNI := clientHello(t, &tls.Config{InsecureSkipVerify: true})
	peek := func(b []byte) (string, bool, bool, error) {
		br := bufio.NewReaderSize(bytes.NewReader(b), minReadBuf)
		body, isTLS, err := peekClientHello(br)
		if !isTLS || err != nil {
			return "", false, isTLS, err
		}
		if br.Buffered() != len(b) {
			t.Errorf("peekClientHello consumed input")
		}
		sni, has, err := parseClientHelloSNI(body)
		return sni, has, true, err
	}
	if sni, has, isTLS, err := peek(hello); err != nil || !isTLS || !has || !strings.EqualFold(sni, "secure.example.com") {
		t.Errorf("hello: %q %v %v %v", sni, has, isTLS, err)
	}
	if sni, has, isTLS, err := peek(noSNI); err != nil || !isTLS || has {
		t.Errorf("no SNI: %q %v %v %v", sni, has, isTLS, err)
	}
	for _, size := range []int{1, 3, 4, 5, 50, 100} {
		if sni, has, isTLS, err := peek(refragment(hello, size)); err != nil || !isTLS || !has || !strings.EqualFold(sni, "secure.example.com") {
			t.Errorf("fragment %d: %q %v %v %v", size, sni, has, isTLS, err)
		}
	}
	for name, b := range map[string][]byte{
		"ssh":   []byte("SSH-2.0-OpenSSH_9.6\r\n"),
		"http":  []byte("GET / HTTP/1.1\r\n\r\n"),
		"empty": nil,
		"0x16":  {0x16},
		"ssl2":  {0x16, 0x02, 0x00, 0x00, 0x05},
	} {
		if _, _, isTLS, err := peek(b); isTLS || err != nil {
			t.Errorf("%s: isTLS=%v err=%v", name, isTLS, err)
		}
	}
	for name, b := range map[string][]byte{
		"truncated":     hello[:len(hello)-10],
		"not a hello":   {0x16, 0x03, 0x01, 0x00, 0x04, 0x02, 0x00, 0x00, 0x00},
		"empty record":  {0x16, 0x03, 0x01, 0x00, 0x00},
		"huge record":   {0x16, 0x03, 0x01, 0xff, 0xff},
		"second record": append(append([]byte{}, refragment(hello, 20)[:25]...), 0x17, 0x03, 0x03, 0x00, 0x01, 0x00),
	} {
		if _, _, isTLS, err := peek(b); !isTLS || err == nil {
			t.Errorf("%s: isTLS=%v err=%v, want a TLS error", name, isTLS, err)
		}
	}
	// A hello whose body is cut short inside the extensions is malformed.
	body, _, _ := peekClientHello(bufio.NewReaderSize(bytes.NewReader(hello), minReadBuf))
	for i := 0; i < len(body); i++ {
		if _, _, err := parseClientHelloSNI(body[:i]); err == nil && i > 40 {
			// Only a cut exactly before the extensions block is legal.
			if _, rest, ok := takeVec(body[34:i], 1); ok {
				if _, rest, ok = takeVec(rest, 2); ok {
					if _, rest, ok = takeVec(rest, 1); ok && len(rest) == 0 {
						continue
					}
				}
			}
			t.Errorf("truncated hello body (%d of %d bytes) accepted", i, len(body))
		}
	}
}

func TestHeaderHelpers(t *testing.T) {
	for in, want := range map[string]int{
		"GET / HTTP/1.1\r\nHost: a\r\n\r\nbody": 27,
		"GET / HTTP/1.1\nHost: a\n\nbody":       24,
		"GET / HTTP/1.1\r\nHost: a\r\n":         -1,
		"GET / HTTP/1.1\r\nHost: a\n\r\nrest":   26,
		"":                                      -1,
		"\n\n":                                  2,
	} {
		if got := headerEnd([]byte(in)); got != want {
			t.Errorf("headerEnd(%q) = %d, want %d", in, got, want)
		}
	}
	hosts, err := scanHostHeaders([]byte("GET http://a/ HTTP/1.1\r\nhOsT:  a.test:80 \r\nX-Host: no\r\nHost:b\r\n\r\nHost: body\r\n"))
	if err != nil || len(hosts) != 2 || hosts[0] != "a.test:80" || hosts[1] != "b" {
		t.Errorf("scanHostHeaders = %q, %v", hosts, err)
	}
	if _, err := scanHostHeaders([]byte("GET / HTTP/1.1\r\nX: a\r\n Host: folded\r\n\r\n")); err == nil {
		t.Error("folded header accepted")
	}
	tgt := target{host: "a.test", port: 8080}
	for v, want := range map[string]bool{
		"a.test:8080": true, "A.TEST.:8080": true, "a.test": false, "a.test:80": false, "b.test:8080": false, "": false, "a.test:8080:1": false,
	} {
		if got := hostHeaderAgrees(v, tgt, 80); got != want {
			t.Errorf("hostHeaderAgrees(%q, default 80) = %v, want %v", v, got, want)
		}
	}
	if !hostHeaderAgrees("a.test", tgt, 0) || hostHeaderAgrees("a.test:81", tgt, 0) {
		t.Error("hostHeaderAgrees with optional port is wrong")
	}
}

func TestConfigValidation(t *testing.T) {
	for name, cfg := range map[string]Config{
		"negative timeout":  {DialTimeout: -1},
		"negative conns":    {MaxConnsPerSandbox: -1},
		"tiny header":       {MaxHeaderBytes: 10},
		"mode without rw":   {SocketMode: 0o066},
		"mode with setuid":  {SocketMode: 0o4600},
		"port zero":         {AllowedPorts: []uint16{0}},
		"permit public":     {PermitPrivateCIDRs: []string{"8.8.8.0/24"}},
		"permit everything": {PermitPrivateCIDRs: []string{"0.0.0.0/0"}},
	} {
		if m, err := New(cfg); err == nil {
			m.Close()
			t.Errorf("%s: accepted", name)
		}
	}
}

// A policy denial says so in words: in the reason phrase, which is all
// an HTTPS client shows of a refused CONNECT, and in the body.
func TestDenialSaysBlockedByPolicy(t *testing.T) {
	resp := httpErrorText(403, VerdictDenied, ReasonHostNotAllowed, "pypi.org is not in the allowlist")
	head, body, _ := strings.Cut(resp, "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	if lines[0] != "HTTP/1.1 403 Blocked by sandbox network policy: pypi.org is not in the allowlist" {
		t.Errorf("status line: %q", lines[0])
	}
	if body != "blocked by sandbox network policy: pypi.org is not in the allowlist (egress proxy: denied: host_not_allowed)\n" {
		t.Errorf("body: %q", body)
	}
	if !strings.Contains(head, "X-Egress-Verdict: denied\r\nX-Egress-Reason: host_not_allowed") ||
		!strings.Contains(head, "Content-Length: "+strconv.Itoa(len(body))) {
		t.Errorf("headers: %q", head)
	}
	// Free text cannot split the status line.
	resp = httpErrorText(403, VerdictDenied, ReasonHostNotAllowed, "evil\r\nX-Injected: 1")
	if first, _, _ := strings.Cut(resp, "\r\n"); strings.Contains(resp, "\r\nX-Injected") || !strings.HasSuffix(first, "evil  X-Injected: 1") {
		t.Errorf("a detail with a line break reached the header block: %q", resp)
	}
	// An upstream failure is not a policy decision and keeps the standard phrase.
	resp = httpErrorText(502, VerdictError, ReasonResolveFailed, "cannot resolve nowhere.invalid")
	if !strings.HasPrefix(resp, "HTTP/1.1 502 Bad Gateway\r\n") || !strings.HasSuffix(resp, "egress proxy: error: resolve_failed: cannot resolve nowhere.invalid\n") {
		t.Errorf("error response: %q", resp)
	}
}

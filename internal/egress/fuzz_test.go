package egress

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"net/netip"
	"strings"
	"testing"
)

// FuzzParseTarget checks the request-target parser: whatever the input, an
// accepted target is normalised, safe to echo and stable under re-parsing.
func FuzzParseTarget(f *testing.F) {
	for _, s := range []string{
		"example.com:443", "EXAMPLE.com.:80", "[::1]:80", "[::ffff:127.0.0.1]:8080", "127.0.0.1:1", "127.1:80",
		"0x7f.0.0.1:80", "[fe80::1%eth0]:22", "a..b:1", ":80", "host", "host:", "host:0", "host:65536", "[::1", "::1:80",
		"xn--bcher-kva.de:443", "a_b.c:9", "169.254.169.254:80", "user@host:80", "host:80/path", "h\x00st:80", "[1.2.3.4]:80",
		strings.Repeat("a", 300) + ":80", "2130706433:80", "[2002:a9fe:a9fe::1]:443",
	} {
		f.Add(s, uint16(0))
		f.Add(s, uint16(80))
	}
	f.Fuzz(func(t *testing.T, in string, def uint16) {
		tgt, err := parseTarget(in, def)
		if err != nil {
			return
		}
		if tgt.port == 0 {
			t.Fatalf("%q: port 0 accepted", in)
		}
		if tgt.host == "" || len(tgt.host) > maxHostnameLen {
			t.Fatalf("%q: host %q", in, tgt.host)
		}
		for i := 0; i < len(tgt.host); i++ {
			c := tgt.host[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':') {
				t.Fatalf("%q: host %q has byte %#x", in, tgt.host, c)
			}
		}
		if tgt.isIP() {
			if tgt.ip.Is4In6() || tgt.ip.Zone() != "" || tgt.ip.String() != tgt.host {
				t.Fatalf("%q: IP target not canonical: %+v", in, tgt)
			}
		} else {
			if strings.Contains(tgt.host, ":") || strings.HasSuffix(tgt.host, ".") || strings.HasPrefix(tgt.host, ".") || strings.Contains(tgt.host, "..") {
				t.Fatalf("%q: hostname %q", in, tgt.host)
			}
			if _, perr := netip.ParseAddr(tgt.host); perr == nil {
				t.Fatalf("%q: hostname %q parses as an IP", in, tgt.host)
			}
			last := tgt.host[strings.LastIndexByte(tgt.host, '.')+1:]
			if isNumericLabel(last) {
				t.Fatalf("%q: hostname %q ends in a number", in, tgt.host)
			}
		}
		// Re-parsing the canonical form gives the same target.
		again, err := parseTarget(tgt.String(), 0)
		if err != nil || again != tgt {
			t.Fatalf("%q: %+v re-parses to %+v, %v", in, tgt, again, err)
		}
		// The allowlist matcher and Host comparison accept it.
		ds, derr := compileDomains([]string{"*.example.com"})
		if derr != nil {
			t.Fatal(derr)
		}
		if ds.match(tgt.host) && !strings.HasSuffix(tgt.host, ".example.com") {
			t.Fatalf("%q matched *.example.com", tgt.host)
		}
		if !hostHeaderAgrees(tgt.String(), tgt, 0) {
			t.Fatalf("%q does not agree with itself", tgt.String())
		}
	})
}

// FuzzSOCKS5Handshake feeds arbitrary bytes to the SOCKS5 greeting and
// request parsers.
func FuzzSOCKS5Handshake(f *testing.F) {
	f.Add([]byte{5, 1, 0, 5, 1, 0, 3, 11, 'e', 'x', 'a', 'm', 'p', 'l', 'e', '.', 'c', 'o', 'm', 1, 187})
	f.Add([]byte{5, 1, 0, 5, 1, 0, 1, 127, 0, 0, 1, 0, 80})
	f.Add([]byte{5, 2, 0, 2, 5, 1, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 127, 0, 0, 1, 0, 80})
	f.Add([]byte{5, 1, 0, 5, 2, 0, 1, 1, 2, 3, 4, 0, 80})
	f.Add([]byte{5, 1, 0, 5, 1, 0, 3, 0, 0, 80})
	f.Add([]byte{5, 0})
	f.Add([]byte{4, 1, 0, 80, 1, 2, 3, 4, 0})
	f.Add([]byte{5, 255})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, in []byte) {
		br := bufio.NewReaderSize(bytes.NewReader(in), minReadBuf)
		if err := readSocksGreeting(br); err != nil {
			return
		}
		if len(in) < 3 || in[0] != 5 || !bytes.Contains(in[2:2+int(in[1])], []byte{0}) {
			t.Fatalf("greeting accepted: %x", in)
		}
		before := br.Buffered()
		req, err := readSocksRequest(br)
		if err != nil {
			if br.Buffered() != before {
				t.Fatalf("failed request consumed input: %x", in)
			}
			return
		}
		if req.ip.IsValid() == (req.domain != "") {
			t.Fatalf("request has both or neither of ip and domain: %+v", req)
		}
		if req.ip.IsValid() && (req.ip.Is4In6() || req.ip.Zone() != "") {
			t.Fatalf("request IP not normalised: %v", req.ip)
		}
		tgt, err := req.target()
		if err != nil {
			return
		}
		if tgt.port == 0 || tgt.host == "" {
			t.Fatalf("bad target accepted: %+v", tgt)
		}
		again, err := parseTarget(tgt.String(), 0)
		if err != nil || again != tgt {
			t.Fatalf("%+v re-parses to %+v, %v", tgt, again, err)
		}
	})
}

// FuzzClientHello feeds arbitrary bytes to the TLS sniffer.
func FuzzClientHello(f *testing.F) {
	hello := clientHello(f, &tls.Config{ServerName: "example.com", InsecureSkipVerify: true})
	f.Add(hello)
	f.Add(refragment(hello, 9))
	f.Add(clientHello(f, &tls.Config{InsecureSkipVerify: true}))
	f.Add([]byte{0x16, 0x03, 0x01, 0x00, 0x04, 0x01, 0x00, 0x00, 0x00})
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		br := bufio.NewReaderSize(bytes.NewReader(in), minReadBuf)
		body, isTLS, err := peekClientHello(br)
		if br.Buffered() > len(in) {
			t.Fatal("impossible buffer state")
		}
		if !isTLS {
			if body != nil || err != nil {
				t.Fatalf("non-TLS input returned %v, %v", body, err)
			}
			return
		}
		if err != nil {
			return
		}
		sni, has, err := parseClientHelloSNI(body)
		if err != nil {
			return
		}
		if has && sni == "" {
			t.Fatal("empty SNI reported as present")
		}
		// Parsing the body directly is deterministic.
		sni2, has2, err2 := parseClientHelloSNI(body)
		if sni2 != sni || has2 != has || err2 != nil {
			t.Fatal("non-deterministic parse")
		}
	})
}

// FuzzHTTPHead feeds arbitrary bytes to the raw header helpers.
func FuzzHTTPHead(f *testing.F) {
	f.Add([]byte("GET http://a.test/ HTTP/1.1\r\nHost: a.test\r\n\r\n"))
	f.Add([]byte("CONNECT a.test:443 HTTP/1.1\nhost:b\n\n"))
	f.Add([]byte("GET / HTTP/1.1\r\n Host: x\r\n\r\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		n := headerEnd(in)
		if n > len(in) || n == 0 || n < -1 {
			t.Fatalf("headerEnd = %d for %d bytes", n, len(in))
		}
		if n < 0 {
			return
		}
		if in[n-1] != '\n' {
			t.Fatalf("header block does not end in LF")
		}
		if m := headerEnd(in[:n-1]); m >= 0 {
			t.Fatalf("header ends at %d and at %d", n, m)
		}
		hosts, err := scanHostHeaders(in[:n])
		if err != nil {
			return
		}
		for _, h := range hosts {
			if strings.ContainsAny(h, "\r\n") && !strings.Contains(h, "\r") {
				t.Fatalf("host value %q spans lines", h)
			}
			_, _, _ = parseAuthority(h, 80)
		}
	})
}

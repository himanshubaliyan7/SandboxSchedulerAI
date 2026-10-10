package egress

import (
	"net"
	"net/netip"
	"testing"
)

// The exported guard is the proxy's deny set, not a copy of it: what the
// proxy refuses, the guard refuses, with the same exceptions.
func TestAddrGuard(t *testing.T) {
	ifaces := func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("198.51.100.7"), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("8.8.4.4"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	g, err := newAddrGuard([]string{"10.20.0.0/16"}, ifaces)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		addr string
		cat  string // "" = allowed
	}{
		{"93.184.216.34", ""},
		{"2606:2800:220:1:248:1893:25c8:1946", ""},
		{"127.0.0.1", catLoopback},
		{"::1", catLoopback},
		{"::ffff:127.0.0.1", catLoopback},
		{"10.1.2.3", catPrivate},
		{"::ffff:10.1.2.3", catPrivate},
		{"10.20.3.4", ""}, // operator exception
		{"172.16.0.1", catPrivate},
		{"192.168.1.1", catPrivate},
		{"100.64.0.1", catCGNAT},
		{"169.254.1.1", catLinkLocal},
		{"169.254.169.254", catMetadata},
		{"fe80::1", catLinkLocal},
		{"fd00::1", catULA},
		{"fd00:ec2::254", catMetadata},
		{"64:ff9b::a00:1", "embedded_" + catPrivate},
		{"8.8.4.4", catHostInterface},
		{"0.0.0.0", catUnspecified},
	} {
		cat, denied := g.Check(netip.MustParseAddr(c.addr))
		if denied != (c.cat != "") || cat != c.cat {
			t.Errorf("Check(%s) = %q, %v; want %q", c.addr, cat, denied, c.cat)
		}
	}
	if _, err := NewAddrGuard([]string{"8.8.8.0/24"}); err == nil {
		t.Error("a public range was accepted as a private exception")
	}
	if _, err := NewAddrGuard([]string{"127.0.0.0/8"}); err == nil {
		t.Error("loopback was accepted as a private exception")
	}
}

func TestNormalizeHostExported(t *testing.T) {
	if h, ip, err := NormalizeHost("Example.COM."); err != nil || h != "example.com" || ip.IsValid() {
		t.Errorf("NormalizeHost(name) = %q, %v, %v", h, ip, err)
	}
	if h, ip, err := NormalizeHost("::ffff:10.0.0.1"); err != nil || h != "10.0.0.1" || ip != netip.MustParseAddr("10.0.0.1") {
		t.Errorf("NormalizeHost(mapped) = %q, %v, %v", h, ip, err)
	}
	for _, bad := range []string{"", "127.1", "0x7f.0.0.1", "2130706433", "fe80::1%eth0", "a b", "exa_mple..com"} {
		if _, _, err := NormalizeHost(bad); err == nil {
			t.Errorf("NormalizeHost(%q) accepted", bad)
		}
	}
}

package egress

import (
	"net"
	"net/netip"
)

// AddrGuard is the proxy's destination check, for other parts of sbxd
// that connect to an address chosen by a tenant (the webhook sender). It
// is the same deny set the proxy applies to sandbox traffic: loopback,
// link-local, RFC 1918, CGNAT, ULA, cloud metadata, reserved and
// documentation ranges, non-global IPv6, IPv4 addresses embedded in
// NAT64, 6to4 and Teredo addresses, and the addresses of the host's own
// interfaces, which are re-read every DefaultHostAddrRefresh.
//
// A guard only classifies addresses. Its caller must resolve a name once,
// check every address it is about to use and connect to exactly that
// address; checking a name and then letting a dialer resolve it again is
// the DNS-rebinding hole the proxy is built to avoid.
type AddrGuard struct {
	deny *denySet
}

// NewAddrGuard returns a guard. permitPrivateCIDRs removes sub-ranges of
// RFC 1918, CGNAT or ULA space from the deny set, exactly as
// Config.PermitPrivateCIDRs does for the proxy; metadata addresses and
// host interface addresses stay denied whatever it says.
func NewAddrGuard(permitPrivateCIDRs []string) (*AddrGuard, error) {
	return newAddrGuard(permitPrivateCIDRs, net.InterfaceAddrs)
}

func newAddrGuard(permit []string, ifaceAddrs func() ([]net.Addr, error)) (*AddrGuard, error) {
	deny, err := newDenySet(permit, DefaultHostAddrRefresh, ifaceAddrs)
	if err != nil {
		return nil, err
	}
	return &AddrGuard{deny: deny}, nil
}

// Check reports whether a must not be connected to and, if so, the
// category of the rule that says so ("loopback", "private", "metadata",
// "host_interface", "embedded_private", ...). An IPv4-mapped IPv6 address
// is judged as the IPv4 address it carries.
func (g *AddrGuard) Check(a netip.Addr) (category string, denied bool) {
	return g.deny.check(a)
}

// NormalizeHost validates the host part of a URL or authority, given
// without brackets and without a port. It returns either a canonical IP
// literal (ip is valid, unmapped and has no zone) or a lower-case DNS name
// without a trailing dot. It refuses what lenient parsers read as an
// address ("127.1", "0x7f.0.0.1", "2130706433"), zones, and any character
// that cannot appear in a hostname.
func NormalizeHost(raw string) (host string, ip netip.Addr, err error) {
	return normalizeHost(raw)
}

// SystemResolver is the resolver the proxy uses by default: the system
// resolver, queried with a rooted name so that resolv.conf search domains
// cannot turn "db" into "db.corp.example".
func SystemResolver() Resolver {
	return rootedResolver{net.DefaultResolver}
}

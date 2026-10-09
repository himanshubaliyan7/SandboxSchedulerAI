package egress

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
)

// Deny categories reported in Decision.Detail.
const (
	catInvalid       = "invalid"
	catMetadata      = "metadata"
	catUnspecified   = "unspecified"
	catLoopback      = "loopback"
	catLinkLocal     = "link_local"
	catPrivate       = "private"
	catCGNAT         = "cgnat"
	catULA           = "ula"
	catMulticast     = "multicast"
	catBroadcast     = "broadcast"
	catDocumentation = "documentation"
	catBenchmark     = "benchmark"
	catReserved      = "reserved"
	catHostInterface = "host_interface"
	catPolicy        = "policy_deny_cidr"
)

type denyRule struct {
	prefix netip.Prefix
	cat    string
}

func rule(cidr, cat string) denyRule {
	return denyRule{netip.MustParsePrefix(cidr), cat}
}

// builtinDeny is checked in order, so the metadata addresses come before
// the ranges that contain them and keep their own, non-permittable,
// category.
var builtinDeny = []denyRule{
	rule("169.254.169.254/32", catMetadata), // AWS, GCP, Azure IMDS, OpenStack, ...
	rule("100.100.100.200/32", catMetadata), // Alibaba Cloud
	rule("168.63.129.16/32", catMetadata),   // Azure WireServer
	rule("fd00:ec2::254/128", catMetadata),  // AWS IMDS over IPv6

	rule("0.0.0.0/8", catUnspecified),
	rule("127.0.0.0/8", catLoopback),
	rule("169.254.0.0/16", catLinkLocal),
	rule("10.0.0.0/8", catPrivate),
	rule("172.16.0.0/12", catPrivate),
	rule("192.168.0.0/16", catPrivate),
	rule("100.64.0.0/10", catCGNAT),
	rule("192.0.0.0/24", catReserved), // IETF protocol assignments
	rule("192.0.2.0/24", catDocumentation),
	rule("198.51.100.0/24", catDocumentation),
	rule("203.0.113.0/24", catDocumentation),
	rule("192.88.99.0/24", catReserved), // deprecated 6to4 relay anycast
	rule("198.18.0.0/15", catBenchmark),
	rule("224.0.0.0/4", catMulticast),
	rule("255.255.255.255/32", catBroadcast),
	rule("240.0.0.0/4", catReserved),

	rule("::/128", catUnspecified),
	rule("::1/128", catLoopback),
	rule("fe80::/10", catLinkLocal),
	rule("fc00::/7", catULA),
	rule("ff00::/8", catMulticast),
	rule("fec0::/10", catReserved), // deprecated site-local
	rule("2001:db8::/32", catDocumentation),
	rule("3fff::/20", catDocumentation),
	rule("2001:2::/48", catBenchmark),
	rule("100::/64", catReserved),       // discard-only
	rule("64:ff9b:1::/48", catReserved), // local-use NAT64
}

var (
	prefixNAT64      = netip.MustParsePrefix("64:ff9b::/96")
	prefix6to4       = netip.MustParsePrefix("2002::/16")
	prefixTeredo     = netip.MustParsePrefix("2001::/32")
	prefixIETF       = netip.MustParsePrefix("2001::/23")
	prefixGlobalV6   = netip.MustParsePrefix("2000::/3")
	permittableRoots = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("fc00::/7"),
	}
)

// classifyBuiltin reports whether a itself (ignoring any IPv4 address it
// embeds, operator exceptions and host interfaces) is in the built-in deny
// set. a must already be unmapped.
func classifyBuiltin(a netip.Addr) (cat string, denied bool) {
	if !a.IsValid() || a.Zone() != "" {
		return catInvalid, true
	}
	for _, r := range builtinDeny {
		if r.prefix.Contains(a) {
			return r.cat, true
		}
	}
	if a.Is4() {
		return "", false
	}
	if prefixNAT64.Contains(a) {
		return "", false // judged by its embedded address
	}
	// Only 2000::/3 is global unicast. Everything else, including the
	// IPv4-compatible (::/96) and IPv4-translated (::ffff:0:0:0/96)
	// forms, is refused.
	if !prefixGlobalV6.Contains(a) {
		return catReserved, true
	}
	if prefixIETF.Contains(a) && !prefixTeredo.Contains(a) {
		return catReserved, true
	}
	return "", false
}

// embeddedV4 returns the IPv4 addresses carried inside a NAT64, 6to4 or
// Teredo address.
func embeddedV4(a netip.Addr) []netip.Addr {
	if !a.Is6() {
		return nil
	}
	b := a.As16()
	switch {
	case prefixNAT64.Contains(a):
		return []netip.Addr{netip.AddrFrom4([4]byte(b[12:16]))}
	case prefix6to4.Contains(a):
		return []netip.Addr{netip.AddrFrom4([4]byte(b[2:6]))}
	case prefixTeredo.Contains(a):
		// Server address in clear, client address bit-inverted.
		return []netip.Addr{
			netip.AddrFrom4([4]byte(b[4:8])),
			netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]}),
		}
	}
	return nil
}

// normalizeAddr converts an IPv4-mapped IPv6 address to IPv4. It must be
// applied before any range check.
func normalizeAddr(a netip.Addr) netip.Addr { return a.Unmap() }

// denySet is the built-in deny set with operator exceptions and the live
// set of host interface addresses.
type denySet struct {
	permit []netip.Prefix

	ifaceAddrs func() ([]net.Addr, error)
	refresh    time.Duration
	mu         sync.Mutex
	hostAddrs  map[netip.Addr]struct{}
	loadedAt   time.Time
}

func newDenySet(permit []string, refresh time.Duration, ifaceAddrs func() ([]net.Addr, error)) (*denySet, error) {
	d := &denySet{ifaceAddrs: ifaceAddrs, refresh: refresh}
	for _, s := range permit {
		p, err := parseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("egress: PermitPrivateCIDRs %q: %w", s, err)
		}
		ok := false
		for _, root := range permittableRoots {
			if root.Bits() <= p.Bits() && root.Contains(p.Addr()) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("egress: PermitPrivateCIDRs %q is not inside RFC 1918, CGNAT or ULA space", s)
		}
		d.permit = append(d.permit, p)
	}
	if err := d.reloadHostAddrs(); err != nil {
		return nil, fmt.Errorf("egress: reading host interface addresses: %w", err)
	}
	return d, nil
}

func (d *denySet) reloadHostAddrs() error {
	addrs, err := d.ifaceAddrs()
	if err != nil {
		return err
	}
	set := make(map[netip.Addr]struct{}, len(addrs))
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		if na, ok := netip.AddrFromSlice(ip); ok {
			set[na.Unmap().WithZone("")] = struct{}{}
		}
	}
	d.mu.Lock()
	d.hostAddrs = set
	d.loadedAt = time.Now()
	d.mu.Unlock()
	return nil
}

func (d *denySet) isHostAddr(a netip.Addr) bool {
	d.mu.Lock()
	stale := time.Since(d.loadedAt) > d.refresh
	if stale {
		// Claim the refresh so concurrent callers do not all re-read.
		d.loadedAt = time.Now()
	}
	d.mu.Unlock()
	if stale {
		// On failure the previous set stays in force.
		_ = d.reloadHostAddrs()
	}
	d.mu.Lock()
	_, ok := d.hostAddrs[a]
	d.mu.Unlock()
	return ok
}

// check reports whether a is denied by the built-in set. It handles
// IPv4-mapped addresses and addresses that embed an IPv4 address.
func (d *denySet) check(a netip.Addr) (cat string, denied bool) {
	a = normalizeAddr(a)
	if cat, denied := classifyBuiltin(a); denied {
		if !d.permitted(a, cat) {
			return cat, true
		}
	}
	if d.isHostAddr(a) {
		return catHostInterface, true
	}
	for _, e := range embeddedV4(a) {
		if cat, denied := d.check(e); denied {
			return "embedded_" + cat, true
		}
	}
	return "", false
}

func (d *denySet) permitted(a netip.Addr, cat string) bool {
	if cat != catPrivate && cat != catCGNAT && cat != catULA {
		return false
	}
	for _, p := range d.permit {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// parseCIDR parses a CIDR or a bare address. An IPv4-mapped IPv6 prefix is
// converted to the IPv4 prefix it denotes.
func parseCIDR(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		a, aerr := netip.ParseAddr(s)
		if aerr != nil {
			return netip.Prefix{}, fmt.Errorf("not a CIDR or IP address")
		}
		if a.Zone() != "" {
			return netip.Prefix{}, errZone
		}
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	if p.Addr().Is4In6() {
		if p.Bits() < 96 {
			return netip.Prefix{}, fmt.Errorf("IPv4-mapped prefix shorter than /96")
		}
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	return p.Masked(), nil
}

func parseCIDRs(field string, in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, err := parseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", field, s, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func containsAddr(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// policy is a compiled core.NetworkPolicy. It is immutable.
type policy struct {
	mode    core.NetworkMode
	domains domainSet
	allow   []netip.Prefix
	deny    []netip.Prefix
}

func compilePolicy(p core.NetworkPolicy) (*policy, error) {
	out := &policy{mode: p.Mode}
	switch p.Mode {
	case "":
		out.mode = core.NetworkNone
	case core.NetworkNone, core.NetworkAllowlist, core.NetworkFull:
	default:
		return nil, fmt.Errorf("egress: unknown network mode %q", p.Mode)
	}
	var err error
	if out.domains, err = compileDomains(p.AllowDomains); err != nil {
		return nil, fmt.Errorf("egress: %w", err)
	}
	if out.allow, err = parseCIDRs("allow CIDR", p.AllowCIDRs); err != nil {
		return nil, fmt.Errorf("egress: %w", err)
	}
	if out.deny, err = parseCIDRs("deny CIDR", p.DenyCIDRs); err != nil {
		return nil, fmt.Errorf("egress: %w", err)
	}
	return out, nil
}

// permitsTarget is pipeline steps 1 and 3: mode and allowlist.
func (p *policy) permitsTarget(t target) (Reason, bool) {
	switch p.mode {
	case core.NetworkFull:
		return "", true
	case core.NetworkAllowlist:
		if t.isIP() {
			if containsAddr(p.allow, t.ip) {
				return "", true
			}
			return ReasonIPNotAllowed, false
		}
		if p.domains.match(t.host) {
			return "", true
		}
		return ReasonHostNotAllowed, false
	default:
		return ReasonModeNone, false
	}
}

// deniesAddr reports whether the policy's DenyCIDRs cover a or an IPv4
// address embedded in it.
func (p *policy) deniesAddr(a netip.Addr) bool {
	a = normalizeAddr(a)
	if containsAddr(p.deny, a) {
		return true
	}
	for _, e := range embeddedV4(a) {
		if containsAddr(p.deny, e) {
			return true
		}
	}
	return false
}

// permitsEstablished reports whether a connection to t at ip that was
// opened under an earlier policy may stay open under p.
func (p *policy) permitsEstablished(t target, ip netip.Addr) bool {
	if _, ok := p.permitsTarget(t); !ok {
		return false
	}
	return !p.deniesAddr(ip)
}

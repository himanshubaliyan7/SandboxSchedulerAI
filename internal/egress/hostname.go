package egress

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// target is a parsed, normalised destination.
type target struct {
	// host is the lower-case hostname without trailing dot, or the
	// canonical text of ip.
	host string
	// ip is valid (and never IPv4-mapped, never zoned) iff the target was
	// an IP literal.
	ip   netip.Addr
	port uint16
}

func (t target) isIP() bool { return t.ip.IsValid() }

func (t target) String() string {
	return net.JoinHostPort(t.host, strconv.Itoa(int(t.port)))
}

var (
	errEmptyHost    = errors.New("empty host")
	errHostTooLong  = errors.New("host too long")
	errBadHostChar  = errors.New("invalid character in host")
	errBadLabel     = errors.New("invalid hostname label")
	errNumericHost  = errors.New("numeric host is not a canonical IP literal")
	errZone         = errors.New("IPv6 zone not allowed")
	errBadBrackets  = errors.New("malformed IPv6 brackets")
	errBareIPv6     = errors.New("IPv6 literal must be in brackets")
	errBadPort      = errors.New("invalid port")
	errMissingPort  = errors.New("missing port")
	errNotIPv6      = errors.New("bracketed host is not an IPv6 literal")
	maxHostnameLen  = 253
	maxHostInputLen = 255
)

// normalizeHost validates and normalises a host given without brackets and
// without a port. It returns either a canonical IP literal (ip valid) or a
// plausible DNS hostname.
func normalizeHost(raw string) (host string, ip netip.Addr, err error) {
	if raw == "" {
		return "", netip.Addr{}, errEmptyHost
	}
	if len(raw) > maxHostInputLen {
		return "", netip.Addr{}, errHostTooLong
	}
	b := []byte(raw)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c + ('a' - 'A')
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == ':':
		case c == '%':
			return "", netip.Addr{}, errZone
		default:
			return "", netip.Addr{}, errBadHostChar
		}
	}
	h := string(b)
	if strings.IndexByte(h, ':') >= 0 {
		// Only an IPv6 literal may contain a colon.
		a, perr := netip.ParseAddr(h)
		if perr != nil || !a.Is6() {
			return "", netip.Addr{}, errBadHostChar
		}
		a = a.Unmap()
		return a.String(), a, nil
	}
	if a, perr := netip.ParseAddr(h); perr == nil {
		return a.String(), a, nil
	}
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return "", netip.Addr{}, errEmptyHost
	}
	if len(h) > maxHostnameLen {
		return "", netip.Addr{}, errHostTooLong
	}
	last := ""
	for rest := h; ; {
		label := rest
		i := strings.IndexByte(rest, '.')
		if i >= 0 {
			label = rest[:i]
		}
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", netip.Addr{}, errBadLabel
		}
		last = label
		if i < 0 {
			break
		}
		rest = rest[i+1:]
	}
	// A name whose last label is a number (decimal, or 0x-prefixed hex)
	// is what inet_aton-style parsers read as an address ("127.1",
	// "0x7f.0.0.1", "2130706433"). It is not a hostname and, having
	// failed strict parsing above, not an acceptable IP literal either.
	if isNumericLabel(last) {
		return "", netip.Addr{}, errNumericHost
	}
	return h, netip.Addr{}, nil
}

func isNumericLabel(l string) bool {
	if len(l) >= 2 && l[0] == '0' && l[1] == 'x' {
		for _, c := range []byte(l[2:]) {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	}
	for _, c := range []byte(l) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parsePort accepts only a plain decimal 1..65535 without leading zeros.
func parsePort(s string) (uint16, error) {
	if len(s) == 0 || len(s) > 5 || s[0] == '0' {
		return 0, errBadPort
	}
	n := 0
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return 0, errBadPort
		}
		n = n*10 + int(c-'0')
	}
	if n < 1 || n > 65535 {
		return 0, errBadPort
	}
	return uint16(n), nil
}

// splitAuthority splits "host", "host:port", "[v6]" or "[v6]:port". The
// returned host has no brackets.
func splitAuthority(s string) (host, port string, hasPort, bracketed bool, err error) {
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return "", "", false, false, errBadBrackets
		}
		host = s[1:end]
		rest := s[end+1:]
		switch {
		case rest == "":
			return host, "", false, true, nil
		case rest[0] == ':':
			return host, rest[1:], true, true, nil
		default:
			return "", "", false, false, errBadBrackets
		}
	}
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return s, "", false, false, nil
	}
	host, port = s[:i], s[i+1:]
	if strings.IndexByte(host, ':') >= 0 {
		return "", "", false, false, errBareIPv6
	}
	return host, port, true, false, nil
}

// parseAuthority parses the authority of a CONNECT request line, an
// absolute-form URI or a Host header. hasPort reports whether a port was
// written; when it was not, t.port is defaultPort.
func parseAuthority(s string, defaultPort uint16) (t target, hasPort bool, err error) {
	h, p, hasPort, bracketed, err := splitAuthority(s)
	if err != nil {
		return target{}, false, err
	}
	if strings.ContainsAny(h, "[]") {
		return target{}, false, errBadBrackets
	}
	host, ip, err := normalizeHost(h)
	if err != nil {
		return target{}, false, err
	}
	if bracketed && strings.IndexByte(h, ':') < 0 {
		return target{}, false, errNotIPv6
	}
	t = target{host: host, ip: ip, port: defaultPort}
	if hasPort {
		if t.port, err = parsePort(p); err != nil {
			return target{}, false, err
		}
	}
	return t, hasPort, nil
}

// parseTarget parses a "host:port" target. If defaultPort is zero the port
// is mandatory.
func parseTarget(s string, defaultPort uint16) (target, error) {
	t, hasPort, err := parseAuthority(s, defaultPort)
	if err != nil {
		return target{}, err
	}
	if !hasPort && defaultPort == 0 {
		return target{}, errMissingPort
	}
	return t, nil
}

// domainSet is a compiled AllowDomains list.
type domainSet struct {
	exact map[string]struct{}
	// wild holds the part after "*." of each wildcard entry.
	wild map[string]struct{}
}

func compileDomains(entries []string) (domainSet, error) {
	ds := domainSet{exact: map[string]struct{}{}, wild: map[string]struct{}{}}
	for _, e := range entries {
		name, wild := e, false
		if strings.HasPrefix(e, "*.") {
			name, wild = e[2:], true
		}
		host, ip, err := normalizeHost(name)
		if err != nil {
			return domainSet{}, fmt.Errorf("allow domain %q: %w", e, err)
		}
		if ip.IsValid() {
			return domainSet{}, fmt.Errorf("allow domain %q: IP literals belong in AllowCIDRs", e)
		}
		if wild {
			ds.wild[host] = struct{}{}
		} else {
			ds.exact[host] = struct{}{}
		}
	}
	return ds, nil
}

// match reports whether a normalised hostname is allowed. Matching is by
// whole labels: a wildcard entry matches when some proper parent domain of
// host equals the wildcard's base, so it never matches the base itself and
// never a name that merely ends with the same characters.
func (ds domainSet) match(host string) bool {
	if _, ok := ds.exact[host]; ok {
		return true
	}
	if len(ds.wild) == 0 {
		return false
	}
	for i := 0; i < len(host); i++ {
		if host[i] == '.' {
			if _, ok := ds.wild[host[i+1:]]; ok {
				return true
			}
		}
	}
	return false
}

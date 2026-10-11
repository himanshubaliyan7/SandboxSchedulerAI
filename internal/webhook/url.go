package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/egress"
)

// A webhook URL is chosen by a tenant, and sbxd sits on a private
// network next to a database, a container engine and perhaps a cloud
// metadata service. Everything in this file and in sender.go exists so
// that a webhook cannot be pointed at any of those.
//
// The rules, applied when a URL is stored and again for every delivery:
//
//  1. The URL must be https (http only with AllowHTTP), without userinfo
//     and without a fragment, and its host must be a plain DNS name or a
//     canonical IP literal (egress.NormalizeHost: no "127.1", no
//     "0x7f.0.0.1", no zone).
//  2. The host is resolved by sbxd, once per delivery.
//  3. Every address is checked against the egress proxy's deny set
//     (egress.AddrGuard): loopback, link-local, RFC 1918, CGNAT, ULA,
//     cloud metadata, reserved ranges, IPv4 embedded in IPv6 and the
//     host's own addresses are refused.
//  4. The connection is made to one of the addresses that passed, and a
//     hook on the socket refuses any other address. The name is never
//     resolved a second time, so a DNS answer that changes between the
//     check and the connection (DNS rebinding) has nothing to change.
//  5. Redirects are not followed, the system's proxy settings are not
//     used, and only a bounded part of the response is read.

// maxURLLength bounds a webhook URL.
const maxURLLength = 2048

// maxResolved bounds how many addresses of a name are considered, and
// maxDialAttempts how many of them are tried.
const (
	maxResolved     = 16
	maxDialAttempts = 3
)

// Error codes of a refused destination and of a failed attempt. They are
// part of the API: the test route and the delivery log report them.
const (
	CodeInvalidURL            = "invalid_url"
	CodeDestinationNotAllowed = "destination_not_allowed"
	CodeTimeout               = "timeout"
	CodeConnectionFailed      = "connection_failed"
	CodeTLSError              = "tls_error"
	CodeRedirect              = "redirect"
	CodeHTTPStatus            = "http_status"
	// Codes of events that were given up on.
	CodeRetentionExpired = "retention_expired"
	CodeRejected         = "rejected"
)

// DestinationError says why a URL or the addresses behind it were
// refused. Message is safe to show to the tenant; Detail is for the
// server's log only.
type DestinationError struct {
	Code    string
	Message string
	Detail  string
}

func (e *DestinationError) Error() string { return e.Code + ": " + e.Message }

func invalidURL(format string, args ...any) *DestinationError {
	return &DestinationError{Code: CodeInvalidURL, Message: fmt.Sprintf(format, args...)}
}

// errUnresolvable is the one message for "the name does not resolve",
// "the lookup failed" and "the name resolves to an address that is not
// allowed". They are deliberately indistinguishable to the tenant: a
// different answer for each would let a webhook be used to find out which
// internal names exist and where they point.
func errUnresolvable(host, detail string) *DestinationError {
	return &DestinationError{Code: CodeDestinationNotAllowed,
		Message: fmt.Sprintf("the host %q does not resolve to a public address", host), Detail: detail}
}

// DestinationConfig is the operator's policy for webhook destinations.
type DestinationConfig struct {
	// AllowHTTP accepts http:// URLs. For development: the payload and
	// the signature travel in the clear.
	AllowHTTP bool
	// AllowLoopback accepts loopback addresses (127.0.0.0/8, ::1), so a
	// receiver can run on the developer's machine. Never set it on a
	// server other people can create webhooks on: it lets a webhook reach
	// every service that listens on the server's loopback interface.
	AllowLoopback bool
	// PermitPrivateCIDRs opens sub-ranges of RFC 1918, CGNAT or ULA space
	// for receivers inside the operator's network. Metadata addresses and
	// the host's own addresses stay refused.
	PermitPrivateCIDRs []string
	// Resolver resolves host names. Default: the system resolver, with
	// rooted names (egress.SystemResolver).
	Resolver egress.Resolver
	// ResolveTimeout bounds the lookup. Default 5s.
	ResolveTimeout time.Duration
}

// Destinations validates webhook URLs and the addresses behind them.
type Destinations struct {
	cfg   DestinationConfig
	guard interface {
		Check(netip.Addr) (string, bool)
	}
}

// NewDestinations returns the validator for cfg.
func NewDestinations(cfg DestinationConfig) (*Destinations, error) {
	guard, err := egress.NewAddrGuard(cfg.PermitPrivateCIDRs)
	if err != nil {
		return nil, fmt.Errorf("webhook: %w", err)
	}
	if cfg.Resolver == nil {
		cfg.Resolver = egress.SystemResolver()
	}
	if cfg.ResolveTimeout <= 0 {
		cfg.ResolveTimeout = 5 * time.Second
	}
	return &Destinations{cfg: cfg, guard: guard}, nil
}

// Target is a webhook URL that passed the static checks.
type Target struct {
	// URL is the normalised URL: lower-case scheme and host, no default
	// port, no fragment.
	URL *url.URL
	// Host is the DNS name, or the canonical text of IP.
	Host string
	// IP is valid iff the host is an IP literal.
	IP   netip.Addr
	Port uint16
}

// Parse applies the static rules to a URL. It does not resolve anything.
func (d *Destinations) Parse(raw string) (Target, error) {
	if raw == "" {
		return Target{}, invalidURL("a url is required")
	}
	if len(raw) > maxURLLength {
		return Target{}, invalidURL("the url is longer than %d bytes", maxURLLength)
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c <= ' ' || c == 0x7f || c == '\\' {
			return Target{}, invalidURL("the url contains a space, a backslash or a control character")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, invalidURL("the url cannot be parsed")
	}
	scheme := strings.ToLower(u.Scheme)
	switch {
	case scheme == "https":
	case scheme == "http" && d.cfg.AllowHTTP:
	case scheme == "http":
		return Target{}, invalidURL("the url must use https")
	default:
		return Target{}, invalidURL("the url must be an absolute https url")
	}
	if u.Opaque != "" || u.Host == "" {
		return Target{}, invalidURL("the url must be an absolute https url with a host")
	}
	if u.User != nil {
		return Target{}, invalidURL("the url must not contain a user name or password")
	}
	if u.Fragment != "" || u.RawFragment != "" || strings.Contains(raw, "#") {
		return Target{}, invalidURL("the url must not contain a fragment")
	}
	port := uint16(443)
	if scheme == "http" {
		port = 80
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return Target{}, invalidURL("the url has an invalid port")
		}
		port = uint16(n)
	} else if strings.HasSuffix(u.Host, ":") {
		return Target{}, invalidURL("the url has an invalid port")
	}
	host, ip, err := egress.NormalizeHost(u.Hostname())
	if err != nil {
		return Target{}, invalidURL("the url has an invalid host: %v", err)
	}
	t := Target{Host: host, IP: ip, Port: port}
	if ip.IsValid() {
		if cat, denied := d.check(ip); denied {
			return Target{}, &DestinationError{Code: CodeDestinationNotAllowed,
				Message: fmt.Sprintf("the address %s is not allowed as a webhook destination", ip), Detail: cat}
		}
	}
	// Rebuild the URL from the validated parts, so that what is stored
	// and requested is what was checked.
	out := &url.URL{Scheme: scheme, Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery, ForceQuery: u.ForceQuery}
	out.Host = host
	if ip.Is6() {
		out.Host = "[" + host + "]"
	}
	if (scheme == "https" && port != 443) || (scheme == "http" && port != 80) {
		out.Host += ":" + strconv.Itoa(int(port))
	}
	t.URL = out
	return t, nil
}

// check is the address rule: the egress deny set, minus loopback when the
// operator allowed it.
func (d *Destinations) check(a netip.Addr) (category string, denied bool) {
	a = a.Unmap()
	if d.cfg.AllowLoopback && a.IsLoopback() {
		return "", false
	}
	return d.guard.Check(a)
}

// Resolve returns the addresses of t that may be connected to, in
// resolver order. It performs exactly one lookup (none for an IP
// literal); the caller must connect to one of the returned addresses and
// must not resolve the name again.
func (d *Destinations) Resolve(ctx context.Context, t Target) ([]netip.Addr, error) {
	if t.IP.IsValid() {
		if cat, denied := d.check(t.IP); denied {
			return nil, &DestinationError{Code: CodeDestinationNotAllowed,
				Message: fmt.Sprintf("the address %s is not allowed as a webhook destination", t.IP), Detail: cat}
		}
		return []netip.Addr{t.IP.Unmap()}, nil
	}
	rctx, cancel := context.WithTimeout(ctx, d.cfg.ResolveTimeout)
	addrs, err := d.cfg.Resolver.LookupNetIP(rctx, "ip", t.Host)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errUnresolvable(t.Host, "lookup failed: "+err.Error())
	}
	if len(addrs) > maxResolved {
		addrs = addrs[:maxResolved]
	}
	var (
		ok       []netip.Addr
		firstCat string
	)
	for _, a := range addrs {
		a = a.Unmap()
		if cat, denied := d.check(a); denied {
			if firstCat == "" {
				firstCat = fmt.Sprintf("%s is %s", a, cat)
			}
			continue
		}
		dup := false
		for _, seen := range ok {
			dup = dup || seen == a
		}
		if !dup {
			ok = append(ok, a)
		}
	}
	if len(ok) == 0 {
		if firstCat == "" {
			firstCat = "no addresses"
		}
		return nil, errUnresolvable(t.Host, firstCat)
	}
	return ok, nil
}

// Check validates a URL for storing: the static rules, and one lookup to
// refuse right away a host that has no public address. It returns the
// normalised URL. Passing it proves nothing about later deliveries, which
// are checked again each time.
func (d *Destinations) Check(ctx context.Context, raw string) (string, error) {
	t, err := d.Parse(raw)
	if err != nil {
		return "", err
	}
	if _, err := d.Resolve(ctx, t); err != nil {
		return "", err
	}
	return t.URL.String(), nil
}

// asDestinationError unwraps a DestinationError.
func asDestinationError(err error) (*DestinationError, bool) {
	var de *DestinationError
	ok := errors.As(err, &de)
	return de, ok
}

// LogURL is a webhook URL as it may appear in a log line: scheme and
// host only. The path and the query of a receiver's URL are often its
// credential (an unguessable path, a ?token=), and logs are read by more
// people than the webhook's owner.
func LogURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparsable)"
	}
	return u.Scheme + "://" + u.Host + "/..."
}

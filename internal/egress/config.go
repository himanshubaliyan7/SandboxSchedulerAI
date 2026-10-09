package egress

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"time"
)

// Resolver resolves a hostname to addresses. *net.Resolver implements it.
// The proxy calls it at most once per request, with a normalised lower-case
// name without a trailing dot and network "ip".
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Defaults applied by New to zero Config fields.
const (
	DefaultMaxConnsPerSandbox    = 256
	DefaultSocketMode            = fs.FileMode(0o600)
	DefaultDialTimeout           = 10 * time.Second
	DefaultResolveTimeout        = 5 * time.Second
	DefaultHeaderTimeout         = 10 * time.Second
	DefaultIdleTimeout           = 5 * time.Minute
	DefaultMaxTunnelLifetime     = time.Hour
	DefaultResponseHeaderTimeout = 60 * time.Second
	DefaultMaxHeaderBytes        = 32 << 10
	DefaultHostAddrRefresh       = 15 * time.Second
)

// Config is operator-level configuration for a Manager. The zero value is
// usable: every zero field takes the default documented on it.
type Config struct {
	// Resolver resolves target hostnames. Default: net.DefaultResolver,
	// queried with a rooted name so resolv.conf search domains never apply.
	Resolver Resolver
	// Logger receives one record per decision. Default: slog.Default().
	Logger *slog.Logger
	// OnDecision, if set, is called once per decision (see Decision). It is
	// called from connection goroutines, possibly concurrently, and must
	// not block.
	OnDecision func(Decision)

	// AllowedPorts restricts target ports for every sandbox. Empty means
	// all ports.
	AllowedPorts []uint16
	// PermitPrivateCIDRs removes sub-ranges of RFC 1918, CGNAT or ULA
	// space from the built-in deny set, for intranet destinations. Each
	// entry must lie entirely inside 10.0.0.0/8, 172.16.0.0/12,
	// 192.168.0.0/16, 100.64.0.0/10 or fc00::/7. Metadata addresses and
	// host interface addresses stay denied. This is operator config; no
	// per-sandbox policy can do this. Default: empty.
	PermitPrivateCIDRs []string
	// DisableSNICheck turns off the check that a TLS ClientHello's SNI
	// matches the tunnel target. The check is on by default.
	DisableSNICheck bool

	// MaxConnsPerSandbox caps concurrent client connections per sandbox.
	MaxConnsPerSandbox int
	// SocketMode is the permission of each sandbox socket. Default 0600;
	// see the package documentation for what the caller must ensure.
	SocketMode fs.FileMode

	// DialTimeout bounds connecting to the upstream (all attempts).
	DialTimeout time.Duration
	// ResolveTimeout bounds the single name lookup.
	ResolveTimeout time.Duration
	// HeaderTimeout bounds reading a request header or SOCKS handshake.
	HeaderTimeout time.Duration
	// IdleTimeout closes a tunnel or exchange with no bytes in either
	// direction, and an idle keep-alive client connection.
	IdleTimeout time.Duration
	// MaxTunnelLifetime is the hard cap on one tunnel or HTTP exchange.
	MaxTunnelLifetime time.Duration
	// ResponseHeaderTimeout bounds waiting for a plain-HTTP response
	// header after the request has been written.
	ResponseHeaderTimeout time.Duration
	// MaxHeaderBytes caps a request header block (minimum 1024).
	MaxHeaderBytes int
	// HostAddrRefresh is how long the list of host interface addresses is
	// cached before it is re-read.
	HostAddrRefresh time.Duration
}

// rootedResolver makes lookups absolute so that resolv.conf search domains
// cannot turn "db" into "db.corp.example".
type rootedResolver struct{ r *net.Resolver }

func (r rootedResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return r.r.LookupNetIP(ctx, network, host+".")
}

func (c Config) withDefaults() (Config, error) {
	if c.Resolver == nil {
		c.Resolver = rootedResolver{net.DefaultResolver}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.MaxConnsPerSandbox == 0 {
		c.MaxConnsPerSandbox = DefaultMaxConnsPerSandbox
	}
	if c.MaxConnsPerSandbox < 0 {
		return c, errors.New("egress: MaxConnsPerSandbox must not be negative")
	}
	if c.SocketMode == 0 {
		c.SocketMode = DefaultSocketMode
	}
	if c.SocketMode&^fs.ModePerm != 0 || c.SocketMode&0o600 != 0o600 {
		return c, fmt.Errorf("egress: SocketMode %v must be permission bits including owner read/write", c.SocketMode)
	}
	if c.MaxHeaderBytes == 0 {
		c.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	if c.MaxHeaderBytes < 1024 || c.MaxHeaderBytes > 1<<20 {
		return c, errors.New("egress: MaxHeaderBytes must be between 1024 and 1048576")
	}
	for _, p := range c.AllowedPorts {
		if p == 0 {
			return c, errors.New("egress: AllowedPorts must not contain 0")
		}
	}
	durations := []struct {
		name string
		v    *time.Duration
		def  time.Duration
	}{
		{"DialTimeout", &c.DialTimeout, DefaultDialTimeout},
		{"ResolveTimeout", &c.ResolveTimeout, DefaultResolveTimeout},
		{"HeaderTimeout", &c.HeaderTimeout, DefaultHeaderTimeout},
		{"IdleTimeout", &c.IdleTimeout, DefaultIdleTimeout},
		{"MaxTunnelLifetime", &c.MaxTunnelLifetime, DefaultMaxTunnelLifetime},
		{"ResponseHeaderTimeout", &c.ResponseHeaderTimeout, DefaultResponseHeaderTimeout},
		{"HostAddrRefresh", &c.HostAddrRefresh, DefaultHostAddrRefresh},
	}
	for _, d := range durations {
		if *d.v < 0 {
			return c, fmt.Errorf("egress: %s must not be negative", d.name)
		}
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	return c, nil
}

// Verdict is the outcome of one decision.
type Verdict string

const (
	// VerdictAllowed: the request passed the pipeline and an upstream
	// connection was established.
	VerdictAllowed Verdict = "allowed"
	// VerdictDenied: refused by policy.
	VerdictDenied Verdict = "denied"
	// VerdictError: not a policy refusal; the request was malformed or
	// the upstream could not be resolved or reached.
	VerdictError Verdict = "error"
)

// Reason says why a decision came out as it did. For VerdictAllowed it is
// how the connection ended.
type Reason string

const (
	// Denied.
	ReasonModeNone       Reason = "mode_none"
	ReasonBadTarget      Reason = "bad_target"
	ReasonPortNotAllowed Reason = "port_not_allowed"
	ReasonHostNotAllowed Reason = "host_not_allowed"
	ReasonIPNotAllowed   Reason = "ip_not_allowed"
	ReasonDeniedAddress  Reason = "denied_address"
	ReasonHostMismatch   Reason = "host_header_mismatch"
	ReasonSNIMismatch    Reason = "sni_mismatch"
	ReasonTLSMalformed   Reason = "tls_malformed"
	ReasonConnLimit      Reason = "conn_limit"
	ReasonPolicyChanged  Reason = "policy_changed"

	// Errors.
	ReasonBadRequest     Reason = "bad_request"
	ReasonHeaderTimeout  Reason = "header_timeout"
	ReasonHeaderTooLarge Reason = "header_too_large"
	ReasonResolveFailed  Reason = "resolve_failed"
	ReasonDialFailed     Reason = "dial_failed"
	ReasonDialTimeout    Reason = "dial_timeout"
	ReasonUpstreamFailed Reason = "upstream_failed"

	// How an allowed connection ended.
	ReasonClosed        Reason = "closed"
	ReasonIdleTimeout   Reason = "idle_timeout"
	ReasonMaxLifetime   Reason = "max_lifetime"
	ReasonPolicyUpdated Reason = "policy_updated"
	ReasonDetached      Reason = "detached"
	ReasonIOError       Reason = "io_error"
)

// Decision is the record of one CONNECT tunnel, SOCKS5 tunnel or plain-HTTP
// request (or of a connection refused before a request could be read). For
// allowed connections it is emitted when the connection ends, so the byte
// counts and duration are final.
type Decision struct {
	Time      time.Time // when the decision was emitted
	SandboxID string
	Protocol  string // "connect", "http", "socks5" or "" if never determined
	Host      string // normalised target host, "" if it could not be parsed
	Port      uint16
	// ResolvedIP is the address dialled, or the address that caused the
	// refusal; the zero Addr if resolution was never reached.
	ResolvedIP netip.Addr
	Verdict    Verdict
	Reason     Reason
	Detail     string
	BytesUp    int64 // sandbox -> upstream
	BytesDown  int64 // upstream -> sandbox
	Duration   time.Duration
}

// Stats is a snapshot of one sandbox's counters since Attach.
type Stats struct {
	// Allowed counts requests that reached an upstream, including
	// connections still open.
	Allowed uint64
	// Denied counts policy refusals by reason. A tunnel closed for an SNI
	// mismatch is counted here and not in Allowed.
	Denied map[Reason]uint64
	// Errors counts malformed requests and upstream failures by reason.
	Errors map[Reason]uint64
	// BytesUp and BytesDown include connections still open.
	BytesUp   int64
	BytesDown int64
	// ActiveConns is the number of client connections currently open.
	ActiveConns int
}

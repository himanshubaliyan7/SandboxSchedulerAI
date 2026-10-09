package egress

import (
	"bufio"
	"errors"
	"net/netip"
	"time"
)

// SOCKS5 (RFC 1928) constants.
const (
	socksVersion = 0x05

	socksAuthNone         = 0x00
	socksAuthUnacceptable = 0xff

	socksCmdConnect = 0x01

	socksAtypIPv4   = 0x01
	socksAtypDomain = 0x03
	socksAtypIPv6   = 0x04

	socksSucceeded         = 0x00
	socksGeneralFailure    = 0x01
	socksNotAllowed        = 0x02
	socksNetUnreachable    = 0x03
	socksHostUnreachable   = 0x04
	socksConnRefused       = 0x05
	socksTTLExpired        = 0x06
	socksCmdNotSupported   = 0x07
	socksAtypNotSupported  = 0x08
	socksMaxHandshakeBytes = 4 + 1 + 255 + 2
)

var (
	errSocksVersion = errors.New("socks: unsupported version")
	errSocksNoAuth  = errors.New("socks: client does not offer the no-authentication method")
	errSocksCommand = errors.New("socks: unsupported command")
	errSocksAtyp    = errors.New("socks: unsupported address type")
	errSocksFormat  = errors.New("socks: malformed request")
)

// readSocksGreeting consumes the client's method negotiation message.
func readSocksGreeting(br *bufio.Reader) error {
	h, err := br.Peek(2)
	if err != nil {
		return err
	}
	if h[0] != socksVersion {
		return errSocksVersion
	}
	n := int(h[1])
	b, err := br.Peek(2 + n)
	if err != nil {
		return err
	}
	ok := false
	for _, m := range b[2:] {
		if m == socksAuthNone {
			ok = true
		}
	}
	_, _ = br.Discard(2 + n)
	if !ok {
		return errSocksNoAuth
	}
	return nil
}

// socksRequest is a parsed SOCKS5 request. Exactly one of domain and ip is
// set.
type socksRequest struct {
	domain string
	ip     netip.Addr
	port   uint16
}

// readSocksRequest consumes a SOCKS5 request. Only CONNECT is accepted.
func readSocksRequest(br *bufio.Reader) (socksRequest, error) {
	var req socksRequest
	h, err := br.Peek(4)
	if err != nil {
		return req, err
	}
	if h[0] != socksVersion {
		return req, errSocksVersion
	}
	if h[2] != 0 {
		return req, errSocksFormat
	}
	cmd, atyp := h[1], h[3]
	if cmd != socksCmdConnect {
		return req, errSocksCommand
	}
	total := 4
	switch atyp {
	case socksAtypIPv4:
		total += 4
	case socksAtypIPv6:
		total += 16
	case socksAtypDomain:
		l, err := br.Peek(5)
		if err != nil {
			return req, err
		}
		if l[4] == 0 {
			return req, errSocksFormat
		}
		total += 1 + int(l[4])
	default:
		return req, errSocksAtyp
	}
	total += 2
	b, err := br.Peek(total)
	if err != nil {
		return req, err
	}
	addr := b[4 : total-2]
	switch atyp {
	case socksAtypIPv4:
		req.ip = netip.AddrFrom4([4]byte(addr))
	case socksAtypIPv6:
		req.ip = netip.AddrFrom16([16]byte(addr)).Unmap()
	case socksAtypDomain:
		req.domain = string(addr[1:])
	}
	req.port = uint16(b[total-2])<<8 | uint16(b[total-1])
	_, _ = br.Discard(total)
	return req, nil
}

// target validates the request's destination like any other target.
func (r socksRequest) target() (target, error) {
	if r.port == 0 {
		return target{}, errBadPort
	}
	if r.ip.IsValid() {
		return target{host: r.ip.String(), ip: r.ip, port: r.port}, nil
	}
	host, ip, err := normalizeHost(r.domain)
	if err != nil {
		return target{}, err
	}
	return target{host: host, ip: ip, port: r.port}, nil
}

func (c *conn) socksReply(code byte) {
	_ = c.raw.SetWriteDeadline(time.Now().Add(errorWriteTimeout))
	// BND.ADDR is not meaningful for a proxy that hides its addresses.
	_, _ = c.raw.Write([]byte{socksVersion, code, 0, socksAtypIPv4, 0, 0, 0, 0, 0, 0})
}

func (c *conn) serveSOCKS(start time.Time) {
	const proto = "socks5"
	fail := func(reason Reason, detail string) {
		c.sb.record(Decision{Protocol: proto, Verdict: VerdictError, Reason: reason, Detail: detail, Duration: time.Since(start)})
	}
	handshakeErr := func(err error) {
		if isTimeout(err) {
			fail(ReasonHeaderTimeout, "handshake not completed in time")
			return
		}
		fail(ReasonBadRequest, err.Error())
	}
	// The header deadline set in serve covers the whole handshake.
	if err := readSocksGreeting(c.br); err != nil {
		if errors.Is(err, errSocksNoAuth) {
			_ = c.raw.SetWriteDeadline(time.Now().Add(errorWriteTimeout))
			_, _ = c.raw.Write([]byte{socksVersion, socksAuthUnacceptable})
		}
		handshakeErr(err)
		return
	}
	_ = c.raw.SetWriteDeadline(time.Now().Add(errorWriteTimeout))
	if _, err := c.raw.Write([]byte{socksVersion, socksAuthNone}); err != nil {
		return
	}
	req, err := readSocksRequest(c.br)
	if err != nil {
		switch {
		case errors.Is(err, errSocksCommand):
			c.socksReply(socksCmdNotSupported)
		case errors.Is(err, errSocksAtyp):
			c.socksReply(socksAtypNotSupported)
		case errors.Is(err, errSocksFormat), errors.Is(err, errSocksVersion):
			c.socksReply(socksGeneralFailure)
		}
		handshakeErr(err)
		return
	}
	t, err := req.target()
	if err != nil {
		c.socksReply(socksNotAllowed)
		c.sb.record(Decision{Protocol: proto, Port: req.port, Verdict: VerdictDenied, Reason: ReasonBadTarget, Detail: err.Error(), Duration: time.Since(start)})
		return
	}
	_ = c.raw.SetDeadline(time.Time{})
	f, ref := c.open(proto, t, start)
	if ref != nil {
		c.socksReply(ref.socks)
		c.sb.record(ref.decision(proto, t, start))
		return
	}
	_ = c.raw.SetWriteDeadline(time.Now().Add(errorWriteTimeout))
	if _, err := c.raw.Write([]byte{socksVersion, socksSucceeded, 0, socksAtypIPv4, 0, 0, 0, 0, 0, 0}); err != nil {
		f.setEnd(ReasonIOError)
		f.finish()
		return
	}
	c.tunnel(f)
}

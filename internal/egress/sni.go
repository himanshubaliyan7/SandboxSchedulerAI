package egress

import (
	"bufio"
	"errors"
	"strings"
)

const (
	tlsRecordHandshake  = 0x16
	tlsHandshakeHello   = 0x01
	tlsMaxRecordLen     = 16384 + 256
	tlsExtServerName    = 0
	tlsAlertAccessDeny  = 49
	tlsAlertDecodeError = 50
)

var errTLSMalformed = errors.New("malformed TLS ClientHello")

// tlsAlert is a fatal TLS alert record, sent in the clear before the
// handshake so the client reports something better than a reset.
func tlsAlert(code byte) []byte {
	return []byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, code}
}

// checkSNI inspects, without consuming, the first bytes the client sends
// on a tunnel. It returns a non-empty reason (and the alert to send) if the
// tunnel must be refused.
func (c *conn) checkSNI(host string) (Reason, []byte) {
	hello, isTLS, err := peekClientHello(c.br)
	if !isTLS {
		return "", nil // not TLS, or the client went away: nothing to check
	}
	if err != nil {
		return ReasonTLSMalformed, tlsAlert(tlsAlertDecodeError)
	}
	sni, has, err := parseClientHelloSNI(hello)
	if err != nil {
		return ReasonTLSMalformed, tlsAlert(tlsAlertDecodeError)
	}
	if !has {
		return "", nil
	}
	if strings.TrimSuffix(strings.ToLower(sni), ".") != host {
		return ReasonSNIMismatch, tlsAlert(tlsAlertAccessDeny)
	}
	return "", nil
}

// peekClientHello returns the body of a TLS ClientHello at the front of br
// without consuming anything. isTLS is false if the stream does not start
// like a TLS handshake record (hello and err are then nil). Once it does,
// anything that is not a complete ClientHello fitting in br's buffer is an
// error. The ClientHello may be split over several records.
func peekClientHello(br *bufio.Reader) (hello []byte, isTLS bool, err error) {
	b, err := br.Peek(1)
	if err != nil || b[0] != tlsRecordHandshake {
		return nil, false, nil
	}
	b, err = br.Peek(3)
	if err != nil || b[1] != 0x03 || b[2] > 0x04 {
		return nil, false, nil
	}
	var msg []byte
	need := -1
	for off := 0; ; {
		if off+5 > br.Size() {
			return nil, true, errTLSMalformed
		}
		b, err = br.Peek(off + 5)
		if err != nil {
			return nil, true, errTLSMalformed
		}
		if b[off] != tlsRecordHandshake || b[off+1] != 0x03 {
			return nil, true, errTLSMalformed
		}
		n := int(b[off+3])<<8 | int(b[off+4])
		if n == 0 || n > tlsMaxRecordLen || off+5+n > br.Size() {
			return nil, true, errTLSMalformed
		}
		b, err = br.Peek(off + 5 + n)
		if err != nil {
			return nil, true, errTLSMalformed
		}
		msg = append(msg, b[off+5:off+5+n]...)
		off += 5 + n
		if need < 0 && len(msg) >= 4 {
			if msg[0] != tlsHandshakeHello {
				return nil, true, errTLSMalformed
			}
			need = 4 + (int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3]))
		}
		if need >= 0 && len(msg) >= need {
			return msg[4:need], true, nil
		}
	}
}

// parseClientHelloSNI extracts the host_name from a ClientHello body (the
// handshake message without its 4-byte header).
func parseClientHelloSNI(b []byte) (sni string, has bool, err error) {
	// client_version(2) random(32)
	if len(b) < 34 {
		return "", false, errTLSMalformed
	}
	b = b[34:]
	var ok bool
	if _, b, ok = takeVec(b, 1); !ok { // session_id
		return "", false, errTLSMalformed
	}
	if _, b, ok = takeVec(b, 2); !ok { // cipher_suites
		return "", false, errTLSMalformed
	}
	if _, b, ok = takeVec(b, 1); !ok { // compression_methods
		return "", false, errTLSMalformed
	}
	if len(b) == 0 {
		return "", false, nil // no extensions
	}
	exts, rest, ok := takeVec(b, 2)
	if !ok || len(rest) != 0 {
		return "", false, errTLSMalformed
	}
	for len(exts) > 0 {
		if len(exts) < 4 {
			return "", false, errTLSMalformed
		}
		typ := int(exts[0])<<8 | int(exts[1])
		var data []byte
		if data, exts, ok = takeVec(exts[2:], 2); !ok {
			return "", false, errTLSMalformed
		}
		if typ != tlsExtServerName {
			continue
		}
		if has {
			return "", false, errTLSMalformed // duplicate extension
		}
		list, tail, ok := takeVec(data, 2)
		if !ok || len(tail) != 0 || len(list) == 0 {
			return "", false, errTLSMalformed
		}
		found := false
		for len(list) > 0 {
			nameType := list[0]
			var name []byte
			if name, list, ok = takeVec(list[1:], 2); !ok {
				return "", false, errTLSMalformed
			}
			if nameType != 0 {
				continue
			}
			if found || len(name) == 0 {
				return "", false, errTLSMalformed
			}
			sni, found = string(name), true
		}
		if !found {
			return "", false, errTLSMalformed
		}
		has = true
	}
	return sni, has, nil
}

// takeVec reads a vector with an n-byte big-endian length prefix.
func takeVec(b []byte, n int) (vec, rest []byte, ok bool) {
	if len(b) < n {
		return nil, nil, false
	}
	l := 0
	for i := 0; i < n; i++ {
		l = l<<8 | int(b[i])
	}
	b = b[n:]
	if len(b) < l {
		return nil, nil, false
	}
	return b[:l], b[l:], true
}

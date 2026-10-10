package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Signing follows the Standard Webhooks specification
// (https://www.standardwebhooks.com), so its verification libraries work
// unchanged:
//
//	Webhook-Id:        the event id, the same on every attempt
//	Webhook-Timestamp: seconds since the epoch, when this attempt was made
//	Webhook-Signature: "v1," + base64(HMAC-SHA256(key, id "." timestamp "." body))
//
// The key is the base64-decoded part of the secret after "whsec_". While
// a rotated secret is in its grace period the header carries two
// signatures separated by a space, the current secret's first.
const (
	HeaderID        = "Webhook-Id"
	HeaderTimestamp = "Webhook-Timestamp"
	HeaderSignature = "Webhook-Signature"

	// SecretPrefix starts every signing secret.
	SecretPrefix = "whsec_"
	// secretBytes is the size of a generated key. The specification asks
	// for 24 to 64 bytes.
	secretBytes = 32

	signatureVersion = "v1"

	// DefaultTolerance is how far a Webhook-Timestamp may be from the
	// receiver's clock, in either direction, before Verify refuses it.
	DefaultTolerance = 5 * time.Minute
)

// Verification errors.
var (
	ErrInvalidSecret    = errors.New("webhook: the secret is not a whsec_ secret")
	ErrMissingHeaders   = errors.New("webhook: a signature header is missing")
	ErrInvalidTimestamp = errors.New("webhook: the timestamp header is not a number")
	ErrTimestampTooOld  = errors.New("webhook: the timestamp is too old")
	ErrTimestampTooNew  = errors.New("webhook: the timestamp is in the future")
	ErrNoMatch          = errors.New("webhook: no signature matches")
)

// NewSecret generates a signing secret: "whsec_" and 32 random bytes in
// base64.
func NewSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("webhook: generate secret: %w", err)
	}
	return SecretPrefix + base64.StdEncoding.EncodeToString(b), nil
}

// secretKey returns the HMAC key of a secret.
func secretKey(secret string) ([]byte, error) {
	enc, ok := strings.CutPrefix(secret, SecretPrefix)
	if !ok || enc == "" {
		return nil, ErrInvalidSecret
	}
	key, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(key) == 0 {
		return nil, ErrInvalidSecret
	}
	return key, nil
}

// Sign returns one signature ("v1,<base64>") of body for the event id and
// the attempt time.
func Sign(secret, id string, at time.Time, body []byte) (string, error) {
	key, err := secretKey(secret)
	if err != nil {
		return "", err
	}
	return signatureVersion + "," + base64.StdEncoding.EncodeToString(mac(key, id, at.Unix(), body)), nil
}

func mac(key []byte, id string, unix int64, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(id))
	h.Write([]byte{'.'})
	h.Write([]byte(strconv.FormatInt(unix, 10)))
	h.Write([]byte{'.'})
	h.Write(body)
	return h.Sum(nil)
}

// signatureHeaders returns the three signature headers of one attempt,
// signed with every secret given (the current one first).
func signatureHeaders(id string, at time.Time, body []byte, secrets ...string) (http.Header, error) {
	sigs := make([]string, 0, len(secrets))
	for _, s := range secrets {
		sig, err := Sign(s, id, at, body)
		if err != nil {
			return nil, err
		}
		sigs = append(sigs, sig)
	}
	h := http.Header{}
	h.Set(HeaderID, id)
	h.Set(HeaderTimestamp, strconv.FormatInt(at.Unix(), 10))
	h.Set(HeaderSignature, strings.Join(sigs, " "))
	return h, nil
}

// Verify checks the signature of a received webhook: body is the raw
// request body, exactly as it arrived. It fails unless one of the v1
// signatures in the header matches and the timestamp is within tolerance
// of now (DefaultTolerance when tolerance is zero). The comparison takes
// the same time whether or not a signature matches.
//
// It is what a receiver written in Go calls; the server itself only signs.
func Verify(secret string, header http.Header, body []byte, now time.Time, tolerance time.Duration) error {
	key, err := secretKey(secret)
	if err != nil {
		return err
	}
	id, ts, sigs := header.Get(HeaderID), header.Get(HeaderTimestamp), header.Get(HeaderSignature)
	if id == "" || ts == "" || sigs == "" {
		return ErrMissingHeaders
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrInvalidTimestamp
	}
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	sent := time.Unix(unix, 0)
	if now.Sub(sent) > tolerance {
		return ErrTimestampTooOld
	}
	if sent.Sub(now) > tolerance {
		return ErrTimestampTooNew
	}
	want := mac(key, id, unix, body)
	matched := 0
	for _, sig := range strings.Fields(sigs) {
		version, enc, ok := strings.Cut(sig, ",")
		if !ok || version != signatureVersion {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			continue
		}
		matched |= subtle.ConstantTimeCompare(got, want)
	}
	if matched != 1 {
		return ErrNoMatch
	}
	return nil
}

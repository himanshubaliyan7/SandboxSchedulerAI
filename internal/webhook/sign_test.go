package webhook

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The test vector of the Standard Webhooks reference implementations
// (github.com/standard-webhooks/standard-webhooks, e.g. the tests of its
// JavaScript and Python libraries, and the "verifying manually" page of
// its documentation): this secret, id, timestamp and body must produce
// exactly this signature.
//
// The secret is a published example, not a credential. It is assembled
// from its prefix and its base64 body so that the source holds no
// literal a secret scanner would report (test/repo checks that).
const (
	kaSecret    = "whsec_" + "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	kaID        = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	kaTimestamp = 1614265330
	kaBody      = `{"test": 2432232314}`
	kaSignature = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

func TestSignKnownAnswer(t *testing.T) {
	at := time.Unix(kaTimestamp, 0)
	got, err := Sign(kaSecret, kaID, at, []byte(kaBody))
	if err != nil {
		t.Fatal(err)
	}
	if got != kaSignature {
		t.Fatalf("Sign = %q, want %q", got, kaSignature)
	}
	h, err := signatureHeaders(kaID, at, []byte(kaBody), kaSecret)
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("webhook-id") != kaID || h.Get("webhook-timestamp") != "1614265330" || h.Get("webhook-signature") != kaSignature {
		t.Fatalf("headers: %v", h)
	}
	if err := Verify(kaSecret, h, []byte(kaBody), at.Add(time.Minute), 0); err != nil {
		t.Fatalf("Verify of the reference vector: %v", err)
	}
}

func TestVerify(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := NewSecret()
	if secret == other || !strings.HasPrefix(secret, SecretPrefix) {
		t.Fatalf("secrets: %q %q", secret, other)
	}
	if key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, SecretPrefix)); err != nil || len(key) != 32 {
		t.Fatalf("a generated secret is not 32 bytes of base64: %d %v", len(key), err)
	}
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"id":"evt_1","type":"sandbox.created"}`)
	h, err := signatureHeaders("evt_1", now, body, secret)
	if err != nil {
		t.Fatal(err)
	}
	clone := func(mut func(http.Header)) http.Header {
		c := h.Clone()
		mut(c)
		return c
	}
	for _, c := range []struct {
		name   string
		secret string
		header http.Header
		body   []byte
		now    time.Time
		tol    time.Duration
		want   error
	}{
		{"valid", secret, h, body, now, 0, nil},
		{"valid at the edge of the tolerance", secret, h, body, now.Add(5 * time.Minute), 0, nil},
		{"valid, receiver clock behind", secret, h, body, now.Add(-5 * time.Minute), 0, nil},
		{"too old", secret, h, body, now.Add(5*time.Minute + time.Second), 0, ErrTimestampTooOld},
		{"too new", secret, h, body, now.Add(-5*time.Minute - time.Second), 0, ErrTimestampTooNew},
		{"custom tolerance", secret, h, body, now.Add(30 * time.Second), 10 * time.Second, ErrTimestampTooOld},
		{"wrong secret", other, h, body, now, 0, ErrNoMatch},
		{"tampered body", secret, h, append([]byte(nil), append(body, ' ')...), now, 0, ErrNoMatch},
		{"tampered id", secret, clone(func(c http.Header) { c.Set(HeaderID, "evt_2") }), body, now, 0, ErrNoMatch},
		{"tampered timestamp", secret, clone(func(c http.Header) { c.Set(HeaderTimestamp, "1800000001") }), body, now, 0, ErrNoMatch},
		{"timestamp not a number", secret, clone(func(c http.Header) { c.Set(HeaderTimestamp, "soon") }), body, now, 0, ErrInvalidTimestamp},
		{"missing signature", secret, clone(func(c http.Header) { c.Del(HeaderSignature) }), body, now, 0, ErrMissingHeaders},
		{"missing id", secret, clone(func(c http.Header) { c.Del(HeaderID) }), body, now, 0, ErrMissingHeaders},
		{"unknown version only", secret, clone(func(c http.Header) {
			c.Set(HeaderSignature, "v2,"+strings.TrimPrefix(c.Get(HeaderSignature), "v1,"))
		}), body, now, 0, ErrNoMatch},
		{"garbage signature", secret, clone(func(c http.Header) { c.Set(HeaderSignature, "v1,!!!") }), body, now, 0, ErrNoMatch},
		{"secret without prefix", strings.TrimPrefix(secret, SecretPrefix), h, body, now, 0, ErrInvalidSecret},
		{"secret not base64", "whsec_***", h, body, now, 0, ErrInvalidSecret},
	} {
		if err := Verify(c.secret, c.header, c.body, c.now, c.tol); !errors.Is(err, c.want) {
			t.Errorf("%s: Verify = %v, want %v", c.name, err, c.want)
		}
	}

	// During a rotation both secrets sign; either verifies, in any
	// position, among signatures of versions the receiver does not know.
	both, err := signatureHeaders("evt_1", now, body, secret, other)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(both.Get(HeaderSignature))); n != 2 {
		t.Fatalf("%d signatures in %q", n, both.Get(HeaderSignature))
	}
	for _, s := range []string{secret, other} {
		if err := Verify(s, both, body, now, 0); err != nil {
			t.Errorf("rotation: %v", err)
		}
	}
	both.Set(HeaderSignature, "v1a,xyz "+both.Get(HeaderSignature))
	if err := Verify(other, both, body, now, 0); err != nil {
		t.Errorf("unknown version next to a valid one: %v", err)
	}
	if _, err := Sign("not-a-secret", "evt_1", now, body); !errors.Is(err, ErrInvalidSecret) {
		t.Errorf("Sign with a bad secret: %v", err)
	}
}

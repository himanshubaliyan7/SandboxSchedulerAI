package store

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestOpenErrorsNeverCarryThePassword opens the store with database URLs
// that fail in every way one can (they do not parse, nothing listens, the
// password is wrong) and checks that the error, which ends up on the
// operator's terminal and in logs, never contains the password.
func TestOpenErrorsNeverCarryThePassword(t *testing.T) {
	const pw = "S3cr3t-Pa55w0rd!x"
	esc := url.QueryEscape(pw)
	cases := map[string]string{
		"port is not a number":  "postgres://sbx:" + esc + "@127.0.0.1:notaport/sbx",
		"nothing listens":       "postgres://sbx:" + esc + "@127.0.0.1:1/sbx?sslmode=disable&connect_timeout=2",
		"bad parameter":         "postgres://sbx:" + esc + "@127.0.0.1:1/sbx?sslmode=bogus",
		"bad percent escape":    "postgres://sbx:" + pw + "%zz@127.0.0.1:1/sbx",
		"keyword/value form":    "host=127.0.0.1 port=1 user=sbx password=" + pw + " dbname=sbx sslmode=disable connect_timeout=2",
		"quoted keyword/value":  "host=127.0.0.1 port=1 user=sbx password='" + pw + "' dbname=sbx sslmode=bogus",
		"unbalanced quote":      "host=127.0.0.1 port=1 user=sbx password='" + pw + " dbname=sbx",
		"password as parameter": "postgres://sbx@127.0.0.1:1/sbx?sslmode=disable&connect_timeout=2&password=" + esc,
		"not a URL at all":      "://sbx:" + pw + "@",
	}
	// With a real server, also the case that matters most in practice:
	// the right server, the wrong password.
	if real := os.Getenv("SBX_TEST_DATABASE_URL"); real != "" {
		if u, err := url.Parse(real); err == nil && u.User != nil {
			u.User = url.UserPassword(u.User.Username(), pw)
			cases["wrong password on a real server"] = u.String()
		}
	}
	for name, dsn := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		st, err := Open(ctx, dsn)
		cancel()
		if err == nil {
			st.Close()
			t.Errorf("%s: Open succeeded", name)
			continue
		}
		t.Logf("%s: %v", name, err)
		for _, secret := range []string{pw, esc, "S3cr3t"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error contains the password: %v", name, err)
			}
		}
		// Wrapped causes must not bring it back when formatted on their own.
		for e := err; e != nil; e = errors.Unwrap(e) {
			if _, ok := e.(*redactedError); ok {
				break // below this point is the unredacted original, reachable by type only
			}
			if strings.Contains(e.Error(), pw) {
				t.Errorf("%s: a wrapped error contains the password: %v", name, e)
			}
		}
	}
}

func TestRedactErrorKeepsTheChain(t *testing.T) {
	base := errors.New("cannot reach postgres://u:hunter2@db/x")
	err := redactError(base, "postgres://u:hunter2@db/x")
	if strings.Contains(err.Error(), "hunter2") || !errors.Is(err, base) {
		t.Errorf("redacted error: %q, Is(base)=%v", err, errors.Is(err, base))
	}
	plain := errors.New("connection refused")
	if got := redactError(plain, "postgres://u:hunter2@db/x"); got != plain {
		t.Errorf("an error without the secret was rewrapped: %v", got)
	}
}

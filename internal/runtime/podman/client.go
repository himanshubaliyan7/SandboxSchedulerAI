package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// fallbackAPIVersion is used only if the service does not report a version.
// 4.0.0 is the minimum Libpod API version of both Podman 4.9 and 6.x.
const fallbackAPIVersion = "4.0.0"

// maxErrorBody bounds how much of a non-2xx body is read into an APIError.
const maxErrorBody = 64 << 10

// maxResponseBody bounds a successful JSON response (a list of thousands
// of containers is a few megabytes).
const maxResponseBody = 64 << 20

// APIError is a non-2xx response from the Podman service.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string // "message" of Libpod's error JSON, or the raw body
	Cause   string // "cause" of Libpod's error JSON
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("podman: %s %s: %d: %s", e.Method, e.Path, e.Status, msg)
}

// IsStatus reports whether err is an APIError with the given HTTP status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// client is a minimal Libpod REST client over a Unix socket.
type client struct {
	socket  string
	timeout time.Duration
	http    *http.Client

	mu      sync.Mutex
	version string // discovered Libpod API version, "" until first use
}

func newClient(socket string, dialTimeout, requestTimeout time.Duration) *client {
	dialer := &net.Dialer{Timeout: dialTimeout}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
	}
	return &client{socket: socket, timeout: requestTimeout, http: &http.Client{Transport: tr}}
}

// apiVersion returns the service's Libpod API version, discovering it on
// first use from the Libpod-API-Version header of the unversioned /_ping
// (set on every response by both Podman 4.9 and 6.x). A failed discovery
// is not cached, so a service that comes up later is picked up.
func (c *client) apiVersion(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version != "" {
		return c.version, nil
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://d/_ping", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("podman: ping %s: %w", c.socket, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	if resp.StatusCode/100 != 2 {
		return "", &APIError{Method: http.MethodGet, Path: "/_ping", Status: resp.StatusCode}
	}
	v := strings.TrimSpace(resp.Header.Get("Libpod-API-Version"))
	if !validVersion(v) {
		v = fallbackAPIVersion
	}
	c.version = v
	return v, nil
}

// validVersion accepts dotted numeric versions with an optional suffix
// ("4.9.3", "6.0.0-dev"); anything else is not put into a URL path.
func validVersion(v string) bool {
	if v == "" || len(v) > 32 {
		return false
	}
	for i, r := range v {
		switch {
		case r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '-' || r == '+' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')):
		default:
			return false
		}
	}
	return true
}

func (c *client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || c.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.timeout)
}

// do sends one request to /v<version>/libpod<path>. A non-nil in is sent
// as JSON; a non-nil out receives the decoded JSON body. Statuses in ok
// (default: any 2xx) succeed; everything else becomes an *APIError.
func (c *client) do(ctx context.Context, method, path string, query url.Values, in, out any, ok ...int) (int, error) {
	version, err := c.apiVersion(ctx)
	if err != nil {
		return 0, err
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, fmt.Errorf("podman: encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	u := "http://d/v" + version + "/libpod" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("podman: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if !statusOK(resp.StatusCode, ok) {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		ae := &APIError{Method: method, Path: path, Status: resp.StatusCode}
		var em struct {
			Cause   string `json:"cause"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &em) == nil && em.Message != "" {
			ae.Message, ae.Cause = em.Message, em.Cause
		} else {
			ae.Message = strings.TrimSpace(string(raw))
		}
		return resp.StatusCode, ae
	}
	if out == nil || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("podman: decode %s %s: %w", method, path, err)
	}
	return resp.StatusCode, nil
}

func statusOK(status int, ok []int) bool {
	if len(ok) == 0 {
		return status/100 == 2
	}
	for _, s := range ok {
		if s == status {
			return true
		}
	}
	return false
}

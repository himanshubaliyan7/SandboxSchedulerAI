package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is the small part of the sbxd REST API the MCP tools need. It
// authenticates with the tenant API key on every route, so it never has
// to keep per-sandbox access tokens.
type Client struct {
	base   string
	apiKey string
	http   *http.Client
}

// NewClient returns a client for the sbxd at baseURL. The HTTP client has
// no overall timeout: every call is bounded by its context instead,
// because a waited command legitimately takes as long as its own timeout.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: &http.Client{}}
}

// APIError is an error answered by sbxd.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("sbxd answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// controlTimeout bounds calls that do not wait for sandboxed work.
const controlTimeout = 30 * time.Second

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("User-Agent", "sbx-mcp/"+Version)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach sbxd at %s: %w", c.base, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &env)
		return nil, &APIError{Status: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
	}
	return resp, nil
}

// json sends in (when not nil) as JSON and returns the raw response body.
func (c *Client) json(ctx context.Context, method, path string, query url.Values, in any) ([]byte, error) {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, method, path, query, body, ct)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

func sandboxPath(id string, rest string) string {
	return "/v1/sandboxes/" + url.PathEscape(id) + rest
}

// WaitResult is the answer of a command started with wait=true.
type WaitResult struct {
	PID       string `json:"pid"`
	Stdout    []byte `json:"stdout"`
	Stderr    []byte `json:"stderr"`
	Truncated bool   `json:"truncated"`
	Exit      struct {
		ExitCode int    `json:"exit_code"`
		Signal   string `json:"signal"`
		Reason   string `json:"reason"`
	} `json:"exit"`
}

// Run starts a command and waits for it to exit.
func (c *Client) Run(ctx context.Context, sandboxID string, start map[string]any) (WaitResult, error) {
	var out WaitResult
	raw, err := c.json(ctx, http.MethodPost, sandboxPath(sandboxID, "/commands"), url.Values{"wait": {"true"}}, start)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("unexpected answer from sbxd: %w", err)
	}
	return out, nil
}

// ReadFile returns at most limit bytes of a file and whether more exist.
func (c *Client) ReadFile(ctx context.Context, sandboxID, path string, limit int) (data []byte, more bool, err error) {
	resp, err := c.do(ctx, http.MethodGet, sandboxPath(sandboxID, "/files"), url.Values{"path": {path}}, nil, "")
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

// WriteFile uploads data as the file at path.
func (c *Client) WriteFile(ctx context.Context, sandboxID, path, mode string, data []byte) error {
	q := url.Values{"path": {path}}
	if mode != "" {
		q.Set("mode", mode)
	}
	resp, err := c.do(ctx, http.MethodPut, sandboxPath(sandboxID, "/files"), q, bytes.NewReader(data), "application/octet-stream")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

package agentclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
)

func fileURL(route, path string, extra url.Values) string {
	q := url.Values{"path": {path}}
	for k, v := range extra {
		q[k] = v
	}
	return base + route + "?" + q.Encode()
}

// OpenFile opens a file in the sandbox for reading. size is the file's
// length, or -1 if the agent did not announce one. The caller must close
// the reader; a read that ends before size bytes returns
// io.ErrUnexpectedEOF.
func (c *Client) OpenFile(ctx context.Context, path string) (body io.ReadCloser, size int64, err error) {
	resp, err := c.do(ctx, http.MethodGet, fileURL("/files", path, nil), nil, "")
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// ReadFile copies a file in the sandbox to w and returns the number of
// bytes copied.
func (c *Client) ReadFile(ctx context.Context, path string, w io.Writer) (int64, error) {
	body, _, err := c.OpenFile(ctx, path)
	if err != nil {
		return 0, err
	}
	defer body.Close()
	return io.Copy(w, body)
}

// WriteFile streams r to a file in the sandbox, creating parent
// directories. The file appears atomically once r is exhausted. A mode
// of 0 means 0644.
func (c *Client) WriteFile(ctx context.Context, path string, mode os.FileMode, r io.Reader) error {
	if mode == 0 {
		mode = 0o644
	}
	extra := url.Values{"mode": {fmt.Sprintf("%04o", uint32(mode.Perm()))}}
	// Hide any Len method so the body is sent chunked and streamed.
	resp, err := c.do(ctx, http.MethodPut, fileURL("/files", path, extra), io.NopCloser(r), "application/octet-stream")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// DeleteFile removes a file, or a directory and everything in it.
func (c *Client) DeleteFile(ctx context.Context, path string) error {
	return c.call(ctx, http.MethodDelete, fileURL("/files", path, nil), nil, nil)
}

// ListFiles lists a directory, sorted by name.
func (c *Client) ListFiles(ctx context.Context, path string) ([]agentapi.FileInfo, error) {
	var out []agentapi.FileInfo
	err := c.call(ctx, http.MethodGet, fileURL("/files/list", path, nil), nil, &out)
	return out, err
}

// Mkdir creates a directory and any missing parents. It succeeds if the
// directory already exists; created says whether this call made it. (An
// agent older than the stat route always answers 204, which reads as
// "not created".)
func (c *Client) Mkdir(ctx context.Context, path string) (created bool, err error) {
	resp, err := c.do(ctx, http.MethodPost, fileURL("/files/mkdir", path, nil), nil, "")
	if err != nil {
		return false, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusCreated, nil
}

// StatFile describes one path. A missing path is a 404 with code
// "not_found"; an agent that predates the route answers 404 "no_route".
func (c *Client) StatFile(ctx context.Context, path string) (agentapi.FileInfo, error) {
	var out agentapi.FileInfo
	err := c.call(ctx, http.MethodGet, fileURL("/files/stat", path, nil), nil, &out)
	return out, err
}

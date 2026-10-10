// Package agentclient is the host-side client of the in-sandbox agent
// (cmd/sbx-agent). It speaks the agentapi contract over the agent's Unix
// socket.
package agentclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
)

// Client talks to one agent. It is safe for concurrent use.
type Client struct {
	socket string
	http   *http.Client
}

// New returns a client for the agent listening on socketPath. It does
// not connect; the socket need not exist yet (see WaitReady).
func New(socketPath string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialUnix(ctx, socketPath)
		},
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
	}
	// No client-wide timeout: streams are long-lived. Use contexts.
	return &Client{socket: socketPath, http: &http.Client{Transport: tr}}
}

// maxSunPath is the longest path connect(2) accepts in sockaddr_un
// (108 bytes including the terminating NUL).
const maxSunPath = 107

// dialUnix connects to a Unix socket. Host run directories can be deeper
// than sockaddr_un allows, so a path that does not fit is reached through
// a descriptor of its directory instead.
func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	if len(path) <= maxSunPath {
		return d.DialContext(ctx, "unix", path)
	}
	dirfd, err := unix.Open(filepath.Dir(path), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: fmt.Errorf("open %s: %w", filepath.Dir(path), err)}
	}
	defer unix.Close(dirfd)
	return d.DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d/%s", dirfd, filepath.Base(path)))
}

// SocketPath is the path this client dials.
func (c *Client) SocketPath() string { return c.socket }

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Error is an error response from the agent.
type Error struct {
	Status  int    // HTTP status
	Code    string // agentapi.Error.Code
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("agent: %s (%d %s)", e.Message, e.Status, e.Code)
}

// StatusOf returns the HTTP status of an agent error response, or 0 if
// err is not one (for example a connection failure).
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// IsNotFound reports whether the agent answered 404.
func IsNotFound(err error) bool { return StatusOf(err) == http.StatusNotFound }

// IsConflict reports whether the agent answered 409.
func IsConflict(err error) bool { return StatusOf(err) == http.StatusConflict }

// base is a placeholder authority; the transport ignores it.
const base = "http://sbx-agent/" + agentapi.Version

func procURL(id, suffix string) string {
	return base + "/processes/" + url.PathEscape(id) + suffix
}

func responseError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{Status: resp.StatusCode}
	var ae agentapi.Error
	if json.Unmarshal(b, &ae) == nil && ae.Code != "" {
		e.Code, e.Message = ae.Code, ae.Message
	} else {
		e.Code, e.Message = "unknown", string(bytes.TrimSpace(b))
	}
	return e
}

// do sends a request and returns the response if its status is 2xx.
func (c *Client) do(ctx context.Context, method, u string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, responseError(resp)
	}
	return resp, nil
}

// call sends in (if non-nil) as JSON and decodes a JSON response into
// out (if non-nil).
func (c *Client) call(ctx context.Context, method, u string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, method, u, body, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Health calls GET /health.
func (c *Client) Health(ctx context.Context) (agentapi.Health, error) {
	var h agentapi.Health
	err := c.call(ctx, http.MethodGet, base+"/health", nil, &h)
	return h, err
}

// WaitReady polls until the socket exists and the agent reports ready,
// or ctx is done. The returned error then wraps both ctx.Err() and the
// last failure.
func (c *Client) WaitReady(ctx context.Context) error {
	delay := 5 * time.Millisecond
	var last error
	for {
		if _, err := os.Stat(c.socket); err != nil {
			last = err
		} else {
			hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			h, err := c.Health(hctx)
			cancel()
			switch {
			case err != nil:
				last = err
			case !h.Ready:
				last = errors.New("agent reports not ready")
			default:
				return nil
			}
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("agent at %s not ready: %w (last error: %v)", c.socket, ctx.Err(), last)
		case <-t.C:
		}
		if delay < 100*time.Millisecond {
			delay *= 2
		}
	}
}

// Init calls POST /init.
func (c *Client) Init(ctx context.Context, req agentapi.InitRequest) error {
	return c.call(ctx, http.MethodPost, base+"/init", req, nil)
}

// Start starts a process.
func (c *Client) Start(ctx context.Context, req agentapi.StartRequest) (agentapi.ProcessInfo, error) {
	var pi agentapi.ProcessInfo
	err := c.call(ctx, http.MethodPost, base+"/processes", req, &pi)
	return pi, err
}

// List returns every process the agent knows, in start order.
func (c *Client) List(ctx context.Context) ([]agentapi.ProcessInfo, error) {
	var ps []agentapi.ProcessInfo
	err := c.call(ctx, http.MethodGet, base+"/processes", nil, &ps)
	return ps, err
}

// Get returns one process.
func (c *Client) Get(ctx context.Context, id string) (agentapi.ProcessInfo, error) {
	var pi agentapi.ProcessInfo
	err := c.call(ctx, http.MethodGet, procURL(id, ""), nil, &pi)
	return pi, err
}

// EventStream is an open NDJSON event stream of a process: the agent has
// accepted the request and sent its response headers. Read it with Events
// and Close it when done.
type EventStream struct {
	ctx  context.Context
	body io.ReadCloser
}

// OpenStream opens the event stream of a process starting at offset from
// and returns once the agent has answered: an unknown process or an
// unreachable agent is an error here, before any event. The agent answers
// at once, whether or not the process has produced anything. ctx governs
// the whole life of the stream.
func (c *Client) OpenStream(ctx context.Context, id string, from int64) (*EventStream, error) {
	resp, err := c.do(ctx, http.MethodGet, procURL(id, "/stream?from="+strconv.FormatInt(from, 10)), nil, "")
	if err != nil {
		return nil, err
	}
	return &EventStream{ctx: ctx, body: resp.Body}, nil
}

// Close releases the stream's connection. It is safe to call twice.
func (s *EventStream) Close() error { return s.body.Close() }

// Events yields the stream's events. The sequence ends after the exit
// event, on the first error (yielded once, with a zero Event), or when
// the stream's context is done (its error is yielded). A sequence that
// ends without an exit event and without an error does not happen: a
// connection cut short yields io.ErrUnexpectedEOF. Keepalive events are
// yielded like any other. Events may be ranged over once.
func (s *EventStream) Events() iter.Seq2[agentapi.Event, error] {
	return func(yield func(agentapi.Event, error) bool) {
		defer s.body.Close()
		// A line is one Event: at most MaxFrameBytes of data, base64.
		br := bufio.NewReaderSize(s.body, 64<<10)
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				if cerr := s.ctx.Err(); cerr != nil {
					err = cerr
				} else if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				yield(agentapi.Event{}, err)
				return
			}
			var ev agentapi.Event
			if err := json.Unmarshal(line, &ev); err != nil {
				yield(agentapi.Event{}, fmt.Errorf("agent: invalid stream event: %w", err))
				return
			}
			if !yield(ev, nil) || ev.Type == agentapi.EventExit {
				return
			}
		}
	}
}

// Stream is OpenStream followed by Events: a failure to open is yielded
// as the sequence's first and only item.
//
// To resume after an error, call Stream again with from set to the
// Offset+len(Data) of the last output event received.
func (c *Client) Stream(ctx context.Context, id string, from int64) iter.Seq2[agentapi.Event, error] {
	return func(yield func(agentapi.Event, error) bool) {
		s, err := c.OpenStream(ctx, id, from)
		if err != nil {
			yield(agentapi.Event{}, err)
			return
		}
		s.Events()(yield)
	}
}

// WriteStdin writes data to the process's stdin (pipe mode started with
// Stdin, or a PTY). It returns once the process's stdin has accepted it.
func (c *Client) WriteStdin(ctx context.Context, id string, data []byte) error {
	return c.WriteStdinFrom(ctx, id, bytes.NewReader(data))
}

// WriteStdinFrom streams r to the process's stdin until r returns EOF.
func (c *Client) WriteStdinFrom(ctx context.Context, id string, r io.Reader) error {
	resp, err := c.do(ctx, http.MethodPost, procURL(id, "/stdin"), r, "application/octet-stream")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// CloseStdin closes the process's stdin (pipe mode only).
func (c *Client) CloseStdin(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, procURL(id, "/close-stdin"), nil, nil)
}

// Signal sends a signal (e.g. "SIGINT") to the process group.
func (c *Client) Signal(ctx context.Context, id, signal string) error {
	return c.call(ctx, http.MethodPost, procURL(id, "/signal"), agentapi.SignalRequest{Signal: signal}, nil)
}

// Resize resizes the process's PTY.
func (c *Client) Resize(ctx context.Context, id string, cols, rows uint16) error {
	return c.call(ctx, http.MethodPost, procURL(id, "/resize"), agentapi.ResizeRequest{Cols: cols, Rows: rows}, nil)
}

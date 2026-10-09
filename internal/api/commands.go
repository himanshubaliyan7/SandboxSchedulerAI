package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
)

// ExitView is the terminal status of a command.
type ExitView struct {
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
	// Reason is one of exited, killed, timeout, oom.
	Reason string `json:"reason"`
}

func exitView(e *agentapi.ExitStatus) *ExitView {
	if e == nil {
		return nil
	}
	return &ExitView{ExitCode: e.ExitCode, Signal: e.Signal, Reason: string(e.Reason)}
}

// CommandView describes a command (a process in the sandbox). Pid is the
// agent's opaque process id, not a kernel pid.
type CommandView struct {
	Pid       string    `json:"pid"`
	Tag       string    `json:"tag,omitempty"`
	Cmd       string    `json:"cmd"`
	Args      []string  `json:"args,omitempty"`
	PTY       bool      `json:"pty"`
	StartedAt time.Time `json:"started_at"`
	Exit      *ExitView `json:"exit,omitempty"`
	// NextOffset is the offset the next output byte will get.
	NextOffset int64 `json:"next_offset"`
}

func commandView(p agentapi.ProcessInfo) CommandView {
	return CommandView{Pid: p.ID, Tag: p.Tag, Cmd: p.Cmd, Args: p.Args, PTY: p.PTY, StartedAt: p.StartedAt,
		Exit: exitView(p.Exit), NextOffset: p.NextOffset}
}

// StartCommandRequest is the body of POST /v1/sandboxes/{id}/commands.
type StartCommandRequest struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
	// TimeoutMs is the per-command wall-clock limit; zero means none.
	TimeoutMs int64 `json:"timeout_ms,omitempty"`
	// Stdin keeps stdin open in pipe mode; by default it is /dev/null.
	Stdin bool `json:"stdin,omitempty"`
	// Tag is an optional name, unique among the sandbox's live commands.
	Tag string `json:"tag,omitempty"`
	// PTY runs the command on a pseudo-terminal of this size.
	PTY *agentapi.PTYSize `json:"pty,omitempty"`
}

// StreamEvent is one line of an NDJSON command stream. The gateway adds
// two types to the agent's: "start", the first line of
// POST .../commands?wait=stream, and "error", the last line of a stream
// that could not be continued.
type StreamEvent struct {
	Type    string       `json:"type"`
	Offset  int64        `json:"offset"`
	Data    []byte       `json:"data,omitempty"` // base64 in JSON
	Dropped int64        `json:"dropped,omitempty"`
	Exit    *ExitView    `json:"exit,omitempty"`
	Command *CommandView `json:"command,omitempty"`
	Error   *ErrorBody   `json:"error,omitempty"`
}

// Stream event types added by the gateway.
const (
	EventStart = "start"
	EventError = "error"
)

// WaitResult is the response of POST .../commands?wait=true.
type WaitResult struct {
	CommandView
	Stdout []byte `json:"stdout"` // base64 in JSON; PTY output is here
	Stderr []byte `json:"stderr"` // base64 in JSON
	// Truncated is true when stdout or stderr was cut at the server's
	// limit, or when output was lost from the agent's replay buffer.
	Truncated bool `json:"truncated"`
}

func (s *Server) handleStartCommand(w http.ResponseWriter, r *http.Request, sess *session) error {
	wait := r.URL.Query().Get("wait")
	switch wait {
	case "", "false", "true", "stream":
	default:
		return badRequest("wait must be true, false or stream")
	}
	var req StartCommandRequest
	if err := s.decodeJSON(w, r, &req, false); err != nil {
		return err
	}
	if req.Cmd == "" {
		return badRequest("cmd is required")
	}
	if req.TimeoutMs < 0 {
		return badRequest("timeout_ms must not be negative")
	}
	if err := validateEnv(req.Env); err != nil {
		return err
	}
	if req.PTY != nil && (req.PTY.Cols == 0 || req.PTY.Rows == 0) {
		return badRequest("pty.cols and pty.rows must be positive")
	}
	start := agentapi.StartRequest{Cmd: req.Cmd, Args: req.Args, Env: req.Env, Cwd: req.Cwd, PTY: req.PTY,
		Stdin: req.Stdin, Tag: req.Tag, TimeoutMs: req.TimeoutMs}

	ctx, cancel := s.opCtx(r)
	info, err := sess.client.Start(ctx, start)
	cancel()
	if err != nil {
		return s.agentFailure(sess, err, "")
	}
	s.watch(sess, info, start)

	switch wait {
	case "stream":
		first := commandView(info)
		return s.pipeStream(w, r, sess, info.ID, 0, &first)
	case "true":
		return s.waitCommand(w, r, sess, info)
	}
	w.Header().Set("Location", r.URL.Path+"/"+info.ID)
	writeJSON(w, http.StatusCreated, commandView(info))
	return nil
}

// waitCommand collects a command's output until it exits.
func (s *Server) waitCommand(w http.ResponseWriter, r *http.Request, sess *session, info agentapi.ProcessInfo) error {
	ctx, cancel := s.streamCtx(r)
	defer cancel()
	s.m.streamStarted("wait")
	defer s.m.streamEnded("wait")

	res := WaitResult{CommandView: commandView(info), Stdout: []byte{}, Stderr: []byte{}}
	limit := s.cfg.MaxWaitOutputBytes
	add := func(dst *[]byte, data []byte) {
		if room := limit - len(*dst); room < len(data) {
			data = data[:max(room, 0)]
			res.Truncated = true
		}
		*dst = append(*dst, data...)
	}
	for ev, err := range sess.client.Stream(ctx, info.ID, 0) {
		if err != nil {
			return s.agentFailure(sess, err, CodeProcessNotFound)
		}
		switch ev.Type {
		case agentapi.EventStdout, agentapi.EventPTY:
			add(&res.Stdout, ev.Data)
			s.touch(sess)
		case agentapi.EventStderr:
			add(&res.Stderr, ev.Data)
			s.touch(sess)
		case agentapi.EventTruncated:
			res.Truncated = true
		case agentapi.EventExit:
			res.Exit = exitView(ev.Exit)
			res.NextOffset = ev.Offset
		}
	}
	if res.Exit == nil {
		return s.agentFailure(sess, errors.New("stream ended without an exit event"), CodeProcessNotFound)
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func parseFrom(r *http.Request) (int64, error) {
	v := r.URL.Query().Get("from")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, badRequest("from must be a non-negative integer")
	}
	return n, nil
}

func (s *Server) handleStreamCommand(w http.ResponseWriter, r *http.Request, sess *session) error {
	from, err := parseFrom(r)
	if err != nil {
		return err
	}
	return s.pipeStream(w, r, sess, r.PathValue("pid"), from, nil)
}

// streamFlushInterval bounds how long a written event can sit in the
// response buffer; a burst of output is flushed at most this often.
const streamFlushInterval = 20 * time.Millisecond

// ndjsonWriter writes one JSON document per line and flushes promptly
// without flushing on every event of a burst.
type ndjsonWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	rc      *http.ResponseController
	enc     *json.Encoder
	last    time.Time
	pending bool
	timer   *time.Timer
	closed  bool
}

func newNDJSONWriter(w http.ResponseWriter) *ndjsonWriter {
	return &ndjsonWriter{w: w, rc: http.NewResponseController(w), enc: json.NewEncoder(w)}
}

func (n *ndjsonWriter) write(ev StreamEvent) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.enc.Encode(ev); err != nil {
		return err
	}
	if time.Since(n.last) >= streamFlushInterval {
		n.last = time.Now()
		n.pending = false
		return n.rc.Flush()
	}
	if !n.pending {
		n.pending = true
		if n.timer == nil {
			n.timer = time.AfterFunc(streamFlushInterval, n.flushLater)
		} else {
			n.timer.Reset(streamFlushInterval)
		}
	}
	return nil
}

func (n *ndjsonWriter) flushLater() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || !n.pending {
		return
	}
	n.pending = false
	n.last = time.Now()
	_ = n.rc.Flush()
}

// close flushes what is pending and stops the timer. The handler must
// call it before returning: the ResponseWriter is invalid afterwards.
func (n *ndjsonWriter) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	if n.timer != nil {
		n.timer.Stop()
	}
	_ = n.rc.Flush()
}

// pipeStream relays a process's event stream from the agent to the
// client as NDJSON, starting at offset from. It ends with the exit
// event, or with an error event if the stream cannot continue. Nothing is
// buffered beyond one event: a slow client slows the reads from the
// agent, and the agent then drops from its ring buffer and says so with a
// truncated event. first, when set, is sent as a "start" event.
func (s *Server) pipeStream(w http.ResponseWriter, r *http.Request, sess *session, pid string, from int64, first *CommandView) error {
	ctx, cancel := s.streamCtx(r)
	defer cancel()
	s.m.streamStarted("ndjson")
	defer s.m.streamEnded("ndjson")

	var out *ndjsonWriter
	begin := func() {
		h := w.Header()
		h.Set("Content-Type", "application/x-ndjson")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		out = newNDJSONWriter(w)
	}
	defer func() {
		if out != nil {
			out.close()
		}
	}()
	if first != nil {
		begin()
		if err := out.write(StreamEvent{Type: EventStart, Command: first}); err != nil {
			return nil
		}
	}
	for ev, err := range sess.client.Stream(ctx, pid, from) {
		if err != nil {
			if ctx.Err() != nil && r.Context().Err() != nil {
				return nil // the client went away
			}
			if ctx.Err() != nil {
				err = &Error{Status: http.StatusServiceUnavailable, Code: CodeShuttingDown,
					Message: "the server is shutting down; reconnect with from=<last offset>"}
			} else {
				err = s.agentFailure(sess, err, CodeProcessNotFound)
			}
			if out == nil {
				return err // nothing sent yet: a plain HTTP error
			}
			e := toError(err)
			_ = out.write(StreamEvent{Type: EventError, Error: &ErrorBody{
				Code: e.Code, Message: e.Message, RequestID: requestID(r), Details: e.Details}})
			return nil
		}
		if out == nil {
			begin()
		}
		if ev.Type == agentapi.EventStdout || ev.Type == agentapi.EventStderr || ev.Type == agentapi.EventPTY {
			s.touch(sess)
		}
		if err := out.write(StreamEvent{Type: string(ev.Type), Offset: ev.Offset, Data: ev.Data, Dropped: ev.Dropped,
			Exit: exitView(ev.Exit)}); err != nil {
			return nil // the client went away
		}
	}
	return nil
}

func (s *Server) handleListCommands(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, cancel := s.opCtx(r)
	defer cancel()
	procs, err := sess.client.List(ctx)
	if err != nil {
		return s.agentFailure(sess, err, "")
	}
	out := make([]CommandView, 0, len(procs))
	for _, p := range procs {
		out = append(out, commandView(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": out})
	return nil
}

func (s *Server) handleGetCommand(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, cancel := s.opCtx(r)
	defer cancel()
	p, err := sess.client.Get(ctx, r.PathValue("pid"))
	if err != nil {
		return s.agentFailure(sess, err, CodeProcessNotFound)
	}
	writeJSON(w, http.StatusOK, commandView(p))
	return nil
}

// handleStdin streams the request body to the command's stdin. It
// returns once the process has accepted all of it.
func (s *Server) handleStdin(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, cancel := s.streamCtx(r)
	defer cancel()
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	if err := sess.client.WriteStdinFrom(ctx, r.PathValue("pid"), body); err != nil {
		return s.agentFailure(sess, bodyError(err), CodeProcessNotFound)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// bodyError recovers a body-size violation that the agent client reports
// wrapped in a transport error.
func bodyError(err error) error {
	var maxb *http.MaxBytesError
	if errors.As(err, &maxb) {
		return maxb
	}
	return err
}

func (s *Server) handleCloseStdin(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, cancel := s.opCtx(r)
	defer cancel()
	if err := sess.client.CloseStdin(ctx, r.PathValue("pid")); err != nil {
		return s.agentFailure(sess, err, CodeProcessNotFound)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleSignal(w http.ResponseWriter, r *http.Request, sess *session) error {
	var req agentapi.SignalRequest
	if err := s.decodeJSON(w, r, &req, false); err != nil {
		return err
	}
	if req.Signal == "" {
		return badRequest("signal is required")
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	if err := sess.client.Signal(ctx, r.PathValue("pid"), req.Signal); err != nil {
		return s.agentFailure(sess, err, CodeProcessNotFound)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleResize(w http.ResponseWriter, r *http.Request, sess *session) error {
	var req agentapi.ResizeRequest
	if err := s.decodeJSON(w, r, &req, false); err != nil {
		return err
	}
	if req.Cols == 0 || req.Rows == 0 {
		return badRequest("cols and rows must be positive")
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	if err := sess.client.Resize(ctx, r.PathValue("pid"), req.Cols, req.Rows); err != nil {
		return s.agentFailure(sess, err, CodeProcessNotFound)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

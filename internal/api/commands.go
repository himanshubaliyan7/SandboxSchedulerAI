package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
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
// that could not be continued. Offset means nothing on those two (it is
// always 0): a client must not move its position because of them.
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
	idemKey := r.Header.Get(headerIdempotencyKey)
	if idemKey != "" && !validIdempotencyKey(idemKey) {
		return badRequest("%s must be 1 to %d characters from [A-Za-z0-9._:-]", headerIdempotencyKey, maxIdempotencyKeyLen)
	}

	ctx, cancel := s.opCtx(r)
	info, replayed, err := s.startOnce(ctx, sess, idemKey, start)
	cancel()
	if err != nil {
		return s.agentFailure(sess, err, "")
	}
	if replayed {
		w.Header().Set(headerIdempotentReplayed, "true")
	}

	switch wait {
	case "stream":
		first := commandView(info)
		return s.pipeStream(w, r, sess, info.ID, 0, &first)
	case "true":
		return s.waitCommand(w, r, sess, info)
	}
	w.Header().Set("Location", r.URL.Path+"/"+info.ID)
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK // nothing was created by this request
	}
	writeJSON(w, status, commandView(info))
	return nil
}

// Idempotent command start.
//
// A start that carries an Idempotency-Key header is executed at most once
// per sandbox and key: a second request with the same key and the same
// body does not start anything, it answers for the command the first one
// started (with the header Idempotent-Replayed: true), in whatever form
// its own wait parameter asks for. So a client whose connection was cut
// before it learned the pid can simply send the request again.
//
// The keys live in the gateway's session for the sandbox, in memory. That
// is enough: a sandbox never outlives the server process that assigned it
// (a restart ends every session with host_drain), so there is no later
// process in which a retry could be mistaken for a new command. A start
// that failed is forgotten, so the same key can be used to try again.
const (
	headerIdempotencyKey     = "Idempotency-Key"
	headerIdempotentReplayed = "Idempotent-Replayed"
	maxIdempotencyKeyLen     = 128
	// maxIdempotencyKeys bounds the keys remembered per sandbox; beyond
	// it the oldest are forgotten.
	maxIdempotencyKeys = 1024
)

func validIdempotencyKey(k string) bool {
	if k == "" || len(k) > maxIdempotencyKeyLen {
		return false
	}
	for _, c := range k {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

type idemEntry struct {
	hash [sha256.Size]byte
	done chan struct{} // closed when info or err is set
	info agentapi.ProcessInfo
	err  error
}

type idemTable struct {
	mu    sync.Mutex
	m     map[string]*idemEntry
	order []string
}

// startOnce starts a command, or with a key that was used before returns
// the command that request started (replayed true).
func (s *Server) startOnce(ctx context.Context, sess *session, key string, req agentapi.StartRequest) (agentapi.ProcessInfo, bool, error) {
	if key == "" {
		info, err := sess.client.Start(ctx, req)
		if err == nil {
			s.watch(sess, info, req)
		}
		return info, false, err
	}
	canon, err := json.Marshal(req) // map keys are sorted: equal requests give equal bytes
	if err != nil {
		return agentapi.ProcessInfo{}, false, err
	}
	hash := sha256.Sum256(canon)

	t := &sess.idem
	t.mu.Lock()
	if e := t.m[key]; e != nil {
		t.mu.Unlock()
		if e.hash != hash {
			return agentapi.ProcessInfo{}, false, errf(http.StatusConflict, CodeIdempotencyKeyUsed,
				"this %s was already used for a different command", headerIdempotencyKey)
		}
		select {
		case <-e.done:
		case <-ctx.Done():
			return agentapi.ProcessInfo{}, false, ctx.Err()
		}
		if e.err != nil {
			return agentapi.ProcessInfo{}, false, e.err
		}
		// Answer with the command as it is now, not as it was at start.
		if cur, err := sess.client.Get(ctx, e.info.ID); err == nil {
			return cur, true, nil
		}
		return e.info, true, nil
	}
	e := &idemEntry{hash: hash, done: make(chan struct{})}
	if t.m == nil {
		t.m = map[string]*idemEntry{}
	}
	t.m[key] = e
	t.order = append(t.order, key)
	for len(t.order) > maxIdempotencyKeys {
		delete(t.m, t.order[0])
		t.order = t.order[1:]
	}
	t.mu.Unlock()

	e.info, e.err = sess.client.Start(ctx, req)
	if e.err != nil {
		t.mu.Lock()
		if t.m[key] == e {
			delete(t.m, key)
		}
		t.mu.Unlock()
	} else {
		s.watch(sess, e.info, req)
	}
	close(e.done)
	return e.info, false, e.err
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

// flush sends what has been written so far, and the status line and
// headers if they have not gone out yet.
func (n *ndjsonWriter) flush() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.last = time.Now()
	n.pending = false
	return n.rc.Flush()
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
// truncated event.
//
// The status line and the headers are sent as soon as it is known that
// the stream can be served, never later than the agent's own answer and
// never waiting for the command to say something:
//
//   - attaching (first nil): the agent's stream is opened first, so that
//     an unknown pid, a dead sandbox or an agent that does not answer is
//     still a plain HTTP error; then the headers are flushed;
//   - starting with wait=stream (first set): the command already runs, so
//     the headers and the "start" event carrying its pid are flushed
//     before anything else is attempted. Whatever goes wrong afterwards
//     is an in-stream "error" event, and the client has the pid to
//     reattach with.
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
	// fail reports err as an HTTP error while that is still possible and
	// as the stream's last event afterwards.
	fail := func(err error) error {
		switch {
		case ctx.Err() != nil && r.Context().Err() != nil:
			return nil // the client went away
		case ctx.Err() != nil:
			err = &Error{Status: http.StatusServiceUnavailable, Code: CodeShuttingDown,
				Message: "the server is shutting down; reconnect with from=<last offset>", RetryAfter: 5 * time.Second}
		default:
			err = s.agentFailure(sess, err, CodeProcessNotFound)
		}
		if out == nil {
			return err
		}
		e := toError(err)
		_ = out.write(StreamEvent{Type: EventError, Error: &ErrorBody{
			Code: e.Code, Message: e.Message, RequestID: requestID(r), Details: e.Details}})
		return nil
	}

	if first != nil {
		begin()
		// The first write of a stream is flushed at once.
		if err := out.write(StreamEvent{Type: EventStart, Command: first}); err != nil {
			return nil
		}
	}

	// The agent answers a stream request immediately; RequestTimeout
	// bounds the wait for that answer, not the stream.
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	var timedOut atomic.Bool
	timer := time.AfterFunc(s.cfg.RequestTimeout, func() { timedOut.Store(true); scancel() })
	stream, err := sess.client.OpenStream(sctx, pid, from)
	if !timer.Stop() && err == nil {
		_ = stream.Close()
		err = context.DeadlineExceeded
	}
	if err != nil {
		if timedOut.Load() && ctx.Err() == nil {
			err = context.DeadlineExceeded
		}
		return fail(err)
	}
	defer stream.Close()
	if out == nil {
		begin()
		if err := out.flush(); err != nil {
			return nil // the client went away
		}
	}

	for ev, err := range stream.Events() {
		if err != nil {
			return fail(err)
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

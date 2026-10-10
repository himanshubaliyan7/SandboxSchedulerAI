package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentclient"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/store"
)

// session is the gateway's in-memory state for one running sandbox: the
// agent client, the generation that fences every call made on its behalf,
// and the debounce state of the activity timestamp. Nothing in it is
// needed for correctness after a restart.
type session struct {
	id       string
	gen      int64 // from the sandbox row; never from the client
	tenantID string
	instance string
	client   *agentclient.Client

	ctx    context.Context // cancelled when the session is dropped
	cancel context.CancelFunc

	lastTouch atomic.Int64 // unix nanoseconds of the last store write
	checking  atomic.Bool  // a lost-instance check is in flight

	idem idemTable // idempotency keys of command starts
}

type sessions struct {
	mu sync.Mutex
	m  map[string]*session
}

func newSessions() *sessions { return &sessions{m: map[string]*session{}} }

// get returns the session of a sandbox, creating it with mk on first use
// and replacing it if the generation has changed.
func (ss *sessions) get(sb core.Sandbox, mk func() *session) *session {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if cur := ss.m[sb.ID]; cur != nil {
		if cur.gen == sb.Generation {
			return cur
		}
		cur.close()
	}
	sess := mk()
	ss.m[sb.ID] = sess
	return sess
}

func (ss *sessions) drop(id string) {
	ss.mu.Lock()
	sess := ss.m[id]
	delete(ss.m, id)
	ss.mu.Unlock()
	if sess != nil {
		sess.close()
	}
}

func (ss *sessions) snapshot() []*session {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make([]*session, 0, len(ss.m))
	for _, s := range ss.m {
		out = append(out, s)
	}
	return out
}

func (ss *sessions) closeAll() {
	for _, s := range ss.snapshot() {
		ss.drop(s.id)
	}
}

func (s *session) close() {
	s.cancel()
	s.client.Close()
}

func (s *Server) newSession(sb core.Sandbox) *session {
	ctx, cancel := context.WithCancel(s.baseCtx)
	sess := &session{
		id: sb.ID, gen: sb.Generation, tenantID: sb.TenantID, instance: sb.InstanceID,
		client: s.cfg.Agents(s.pool.RunDir(sb.ID)),
		ctx:    ctx, cancel: cancel,
	}
	// The claim has just set last_activity_at, or a previous process did
	// within the idle timeout: either way the row is fresh enough.
	return sess
}

// touch records activity on the sandbox, at most once per TouchInterval.
// It is called for every data-plane request and for output delivered to
// an attached stream, so the idle timer measures real inactivity.
func (s *Server) touch(sess *session) {
	now := time.Now().UnixNano()
	last := sess.lastTouch.Load()
	if now-last < int64(s.cfg.TouchInterval) || !sess.lastTouch.CompareAndSwap(last, now) {
		return
	}
	ctx, cancel := context.WithTimeout(s.baseCtx, 5*time.Second)
	defer cancel()
	if err := s.pool.Touch(ctx, sess.id, sess.gen); err != nil {
		s.log.Debug("touch failed", "sandbox", sess.id, "err", err)
	}
}

// agentFailure turns an error from an agent call into the API error to
// return. An error response from the agent is passed through (with the
// ambiguous "not_found" made specific by notFound). A transport failure
// means the agent is gone, so the instance is checked right away: if it
// died, the sandbox is failed with its real reason (oom_killed,
// instance_lost) and the caller is told the sandbox has terminated.
func (s *Server) agentFailure(sess *session, err error, notFound string) error {
	var age *agentclient.Error
	if errors.As(err, &age) {
		e := &Error{Status: age.Status, Code: age.Code, Message: age.Message}
		if age.Status == http.StatusNotFound && age.Code == "not_found" && notFound != "" {
			e.Code = notFound
		}
		return e
	}
	var apiErr *Error
	if errors.As(err, &apiErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var maxb *http.MaxBytesError
	if errors.As(err, &maxb) {
		return err
	}
	if term := s.terminated(sess); term != nil {
		return term
	}
	s.log.Warn("agent unreachable", "sandbox", sess.id, "err", err)
	return &Error{Status: http.StatusBadGateway, Code: CodeAgentUnavailable,
		Message: "the sandbox agent did not answer", RetryAfter: time.Second}
}

// How often, and how many more times, terminated looks at an instance
// that still appears to be running.
const (
	lostCheckInterval = 100 * time.Millisecond
	lostCheckAttempts = 10
)

// terminated checks whether the sandbox behind sess is gone and, if so,
// returns the sandbox_terminated error describing why; nil if it is still
// running as far as the backend and the store can tell.
func (s *Server) terminated(sess *session) *Error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseCtx), 10*time.Second)
	defer cancel()
	var acc store.SandboxAccess
	// The agent's connection breaks the instant its process is killed,
	// which can be a moment before the backend reports the instance as
	// exited. So an instance that still looks alive is looked at again
	// for a short while before the failure is blamed on the agent alone.
	for attempt := 0; ; attempt++ {
		if sess.checking.CompareAndSwap(false, true) {
			if _, err := s.pool.CheckInstance(ctx, sess.id, sess.gen); err != nil {
				s.log.Debug("instance check failed", "sandbox", sess.id, "err", err)
			}
			sess.checking.Store(false)
		}
		var err error
		if acc, err = s.store.GetSandboxAccess(ctx, sess.id); err != nil {
			return nil
		}
		if acc.Sandbox.Generation != sess.gen || acc.Sandbox.State != core.StateRunning {
			break
		}
		if attempt >= lostCheckAttempts {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(lostCheckInterval):
		}
	}
	s.sessions.drop(sess.id)
	e := errf(http.StatusConflict, CodeSandboxTerminated, "sandbox %s has terminated", sess.id)
	e.Details = map[string]any{"state": publicState(acc.Sandbox.State)}
	if acc.Sandbox.Reason != "" {
		e.Details["reason"] = string(acc.Sandbox.Reason)
		e.Message += ": " + string(acc.Sandbox.Reason)
	}
	return e
}

// Event types of the execution record. Every command started through the
// API appends one command_started and one command_finished event to the
// sandbox's event log.
const (
	EventCommandStarted  = "command_started"
	EventCommandFinished = "command_finished"
)

// maxRecordedArgBytes bounds the argv copied into an execution record.
const maxRecordedArgBytes = 4096

func recordedArgs(args []string) (out []string, truncated bool) {
	n := 0
	for _, a := range args {
		if n+len(a) > maxRecordedArgBytes {
			return out, true
		}
		n += len(a)
		out = append(out, a)
	}
	return out, false
}

// watch records the execution of one command: a command_started event
// now, and a command_finished event with exit code, reason, duration and
// output byte counts when the process ends, whether or not any client is
// attached. It reads the process's event stream from the agent for the
// byte counts; the environment is never recorded.
func (s *Server) watch(sess *session, info agentapi.ProcessInfo, req agentapi.StartRequest) {
	args, argsTruncated := recordedArgs(req.Args)
	base := map[string]any{"pid": info.ID, "cmd": req.Cmd, "args": args, "pty": info.PTY}
	if argsTruncated {
		base["args_truncated"] = true
	}
	if req.Tag != "" {
		base["tag"] = req.Tag
	}
	started := map[string]any{"cwd": req.Cwd, "timeout_ms": req.TimeoutMs, "stdin": req.Stdin}
	for k, v := range base {
		started[k] = v
	}
	s.appendEvent(store.Event{SandboxID: sess.id, TenantID: sess.tenantID, Type: EventCommandStarted, Data: started})

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var (
			stdout, stderr, dropped int64
			exit                    *agentapi.ExitStatus
			streamErr               error
		)
		for ev, err := range sess.client.Stream(sess.ctx, info.ID, 0) {
			if err != nil {
				streamErr = err
				break
			}
			switch ev.Type {
			case agentapi.EventStdout, agentapi.EventPTY:
				stdout += int64(len(ev.Data))
			case agentapi.EventStderr:
				stderr += int64(len(ev.Data))
			case agentapi.EventTruncated:
				dropped += ev.Dropped
			case agentapi.EventExit:
				exit = ev.Exit
			}
		}
		data := map[string]any{
			"duration_ms":  time.Since(info.StartedAt).Milliseconds(),
			"stdout_bytes": stdout, "stderr_bytes": stderr,
		}
		for k, v := range base {
			data[k] = v
		}
		if dropped > 0 {
			// Output the recorder was too slow to count by stream.
			data["uncounted_bytes"] = dropped
		}
		switch {
		case exit != nil:
			data["exit_code"] = exit.ExitCode
			data["reason"] = string(exit.Reason)
			if exit.Signal != "" {
				data["signal"] = exit.Signal
			}
		case s.baseCtx.Err() != nil:
			return // the server is stopping; the command may still be running
		default:
			// The stream ended without an exit event: the sandbox went
			// away under the command.
			data["reason"] = "sandbox_terminated"
			if sess.ctx.Err() == nil && streamErr != nil {
				_ = s.terminated(sess) // fail the sandbox now if its instance died
			}
			// Whoever noticed first (this watcher, a request, the sweeper),
			// the row says why the sandbox ended.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseCtx), 5*time.Second)
			acc, err := s.store.GetSandboxAccess(ctx, sess.id)
			cancel()
			if err == nil && acc.Sandbox.Generation == sess.gen && acc.Sandbox.State != core.StateRunning && acc.Sandbox.Reason != "" {
				data["sandbox_reason"] = string(acc.Sandbox.Reason)
			}
		}
		s.appendEvent(store.Event{SandboxID: sess.id, TenantID: sess.tenantID, Type: EventCommandFinished, Data: data})
	}()
}

func (s *Server) appendEvent(e store.Event) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseCtx), 5*time.Second)
	defer cancel()
	if err := s.store.AppendEvent(ctx, e); err != nil {
		s.log.Warn("could not record event", "sandbox", e.SandboxID, "type", e.Type, "err", err)
	}
}

// janitor drops the sessions of sandboxes that are no longer running
// (idle timeout, max lifetime, lost instance), which also ends their
// command watchers.
func (s *Server) janitor() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.JanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-s.baseCtx.Done():
			return
		case <-t.C:
		}
		for _, sess := range s.sessions.snapshot() {
			ctx, cancel := context.WithTimeout(s.baseCtx, 5*time.Second)
			acc, err := s.store.GetSandboxAccess(ctx, sess.id)
			cancel()
			gone := errors.Is(err, store.ErrNotFound) ||
				(err == nil && (acc.Sandbox.Generation != sess.gen || acc.Sandbox.State != core.StateRunning))
			if gone {
				s.sessions.drop(sess.id)
			}
		}
	}
}

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
)

// Agent is one in-sandbox agent instance.
type Agent struct {
	cfg    Config
	reaper *reaper
	procs  *manager

	ctx    context.Context // cancelled when the agent stops
	cancel context.CancelFunc

	initMu     sync.Mutex
	inited     bool
	initReq    agentapi.InitRequest
	egressLn   net.Listener
	egressAddr string
}

// New creates an agent. Call Serve to run it.
func New(cfg Config) *Agent {
	cfg = cfg.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	r := newReaper(cfg.ReapAll)
	return &Agent{
		cfg:    cfg,
		reaper: r,
		procs:  newManager(cfg, r),
		ctx:    ctx,
		cancel: cancel,
	}
}

// apiError is an error with an HTTP status and a stable code.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func writeError(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = &apiError{http.StatusInternalServerError, "internal", err.Error()}
	}
	writeJSON(w, ae.status, agentapi.Error{Code: ae.code, Message: ae.msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func badRequest(format string, args ...any) error {
	return &apiError{http.StatusBadRequest, "bad_request", fmt.Sprintf(format, args...)}
}

// maxJSONBody bounds JSON request bodies (not stdin or file bodies).
const maxJSONBody = 4 << 20

func readJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil {
		return badRequest("reading body: %v", err)
	}
	if len(b) > maxJSONBody {
		return &apiError{http.StatusRequestEntityTooLarge, "too_large", "JSON body exceeds 4 MiB"}
	}
	if err := json.Unmarshal(b, v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

// Handler returns the agent's HTTP handler (the agentapi routes).
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	v := "/" + agentapi.Version
	mux.HandleFunc("GET "+v+"/health", a.handleHealth)
	mux.HandleFunc("POST "+v+"/init", a.handleInit)
	mux.HandleFunc("POST "+v+"/processes", a.handleStart)
	mux.HandleFunc("GET "+v+"/processes", a.handleList)
	mux.HandleFunc("GET "+v+"/processes/{id}", a.withProcess(a.handleGet))
	mux.HandleFunc("GET "+v+"/processes/{id}/stream", a.withProcess(a.handleStream))
	mux.HandleFunc("POST "+v+"/processes/{id}/stdin", a.withProcess(a.handleStdin))
	mux.HandleFunc("POST "+v+"/processes/{id}/close-stdin", a.withProcess(a.handleCloseStdin))
	mux.HandleFunc("POST "+v+"/processes/{id}/signal", a.withProcess(a.handleSignal))
	mux.HandleFunc("POST "+v+"/processes/{id}/resize", a.withProcess(a.handleResize))
	mux.HandleFunc("GET "+v+"/processes/{id}/ws", a.withProcess(a.handleWS))
	mux.HandleFunc("GET "+v+"/files", a.handleFileRead)
	mux.HandleFunc("PUT "+v+"/files", a.handleFileWrite)
	mux.HandleFunc("DELETE "+v+"/files", a.handleFileDelete)
	mux.HandleFunc("GET "+v+"/files/list", a.handleFileList)
	mux.HandleFunc("POST "+v+"/files/mkdir", a.handleMkdir)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, &apiError{http.StatusNotFound, "no_route", "no such route: " + r.Method + " " + r.URL.Path})
	})
	return mux
}

// Listen removes a stale socket, listens on cfg.SocketPath and applies
// the socket mode.
func (a *Agent) Listen() (net.Listener, error) {
	path := a.cfg.SocketPath
	if path == "" {
		return nil, errors.New("agent: SocketPath is required")
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("agent: %s exists and is not a socket", path)
		}
		// Stale unless someone still answers on it.
		if c, err := net.Dial("unix", path); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("agent: another agent is listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("agent: removing stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	if err := os.Chmod(path, a.cfg.SocketMode.Perm()); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("agent: %w", err)
	}
	return ln, nil
}

// Serve listens on the configured socket and serves until ctx is done,
// then kills every process it started and removes the socket.
func (a *Agent) Serve(ctx context.Context) error {
	ln, err := a.Listen()
	if err != nil {
		return err
	}
	return a.ServeListener(ctx, ln)
}

// ServeListener is Serve on an existing listener, which it closes.
func (a *Agent) ServeListener(ctx context.Context, ln net.Listener) error {
	defer a.cancel()

	reaperCtx, stopReaper := context.WithCancel(context.Background())
	reaperDone := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		defer close(reaperDone)
		a.reaper.run(reaperCtx, ready)
	}()
	<-ready // no child is started before SIGCHLD is being watched

	srv := &http.Server{
		Handler:     a.Handler(),
		BaseContext: func(net.Listener) context.Context { return a.ctx },
		ErrorLog:    a.cfg.Logger,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	var err error

	select {
	case err = <-errc:
	case <-ctx.Done():
	case <-a.ctx.Done():
	}
	a.cancel() // ends WebSocket sessions, which srv.Close does not reach
	_ = srv.Close()
	a.initMu.Lock()
	if a.egressLn != nil {
		_ = a.egressLn.Close()
	}
	a.initMu.Unlock()
	a.procs.killAll()
	stopReaper()
	<-reaperDone
	if ul, ok := ln.(*net.UnixListener); ok {
		_ = os.Remove(ul.Addr().String())
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// withProcess resolves the {id} path value.
func (a *Agent) withProcess(h func(http.ResponseWriter, *http.Request, *Process)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := a.procs.get(r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		h(w, r, p)
	}
}

func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	a.initMu.Lock()
	inited := a.inited
	a.initMu.Unlock()
	writeJSON(w, http.StatusOK, agentapi.Health{Ready: true, AgentVersion: AgentVersion, Initialized: inited})
}

// handleInit applies the session configuration once. Repeating the call
// with the same sandbox id and generation is a no-op success, so that a
// host that lost the response can retry; any other second call is
// rejected with 409.
func (a *Agent) handleInit(w http.ResponseWriter, r *http.Request) {
	var req agentapi.InitRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	a.initMu.Lock()
	defer a.initMu.Unlock()
	if a.inited {
		if req.Generation == a.initReq.Generation && req.SandboxID == a.initReq.SandboxID {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, &apiError{http.StatusConflict, "already_initialized", fmt.Sprintf(
			"already initialized as sandbox %q generation %d", a.initReq.SandboxID, a.initReq.Generation)})
		return
	}
	var env []string
	if req.EgressProxy {
		if a.egressLn == nil {
			ln, err := startEgressBridge(a.ctx, a.cfg.EgressListenAddr, a.cfg.EgressSocket, a.cfg.Logger)
			if err != nil {
				writeError(w, &apiError{http.StatusInternalServerError, "egress_failed", err.Error()})
				return
			}
			a.egressLn = ln
			a.egressAddr = ln.Addr().String()
		}
		env = proxyEnv(a.egressAddr)
	}
	// Explicit session variables win over the proxy defaults.
	a.procs.setSessionEnv(mergeEnv(env, envList(req.Env)))
	a.initReq = req
	a.inited = true
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleStart(w http.ResponseWriter, r *http.Request) {
	var req agentapi.StartRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	p, err := a.procs.start(req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p.info())
}

func (a *Agent) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.procs.list())
}

func (a *Agent) handleGet(w http.ResponseWriter, r *http.Request, p *Process) {
	writeJSON(w, http.StatusOK, p.info())
}

// handleStdin streams the request body to the process's stdin.
func (a *Agent) handleStdin(w http.ResponseWriter, r *http.Request, p *Process) {
	buf := make([]byte, agentapi.MaxFrameBytes)
	for {
		n, rerr := r.Body.Read(buf)
		if n > 0 {
			if err := p.writeStdin(r.Context(), buf[:n]); err != nil {
				writeError(w, err)
				return
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			writeError(w, badRequest("reading body: %v", rerr))
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleCloseStdin(w http.ResponseWriter, r *http.Request, p *Process) {
	if err := p.closeStdin(); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleSignal(w http.ResponseWriter, r *http.Request, p *Process) {
	var req agentapi.SignalRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	sig, err := parseSignal(req.Signal)
	if err == nil {
		err = p.signal(sig)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleResize(w http.ResponseWriter, r *http.Request, p *Process) {
	var req agentapi.ResizeRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := p.resize(req.Cols, req.Rows); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseFrom reads the from query parameter (default 0).
func parseFrom(r *http.Request) (int64, error) {
	s := r.URL.Query().Get("from")
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, badRequest("from must be a non-negative integer")
	}
	return n, nil
}

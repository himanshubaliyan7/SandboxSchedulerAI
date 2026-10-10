// Package api is the public REST API of the sandbox scheduler: the
// control plane (sandboxes, templates) and the data plane (commands,
// PTY WebSockets, files), served under /v1 with the standard library's
// net/http routing. api/openapi.yaml describes it.
//
// The API layer owns authentication (tenant API keys and per-sandbox
// access tokens), tenant isolation, rate limiting, error mapping, the
// activity timestamp behind the idle timer and the execution record of
// every command. It never trusts a generation from a client: the
// generation used to fence pool and store calls always comes from the
// sandbox row it has just read.
package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/agentclient"
	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/pool"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// Store is the part of the state store the API needs. *store.Store
// satisfies it.
type Store interface {
	AuthenticateAPIKey(ctx context.Context, plaintext string) (store.APIKey, store.Tenant, error)
	GetTenant(ctx context.Context, id string) (store.Tenant, error)
	GetTemplate(ctx context.Context, id string) (core.Template, error)
	ListTemplates(ctx context.Context) ([]core.Template, error)
	GetSandboxAccess(ctx context.Context, id string) (store.SandboxAccess, error)
	ListSandboxAccess(ctx context.Context, tenantID string, f store.SandboxFilter) ([]store.SandboxAccess, error)
	SetMetadata(ctx context.Context, id string, generation int64, metadata map[string]string) error
	SetLifetime(ctx context.Context, id string, generation int64, fromNow, maxTotal time.Duration) (core.Sandbox, error)
	ListEvents(ctx context.Context, sandboxID string) ([]store.Event, error)
	AppendEvent(ctx context.Context, e store.Event) error
}

var _ Store = (*store.Store)(nil)

// Pool is the part of the pool controller the API needs.
// *pool.Controller satisfies it.
type Pool interface {
	Claim(ctx context.Context, tenantID, templateID string, opts pool.ClaimOptions) (core.Sandbox, error)
	Release(ctx context.Context, sandboxID string, generation int64, reason core.TerminationReason) error
	ReleaseAsync(ctx context.Context, sandboxID string, generation int64, reason core.TerminationReason) error
	Touch(ctx context.Context, sandboxID string, generation int64) error
	UpdateNetwork(ctx context.Context, sandboxID string, generation int64, policy core.NetworkPolicy) error
	CheckInstance(ctx context.Context, sandboxID string, generation int64) (lost bool, err error)
	RunDir(sandboxID string) string
	Metrics() pool.Metrics
}

var _ Pool = (*pool.Controller)(nil)

// StatsSource reads the resource counters of a backend instance.
// runtime.Backend satisfies it.
type StatsSource interface {
	Stats(ctx context.Context, instanceID string) (runtime.Stats, error)
}

// Defaults for Config fields left zero.
const (
	DefaultMaxJSONBytes       = 1 << 20
	DefaultMaxUploadBytes     = 256 << 20
	DefaultMaxWaitOutputBytes = 1 << 20
	DefaultRequestTimeout     = 30 * time.Second
	DefaultTouchInterval      = 5 * time.Second
	DefaultAuthCacheTTL       = 5 * time.Second
	DefaultWSPingInterval     = 25 * time.Second
	DefaultJanitorInterval    = 15 * time.Second
)

// Config wires a Server. Store and Pool are required.
type Config struct {
	Store Store
	Pool  Pool
	// Stats serves GET /v1/sandboxes/{id}/stats; nil disables the route's
	// backend read (it then answers 501).
	Stats StatsSource
	// Agents returns the agent client of the sandbox whose host run dir
	// is given. Default agentclient.ForRunDir.
	Agents func(hostRunDir string) *agentclient.Client
	// ValidatePolicy rejects a network policy the egress proxy would not
	// accept (egress.ValidatePolicy). Nil accepts everything the pool does.
	ValidatePolicy func(core.NetworkPolicy) error
	// RuntimeInfo describes the isolation tier a template's runtime name
	// stands for on this server (runtime.Capabilities.Runtimes); ok is
	// false for a runtime the server has not enabled. It feeds
	// shared_kernel in GET /v1/templates. Nil leaves the field out.
	RuntimeInfo func(name string) (info runtime.RuntimeInfo, ok bool)
	// Ready is the readiness probe behind GET /readyz. Nil means ready.
	Ready func(ctx context.Context) error
	// ExtraMetrics is called at the end of GET /metrics to append
	// metrics owned by other components, in Prometheus text format.
	ExtraMetrics func(w io.Writer)
	// Webhooks serves the /v1/webhooks routes (webhooks.go). Nil leaves
	// the routes in place, answering 501: the server runs without webhooks.
	Webhooks Webhooks
	// MetricsToken, when not empty, is required as a bearer token
	// ("Authorization: Bearer <token>") by GET /metrics, on the main
	// handler and on MetricsHandler alike. Empty means no authentication.
	MetricsToken string
	// NoMetricsRoute leaves GET /metrics out of Handler; the caller then
	// serves MetricsHandler on a listener of its own.
	NoMetricsRoute bool
	Logger         *slog.Logger

	// MaxJSONBytes bounds JSON request bodies. Default 1 MiB.
	MaxJSONBytes int64
	// MaxUploadBytes bounds a file upload and a stdin write. Default 256 MiB.
	MaxUploadBytes int64
	// MaxWaitOutputBytes bounds each of stdout and stderr returned by
	// POST .../commands?wait=true. Default 1 MiB.
	MaxWaitOutputBytes int
	// RequestTimeout bounds handlers that are not streams (and the wait
	// for the agent's first answer on those that are). Default 30s.
	RequestTimeout time.Duration
	// ClaimWait is how long POST /v1/sandboxes waits on an empty pool.
	// Zero means the pool controller's default.
	ClaimWait time.Duration
	// TouchInterval debounces the activity timestamp: at most one store
	// write per sandbox per interval. Default 5s. It must be well below
	// the shortest idle timeout of any template.
	TouchInterval time.Duration
	// AuthCacheTTL is how long a successful API-key lookup and a tenant
	// row are cached; it bounds how long a revoked key keeps working and
	// how stale a rate limit can be. Default 5s; negative disables.
	AuthCacheTTL time.Duration
	// CreateRatePerSec and CreateBurst, when positive, replace the
	// tenant row's rate and burst for the sandbox-creation bucket.
	CreateRatePerSec float64
	CreateBurst      int
	// WSPingInterval is how often the gateway pings a WebSocket client.
	// Default 25s.
	WSPingInterval time.Duration
	// JanitorInterval is how often sessions of sandboxes that are no
	// longer running are dropped. Default 15s.
	JanitorInterval time.Duration

	// CORSAllowedOrigins switches cross-origin browser access to the
	// data-plane routes on. Each entry is an exact origin
	// ("https://app.example.com", no path, no trailing slash) or "*" for
	// any origin. Empty (the default) means off: no CORS header is ever
	// sent and OPTIONS is not a method of any route. See cors.go.
	CORSAllowedOrigins []string
	// CORSAllowAPIKey lets a cross-origin request authenticate with a
	// tenant API key on the data-plane routes. Off by default: an API key
	// has no business in a browser, and with it off a key that reaches
	// one by mistake does not work from there.
	CORSAllowAPIKey bool
}

func (c *Config) defaults() error {
	switch {
	case c.Store == nil:
		return errors.New("api: Config.Store is required")
	case c.Pool == nil:
		return errors.New("api: Config.Pool is required")
	}
	if c.Agents == nil {
		c.Agents = agentclient.ForRunDir
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.MaxJSONBytes <= 0 {
		c.MaxJSONBytes = DefaultMaxJSONBytes
	}
	if c.MaxUploadBytes <= 0 {
		c.MaxUploadBytes = DefaultMaxUploadBytes
	}
	if c.MaxWaitOutputBytes <= 0 {
		c.MaxWaitOutputBytes = DefaultMaxWaitOutputBytes
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.TouchInterval <= 0 {
		c.TouchInterval = DefaultTouchInterval
	}
	if c.AuthCacheTTL == 0 {
		c.AuthCacheTTL = DefaultAuthCacheTTL
	}
	if c.WSPingInterval <= 0 {
		c.WSPingInterval = DefaultWSPingInterval
	}
	if c.JanitorInterval <= 0 {
		c.JanitorInterval = DefaultJanitorInterval
	}
	return nil
}

// Server is the API. Create it with New, serve Handler, and call Close on
// shutdown.
type Server struct {
	cfg   Config
	log   *slog.Logger
	store Store
	pool  Pool
	mux   *http.ServeMux
	m     *metrics

	baseCtx context.Context // cancelled by Close: ends streams and watchers
	cancel  context.CancelFunc
	wg      sync.WaitGroup // watchers and the janitor

	auth     *authCache
	limiters *limiters
	sessions *sessions
	cors     *corsPolicy

	routeList   []string            // "METHOD /path" of every registered route
	dataMethods map[string][]string // data-plane path -> methods, for preflights
}

// New validates cfg and returns a Server.
func New(cfg Config) (*Server, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	cors, err := newCORSPolicy(cfg.CORSAllowedOrigins, cfg.CORSAllowAPIKey)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg: cfg, log: cfg.Logger.With("component", "api"), store: cfg.Store, pool: cfg.Pool,
		mux: http.NewServeMux(), m: newMetrics(),
		baseCtx: ctx, cancel: cancel,
		auth:     newAuthCache(cfg.AuthCacheTTL),
		limiters: newLimiters(cfg.CreateRatePerSec, cfg.CreateBurst),
		sessions: newSessions(),
		cors:     cors, dataMethods: map[string][]string{},
	}
	s.routes()
	s.wg.Add(1)
	go s.janitor()
	return s, nil
}

// Close ends every open stream and WebSocket, stops the command watchers
// and waits for them. Call it after the HTTP server has stopped
// accepting. It is idempotent.
func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
	s.sessions.closeAll()
}

func (s *Server) routes() {
	// Control plane: tenant API key only.
	s.tenant("POST /v1/sandboxes", classCreate, s.handleCreateSandbox)
	s.tenant("GET /v1/sandboxes", classDefault, s.handleListSandboxes)
	s.tenant("GET /v1/sandboxes/{id}", classDefault, s.handleGetSandbox)
	s.tenant("DELETE /v1/sandboxes/{id}", classDefault, s.handleDeleteSandbox)
	s.tenant("POST /v1/sandboxes/{id}/timeout", classDefault, s.handleSetTimeout)
	s.tenant("PUT /v1/sandboxes/{id}/network", classDefault, s.handleSetNetwork)
	s.tenant("GET /v1/sandboxes/{id}/events", classDefault, s.handleEvents)
	s.tenant("GET /v1/sandboxes/{id}/stats", classDefault, s.handleStats)
	s.tenant("GET /v1/templates", classDefault, s.handleListTemplates)
	s.webhookRoutes()

	// Data plane: tenant API key or the sandbox's access token.
	s.data("POST /v1/sandboxes/{id}/commands", s.handleStartCommand)
	s.data("GET /v1/sandboxes/{id}/commands", s.handleListCommands)
	s.data("GET /v1/sandboxes/{id}/commands/{pid}", s.handleGetCommand)
	s.data("GET /v1/sandboxes/{id}/commands/{pid}/stream", s.handleStreamCommand)
	s.data("POST /v1/sandboxes/{id}/commands/{pid}/stdin", s.handleStdin)
	s.data("POST /v1/sandboxes/{id}/commands/{pid}/close-stdin", s.handleCloseStdin)
	s.data("POST /v1/sandboxes/{id}/commands/{pid}/signal", s.handleSignal)
	s.data("POST /v1/sandboxes/{id}/commands/{pid}/resize", s.handleResize)
	s.dataWS("GET /v1/sandboxes/{id}/commands/{pid}/ws", s.handleWS)
	s.data("GET /v1/sandboxes/{id}/files", s.handleFileRead)
	s.data("PUT /v1/sandboxes/{id}/files", s.handleFileWrite)
	s.data("DELETE /v1/sandboxes/{id}/files", s.handleFileDelete)
	s.data("GET /v1/sandboxes/{id}/files/list", s.handleFileList)
	s.data("GET /v1/sandboxes/{id}/files/stat", s.handleFileStat)
	s.data("POST /v1/sandboxes/{id}/files/mkdir", s.handleMkdir)
	s.preflights()

	// Unauthenticated operational endpoints.
	s.handle("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.handle("GET /readyz", s.handleReady)
	if !s.cfg.NoMetricsRoute {
		s.handle("GET /metrics", s.serveMetrics)
	}
}

// handle registers a route and remembers it for Routes.
func (s *Server) handle(pattern string, h http.HandlerFunc) {
	s.routeList = append(s.routeList, pattern)
	s.mux.HandleFunc(pattern, h)
}

// Routes lists every route the server serves, as "METHOD /path" with the
// path parameters in braces, in registration order. The CORS preflight
// handlers (OPTIONS on the data-plane paths, present only when CORS is
// configured) are not in the list. api/openapi.yaml must describe exactly
// these routes; a test compares the two.
func (s *Server) Routes() []string {
	return append([]string(nil), s.routeList...)
}

// MetricsHandler serves the Prometheus metrics at GET /metrics and
// nothing else, for a listener separate from the API's. It applies the
// same MetricsToken check as the route on the main handler.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.serveMetrics)
	return mux
}

// serveMetrics is handleMetrics behind the optional bearer token.
func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if want := s.cfg.MetricsToken; want != "" {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			writeError(w, requestID(r), errf(http.StatusUnauthorized, CodeUnauthorized, "a metrics bearer token is required"))
			return
		}
	}
	s.handleMetrics(w, r)
}

type ctxKey int

const requestIDKey ctxKey = iota

func requestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey).(string)
	return id
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// validRequestID accepts a caller-supplied id only if it is short and
// cannot be used to forge log lines.
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// Handler returns the root handler: request ids, panic recovery, access
// log, request metrics and JSON 404/405 answers around the router.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, id))
		sw := &statusWriter{ResponseWriter: w}

		_, pattern := s.mux.Handler(r)
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				s.log.Error("panic in handler", "request_id", id, "panic", p, "stack", string(debug.Stack()))
				if !sw.wrote {
					writeError(sw, id, errf(http.StatusInternalServerError, CodeInternal, "internal error"))
				}
			}
			route := pattern
			if route == "" {
				route = "unmatched"
			}
			took := time.Since(start)
			s.m.observeRequest(route, sw.status(), took)
			lvl := slog.LevelInfo
			if route == "GET /healthz" || route == "GET /readyz" || route == "GET /metrics" {
				lvl = slog.LevelDebug
			}
			s.log.Log(r.Context(), lvl, "request", "request_id", id, "method", r.Method, "path", r.URL.Path,
				"route", route, "status", sw.status(), "took", took, "bytes", sw.bytes)
		}()

		if pattern == "" {
			// Let the mux decide between 404 and 405, but answer in the
			// API's own envelope.
			probe := &probeWriter{header: http.Header{}}
			s.mux.ServeHTTP(probe, r)
			if probe.code == http.StatusMethodNotAllowed {
				if allow := probe.header.Get("Allow"); allow != "" {
					sw.Header().Set("Allow", allow)
				}
				writeError(sw, id, errf(http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method %s is not allowed on %s", r.Method, r.URL.Path))
				return
			}
			writeError(sw, id, errf(http.StatusNotFound, CodeNotFound, "no such route: %s %s", r.Method, r.URL.Path))
			return
		}
		s.mux.ServeHTTP(sw, r)
	})
}

// fail writes err as the error envelope and logs server-side failures.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return // the client went away; nobody is listening
	}
	e := toError(err)
	if e.Status >= 500 {
		s.log.Warn("request failed", "request_id", requestID(r), "path", r.URL.Path, "status", e.Status, "code", e.Code, "err", err)
	}
	writeError(w, requestID(r), e)
}

// statusWriter records the status and size of a response. It keeps
// Flush, Hijack and deadlines working through Unwrap.
type statusWriter struct {
	http.ResponseWriter
	code  int
	bytes int64
	wrote bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.code, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.code, w.wrote = http.StatusOK, true
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack is what the WebSocket upgrade uses.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("api: connection cannot be hijacked")
	}
	if !w.wrote {
		w.code, w.wrote = http.StatusSwitchingProtocols, true
	}
	return h.Hijack()
}

func (w *statusWriter) status() int {
	if !w.wrote {
		return http.StatusOK
	}
	return w.code
}

// probeWriter captures what the mux would answer for an unmatched request.
type probeWriter struct {
	header http.Header
	code   int
}

func (p *probeWriter) Header() http.Header { return p.header }
func (p *probeWriter) WriteHeader(code int) {
	if p.code == 0 {
		p.code = code
	}
}
func (p *probeWriter) Write(b []byte) (int, error) {
	if p.code == 0 {
		p.code = http.StatusOK
	}
	return len(b), nil
}

// streamCtx is the context of a long-lived handler: it ends when the
// client goes away or the server closes.
func (s *Server) streamCtx(r *http.Request) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.baseCtx, cancel)
	return ctx, func() { stop(); cancel() }
}

// opCtx is the context of a bounded handler.
func (s *Server) opCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
}

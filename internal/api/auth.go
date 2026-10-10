package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/store"
)

// Credentials.
//
// A tenant API key ("sbx_<prefix>_<secret>") is sent as "X-API-Key: <key>"
// or "Authorization: Bearer <key>" and is valid on every route.
//
// A per-sandbox access token ("sbt_<64 hex>", 256 random bits, returned
// once by POST /v1/sandboxes and stored as a SHA-256 hash) is valid only
// on the data-plane routes of the sandbox it was issued for. It is sent
// as "X-Access-Token: <token>", as "Authorization: Bearer <token>", or,
// for browser WebSockets that cannot set headers, as the query parameter
// "token" or as the subprotocol entry "sbx.token.<token>" next to
// WSSubprotocol.
const (
	AccessTokenPrefix = "sbt_"
	// WSSubprotocol is the subprotocol the gateway selects on a WebSocket.
	WSSubprotocol = "sbx.v1"
	// wsTokenProtocolPrefix marks the subprotocol entry carrying a token.
	wsTokenProtocolPrefix = "sbx.token."
)

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// credentials extracts the API key and the access token of a request.
// Either may be empty.
func credentials(r *http.Request) (apiKey, accessToken string) {
	apiKey = strings.TrimSpace(r.Header.Get("X-API-Key"))
	accessToken = strings.TrimSpace(r.Header.Get("X-Access-Token"))
	if auth := r.Header.Get("Authorization"); auth != "" {
		if scheme, value, ok := strings.Cut(auth, " "); ok && strings.EqualFold(scheme, "Bearer") {
			value = strings.TrimSpace(value)
			switch {
			case strings.HasPrefix(value, AccessTokenPrefix):
				if accessToken == "" {
					accessToken = value
				}
			case apiKey == "":
				apiKey = value
			}
		}
	}
	if accessToken == "" {
		accessToken = r.URL.Query().Get("token")
	}
	if accessToken == "" {
		for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
			for _, p := range strings.Split(h, ",") {
				if tok, ok := strings.CutPrefix(strings.TrimSpace(p), wsTokenProtocolPrefix); ok && tok != "" {
					accessToken = tok
				}
			}
		}
	}
	return apiKey, accessToken
}

// authCache remembers successful API-key lookups and tenant rows for a
// short time, so that the hot path costs one map lookup instead of two
// queries. Failures are never cached.
type authCache struct {
	ttl time.Duration

	mu      sync.Mutex
	keys    map[[sha256.Size]byte]cachedTenant
	tenants map[string]cachedTenant
}

type cachedTenant struct {
	tenant  store.Tenant
	expires time.Time
}

func newAuthCache(ttl time.Duration) *authCache {
	return &authCache{ttl: ttl, keys: map[[sha256.Size]byte]cachedTenant{}, tenants: map[string]cachedTenant{}}
}

const authCacheMax = 10_000

// byKey resolves an API key to its tenant.
func (c *authCache) byKey(ctx context.Context, st Store, key string) (store.Tenant, error) {
	sum := sha256.Sum256([]byte(key))
	now := time.Now()
	if c.ttl > 0 {
		c.mu.Lock()
		e, ok := c.keys[sum]
		c.mu.Unlock()
		if ok && now.Before(e.expires) {
			return e.tenant, nil
		}
	}
	_, tenant, err := st.AuthenticateAPIKey(ctx, key)
	if err != nil {
		return store.Tenant{}, err
	}
	if c.ttl > 0 {
		c.mu.Lock()
		if len(c.keys) >= authCacheMax {
			clear(c.keys)
		}
		c.keys[sum] = cachedTenant{tenant, now.Add(c.ttl)}
		c.mu.Unlock()
	}
	return tenant, nil
}

// byID returns a tenant row (for requests authenticated by a sandbox token).
func (c *authCache) byID(ctx context.Context, st Store, id string) (store.Tenant, error) {
	now := time.Now()
	if c.ttl > 0 {
		c.mu.Lock()
		e, ok := c.tenants[id]
		c.mu.Unlock()
		if ok && now.Before(e.expires) {
			return e.tenant, nil
		}
	}
	tenant, err := st.GetTenant(ctx, id)
	if err != nil {
		return store.Tenant{}, err
	}
	if c.ttl > 0 {
		c.mu.Lock()
		if len(c.tenants) >= authCacheMax {
			clear(c.tenants)
		}
		c.tenants[id] = cachedTenant{tenant, now.Add(c.ttl)}
		c.mu.Unlock()
	}
	return tenant, nil
}

// Rate-limit classes. Each tenant has one token bucket per class, so a
// burst of data-plane calls cannot starve sandbox creation or the reverse.
type rateClass string

const (
	classCreate  rateClass = "create"
	classDefault rateClass = "default"
)

type limiterKey struct {
	tenant string
	class  rateClass
}

type limiters struct {
	createRate  float64
	createBurst int

	mu sync.Mutex
	m  map[limiterKey]*rate.Limiter
}

func newLimiters(createRate float64, createBurst int) *limiters {
	return &limiters{createRate: createRate, createBurst: createBurst, m: map[limiterKey]*rate.Limiter{}}
}

// allow takes one token from the tenant's bucket for class. The bucket is
// sized from the tenant row (rate_per_sec, burst) and follows it when the
// row changes. A tenant whose rate is zero or negative is not limited. It
// returns how long to wait when the bucket is empty.
func (l *limiters) allow(t store.Tenant, class rateClass) (retryAfter time.Duration, ok bool) {
	perSec, burst := t.RatePerSec, t.Burst
	if class == classCreate {
		if l.createRate > 0 {
			perSec = l.createRate
		}
		if l.createBurst > 0 {
			burst = l.createBurst
		}
	}
	if perSec <= 0 {
		return 0, true
	}
	if burst < 1 {
		burst = 1
	}
	key := limiterKey{t.ID, class}
	l.mu.Lock()
	lim := l.m[key]
	if lim == nil {
		lim = rate.NewLimiter(rate.Limit(perSec), burst)
		l.m[key] = lim
	} else if lim.Limit() != rate.Limit(perSec) || lim.Burst() != burst {
		lim.SetLimit(rate.Limit(perSec))
		lim.SetBurst(burst)
	}
	l.mu.Unlock()

	res := lim.Reserve()
	if !res.OK() {
		return time.Second, false
	}
	if d := res.Delay(); d > 0 {
		res.Cancel()
		return d, false
	}
	return 0, true
}

func (s *Server) rateLimit(t store.Tenant, class rateClass) error {
	retry, ok := s.limiters.allow(t, class)
	if ok {
		return nil
	}
	s.m.rateLimited(class)
	return &Error{Status: http.StatusTooManyRequests, Code: CodeRateLimited,
		Message: "rate limit exceeded for this tenant", RetryAfter: retry,
		Details: map[string]any{"class": string(class)}}
}

// tenantHandler is a control-plane handler: the caller is a tenant.
type tenantHandler func(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error

// tenant registers a route that requires a tenant API key.
func (s *Server) tenant(pattern string, class rateClass, h tenantHandler) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		key, _ := credentials(r)
		if key == "" {
			s.fail(w, r, errUnauthorized)
			return
		}
		tenant, err := s.auth.byKey(r.Context(), s.store, key)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				err = errUnauthorized
			}
			s.fail(w, r, err)
			return
		}
		if err := s.rateLimit(tenant, class); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := h(w, r, tenant); err != nil {
			s.fail(w, r, err)
		}
	})
}

// dataHandler is a data-plane handler: the sandbox has been resolved,
// belongs to the caller, is RUNNING, and its activity has been recorded.
type dataHandler func(w http.ResponseWriter, r *http.Request, sess *session) error

// data registers a data-plane route under /v1/sandboxes/{id}. It accepts
// a tenant API key or the access token of that one sandbox.
func (s *Server) data(pattern string, h dataHandler) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.authorizeData(r)
		if err == nil {
			err = h(w, r, sess)
		}
		if err != nil {
			s.fail(w, r, err)
		}
	})
}

func (s *Server) authorizeData(r *http.Request) (*session, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	key, token := credentials(r)
	var (
		acc    store.SandboxAccess
		tenant store.Tenant
		err    error
	)
	switch {
	case key != "":
		if tenant, err = s.auth.byKey(ctx, s.store, key); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, errUnauthorized
			}
			return nil, err
		}
		if acc, err = s.ownedSandbox(ctx, tenant.ID, id); err != nil {
			return nil, err
		}
	case token != "":
		// An unknown sandbox and a wrong token are indistinguishable, so
		// a token holder cannot probe for sandbox ids.
		acc, err = s.store.GetSandboxAccess(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, errUnauthorized
		}
		if err != nil {
			return nil, err
		}
		if len(acc.AccessTokenHash) == 0 || acc.Sandbox.TenantID == "" ||
			subtle.ConstantTimeCompare(acc.AccessTokenHash, hashToken(token)) != 1 {
			return nil, errUnauthorized
		}
		if tenant, err = s.auth.byID(ctx, s.store, acc.Sandbox.TenantID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, errUnauthorized
			}
			return nil, err
		}
	default:
		return nil, errUnauthorized
	}
	if err := s.rateLimit(tenant, classDefault); err != nil {
		return nil, err
	}
	if acc.Sandbox.State != core.StateRunning {
		return nil, notRunning(acc.Sandbox)
	}
	sess := s.sessions.get(acc.Sandbox, func() *session { return s.newSession(acc.Sandbox) })
	s.touch(sess)
	return sess, nil
}

// ownedSandbox returns a sandbox only if it is, or was, assigned to the
// tenant. Every other case, including a sandbox of another tenant and a
// warm sandbox nobody owns, is the same 404.
func (s *Server) ownedSandbox(ctx context.Context, tenantID, id string) (store.SandboxAccess, error) {
	acc, err := s.store.GetSandboxAccess(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.SandboxAccess{}, errSandboxNotFound(id)
	}
	if err != nil {
		return store.SandboxAccess{}, err
	}
	if acc.Sandbox.TenantID == "" || acc.Sandbox.TenantID != tenantID {
		return store.SandboxAccess{}, errSandboxNotFound(id)
	}
	return acc, nil
}

func notRunning(sb core.Sandbox) *Error {
	e := errf(http.StatusConflict, CodeSandboxNotRunning, "sandbox %s is %s", sb.ID, publicState(sb.State))
	if sb.State == core.StatePaused {
		// Reported as "running" (publicState), which would read oddly here.
		e.Message = "sandbox " + sb.ID + " is suspended"
	}
	e.Details = map[string]any{"state": publicState(sb.State)}
	if sb.Reason != "" {
		e.Details["reason"] = string(sb.Reason)
		e.Message += " (" + string(sb.Reason) + ")"
	}
	return e
}

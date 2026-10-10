package api

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Cross-origin access from browsers (CORS).
//
// It is off unless Config.CORSAllowedOrigins is set, and then it covers
// the data-plane routes only: the routes a sandbox access token opens.
// The control plane (create, list, delete, timeout, network, events,
// stats, templates) never answers a preflight and never sends a CORS
// header, whatever is configured, so a browser on another origin cannot
// use it: its credential, the tenant API key, travels in X-API-Key or
// Authorization, both of which make the browser send a preflight first.
//
// With CORS on, for a request that carries an Origin header naming
// another host than the one it was sent to:
//
//   - the origin must be configured (or "*" must be); otherwise the
//     request is refused with 403 origin_not_allowed and no CORS header.
//     An origin is never reflected unless it is configured;
//   - the response carries Access-Control-Allow-Origin (the origin, or
//     "*" when "*" is configured), Vary: Origin and
//     Access-Control-Expose-Headers. Access-Control-Allow-Credentials is
//     never sent: authentication is by token, not by cookie;
//   - a tenant API key is refused with 403 api_key_not_allowed (unless
//     Config.CORSAllowAPIKey), so that the data plane cannot be driven
//     with the tenant's key from a page either. X-API-Key and
//     Authorization are also absent from Access-Control-Allow-Headers.
//
// The same check guards the WebSocket handshake, which browsers do not
// subject to CORS. Requests without an Origin header (every non-browser
// client) and same-origin requests are served exactly as without CORS.

const corsMaxAge = "600"

// corsExposedHeaders are the response headers a script may read: the
// ones the SDKs use (request id, Retry-After) and the ones that carry
// the result of a call (Location of a started command, the replay marker
// of an idempotent start, the challenge of a 401). Content-Length and
// Content-Type are readable without being listed.
var corsExposedHeaders = strings.Join([]string{
	"X-Request-Id", "Retry-After", "Location", headerIdempotentReplayed, "WWW-Authenticate",
}, ", ")

type corsPolicy struct {
	any         bool
	origins     map[string]struct{}
	allowAPIKey bool
}

func newCORSPolicy(origins []string, allowAPIKey bool) (*corsPolicy, error) {
	p := &corsPolicy{origins: map[string]struct{}{}, allowAPIKey: allowAPIKey}
	for _, o := range origins {
		o = strings.TrimSpace(o)
		switch {
		case o == "":
			continue
		case o == "*":
			p.any = true
			continue
		}
		norm, err := normalizeOrigin(o)
		if err != nil {
			return nil, fmt.Errorf("api: CORS origin %q: %w", o, err)
		}
		p.origins[norm] = struct{}{}
	}
	return p, nil
}

// normalizeOrigin checks that s is a serialized origin, scheme://host or
// scheme://host:port, and lower-cases it.
func normalizeOrigin(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("not an origin: %v", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("want http://host[:port] or https://host[:port], or *")
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("an origin has no path, query or user (and no trailing slash)")
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}

func (p *corsPolicy) enabled() bool { return p.any || len(p.origins) > 0 }

func (p *corsPolicy) allowed(origin string) bool {
	if p.any {
		return true
	}
	_, ok := p.origins[strings.ToLower(origin)]
	return ok
}

// sameHost reports whether origin names the host the request was sent
// to, i.e. the request is same-origin as far as the server can tell.
func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, host)
}

// apply enforces the policy on a data-plane request and sets the response
// headers. crossOrigin is true when the request comes from a configured
// origin other than the server's own.
func (p *corsPolicy) apply(w http.ResponseWriter, r *http.Request) (crossOrigin bool, err error) {
	origin := r.Header.Get("Origin")
	if !p.enabled() || origin == "" || sameHost(origin, r.Host) {
		return false, nil
	}
	h := w.Header()
	h.Add("Vary", "Origin")
	if !p.allowed(origin) {
		return false, errf(http.StatusForbidden, CodeOriginNotAllowed, "origin %q is not allowed", origin)
	}
	p.allowOrigin(h, origin)
	h.Set("Access-Control-Expose-Headers", corsExposedHeaders)
	return true, nil
}

func (p *corsPolicy) allowOrigin(h http.Header, origin string) {
	if p.any {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
	}
}

func (p *corsPolicy) allowedHeaders() string {
	hs := []string{"X-Access-Token", "Content-Type", "Accept", "X-Request-Id", "Idempotency-Key"}
	if p.allowAPIKey {
		hs = append(hs, "X-API-Key", "Authorization")
	}
	return strings.Join(hs, ", ")
}

// preflights registers an OPTIONS handler on every data-plane path. It
// does nothing when CORS is off, so OPTIONS then stays a 405.
func (s *Server) preflights() {
	if !s.cors.enabled() {
		return
	}
	for path, methods := range s.dataMethods {
		sort.Strings(methods)
		allow := strings.Join(methods, ", ")
		s.mux.HandleFunc("OPTIONS "+path, func(w http.ResponseWriter, r *http.Request) {
			s.preflight(w, r, methods, allow)
		})
	}
}

// preflight answers a CORS preflight. It needs no credentials (a browser
// sends none with it) and touches nothing: it only says whether the real
// request may be sent.
func (s *Server) preflight(w http.ResponseWriter, r *http.Request, methods []string, allow string) {
	h := w.Header()
	h.Add("Vary", "Origin")
	h.Add("Vary", "Access-Control-Request-Method")
	h.Add("Vary", "Access-Control-Request-Headers")
	origin := r.Header.Get("Origin")
	want := r.Header.Get("Access-Control-Request-Method")
	if origin == "" || want == "" {
		h.Set("Allow", allow+", OPTIONS")
		writeError(w, requestID(r), badRequest("OPTIONS is only served as a CORS preflight (Origin and Access-Control-Request-Method are required)"))
		return
	}
	if !sameHost(origin, r.Host) && !s.cors.allowed(origin) {
		writeError(w, requestID(r), errf(http.StatusForbidden, CodeOriginNotAllowed, "origin %q is not allowed", origin))
		return
	}
	ok := false
	for _, m := range methods {
		ok = ok || m == want
	}
	if !ok {
		h.Set("Allow", allow+", OPTIONS")
		writeError(w, requestID(r), errf(http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method %s is not allowed on %s", want, r.URL.Path))
		return
	}
	s.cors.allowOrigin(h, origin)
	h.Set("Access-Control-Allow-Methods", allow)
	h.Set("Access-Control-Allow-Headers", s.cors.allowedHeaders())
	h.Set("Access-Control-Max-Age", corsMaxAge)
	w.WriteHeader(http.StatusNoContent)
}

package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/api"
)

// headerBudget is how long the response headers of a stream may take. They
// are sent without waiting for the command, so this is a few milliseconds
// of local work; the bound leaves room for a loaded CI machine while
// staying far below the 2 s the test command stays silent.
const headerBudget = 250 * time.Millisecond

// A stream's status line and headers arrive at once, however long the
// command stays silent.
func TestStreamHeadersDoNotWaitForOutput(t *testing.T) {
	h := newHarness(t, options{}) // agent keepalive: the 25 s default
	sb := h.create(keyA, nil)
	base := "/v1/sandboxes/" + sb.ID + "/commands"
	tok := withToken(sb.AccessToken)

	var cmd api.CommandView
	h.want(h.do("POST", base, sh("sleep 2; echo late"), tok), 201, "").json(t, &cmd)

	start := time.Now()
	resp := h.raw("GET", base+"/"+cmd.Pid+"/stream", nil, tok) // returns once the headers are in
	headers := time.Since(start)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("status %d, headers %v", resp.StatusCode, resp.Header)
	}
	t.Logf("attach: headers after %v (the command is silent for 2 s)", headers)
	if headers > headerBudget {
		t.Errorf("headers took %v, want less than %v", headers, headerBudget)
	}
	events := readStream(t, resp.Body)
	if first := time.Since(start); first < time.Second {
		t.Errorf("the stream ended after %v: the command should have been silent for 2 s", first)
	}
	if output(events, "stdout") != "late\n" || last(events).Type != "exit" {
		t.Errorf("events: %+v", events)
	}

	// Errors that have an HTTP status are still reported as one.
	h.want(h.do("GET", base+"/p-nope/stream", nil, tok), 404, api.CodeProcessNotFound)
	h.want(h.do("GET", base+"/"+cmd.Pid+"/stream?from=-1", nil, tok), 400, api.CodeBadRequest)

	// wait=stream: headers and the start event before the command says
	// anything, so the client always has the pid.
	start = time.Now()
	resp2 := h.raw("POST", base+"?wait=stream", sh("sleep 2; echo late"), tok)
	defer resp2.Body.Close()
	br := bufio.NewReader(resp2.Body)
	line, err := br.ReadBytes('\n')
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	var ev api.StreamEvent
	if err := json.Unmarshal(line, &ev); err != nil || ev.Type != api.EventStart || ev.Command == nil || ev.Command.Pid == "" || ev.Offset != 0 {
		t.Fatalf("first line %q (%v)", line, err)
	}
	t.Logf("wait=stream: start event after %v", took)
	if took > headerBudget {
		t.Errorf("the start event took %v, want less than %v", took, headerBudget)
	}
	h.want(h.do("POST", base+"/"+ev.Command.Pid+"/signal", map[string]string{"signal": "SIGKILL"}, tok), 204, "")
}

// An agent that dies between the start and the stream of wait=stream is
// reported in-stream, after the start event: the pid is never lost.
func TestWaitStreamReportsLateFailureAfterStart(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	base := "/v1/sandboxes/" + sb.ID + "/commands"
	resp := h.raw("POST", base+"?wait=stream", sh("echo up; sleep 30"), withToken(sb.AccessToken))
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	var ev api.StreamEvent
	if err := dec.Decode(&ev); err != nil || ev.Type != api.EventStart {
		t.Fatalf("first event %+v (%v)", ev, err)
	}
	h.agents.kill(h.runDir(sb.ID))
	for ev.Type != api.EventError {
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("no error event: %v", err)
		}
	}
	if ev.Error.Code != api.CodeAgentUnavailable || ev.Offset != 0 {
		t.Errorf("error event: offset %d, %+v", ev.Offset, ev.Error)
	}
}

func TestIdempotentCommandStart(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	base := "/v1/sandboxes/" + sb.ID + "/commands"
	tok := withToken(sb.AccessToken)
	key := func(k string) reqOpt { return withHeader("Idempotency-Key", k) }
	body := sh("echo once; sleep 0.2")

	var first, again api.CommandView
	r1 := h.want(h.do("POST", base, body, tok, key("job-1")), 201, "")
	r1.json(t, &first)
	if r1.Header.Get("Idempotent-Replayed") != "" {
		t.Errorf("the first request is marked as a replay")
	}
	r2 := h.want(h.do("POST", base, body, tok, key("job-1")), 200, "")
	r2.json(t, &again)
	if again.Pid != first.Pid || r2.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay: pid %q (first %q), header %q", again.Pid, first.Pid, r2.Header.Get("Idempotent-Replayed"))
	}

	// The retry may ask for another form of answer: here the stream, which
	// starts with the same command and replays its output.
	resp := h.raw("POST", base+"?wait=stream", body, tok, key("job-1"))
	events := readStream(t, resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Idempotent-Replayed") != "true" || len(events) == 0 || events[0].Type != api.EventStart ||
		events[0].Command.Pid != first.Pid || output(events, "stdout") != "once\n" || last(events).Type != "exit" {
		t.Errorf("stream replay: header %q, events %+v", resp.Header.Get("Idempotent-Replayed"), events)
	}

	// Same key, another command: refused, nothing started.
	h.want(h.do("POST", base, sh("echo other"), tok, key("job-1")), 409, "idempotency_key_reused")
	h.want(h.do("POST", base, body, tok, key("has space")), 400, api.CodeBadRequest)
	h.want(h.do("POST", base, body, tok, key(strings.Repeat("k", 129))), 400, api.CodeBadRequest)

	// Concurrent retries start one command between them.
	var wg sync.WaitGroup
	pids := make([]string, 8)
	for i := range pids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var v api.CommandView
			if r := h.do("POST", base, body, tok, key("job-2")); r.Status == 200 || r.Status == 201 {
				r.json(t, &v)
			}
			pids[i] = v.Pid
		}()
	}
	wg.Wait()
	for _, p := range pids {
		if p == "" || p != pids[0] {
			t.Fatalf("concurrent starts with one key: %v", pids)
		}
	}

	// A start that failed is not remembered: the key can be used again.
	h.want(h.do("POST", base, map[string]any{"cmd": "/no/such/binary"}, tok, key("job-3")), 400, "command_not_found")
	h.want(h.do("POST", base, map[string]any{"cmd": "/no/such/binary"}, tok, key("job-3")), 400, "command_not_found")

	var list struct {
		Commands []api.CommandView `json:"commands"`
	}
	h.want(h.do("GET", base, nil, tok), 200, "").json(t, &list)
	if len(list.Commands) != 2 {
		t.Errorf("%d commands exist, want 2 (job-1 and job-2): %+v", len(list.Commands), list.Commands)
	}
	started := 0
	for _, e := range h.st.Events(sb.ID) {
		if e.Type == api.EventCommandStarted {
			started++
		}
	}
	if started != 2 {
		t.Errorf("%d command_started records, want 2", started)
	}
}

func TestFileStat(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	base := "/v1/sandboxes/" + sb.ID + "/files"
	tok := withToken(sb.AccessToken)
	h.want(h.do("PUT", base+"?path=dir/a.txt&mode=0600", "hello", tok), 204, "")

	var fi api.FileView
	h.want(h.do("GET", base+"/stat?path=dir/a.txt", nil, tok), 200, "").json(t, &fi)
	if fi.Name != "a.txt" || fi.Size != 5 || fi.IsDir || fi.Mode != "0600" || !strings.HasSuffix(fi.Path, "/dir/a.txt") || fi.ModTime == "" {
		t.Errorf("stat file: %+v", fi)
	}
	h.want(h.do("GET", base+"/stat?path=dir", nil, tok), 200, "").json(t, &fi)
	if !fi.IsDir || fi.Name != "dir" {
		t.Errorf("stat directory: %+v", fi)
	}
	h.want(h.do("GET", base+"/stat?path=dir/nope", nil, tok), 404, api.CodeFileNotFound)
	h.want(h.do("GET", base+"/stat", nil, tok), 400, api.CodeBadRequest)
	h.want(h.do("GET", base+"/stat?path=dir", nil), 401, api.CodeUnauthorized)
	// The 409 conflict cases of the file routes.
	h.want(h.do("GET", base+"/stat?path=dir/a.txt/x", nil, tok), 409, api.CodeConflict)
	h.want(h.do("GET", base+"/list?path=dir/a.txt", nil, tok), 409, api.CodeConflict)
	h.want(h.do("POST", base+"/mkdir?path=dir/a.txt", nil, tok), 409, api.CodeConflict)
	h.want(h.do("PUT", base+"?path=dir/a.txt/x", "x", tok), 409, api.CodeConflict)
	h.want(h.do("PUT", base+"?path=dir", "x", tok), 409, "is_directory")
	h.want(h.do("GET", base+"?path=dir", nil, tok), 409, "is_directory")
}

func corsHeaders(h http.Header) []string {
	var out []string
	for k := range h {
		if strings.HasPrefix(k, "Access-Control-") {
			out = append(out, k+": "+h.Get(k))
		}
	}
	return out
}

func TestCORS(t *testing.T) {
	const app, evil = "https://app.example", "https://evil.example"
	origin := func(o string) reqOpt { return withHeader("Origin", o) }

	t.Run("off by default", func(t *testing.T) {
		h := newHarness(t, options{})
		sb := h.create(keyA, nil)
		base := "/v1/sandboxes/" + sb.ID + "/commands"
		r := h.want(h.do("OPTIONS", base, nil, origin(app), withHeader("Access-Control-Request-Method", "POST")), 405, api.CodeMethodNotAllowed)
		if c := corsHeaders(r.Header); len(c) != 0 {
			t.Errorf("CORS headers with CORS off: %v", c)
		}
		// An Origin header changes nothing: no header, no refusal.
		r = h.want(h.do("GET", base, nil, origin(evil), withToken(sb.AccessToken)), 200, "")
		if c := corsHeaders(r.Header); len(c) != 0 {
			t.Errorf("CORS headers with CORS off: %v", c)
		}
		h.want(h.do("GET", base, nil, origin(evil), withKey(keyA)), 200, "")
	})

	t.Run("configured origin", func(t *testing.T) {
		h := newHarness(t, options{api: func(c *api.Config) { c.CORSAllowedOrigins = []string{app, "http://localhost:5173"} }})
		sb := h.create(keyA, nil)
		base := "/v1/sandboxes/" + sb.ID
		tok := withToken(sb.AccessToken)
		preflight := func(path, o, method string) response {
			return h.do("OPTIONS", path, nil, origin(o), withHeader("Access-Control-Request-Method", method),
				withHeader("Access-Control-Request-Headers", "x-access-token, content-type"))
		}

		// Preflight from a configured origin.
		r := h.want(preflight(base+"/commands", app, "POST"), 204, "")
		if r.Header.Get("Access-Control-Allow-Origin") != app ||
			r.Header.Get("Access-Control-Allow-Methods") != "GET, POST" ||
			!strings.Contains(r.Header.Get("Access-Control-Allow-Headers"), "X-Access-Token") ||
			!strings.Contains(r.Header.Get("Access-Control-Allow-Headers"), "Content-Type") ||
			strings.Contains(r.Header.Get("Access-Control-Allow-Headers"), "X-API-Key") ||
			strings.Contains(r.Header.Get("Access-Control-Allow-Headers"), "Authorization") ||
			r.Header.Get("Access-Control-Max-Age") == "" || !strings.Contains(strings.Join(r.Header.Values("Vary"), ","), "Origin") {
			t.Errorf("preflight headers: %v", r.Header)
		}
		if r.Header.Get("Access-Control-Allow-Credentials") != "" {
			t.Error("Access-Control-Allow-Credentials must never be sent")
		}
		h.want(preflight(base+"/files", app, "PUT"), 204, "")
		h.want(preflight(base+"/files/stat", app, "GET"), 204, "")
		h.want(preflight(base+"/commands", app, "DELETE"), 405, api.CodeMethodNotAllowed)
		h.want(h.do("OPTIONS", base+"/commands", nil), 400, api.CodeBadRequest) // not a preflight

		// Preflight from another origin: refused, nothing reflected.
		r = h.want(preflight(base+"/commands", evil, "POST"), 403, "origin_not_allowed")
		if c := corsHeaders(r.Header); len(c) != 0 {
			t.Errorf("an origin that is not configured got CORS headers: %v", c)
		}

		// The real request.
		r = h.want(h.do("GET", base+"/commands", nil, origin(app), tok), 200, "")
		if r.Header.Get("Access-Control-Allow-Origin") != app || r.Header.Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("response headers: %v", r.Header)
		}
		exposed := r.Header.Get("Access-Control-Expose-Headers")
		for _, want := range []string{"X-Request-Id", "Retry-After", "Location", "Idempotent-Replayed"} {
			if !strings.Contains(exposed, want) {
				t.Errorf("Access-Control-Expose-Headers %q lacks %s", exposed, want)
			}
		}
		// An error answer is readable too.
		r = h.want(h.do("GET", base+"/commands", nil, origin(app), withToken("sbt_"+strings.Repeat("0", 64))), 401, api.CodeUnauthorized)
		if r.Header.Get("Access-Control-Allow-Origin") != app {
			t.Errorf("401 without CORS headers: %v", r.Header)
		}
		// The second configured origin, case-insensitively.
		r = h.want(h.do("GET", base+"/commands", nil, origin("HTTP://LOCALHOST:5173"), tok), 200, "")
		if r.Header.Get("Access-Control-Allow-Origin") != "HTTP://LOCALHOST:5173" {
			t.Errorf("second origin: %v", r.Header)
		}

		// Another origin: refused before anything is done.
		r = h.want(h.do("POST", base+"/commands", sh("touch /tmp/evil"), origin(evil), tok), 403, "origin_not_allowed")
		if c := corsHeaders(r.Header); len(c) != 0 {
			t.Errorf("an origin that is not configured got CORS headers: %v", c)
		}
		var list struct {
			Commands []api.CommandView `json:"commands"`
		}
		h.want(h.do("GET", base+"/commands", nil, tok), 200, "").json(t, &list)
		if len(list.Commands) != 0 {
			t.Errorf("a refused origin started a command: %+v", list.Commands)
		}

		// No Origin (any non-browser client) and same-origin: as without CORS.
		r = h.want(h.do("GET", base+"/commands", nil, tok), 200, "")
		if c := corsHeaders(r.Header); len(c) != 0 {
			t.Errorf("CORS headers without an Origin: %v", c)
		}
		h.want(h.do("GET", base+"/commands", nil, withKey(keyA)), 200, "")
		r = h.want(h.do("GET", base+"/commands", nil, origin(h.srv.URL), withKey(keyA)), 200, "")
		if c := corsHeaders(r.Header); len(c) != 0 {
			t.Errorf("CORS headers on a same-origin request: %v", c)
		}

		// A tenant API key does not work from a browser origin, by header or by bearer.
		h.want(h.do("GET", base+"/commands", nil, origin(app), withKey(keyA)), 403, "api_key_not_allowed")
		h.want(h.do("GET", base+"/commands", nil, origin(app), withHeader("Authorization", "Bearer "+keyA)), 403, "api_key_not_allowed")

		// The control plane takes no part in CORS.
		r = h.want(preflight("/v1/sandboxes", app, "POST"), 405, api.CodeMethodNotAllowed)
		if c := corsHeaders(r.Header); len(c) != 0 {
			t.Errorf("control-plane preflight got CORS headers: %v", c)
		}
		r = h.want(preflight(base, app, "DELETE"), 405, api.CodeMethodNotAllowed)
		if c := corsHeaders(r.Header); len(c) != 0 {
			t.Errorf("control-plane preflight got CORS headers: %v", c)
		}
		r = h.want(h.do("GET", "/v1/sandboxes", nil, origin(app), withKey(keyA)), 200, "")
		if c := corsHeaders(r.Header); len(c) != 0 {
			t.Errorf("control-plane response got CORS headers: %v", c)
		}

		// The WebSocket handshake is held to the same list.
		var cmd api.CommandView
		h.want(h.do("POST", base+"/commands", sh("sleep 30"), tok), 201, "").json(t, &cmd)
		wsURL := "ws" + strings.TrimPrefix(h.srv.URL, "http") + base + "/commands/" + cmd.Pid + "/ws?token=" + sb.AccessToken
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for name, tc := range map[string]struct {
			origin string
			status int
		}{"allowed": {app, 101}, "disallowed": {evil, 403}, "absent": {"", 101}, "same host": {h.srv.URL, 101}} {
			hdr := http.Header{}
			if tc.origin != "" {
				hdr.Set("Origin", tc.origin)
			}
			c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
			if c != nil {
				c.CloseNow()
			}
			if resp == nil || resp.StatusCode != tc.status {
				t.Errorf("websocket, origin %s: response %v, err %v, want %d", name, resp, err, tc.status)
			}
		}
		// A refused origin is not told anything through the close-code channel either.
		_, resp, _ := websocket.Dial(ctx, wsURL+"&handshake_errors=close", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {evil}}})
		if resp == nil || resp.StatusCode != 403 {
			t.Errorf("websocket, disallowed origin with handshake_errors=close: %v", resp)
		}
	})

	t.Run("wildcard and API key opt-in", func(t *testing.T) {
		h := newHarness(t, options{api: func(c *api.Config) { c.CORSAllowedOrigins = []string{"*"}; c.CORSAllowAPIKey = true }})
		sb := h.create(keyA, nil)
		base := "/v1/sandboxes/" + sb.ID + "/commands"
		r := h.want(h.do("GET", base, nil, origin(evil), withToken(sb.AccessToken)), 200, "")
		if r.Header.Get("Access-Control-Allow-Origin") != "*" || r.Header.Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("wildcard: %v", r.Header)
		}
		h.want(h.do("GET", base, nil, origin(evil), withKey(keyA)), 200, "")
		r = h.want(h.do("OPTIONS", base, nil, origin(evil), withHeader("Access-Control-Request-Method", "GET")), 204, "")
		if !strings.Contains(r.Header.Get("Access-Control-Allow-Headers"), "X-API-Key") {
			t.Errorf("opt-in does not allow X-API-Key: %v", r.Header)
		}
		// Still not on the control plane.
		h.want(h.do("OPTIONS", "/v1/sandboxes", nil, origin(evil), withHeader("Access-Control-Request-Method", "POST")), 405, "")
	})

	t.Run("configuration is validated", func(t *testing.T) {
		st := newFakeStore()
		for _, bad := range []string{"app.example", "https://app.example/", "https://app.example/path", "null", "ftp://x"} {
			_, err := api.New(api.Config{Store: st, Pool: nopPool{}, CORSAllowedOrigins: []string{bad},
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err == nil {
				t.Errorf("origin %q was accepted", bad)
			}
		}
	})
}

// nopPool lets api.New be called without a controller.
type nopPool struct{ api.Pool }

// wsEndOf reads a WebSocket to its end and returns the last status frame
// and the close error.
func wsEndOf(t *testing.T, ctx context.Context, c *websocket.Conn) (*agentapi.StatusFrame, websocket.CloseError) {
	t.Helper()
	var status *agentapi.StatusFrame
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) {
				t.Fatalf("the connection ended without a close frame: %v", err)
			}
			return status, ce
		}
		if len(data) > 0 && data[0] == agentapi.ChannelStatus {
			status = &agentapi.StatusFrame{}
			if err := json.Unmarshal(data[1:], status); err != nil {
				t.Fatalf("status frame %q: %v", data[1:], err)
			}
		}
	}
}

func wantWSEnd(t *testing.T, what string, st *agentapi.StatusFrame, ce websocket.CloseError, code int, reason string) {
	t.Helper()
	if int(ce.Code) != code || ce.Reason != reason {
		t.Errorf("%s: close %d %q, want %d %q", what, ce.Code, ce.Reason, code, reason)
	}
	if st == nil || st.Error == nil || st.Error.Code != reason || st.Error.Message == "" {
		t.Errorf("%s: status frame %+v, want an error with code %q", what, st, reason)
	}
	if len(ce.Reason) > 123 {
		t.Errorf("%s: close reason of %d bytes", what, len(ce.Reason))
	}
}

func TestWebSocketCloseCodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type session struct {
		h      *harness
		sb     api.SandboxView
		cmd    api.CommandView
		wsBase string
	}
	setup := func(t *testing.T) session {
		h := newHarness(t, options{})
		sb := h.create(keyA, nil)
		var cmd api.CommandView
		h.want(h.do("POST", "/v1/sandboxes/"+sb.ID+"/commands", sh("echo ready; sleep 30"), withToken(sb.AccessToken)), 201, "").json(t, &cmd)
		return session{h, sb, cmd, "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/v1/sandboxes/" + sb.ID + "/commands/"}
	}
	attach := func(t *testing.T, s session) *websocket.Conn {
		c, _, err := websocket.Dial(ctx, s.wsBase+s.cmd.Pid+"/ws?token="+s.sb.AccessToken, nil)
		if err != nil {
			t.Fatal(err)
		}
		c.SetReadLimit(wsFrameLimit)
		wsUntil(t, ctx, c, "ready")
		return c
	}

	t.Run("refused handshake as a close", func(t *testing.T) {
		s := setup(t)
		for name, tc := range map[string]struct {
			url    string
			code   int
			reason string
		}{
			"wrong token":    {s.wsBase + s.cmd.Pid + "/ws?token=sbt_" + strings.Repeat("0", 64), agentapi.CloseUnauthorized, "unauthorized"},
			"no credentials": {s.wsBase + s.cmd.Pid + "/ws", agentapi.CloseUnauthorized, "unauthorized"},
			"unknown pid":    {s.wsBase + "p-nope/ws?token=" + s.sb.AccessToken, agentapi.CloseNotFound, "process_not_found"},
			"bad from":       {s.wsBase + s.cmd.Pid + "/ws?from=x&token=" + s.sb.AccessToken, agentapi.CloseBadRequest, "bad_request"},
		} {
			sep := "?"
			if strings.Contains(tc.url, "?") {
				sep = "&"
			}
			// Without the parameter: a plain HTTP error, as before.
			if _, resp, err := websocket.Dial(ctx, tc.url, nil); err == nil || resp == nil || resp.StatusCode < 400 {
				t.Errorf("%s: default mode did not fail the handshake: %v %v", name, resp, err)
			}
			c, _, err := websocket.Dial(ctx, tc.url+sep+"handshake_errors=close", nil)
			if err != nil {
				t.Errorf("%s: the handshake failed: %v", name, err)
				continue
			}
			st, ce := wsEndOf(t, ctx, c)
			wantWSEnd(t, name, st, ce, tc.code, tc.reason)
		}
		if _, resp, _ := websocket.Dial(ctx, s.wsBase+s.cmd.Pid+"/ws?handshake_errors=nonsense&token="+s.sb.AccessToken, nil); resp == nil || resp.StatusCode != 400 {
			t.Errorf("handshake_errors=nonsense: %v", resp)
		}
		// A plain GET with the parameter is still an HTTP error.
		s.h.want(s.h.do("GET", "/v1/sandboxes/"+s.sb.ID+"/commands/"+s.cmd.Pid+"/ws?handshake_errors=close", nil), 401, api.CodeUnauthorized)
	})

	t.Run("sandbox deleted", func(t *testing.T) {
		s := setup(t)
		c := attach(t, s)
		defer c.CloseNow()
		s.h.want(s.h.do("DELETE", "/v1/sandboxes/"+s.sb.ID, nil, withKey(keyA)), 204, "")
		st, ce := wsEndOf(t, ctx, c)
		wantWSEnd(t, "deleted", st, ce, agentapi.CloseSandboxTerminated, "sandbox_terminated")
		if st != nil && st.Error != nil && st.Error.Details["reason"] != "released" {
			t.Errorf("status frame details: %v, want reason released", st.Error.Details)
		}
		// Reattaching to it is refused with the same close code.
		c2, _, err := websocket.Dial(ctx, s.wsBase+s.cmd.Pid+"/ws?handshake_errors=close&token="+s.sb.AccessToken, nil)
		if err != nil {
			t.Fatal(err)
		}
		st, ce = wsEndOf(t, ctx, c2)
		wantWSEnd(t, "reattach", st, ce, agentapi.CloseSandboxTerminated, "sandbox_not_running")
	})

	t.Run("sandbox died", func(t *testing.T) {
		s := setup(t)
		c := attach(t, s)
		defer c.CloseNow()
		inst, _ := s.h.be.BySandbox(s.sb.ID)
		s.h.be.KillInstance(inst.ID, 137, true)
		s.h.agents.kill(s.h.runDir(s.sb.ID))
		st, ce := wsEndOf(t, ctx, c)
		wantWSEnd(t, "died", st, ce, agentapi.CloseSandboxTerminated, "sandbox_terminated")
		if !strings.Contains(st.Error.Message, "oom_killed") || st.Error.Details["reason"] != "oom_killed" {
			t.Errorf("the status frame does not say why: %q", st.Error.Message)
		}
	})

	t.Run("agent link lost", func(t *testing.T) {
		s := setup(t)
		c := attach(t, s)
		defer c.CloseNow()
		s.h.agents.kill(s.h.runDir(s.sb.ID)) // the instance itself stays up
		st, ce := wsEndOf(t, ctx, c)
		wantWSEnd(t, "agent lost", st, ce, agentapi.CloseAgentUnavailable, "agent_unavailable")
	})

	t.Run("server shutting down", func(t *testing.T) {
		s := setup(t)
		c := attach(t, s)
		defer c.CloseNow()
		go s.h.api.Close()
		st, ce := wsEndOf(t, ctx, c)
		wantWSEnd(t, "shutdown", st, ce, agentapi.CloseShuttingDown, "shutting_down")
	})

	t.Run("normal exit is 1000", func(t *testing.T) {
		s := setup(t)
		c := attach(t, s)
		defer c.CloseNow()
		s.h.want(s.h.do("POST", "/v1/sandboxes/"+s.sb.ID+"/commands/"+s.cmd.Pid+"/signal", map[string]string{"signal": "SIGKILL"},
			withToken(s.sb.AccessToken)), 204, "")
		st, ce := wsEndOf(t, ctx, c)
		if ce.Code != websocket.StatusNormalClosure || st == nil || st.Exit == nil || st.Exit.Signal != "SIGKILL" {
			t.Errorf("close %d %q, status %+v", ce.Code, ce.Reason, st)
		}
	})
}

// The close-code table is closed over the API error codes a handshake
// can produce, and every reason fits a close frame.
func TestCloseCodeTable(t *testing.T) {
	for code, want := range map[string]int{
		api.CodeSandboxTerminated: 4000, api.CodeSandboxNotRunning: 4000, api.CodeUnauthorized: 4001,
		api.CodeShuttingDown: 4002, api.CodeAgentUnavailable: 4003, api.CodeNotFound: 4004, api.CodeProcessNotFound: 4004,
		api.CodeBadRequest: 4005, api.CodeRateLimited: 4029, api.CodeInternal: 4099, api.CodeTimeout: 4099,
	} {
		if got := agentapi.CloseCodeFor(code); got != want {
			t.Errorf("CloseCodeFor(%q) = %d, want %d", code, got, want)
		}
		if got := agentapi.CloseCodeFor(code); got < 4000 || got > 4999 {
			t.Errorf("CloseCodeFor(%q) = %d is outside the application range", code, got)
		}
	}
}

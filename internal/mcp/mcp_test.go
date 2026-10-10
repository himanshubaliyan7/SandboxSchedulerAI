package mcp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI imitates the routes of sbxd that the tools call.
type fakeAPI struct {
	mu       sync.Mutex
	files    map[string][]byte
	starts   []map[string]any
	deleted  []string
	policies []map[string]any
	created  []map[string]any
	block    chan struct{} // when set, a command waits on it (or the request context)
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, status int, code, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
	}
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") != "sbx_test" {
				fail(w, 401, "unauthorized", "bad key")
				return
			}
			if r.PathValue("id") == "missing" {
				fail(w, 404, "not_found", "no such sandbox")
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /v1/templates", auth(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"templates":[{"id":"python"}]}`)
	}))
	mux.HandleFunc("POST /v1/sandboxes", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.created = append(f.created, body)
		f.mu.Unlock()
		w.WriteHeader(201)
		io.WriteString(w, `{"id":"sb-1","state":"running","template_id":"python","access_token":"sbt_secret"}`)
	}))
	mux.HandleFunc("GET /v1/sandboxes", auth(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"sandboxes":[{"id":"sb-1"}]}`)
	}))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = append(f.deleted, r.PathValue("id"))
		f.mu.Unlock()
		w.WriteHeader(204)
	}))
	mux.HandleFunc("PUT /v1/sandboxes/{id}/network", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.policies = append(f.policies, body)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(body)
	}))
	mux.HandleFunc("POST /v1/sandboxes/{id}/commands", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("wait") != "true" {
			fail(w, 400, "bad_request", "test expects wait=true")
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.starts = append(f.starts, body)
		block := f.block
		f.mu.Unlock()
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		args, _ := body["args"].([]any)
		res := map[string]any{"pid": "p1", "truncated": false, "exit": map[string]any{"exit_code": 0, "reason": "exited"}}
		switch {
		case body["cmd"] == "python3":
			f.mu.Lock()
			code := f.files[args[0].(string)]
			f.mu.Unlock()
			res["stdout"] = base64.StdEncoding.EncodeToString([]byte("ran: " + string(code)))
		case len(args) == 2 && args[1] == "big":
			res["stdout"] = base64.StdEncoding.EncodeToString([]byte("HEAD" + strings.Repeat("x", 5000) + "TAIL"))
		case len(args) == 2 && args[1] == "fail":
			res["stderr"] = base64.StdEncoding.EncodeToString([]byte("boom\n"))
			res["exit"] = map[string]any{"exit_code": 137, "reason": "timeout", "signal": "SIGKILL"}
		default:
			res["stdout"] = base64.StdEncoding.EncodeToString([]byte("hello\n"))
		}
		json.NewEncoder(w).Encode(res)
	}))
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files", auth(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.files[r.URL.Query().Get("path")] = data
		f.mu.Unlock()
		w.WriteHeader(204)
	}))
	mux.HandleFunc("GET /v1/sandboxes/{id}/files", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		data, ok := f.files[r.URL.Query().Get("path")]
		f.mu.Unlock()
		if !ok {
			fail(w, 404, "file_not_found", "no such file")
			return
		}
		w.Write(data)
	}))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/files", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		delete(f.files, r.URL.Query().Get("path"))
		f.mu.Unlock()
		w.WriteHeader(204)
	}))
	mux.HandleFunc("GET /v1/sandboxes/{id}/files/list", auth(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"entries":[{"name":"a.txt","path":"`+r.URL.Query().Get("path")+`"}]}`)
	}))
	return mux
}

// session drives a Server over pipes the way an MCP client does.
type session struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Scanner
	id  int
}

func newSession(t *testing.T, cfg Config) (*session, *fakeAPI) {
	t.Helper()
	f := &fakeAPI{files: map[string][]byte{}}
	ts := httptest.NewServer(f.handler(t))
	t.Cleanup(ts.Close)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := NewServer(NewClient(ts.URL, "sbx_test"), cfg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(context.Background(), inR, outW)
		outW.Close()
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop when stdin closed")
		}
	})
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 64<<10), maxLineBytes)
	return &session{t: t, in: inW, out: sc}, f
}

func (s *session) send(v any) {
	s.t.Helper()
	b, _ := json.Marshal(v)
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

type reply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (s *session) recv() reply {
	s.t.Helper()
	if !s.out.Scan() {
		s.t.Fatalf("no reply: %v", s.out.Err())
	}
	var r reply
	if err := json.Unmarshal(s.out.Bytes(), &r); err != nil {
		s.t.Fatalf("reply is not JSON: %q", s.out.Text())
	}
	return r
}

func (s *session) request(method string, params any) reply {
	s.t.Helper()
	s.id++
	s.send(map[string]any{"jsonrpc": "2.0", "id": s.id, "method": method, "params": params})
	return s.recv()
}

// call runs a tool and returns its text and whether it reported an error.
func (s *session) call(name string, args map[string]any) (string, bool) {
	s.t.Helper()
	r := s.request("tools/call", map[string]any{"name": name, "arguments": args})
	if r.Error != nil {
		s.t.Fatalf("%s: protocol error %+v", name, r.Error)
	}
	var res toolResult
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Content) != 1 {
		s.t.Fatalf("%s: bad result %s", name, r.Result)
	}
	return res.Content[0].Text, res.IsError
}

func TestHandshakeAndToolList(t *testing.T) {
	s, _ := newSession(t, Config{})
	r := s.request("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}})
	var init struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
	}
	json.Unmarshal(r.Result, &init)
	if init.ProtocolVersion != "2025-03-26" || init.Capabilities["tools"] == nil {
		t.Fatalf("initialize: %s", r.Result)
	}
	// An unknown revision gets the newest one this server speaks.
	r = s.request("initialize", map[string]any{"protocolVersion": "1999-01-01"})
	json.Unmarshal(r.Result, &init)
	if init.ProtocolVersion != protocolVersions[0] {
		t.Fatalf("fallback version = %q", init.ProtocolVersion)
	}
	// Notifications get no reply; the next reply must belong to ping.
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if r = s.request("ping", nil); r.Error != nil || string(r.Result) != "{}" {
		t.Fatalf("ping: %+v", r)
	}
	r = s.request("tools/list", nil)
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	json.Unmarshal(r.Result, &list)
	want := []string{"list_templates", "create_sandbox", "list_sandboxes", "kill_sandbox", "run_command", "run_python", "write_file", "read_file", "list_files", "set_network"}
	if len(list.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(list.Tools), len(want))
	}
	for i, tl := range list.Tools {
		if tl.Name != want[i] || tl.Description == "" || tl.InputSchema["type"] != "object" {
			t.Errorf("tool %d = %+v", i, tl)
		}
	}
	if r = s.request("resources/list", nil); r.Error == nil || r.Error.Code != codeMethodNotFound {
		t.Fatalf("unknown method: %+v", r)
	}
	if r = s.request("tools/call", map[string]any{"name": "nope"}); r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Fatalf("unknown tool: %+v", r)
	}
}

func TestMalformedInput(t *testing.T) {
	s, _ := newSession(t, Config{})
	s.in.Write([]byte("{not json\n"))
	if r := s.recv(); r.Error == nil || r.Error.Code != codeParseError || string(r.ID) != "null" {
		t.Fatalf("parse error reply: %+v", r)
	}
	s.send(map[string]any{"jsonrpc": "1.0", "id": 7, "method": "ping"})
	if r := s.recv(); r.Error == nil || r.Error.Code != codeInvalidRequest {
		t.Fatalf("invalid request reply: %+v", r)
	}
	// The server is still usable afterwards.
	if r := s.request("ping", nil); r.Error != nil {
		t.Fatal(r.Error)
	}
}

func TestSandboxLifecycle(t *testing.T) {
	s, f := newSession(t, Config{DefaultTemplate: "python"})
	text, isErr := s.call("create_sandbox", map[string]any{"timeout_seconds": 90, "allow_domains": []string{"pypi.org"}, "network_mode": "allowlist"})
	if isErr || !strings.Contains(text, `"sandbox_id": "sb-1"`) {
		t.Fatalf("create: %s", text)
	}
	if strings.Contains(text, "sbt_secret") || strings.Contains(text, "access_token") {
		t.Fatalf("the access token reached the tool result: %s", text)
	}
	body := f.created[0]
	if body["template_id"] != "python" || body["timeout_ms"] != float64(90000) {
		t.Fatalf("create body: %v", body)
	}
	if net := body["network"].(map[string]any); net["mode"] != "allowlist" || len(net["allow_domains"].([]any)) != 1 {
		t.Fatalf("network: %v", net)
	}
	if text, _ = s.call("list_sandboxes", nil); !strings.Contains(text, "sb-1") {
		t.Fatalf("list: %s", text)
	}
	if text, _ = s.call("list_templates", map[string]any{}); !strings.Contains(text, "python") {
		t.Fatalf("templates: %s", text)
	}
	if text, isErr = s.call("set_network", map[string]any{"sandbox_id": "sb-1", "network_mode": "none"}); isErr || f.policies[0]["mode"] != "none" {
		t.Fatalf("set_network: %s %v", text, f.policies)
	}
	if text, isErr = s.call("kill_sandbox", map[string]any{"sandbox_id": "sb-1"}); isErr || len(f.deleted) != 1 {
		t.Fatalf("kill: %s", text)
	}
}

func TestCreateNeedsATemplate(t *testing.T) {
	s, _ := newSession(t, Config{})
	text, isErr := s.call("create_sandbox", map[string]any{})
	if !isErr || !strings.Contains(text, "template_id is required") {
		t.Fatalf("got %q", text)
	}
}

func TestRunCommand(t *testing.T) {
	s, f := newSession(t, Config{MaxOutputBytes: 100})
	text, isErr := s.call("run_command", map[string]any{"sandbox_id": "sb-1", "command": "echo hello", "cwd": "/tmp", "env": map[string]string{"A": "1"}})
	if isErr || !strings.Contains(text, "exit_code: 0") || !strings.Contains(text, "stdout:\nhello\n") || !strings.Contains(text, "stderr: (empty)") {
		t.Fatalf("run: %q", text)
	}
	st := f.starts[0]
	if st["cmd"] != "/bin/sh" || st["args"].([]any)[1] != "echo hello" || st["cwd"] != "/tmp" || st["timeout_ms"] != float64(60000) {
		t.Fatalf("start body: %v", st)
	}
	// A failed command is a result the model reads, not a tool error.
	text, isErr = s.call("run_command", map[string]any{"sandbox_id": "sb-1", "command": "fail", "timeout_seconds": 2})
	if isErr || !strings.Contains(text, "exit_code: 137") || !strings.Contains(text, "reason: timeout (killed after 2s)") || !strings.Contains(text, "boom") {
		t.Fatalf("failed run: %q", text)
	}
	// Long output keeps its head and tail.
	text, _ = s.call("run_command", map[string]any{"sandbox_id": "sb-1", "command": "big"})
	if !strings.Contains(text, "HEAD") || !strings.Contains(text, "TAIL") || !strings.Contains(text, "bytes were cut from the middle of stdout") || len(text) > 400 {
		t.Fatalf("truncation: %d bytes: %q", len(text), text)
	}
	for _, bad := range []map[string]any{
		{"sandbox_id": "sb-1", "command": "x", "timeout_seconds": 99999},
		{"sandbox_id": "sb-1", "command": "x", "timeout_seconds": -1},
		{"sandbox_id": "sb-1", "command": "  "},
		{"command": "x"},
		{"sandbox_id": "sb-1", "command": "x", "typo": true},
	} {
		if text, isErr = s.call("run_command", bad); !isErr {
			t.Errorf("%v was accepted: %q", bad, text)
		}
	}
	if text, isErr = s.call("run_command", map[string]any{"sandbox_id": "missing", "command": "x"}); !isErr || !strings.Contains(text, "not_found") {
		t.Fatalf("API error not surfaced: %q", text)
	}
}

func TestRunPythonUploadsAndCleansUp(t *testing.T) {
	s, f := newSession(t, Config{})
	text, isErr := s.call("run_python", map[string]any{"sandbox_id": "sb-1", "code": "print(6*7)"})
	if isErr || !strings.Contains(text, "ran: print(6*7)") {
		t.Fatalf("run_python: %q", text)
	}
	st := f.starts[0]
	if script := st["args"].([]any)[0].(string); st["cmd"] != "python3" || !strings.HasPrefix(script, "/tmp/sbx-mcp/run-") {
		t.Fatalf("start body: %v", st)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.files) != 0 {
		t.Fatalf("script left behind: %v", f.files)
	}
}

func TestFiles(t *testing.T) {
	s, f := newSession(t, Config{MaxFileBytes: 10})
	if text, isErr := s.call("write_file", map[string]any{"sandbox_id": "sb-1", "path": "a.txt", "content": "héllo"}); isErr || !strings.Contains(text, "Wrote 6 bytes") {
		t.Fatalf("write: %q", text)
	}
	if text, _ := s.call("read_file", map[string]any{"sandbox_id": "sb-1", "path": "a.txt"}); text != "héllo" {
		t.Fatalf("read: %q", text)
	}
	bin := []byte{0xff, 0xfe, 0x00, 0x01}
	s.call("write_file", map[string]any{"sandbox_id": "sb-1", "path": "b.bin", "content": base64.StdEncoding.EncodeToString(bin), "encoding": "base64"})
	if string(f.files["b.bin"]) != string(bin) {
		t.Fatalf("binary write: %v", f.files["b.bin"])
	}
	if text, _ := s.call("read_file", map[string]any{"sandbox_id": "sb-1", "path": "b.bin"}); text != "encoding: base64\n"+base64.StdEncoding.EncodeToString(bin) {
		t.Fatalf("binary read: %q", text)
	}
	// A file over the limit is cut on a character boundary and says so.
	s.call("write_file", map[string]any{"sandbox_id": "sb-1", "path": "big.txt", "content": "123456789é and more"})
	text, _ := s.call("read_file", map[string]any{"sandbox_id": "sb-1", "path": "big.txt"})
	if !strings.HasPrefix(text, "123456789\nnote: only the first") {
		t.Fatalf("cut read: %q", text)
	}
	if text, isErr := s.call("read_file", map[string]any{"sandbox_id": "sb-1", "path": "nope"}); !isErr || !strings.Contains(text, "file_not_found") {
		t.Fatalf("missing file: %q", text)
	}
	if text, isErr := s.call("write_file", map[string]any{"sandbox_id": "sb-1", "path": "x", "content": "!!", "encoding": "base64"}); !isErr {
		t.Fatalf("bad base64 accepted: %q", text)
	}
	if text, _ := s.call("list_files", map[string]any{"sandbox_id": "sb-1", "path": "/workspace"}); !strings.Contains(text, "a.txt") {
		t.Fatalf("list: %q", text)
	}
}

// A running tool call must not block other requests, and a cancellation
// notification must end it.
func TestCancellationAndConcurrency(t *testing.T) {
	s, f := newSession(t, Config{})
	f.block = make(chan struct{})
	s.send(map[string]any{"jsonrpc": "2.0", "id": "slow", "method": "tools/call",
		"params": map[string]any{"name": "run_command", "arguments": map[string]any{"sandbox_id": "sb-1", "command": "sleep 100"}}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.starts)
		f.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command never reached the API")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.send(map[string]any{"jsonrpc": "2.0", "id": "p", "method": "ping"})
	if r := s.recv(); string(r.ID) != `"p"` {
		t.Fatalf("ping was blocked behind the tool call: %s", r.ID)
	}
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": "slow"}})
	r := s.recv()
	var res toolResult
	json.Unmarshal(r.Result, &res)
	if string(r.ID) != `"slow"` || !res.IsError || !strings.Contains(res.Content[0].Text, "context canceled") {
		t.Fatalf("cancelled call: %s %s", r.ID, r.Result)
	}
}

func TestHeadTail(t *testing.T) {
	if s, cut := headTail("short", 10); s != "short" || cut != 0 {
		t.Fatal(s, cut)
	}
	s, cut := headTail(strings.Repeat("é", 100), 21)
	if cut <= 0 || !strings.Contains(s, "bytes cut") || strings.ContainsRune(s, '�') || !json.Valid([]byte(`"`+s[:2]+`"`)) {
		t.Fatalf("%q %d", s, cut)
	}
}

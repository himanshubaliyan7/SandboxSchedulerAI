// Package mcp is a Model Context Protocol server that exposes sbxd
// sandboxes as tools: create a sandbox, run a command or Python code in
// it, move files, change its egress policy, destroy it. It is a thin
// client of the public REST API (api/openapi.yaml) and holds no state of
// its own, so it can run next to the agent that uses it, far from sbxd.
//
// Transport is stdio: one JSON-RPC 2.0 message per line on stdin and
// stdout. Nothing else is ever written to stdout; logs go to stderr.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// Version is reported to MCP clients as the server version.
const Version = "0.1.0"

// Protocol revisions this server speaks, newest first. The tool surface
// used here (tools/list, tools/call with text content) is the same in all.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// maxLineBytes bounds one incoming message. A write_file call carries the
// file content inline, so this is also the practical upload limit.
const maxLineBytes = 16 << 20

const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolResult is the result of tools/call. A failed tool is a successful
// JSON-RPC response with IsError set, so the model can read the reason.
type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Server serves MCP over a pair of streams.
type Server struct {
	api    *Client
	cfg    Config
	tools  []tool
	byName map[string]tool
	log    *slog.Logger

	writeMu sync.Mutex
	out     io.Writer

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
}

// Config tunes the tool surface.
type Config struct {
	// DefaultTemplate is used by create_sandbox when the caller names none.
	DefaultTemplate string
	// MaxOutputBytes caps each of stdout and stderr in a tool result; the
	// middle is cut so the model keeps the head and the tail.
	MaxOutputBytes int
	// MaxFileBytes caps what read_file returns.
	MaxFileBytes int
	Logger       *slog.Logger
}

// NewServer returns a server that calls sbxd through api.
func NewServer(api *Client, cfg Config) *Server {
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = 32 << 10
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = 256 << 10
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{api: api, cfg: cfg, log: cfg.Logger, inflight: map[string]context.CancelFunc{}}
	s.tools = s.buildTools()
	s.byName = make(map[string]tool, len(s.tools))
	for _, t := range s.tools {
		s.byName[t.Name] = t
	}
	return s
}

// Serve reads requests from in until EOF or ctx is done. Tool calls run
// concurrently, so a long command does not block a cancellation or ping.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = out
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), maxLineBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.reply(json.RawMessage("null"), nil, &rpcError{codeParseError, "parse error: " + err.Error()})
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			if len(req.ID) > 0 {
				s.reply(req.ID, nil, &rpcError{codeInvalidRequest, "not a JSON-RPC 2.0 request"})
			}
			continue
		}
		if req.Method == "tools/call" && len(req.ID) > 0 {
			callCtx, callCancel := context.WithCancel(ctx)
			key := string(req.ID)
			s.mu.Lock()
			s.inflight[key] = callCancel
			s.mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					s.mu.Lock()
					delete(s.inflight, key)
					s.mu.Unlock()
					callCancel()
				}()
				s.handle(callCtx, req)
			}()
			continue
		}
		s.handle(ctx, req)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("mcp: read: %w", err)
	}
	return nil
}

func (s *Server) handle(ctx context.Context, req request) {
	notification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.reply(req.ID, map[string]any{
			"protocolVersion": negotiate(p.ProtocolVersion),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "sbx", "title": "Sandbox scheduler", "version": Version},
			"instructions": "Tools for running code in isolated, disposable sandboxes. " +
				"Create a sandbox once, reuse its sandbox_id for commands and files, and kill it when done. " +
				"Sandboxes have no network unless their template and policy allow listed hosts.",
		}, nil)
	case "ping":
		s.reply(req.ID, map[string]any{}, nil)
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
		}
		s.reply(req.ID, map[string]any{"tools": list}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.reply(req.ID, nil, &rpcError{codeInvalidParams, "invalid params: " + err.Error()})
			return
		}
		t, ok := s.byName[p.Name]
		if !ok {
			s.reply(req.ID, nil, &rpcError{codeInvalidParams, "unknown tool: " + p.Name})
			return
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		text, err := t.Call(ctx, p.Arguments)
		if err != nil {
			s.log.Info("tool failed", "tool", p.Name, "err", err)
			s.reply(req.ID, toolResult{Content: []content{{"text", err.Error()}}, IsError: true}, nil)
			return
		}
		s.reply(req.ID, toolResult{Content: []content{{"text", text}}}, nil)
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(req.Params, &p) == nil {
			s.mu.Lock()
			cancel := s.inflight[string(p.RequestID)]
			s.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	default:
		if !notification {
			s.reply(req.ID, nil, &rpcError{codeMethodNotFound, "method not found: " + req.Method})
		}
	}
}

func negotiate(requested string) string {
	for _, v := range protocolVersions {
		if v == requested {
			return v
		}
	}
	return protocolVersions[0]
}

func (s *Server) reply(id json.RawMessage, result any, rerr *rpcError) {
	if len(id) == 0 {
		return
	}
	b, err := json.Marshal(response{JSONRPC: "2.0", ID: id, Result: result, Error: rerr})
	if err != nil {
		b, _ = json.Marshal(response{JSONRPC: "2.0", ID: id, Error: &rpcError{-32603, "internal error"}})
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.out.Write(append(b, '\n'))
}

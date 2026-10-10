package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

type tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Call        func(ctx context.Context, args json.RawMessage) (string, error)
}

// Command timeouts. The default matches the SDKs; the ceiling keeps one
// tool call from holding an MCP client for longer than it will wait.
const (
	defaultCommandTimeout = 60 * time.Second
	maxCommandTimeout     = 30 * time.Minute
	// waitSlack is how much longer than the command's own limit the HTTP
	// call is allowed to take, so the server's timeout fires first and
	// reports reason "timeout" instead of the request being cut.
	waitSlack = 15 * time.Second
)

func obj(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
func strList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}
func strMap(desc string) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": desc}
}

var sandboxIDProp = str("Sandbox id returned by create_sandbox.")

// decode parses tool arguments strictly: an unknown argument is an error
// the model can correct, not something to ignore silently.
func decode(args json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func (s *Server) buildTools() []tool {
	networkProps := map[string]any{
		"network_mode": map[string]any{"type": "string", "enum": []string{"none", "allowlist", "full"},
			"description": "Egress mode. It can be the template's mode or stricter, never wider."},
		"allow_domains": strList("Hostnames the sandbox may reach in allowlist mode, exact or '*.example.com'."),
	}
	return []tool{
		{
			Name:        "list_templates",
			Description: "List the sandbox templates (image, resource limits, network mode) available to create_sandbox.",
			Schema:      obj(nil, map[string]any{}),
			Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
				return s.passthrough(ctx, http.MethodGet, "/v1/templates", nil, nil)
			},
		},
		{
			Name: "create_sandbox",
			Description: "Create an isolated sandbox from a template and return its sandbox_id. " +
				"The sandbox has a writable /workspace, CPU, memory and process limits, and no network unless allowed. " +
				"It is destroyed at its timeout, after an idle period, or by kill_sandbox.",
			Schema: obj(nil, map[string]any{
				"template_id":     str("Template to create from. Optional when the server has a default; see list_templates."),
				"timeout_seconds": num("Maximum lifetime, at most the template's. Default: the template's."),
				"env":             strMap("Environment variables for every command in the sandbox."),
				"network_mode":    networkProps["network_mode"],
				"allow_domains":   networkProps["allow_domains"],
			}),
			Call: s.createSandbox,
		},
		{
			Name:        "list_sandboxes",
			Description: "List the sandboxes this account currently holds.",
			Schema:      obj(nil, map[string]any{}),
			Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
				return s.passthrough(ctx, http.MethodGet, "/v1/sandboxes", nil, nil)
			},
		},
		{
			Name:        "kill_sandbox",
			Description: "Destroy a sandbox and everything in it. This cannot be undone.",
			Schema:      obj([]string{"sandbox_id"}, map[string]any{"sandbox_id": sandboxIDProp}),
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					SandboxID string `json:"sandbox_id"`
				}
				if err := decode(args, &a); err != nil {
					return "", err
				}
				if a.SandboxID == "" {
					return "", errors.New("sandbox_id is required")
				}
				if _, err := s.passthrough(ctx, http.MethodDelete, sandboxPath(a.SandboxID, ""), nil, nil); err != nil {
					return "", err
				}
				return "Sandbox " + a.SandboxID + " is being destroyed.", nil
			},
		},
		{
			Name: "run_command",
			Description: "Run a shell command line in a sandbox with /bin/sh -c and return its exit code, stdout and stderr. " +
				"The command runs as an unprivileged user in /workspace. Long output is cut in the middle.",
			Schema: obj([]string{"sandbox_id", "command"}, map[string]any{
				"sandbox_id":      sandboxIDProp,
				"command":         str("Shell command line, for example 'ls -la && cat data.csv | head'."),
				"cwd":             str("Working directory. Default /workspace."),
				"timeout_seconds": num("Wall-clock limit; the command is killed when it expires. Default 60, at most 1800."),
				"env":             strMap("Extra environment variables for this command."),
			}),
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a runArgs
				if err := decode(args, &a); err != nil {
					return "", err
				}
				if strings.TrimSpace(a.Command) == "" {
					return "", errors.New("command is required")
				}
				return s.run(ctx, a, "/bin/sh", []string{"-c", a.Command})
			},
		},
		{
			Name: "run_python",
			Description: "Run Python source code in a sandbox with python3 and return its exit code, stdout and stderr. " +
				"Each call is a fresh interpreter: variables do not persist between calls, files in /workspace do. " +
				"The template's image must contain python3.",
			Schema: obj([]string{"sandbox_id", "code"}, map[string]any{
				"sandbox_id":      sandboxIDProp,
				"code":            str("Python source code to execute."),
				"cwd":             str("Working directory. Default /workspace."),
				"timeout_seconds": num("Wall-clock limit; the interpreter is killed when it expires. Default 60, at most 1800."),
				"env":             strMap("Extra environment variables for this run."),
			}),
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					runArgs
					Code string `json:"code"`
				}
				if err := decode(args, &a); err != nil {
					return "", err
				}
				if strings.TrimSpace(a.Code) == "" {
					return "", errors.New("code is required")
				}
				// The code travels as a file, not as an argument: one argv
				// element is limited to 128 KiB by the kernel.
				if a.SandboxID == "" {
					return "", errors.New("sandbox_id is required")
				}
				script := fmt.Sprintf("/tmp/sbx-mcp/run-%d.py", time.Now().UnixNano())
				if err := s.withControl(ctx, func(ctx context.Context) error {
					return s.api.WriteFile(ctx, a.SandboxID, script, "0600", []byte(a.Code))
				}); err != nil {
					return "", err
				}
				// /tmp is a small tmpfs charged to the sandbox's memory, so
				// the script is removed even when the run was cancelled.
				defer func() {
					_ = s.withControl(context.WithoutCancel(ctx), func(ctx context.Context) error {
						_, err := s.api.json(ctx, http.MethodDelete, sandboxPath(a.SandboxID, "/files"), url.Values{"path": {script}}, nil)
						return err
					})
				}()
				return s.run(ctx, a.runArgs, "python3", []string{script})
			},
		},
		{
			Name:        "write_file",
			Description: "Write a file in a sandbox, creating parent directories and replacing an existing file. Relative paths are under /workspace.",
			Schema: obj([]string{"sandbox_id", "path", "content"}, map[string]any{
				"sandbox_id": sandboxIDProp,
				"path":       str("Destination path."),
				"content":    str("File content."),
				"encoding": map[string]any{"type": "string", "enum": []string{"text", "base64"},
					"description": "How content is encoded. Default text; use base64 for binary data."},
			}),
			Call: s.writeFile,
		},
		{
			Name: "read_file",
			Description: "Read a file from a sandbox. Text is returned as is; binary data is returned base64-encoded. " +
				"Large files are cut at the server's limit, which the result states.",
			Schema: obj([]string{"sandbox_id", "path"}, map[string]any{
				"sandbox_id": sandboxIDProp,
				"path":       str("File to read. Relative paths are under /workspace."),
			}),
			Call: s.readFile,
		},
		{
			Name:        "list_files",
			Description: "List one directory of a sandbox (not recursive): name, size, mode, whether it is a directory.",
			Schema: obj([]string{"sandbox_id"}, map[string]any{
				"sandbox_id": sandboxIDProp,
				"path":       str("Directory to list. Default /workspace."),
			}),
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					SandboxID string `json:"sandbox_id"`
					Path      string `json:"path"`
				}
				if err := decode(args, &a); err != nil {
					return "", err
				}
				q := url.Values{}
				if a.Path != "" {
					q.Set("path", a.Path)
				}
				return s.passthrough(ctx, http.MethodGet, sandboxPath(a.SandboxID, "/files/list"), q, nil)
			},
		},
		{
			Name: "set_network",
			Description: "Replace the egress policy of a running sandbox, effective immediately. " +
				"Typical use: install dependencies with an allowlist, then lock down with mode none before running untrusted code. " +
				"A sandbox created with mode none has no proxy and cannot be given network access later.",
			Schema: obj([]string{"sandbox_id", "network_mode"}, map[string]any{
				"sandbox_id":    sandboxIDProp,
				"network_mode":  networkProps["network_mode"],
				"allow_domains": networkProps["allow_domains"],
			}),
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					SandboxID    string   `json:"sandbox_id"`
					NetworkMode  string   `json:"network_mode"`
					AllowDomains []string `json:"allow_domains"`
				}
				if err := decode(args, &a); err != nil {
					return "", err
				}
				policy := map[string]any{"mode": a.NetworkMode}
				if len(a.AllowDomains) > 0 {
					policy["allow_domains"] = a.AllowDomains
				}
				return s.passthrough(ctx, http.MethodPut, sandboxPath(a.SandboxID, "/network"), nil, policy)
			},
		},
	}
}

// withControl runs fn with the timeout of a call that does no sandboxed work.
func (s *Server) withControl(ctx context.Context, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	return fn(ctx)
}

// passthrough calls one control-plane route and returns its JSON answer
// re-indented, so tool results track the API without a second schema here.
func (s *Server) passthrough(ctx context.Context, method, path string, query url.Values, in any) (string, error) {
	var raw []byte
	err := s.withControl(ctx, func(ctx context.Context) (err error) {
		raw, err = s.api.json(ctx, method, path, query, in)
		return err
	})
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "OK", nil
	}
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") != nil {
		return string(raw), nil
	}
	return buf.String(), nil
}

func (s *Server) createSandbox(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		TemplateID     string            `json:"template_id"`
		TimeoutSeconds float64           `json:"timeout_seconds"`
		Env            map[string]string `json:"env"`
		NetworkMode    string            `json:"network_mode"`
		AllowDomains   []string          `json:"allow_domains"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.TemplateID == "" {
		a.TemplateID = s.cfg.DefaultTemplate
	}
	if a.TemplateID == "" {
		return "", errors.New("template_id is required (no default template is configured); call list_templates to see the choices")
	}
	if a.TimeoutSeconds < 0 {
		return "", errors.New("timeout_seconds must not be negative")
	}
	body := map[string]any{"template_id": a.TemplateID}
	if a.TimeoutSeconds > 0 {
		body["timeout_ms"] = int64(a.TimeoutSeconds * 1000)
	}
	if len(a.Env) > 0 {
		body["env"] = a.Env
	}
	if a.NetworkMode != "" || len(a.AllowDomains) > 0 {
		policy := map[string]any{}
		if a.NetworkMode != "" {
			policy["mode"] = a.NetworkMode
		}
		if len(a.AllowDomains) > 0 {
			policy["allow_domains"] = a.AllowDomains
		}
		body["network"] = policy
	}
	var raw []byte
	err := s.withControl(ctx, func(ctx context.Context) (err error) {
		raw, err = s.api.json(ctx, http.MethodPost, "/v1/sandboxes", nil, body)
		return err
	})
	if err != nil {
		return "", err
	}
	// The access token is a credential for the sandbox. This server uses
	// the API key for everything, so the token is dropped here and never
	// reaches the model's context.
	var created map[string]any
	if err := json.Unmarshal(raw, &created); err != nil {
		return "", fmt.Errorf("unexpected answer from sbxd: %w", err)
	}
	delete(created, "access_token")
	created["sandbox_id"] = created["id"]
	out, _ := json.MarshalIndent(created, "", "  ")
	return string(out), nil
}

type runArgs struct {
	SandboxID      string            `json:"sandbox_id"`
	Command        string            `json:"command"`
	Cwd            string            `json:"cwd"`
	TimeoutSeconds float64           `json:"timeout_seconds"`
	Env            map[string]string `json:"env"`
}

func (s *Server) run(ctx context.Context, a runArgs, cmd string, argv []string) (string, error) {
	if a.SandboxID == "" {
		return "", errors.New("sandbox_id is required")
	}
	timeout := defaultCommandTimeout
	if a.TimeoutSeconds < 0 {
		return "", errors.New("timeout_seconds must not be negative")
	}
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds * float64(time.Second))
	}
	if timeout > maxCommandTimeout {
		return "", fmt.Errorf("timeout_seconds is at most %d", int(maxCommandTimeout/time.Second))
	}
	start := map[string]any{"cmd": cmd, "args": argv, "timeout_ms": timeout.Milliseconds()}
	if a.Cwd != "" {
		start["cwd"] = a.Cwd
	}
	if len(a.Env) > 0 {
		start["env"] = a.Env
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+waitSlack)
	defer cancel()
	res, err := s.api.Run(ctx, a.SandboxID, start)
	if err != nil {
		return "", err
	}
	return s.formatRun(res, timeout), nil
}

func (s *Server) formatRun(res WaitResult, timeout time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\n", res.Exit.ExitCode)
	switch res.Exit.Reason {
	case "", "exited":
	case "timeout":
		fmt.Fprintf(&b, "reason: timeout (killed after %s)\n", timeout)
	case "oom":
		b.WriteString("reason: oom (killed for exceeding the sandbox memory limit)\n")
	default:
		fmt.Fprintf(&b, "reason: %s", res.Exit.Reason)
		if res.Exit.Signal != "" {
			fmt.Fprintf(&b, " (%s)", res.Exit.Signal)
		}
		b.WriteString("\n")
	}
	if res.Truncated {
		b.WriteString("note: the server cut the output at its limit\n")
	}
	writeStream(&b, "stdout", res.Stdout, s.cfg.MaxOutputBytes)
	writeStream(&b, "stderr", res.Stderr, s.cfg.MaxOutputBytes)
	return b.String()
}

func writeStream(b *strings.Builder, name string, data []byte, limit int) {
	if len(data) == 0 {
		fmt.Fprintf(b, "%s: (empty)\n", name)
		return
	}
	text, cut := headTail(strings.ToValidUTF8(string(data), "\uFFFD"), limit)
	fmt.Fprintf(b, "%s:\n%s", name, text)
	if !strings.HasSuffix(text, "\n") {
		b.WriteString("\n")
	}
	if cut > 0 {
		fmt.Fprintf(b, "note: %d bytes were cut from the middle of %s\n", cut, name)
	}
}

// headTail keeps the first and last halves of an over-long text, which is
// where a command's setup and its final error usually are.
func headTail(s string, limit int) (string, int) {
	if len(s) <= limit {
		return s, 0
	}
	half := limit / 2
	head, tail := s[:half], s[len(s)-half:]
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	for len(tail) > 0 && !utf8.ValidString(tail) {
		tail = tail[1:]
	}
	cut := len(s) - len(head) - len(tail)
	return head + fmt.Sprintf("\n[... %d bytes cut ...]\n", cut) + tail, cut
}

func (s *Server) writeFile(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		SandboxID string `json:"sandbox_id"`
		Path      string `json:"path"`
		Content   string `json:"content"`
		Encoding  string `json:"encoding"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.SandboxID == "" || a.Path == "" {
		return "", errors.New("sandbox_id and path are required")
	}
	data := []byte(a.Content)
	switch a.Encoding {
	case "", "text":
	case "base64":
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(a.Content))
		if err != nil {
			return "", fmt.Errorf("content is not valid base64: %w", err)
		}
		data = dec
	default:
		return "", fmt.Errorf("unknown encoding %q: use text or base64", a.Encoding)
	}
	if err := s.withControl(ctx, func(ctx context.Context) error {
		return s.api.WriteFile(ctx, a.SandboxID, a.Path, "", data)
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wrote %d bytes to %s.", len(data), a.Path), nil
}

func (s *Server) readFile(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		SandboxID string `json:"sandbox_id"`
		Path      string `json:"path"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.SandboxID == "" || a.Path == "" {
		return "", errors.New("sandbox_id and path are required")
	}
	var data []byte
	var more bool
	if err := s.withControl(ctx, func(ctx context.Context) (err error) {
		data, more, err = s.api.ReadFile(ctx, a.SandboxID, a.Path, s.cfg.MaxFileBytes)
		return err
	}); err != nil {
		return "", err
	}
	note := ""
	if more {
		note = fmt.Sprintf("\nnote: only the first %d bytes are shown; the file is larger", len(data))
	}
	if utf8.Valid(data) || (more && utf8.Valid(trimPartialRune(data))) {
		return string(trimPartialRune(data)) + note, nil
	}
	return "encoding: base64\n" + base64.StdEncoding.EncodeToString(data) + note, nil
}

// trimPartialRune drops a multi-byte character split by the size limit.
func trimPartialRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.Valid(b); i++ {
		b = b[:len(b)-1]
	}
	return b
}

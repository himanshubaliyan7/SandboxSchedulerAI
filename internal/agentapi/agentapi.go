// Package agentapi is the wire contract between the host and the in-sandbox
// agent (cmd/sbx-agent). The agent serves HTTP on a Unix socket; the host
// side never trusts anything it reads from it beyond this schema.
//
// Routes (all under /v1):
//
//	GET    /health                         -> Health
//	POST   /init                InitRequest -> 204   (once, at assignment)
//	POST   /processes          StartRequest -> ProcessInfo
//	GET    /processes                       -> []ProcessInfo
//	GET    /processes/{id}                  -> ProcessInfo
//	GET    /processes/{id}/stream?from=N    -> NDJSON stream of Event
//	POST   /processes/{id}/stdin  raw body  -> 204
//	POST   /processes/{id}/close-stdin      -> 204   (pipe mode only)
//	POST   /processes/{id}/signal SignalRequest -> 204
//	POST   /processes/{id}/resize ResizeRequest -> 204 (PTY mode only)
//	GET    /processes/{id}/ws?from=N        -> WebSocket, binary frames (see Channel*)
//	GET    /files?path=P                    -> raw bytes
//	PUT    /files?path=P&mode=0644 raw body -> 204
//	DELETE /files?path=P                    -> 204
//	GET    /files/list?path=P               -> []FileInfo
//	GET    /files/stat?path=P               -> FileInfo
//	POST   /files/mkdir?path=P              -> 201 (created) or 204 (already there)
//
// Errors are returned as Error with a matching HTTP status. The codes are
// bad_request, too_large, no_route, already_initialized, egress_failed,
// internal (general); bad_cwd, command_not_found, exec_format,
// permission_denied, tag_in_use, start_failed, pty_failed, not_found,
// process_exited, stdin_closed, is_a_pty, not_a_pty, bad_signal
// (processes); not_found, permission_denied, conflict, is_directory,
// no_space, io_error (files).
//
// The stream route sends its status line and headers at once, before the
// first event, so a caller can tell "accepted, nothing to report yet" from
// "not answered".
package agentapi

import "time"

const Version = "v1"

// Health is returned by GET /health. Ready is the readiness check the pool
// uses before marking a sandbox WARM.
type Health struct {
	Ready        bool   `json:"ready"`
	AgentVersion string `json:"agent_version"`
	Initialized  bool   `json:"initialized"`
}

// InitRequest injects per-session configuration at assignment. It is the
// only way a claim can vary a sandbox; the template itself is immutable.
type InitRequest struct {
	SandboxID  string            `json:"sandbox_id"`
	Generation int64             `json:"generation"`
	Env        map[string]string `json:"env,omitempty"`
	// EgressProxy, when true, makes the agent bridge 127.0.0.1:3128 inside
	// the sandbox to the egress Unix socket and export HTTP(S)_PROXY to
	// every process it starts.
	EgressProxy bool `json:"egress_proxy"`
}

// PTYSize is a terminal size in character cells.
type PTYSize struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// StartRequest starts a process. With PTY nil the process runs in pipe
// mode (separate stdout and stderr, closable stdin); with PTY set it runs
// on a pseudo-terminal (one merged stream, resize, Ctrl+D for EOF).
type StartRequest struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
	PTY  *PTYSize          `json:"pty,omitempty"`
	// Stdin keeps stdin open in pipe mode. Default false: stdin is /dev/null.
	Stdin bool `json:"stdin,omitempty"`
	// Tag is an optional caller-supplied name, unique among live processes.
	Tag string `json:"tag,omitempty"`
	// TimeoutMs is a per-command wall-clock limit; 0 means none. On expiry
	// the whole process group is SIGKILLed and the exit reason is "timeout".
	TimeoutMs int64 `json:"timeout_ms,omitempty"`
}

// ExitReason says why a process ended.
type ExitReason string

const (
	ExitExited  ExitReason = "exited"
	ExitKilled  ExitReason = "killed"  // by signal
	ExitTimeout ExitReason = "timeout" // per-command timeout
	ExitOOM     ExitReason = "oom"
)

// ExitStatus is the explicit terminal event of every process.
type ExitStatus struct {
	ExitCode int        `json:"exit_code"`
	Signal   string     `json:"signal,omitempty"`
	Reason   ExitReason `json:"reason"`
}

// ProcessInfo describes a process known to the agent. Exited processes are
// kept (with their output ring buffer) until the sandbox is destroyed.
type ProcessInfo struct {
	ID        string      `json:"id"`
	Tag       string      `json:"tag,omitempty"`
	Cmd       string      `json:"cmd"`
	Args      []string    `json:"args,omitempty"`
	PTY       bool        `json:"pty"`
	StartedAt time.Time   `json:"started_at"`
	Exit      *ExitStatus `json:"exit,omitempty"`
	// NextOffset is the offset the next output byte will get; a client that
	// has consumed everything resumes with from=NextOffset.
	NextOffset int64 `json:"next_offset"`
}

// EventType discriminates Event.
type EventType string

const (
	EventStdout    EventType = "stdout"
	EventStderr    EventType = "stderr"
	EventPTY       EventType = "pty"
	EventExit      EventType = "exit"
	EventTruncated EventType = "truncated" // requested offset fell out of the ring buffer
	EventKeepalive EventType = "keepalive"
)

// Event is one line of the NDJSON stream. Offsets are a single monotonic
// byte counter per process across stdout, stderr and pty output, so a
// reconnecting client passes from=<last Offset + len(Data)> and receives
// either a lossless replay or one EventTruncated carrying Dropped.
type Event struct {
	Type    EventType   `json:"type"`
	Offset  int64       `json:"offset"`
	Data    []byte      `json:"data,omitempty"` // base64 in JSON
	Dropped int64       `json:"dropped,omitempty"`
	Exit    *ExitStatus `json:"exit,omitempty"`
}

// SignalRequest sends a signal to the process group.
type SignalRequest struct {
	Signal string `json:"signal"` // SIGINT, SIGTERM, SIGKILL, SIGHUP, SIGQUIT
}

// ResizeRequest resizes a PTY.
type ResizeRequest struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// FileInfo describes a file or directory: one entry of a listing, or the
// answer of the stat route.
type FileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    uint32    `json:"mode"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// Error is the JSON error body. Details is only set by the gateway, in
// the status frame that precedes an application close code: the same
// code-specific details as the API's error envelope (state and reason
// for sandbox_terminated).
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// WebSocket binary frames carry a one-byte channel prefix. Channels 0-4
// match the Kubernetes remotecommand protocol; the rest are ours.
const (
	ChannelStdin  byte = 0 // client -> agent: raw input bytes
	ChannelStdout byte = 1 // agent -> client: raw output (PTY output is sent here)
	ChannelStderr byte = 2 // agent -> client: raw stderr (pipe mode)
	ChannelStatus byte = 3 // agent -> client: JSON StatusFrame, then normal close
	ChannelResize byte = 4 // client -> agent: JSON ResizeRequest
	ChannelSignal byte = 5 // client -> agent: JSON SignalRequest
	ChannelAck    byte = 6 // client -> agent: JSON AckFrame (flow control)
	ChannelPing   byte = 7 // either direction: opaque payload, echoed on ChannelPong
	ChannelPong   byte = 8
	ChannelOffset byte = 9 // agent -> client: JSON OffsetFrame, sent first and after a truncation
)

// StatusFrame is the terminal frame on ChannelStatus. Exit is set when the
// process ended; Error when the session was ended for another reason, by
// the agent (bad_frame, bad_signal, stdin_overflow) or by the gateway
// (the API error code that is also the close reason, see Close*).
type StatusFrame struct {
	Exit  *ExitStatus `json:"exit,omitempty"`
	Error *Error      `json:"error,omitempty"`
}

// AckFrame tells the agent the client has processed output up to Offset.
// The agent stops reading from the process once unacknowledged bytes
// exceed FlowHighWatermark and resumes below FlowLowWatermark, so a slow
// consumer pauses the producer instead of growing memory.
type AckFrame struct {
	Offset int64 `json:"offset"`
}

// OffsetFrame announces the offset of the next output byte and how many
// bytes were dropped before it (non-zero when replay was truncated).
type OffsetFrame struct {
	Offset  int64 `json:"offset"`
	Dropped int64 `json:"dropped,omitempty"`
}

const (
	FlowHighWatermark = 100_000
	FlowLowWatermark  = 10_000
	// DefaultRingBytes is the per-process replay buffer.
	DefaultRingBytes = 1 << 20
	// KeepaliveInterval is below the 60 s idle timeout common to proxies.
	KeepaliveInterval = 25 * time.Second
	// MaxFrameBytes bounds a single output frame or event payload.
	MaxFrameBytes = 32 << 10
	// MaxClientPayloadBytes bounds the payload of one WebSocket frame from
	// a client (stdin bytes, or a JSON control document), and
	// MaxClientFrameBytes the whole frame: the channel byte plus the
	// payload. These two are the only definition of the limit: the agent
	// and the gateway both set their read limit from MaxClientFrameBytes,
	// and api/openapi.yaml quotes the same numbers (a test compares them).
	MaxClientPayloadBytes = 1 << 20
	MaxClientFrameBytes   = 1 + MaxClientPayloadBytes
)

// WebSocket close codes.
//
// 1000 follows the ChannelStatus frame of a session that ended by itself:
// the process exited, or the agent refused a frame (StatusFrame.Error).
// 1009 answers a client frame larger than MaxClientFrameBytes. When the
// gateway has to end a session for a reason of its own it uses one of the
// application codes below. The close reason is then the API error code as
// a bare token (for example "sandbox_not_running"), which is more
// specific than the close code and always shorter than the 123 bytes a
// close frame can carry. Where the connection got as far as the upgrade,
// a ChannelStatus frame with the same code and a message for people is
// sent just before the close.
//
// A client that cannot read the HTTP response of a refused handshake (a
// browser) asks for the refusal in this form with the query parameter
// handshake_errors=close; see CloseCodeFor for the mapping.
const (
	// CloseSandboxTerminated: the sandbox is not running (any more). Final.
	CloseSandboxTerminated = 4000
	// CloseUnauthorized: missing or wrong credentials. Final.
	CloseUnauthorized = 4001
	// CloseShuttingDown: the server is stopping. The command may survive a
	// restart only with the "leave" shutdown policy; reattaching is worth
	// one try.
	CloseShuttingDown = 4002
	// CloseAgentUnavailable: the link to the in-sandbox agent broke while
	// the sandbox is still running. Reattach with from=<offset>.
	CloseAgentUnavailable = 4003
	// CloseNotFound: no such sandbox or command. Final.
	CloseNotFound = 4004
	// CloseBadRequest: the handshake request was malformed. Final.
	CloseBadRequest = 4005
	// CloseRateLimited: the tenant's bucket is empty. Retry later.
	CloseRateLimited = 4029
	// CloseError: anything else (internal error, timeout). Retry.
	CloseError = 4099
)

// CloseCodeFor maps an API error code to the WebSocket close code the
// gateway uses for it.
func CloseCodeFor(apiCode string) int {
	switch apiCode {
	case "sandbox_terminated", "sandbox_not_running":
		return CloseSandboxTerminated
	case "unauthorized":
		return CloseUnauthorized
	case "shutting_down":
		return CloseShuttingDown
	case "agent_unavailable":
		return CloseAgentUnavailable
	case "not_found", "process_not_found":
		return CloseNotFound
	case "bad_request":
		return CloseBadRequest
	case "rate_limited":
		return CloseRateLimited
	}
	return CloseError
}

// Package agent is the in-sandbox agent: the daemon that runs as each
// sandbox's long-lived process, owns every process and PTY started in it,
// buffers their output for replay, and serves files. It implements the
// wire contract in package agentapi over HTTP on a Unix socket.
//
// The agent runs inside the sandbox and is not a security boundary: any
// code in the sandbox can do whatever the agent can. Authentication and
// authorisation happen on the host side, before a request reaches the
// socket.
package agent

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime"
)

// AgentVersion is reported by GET /health.
var AgentVersion = "0.1.0"

// Defaults for Config fields left zero.
const (
	DefaultSocketMode       = 0o600
	DefaultEgressListenAddr = "127.0.0.1:3128"
	DefaultDrainGrace       = time.Second
	DefaultMaxExited        = 4096
	DefaultMaxRetainedBytes = 64 << 20
	// maxWSStdinQueue bounds stdin bytes accepted on one WebSocket that
	// the process has not consumed yet. It is one full-size frame, so a
	// frame of the maximum size is always accepted on an empty queue.
	maxWSStdinQueue = agentapi.MaxClientPayloadBytes
)

// Config configures an Agent. The zero value of every field except
// SocketPath selects the production default.
type Config struct {
	// SocketPath is the Unix socket to listen on.
	SocketPath string
	// SocketMode is the socket's permission bits (default 0600).
	SocketMode os.FileMode
	// Workspace is the default working directory of processes and the
	// base of relative file paths (default runtime.Workdir).
	Workspace string
	// RingBytes is the per-process replay buffer (default agentapi.DefaultRingBytes).
	RingBytes int
	// KeepaliveInterval is the idle interval after which an NDJSON stream
	// carries a keepalive event (default agentapi.KeepaliveInterval).
	KeepaliveInterval time.Duration
	// EgressSocket is the Unix socket the egress bridge forwards to
	// (default: runtime.EgressSocket next to SocketPath).
	EgressSocket string
	// EgressListenAddr is the in-sandbox TCP address of the egress bridge
	// (default 127.0.0.1:3128).
	EgressListenAddr string
	// DrainGrace is how long after a process exits the agent keeps
	// reading its output while a descendant still holds the pipe or PTY
	// open, before reporting the exit (default 1s).
	DrainGrace time.Duration
	// ReapAll makes the agent reap every child of its OS process,
	// including orphans re-parented to it. It must be true when the agent
	// is PID 1 or a child subreaper and false when it is embedded in a
	// process that waits for children of its own.
	ReapAll bool
	// BaseEnv is the environment every process starts from (default
	// os.Environ()).
	BaseEnv []string
	// MaxExited bounds how many exited processes are remembered; the
	// oldest are forgotten first (default 4096, negative for no limit).
	MaxExited int
	// MaxRetainedBytes bounds the replay memory held for exited
	// processes; the oldest exited processes lose their buffered output
	// first, after which replaying them yields a truncated event
	// (default 64 MiB, negative for no limit).
	MaxRetainedBytes int64
	// Logger receives diagnostics (default: discard).
	Logger *log.Logger
}

func (c Config) withDefaults() Config {
	if c.SocketMode == 0 {
		c.SocketMode = DefaultSocketMode
	}
	if c.Workspace == "" {
		c.Workspace = runtime.Workdir
	}
	if c.RingBytes <= 0 {
		c.RingBytes = agentapi.DefaultRingBytes
	}
	if c.KeepaliveInterval <= 0 {
		c.KeepaliveInterval = agentapi.KeepaliveInterval
	}
	if c.EgressSocket == "" && c.SocketPath != "" {
		c.EgressSocket = filepath.Join(filepath.Dir(c.SocketPath), runtime.EgressSocket)
	}
	if c.EgressListenAddr == "" {
		c.EgressListenAddr = DefaultEgressListenAddr
	}
	if c.DrainGrace <= 0 {
		c.DrainGrace = DefaultDrainGrace
	}
	if c.BaseEnv == nil {
		c.BaseEnv = os.Environ()
	}
	if c.MaxExited == 0 {
		c.MaxExited = DefaultMaxExited
	}
	if c.MaxRetainedBytes == 0 {
		c.MaxRetainedBytes = DefaultMaxRetainedBytes
	}
	if c.Logger == nil {
		c.Logger = log.New(io.Discard, "", 0)
	}
	return c
}

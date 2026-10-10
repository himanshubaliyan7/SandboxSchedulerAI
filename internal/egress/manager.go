package egress

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/himanshubaliyan7/warmbox/internal/core"
)

var (
	// ErrClosed is returned by Attach and Update after Close.
	ErrClosed = errors.New("egress: manager closed")
	// ErrAlreadyAttached is returned by Attach for a sandbox ID or socket
	// path that is already attached.
	ErrAlreadyAttached = errors.New("egress: sandbox already attached")
	// ErrNotAttached is returned by Update for an unknown sandbox ID.
	ErrNotAttached = errors.New("egress: sandbox not attached")
)

// detachWait bounds how long Detach waits for connection goroutines.
const detachWait = 5 * time.Second

// Manager owns one listening Unix socket per attached sandbox and enforces
// that sandbox's policy on everything arriving on it. All methods are safe
// for concurrent use.
type Manager struct {
	cfg   Config
	deny  *denySet
	ports map[uint16]struct{} // nil: all ports

	mu        sync.Mutex
	closed    bool
	sandboxes map[string]*sandbox

	// dialRewrite is nil in production. It exists so tests can point a
	// validated public address at a loopback test server. Nothing outside
	// _test.go files assigns it (see hooks_test.go, which also asserts
	// that), it is not reachable from Config, and it does not weaken the
	// deny set: the pipeline still validates the original address.
	dialRewrite func(netip.AddrPort) (netip.AddrPort, bool)
}

// New returns a Manager. It fails on invalid configuration or if the host
// interface addresses cannot be read.
func New(cfg Config) (*Manager, error) {
	return newManager(cfg, net.InterfaceAddrs)
}

func newManager(cfg Config, ifaceAddrs func() ([]net.Addr, error)) (*Manager, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	deny, err := newDenySet(cfg.PermitPrivateCIDRs, cfg.HostAddrRefresh, ifaceAddrs)
	if err != nil {
		return nil, err
	}
	m := &Manager{cfg: cfg, deny: deny, sandboxes: map[string]*sandbox{}}
	if len(cfg.AllowedPorts) > 0 {
		m.ports = make(map[uint16]struct{}, len(cfg.AllowedPorts))
		for _, p := range cfg.AllowedPorts {
			m.ports[p] = struct{}{}
		}
	}
	return m, nil
}

// Attach creates the Unix socket at socketPath and starts serving sandboxID
// under policy. A stale socket at the path is replaced; anything else at
// the path is an error. The parent directory must exist. See the package
// documentation for the ownership and mode requirements.
func (m *Manager) Attach(sandboxID, socketPath string, policy core.NetworkPolicy) error {
	if sandboxID == "" || socketPath == "" {
		return errors.New("egress: sandbox ID and socket path are required")
	}
	pol, err := compilePolicy(policy)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if _, ok := m.sandboxes[sandboxID]; ok {
		return fmt.Errorf("%w: %s", ErrAlreadyAttached, sandboxID)
	}
	for _, other := range m.sandboxes {
		if other.path == socketPath {
			return fmt.Errorf("%w: socket %s belongs to %s", ErrAlreadyAttached, socketPath, other.id)
		}
	}
	ln, err := listenUnix(socketPath, m.cfg.SocketMode)
	if err != nil {
		return fmt.Errorf("egress: attach %s: %w", sandboxID, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sb := &sandbox{
		m:      m,
		id:     sandboxID,
		path:   socketPath,
		ln:     ln,
		ctx:    ctx,
		cancel: cancel,
		conns:  map[*conn]struct{}{},
		flows:  map[*flow]struct{}{},
		denied: map[Reason]uint64{},
		errs:   map[Reason]uint64{},
	}
	sb.policy.Store(pol)
	m.sandboxes[sandboxID] = sb
	sb.wg.Add(1)
	go sb.acceptLoop()
	return nil
}

// Update atomically replaces the policy of an attached sandbox. Requests
// decided after Update returns use the new policy; established connections
// the new policy does not permit are closed before Update returns.
func (m *Manager) Update(sandboxID string, policy core.NetworkPolicy) error {
	pol, err := compilePolicy(policy)
	if err != nil {
		return err
	}
	m.mu.Lock()
	closed := m.closed
	sb := m.sandboxes[sandboxID]
	m.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if sb == nil {
		return fmt.Errorf("%w: %s", ErrNotAttached, sandboxID)
	}
	var doomed []*flow
	sb.mu.Lock()
	sb.policy.Store(pol)
	for f := range sb.flows {
		if !pol.permitsEstablished(f.t, f.ip) {
			doomed = append(doomed, f)
		}
	}
	sb.mu.Unlock()
	for _, f := range doomed {
		f.kill(ReasonPolicyUpdated)
	}
	return nil
}

// Detach stops serving a sandbox: it closes the listener and every
// connection and removes the socket. Detaching an unknown sandbox is not an
// error.
func (m *Manager) Detach(sandboxID string) error {
	m.mu.Lock()
	sb := m.sandboxes[sandboxID]
	delete(m.sandboxes, sandboxID)
	m.mu.Unlock()
	if sb == nil {
		return nil
	}
	return sb.shutdown()
}

// Close detaches every sandbox. The Manager cannot be used afterwards.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	all := make([]*sandbox, 0, len(m.sandboxes))
	for id, sb := range m.sandboxes {
		all = append(all, sb)
		delete(m.sandboxes, id)
	}
	m.mu.Unlock()
	for _, sb := range all {
		if err := sb.shutdown(); err != nil {
			m.cfg.Logger.Warn("egress: detach on close", "sandbox_id", sb.id, "error", err)
		}
	}
}

// Stats returns a snapshot of a sandbox's counters. ok is false if the
// sandbox is not attached.
func (m *Manager) Stats(sandboxID string) (st Stats, ok bool) {
	m.mu.Lock()
	sb := m.sandboxes[sandboxID]
	m.mu.Unlock()
	if sb == nil {
		return Stats{}, false
	}
	return sb.snapshot(), true
}

// listenUnix binds a Unix socket with the given mode.
func listenUnix(path string, mode fs.FileMode) (*net.UnixListener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	lc := net.ListenConfig{
		// On Linux the mode of the unbound socket's inode (masked by
		// the umask) becomes the mode of the file bind creates, so
		// restricting it here means the socket never exists with wider
		// permissions than requested. Best effort; the chmod below is
		// what guarantees the final mode.
		Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) { _ = unix.Fchmod(int(fd), uint32(mode.Perm())) })
		},
	}
	l, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, err
	}
	ul := l.(*net.UnixListener)
	if err := chmodSocket(path, mode); err != nil {
		ul.Close()
		return nil, err
	}
	return ul, nil
}

// chmodSocket sets the mode of the socket at path without following a
// symlink: the directory is writable from inside the sandbox, so the path
// may have been swapped for a link to a host file.
func chmodSocket(path string, mode fs.FileMode) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf("%s is no longer a socket", path)
	}
	if fs.FileMode(st.Mode&0o777) == mode.Perm() {
		return nil
	}
	if err := unix.Chmod(fmt.Sprintf("/proc/self/fd/%d", fd), uint32(mode.Perm())); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// sandbox is the state behind one socket.
type sandbox struct {
	m      *Manager
	id     string
	path   string
	ln     *net.UnixListener
	ctx    context.Context
	cancel context.CancelFunc
	policy atomic.Pointer[policy]
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
	conns  map[*conn]struct{}
	flows  map[*flow]struct{}
	// Counters; bytes are those of finished flows.
	allowed   uint64
	denied    map[Reason]uint64
	errs      map[Reason]uint64
	bytesUp   int64
	bytesDown int64
}

func (sb *sandbox) acceptLoop() {
	defer sb.wg.Done()
	backoff := 5 * time.Millisecond
	for {
		uc, err := sb.ln.AcceptUnix()
		if err != nil {
			if sb.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// Typically EMFILE; do not spin.
			select {
			case <-sb.ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Millisecond
		c := newConn(sb, uc)
		sb.mu.Lock()
		switch {
		case sb.closed:
			sb.mu.Unlock()
			uc.Close()
		case len(sb.conns) >= sb.m.cfg.MaxConnsPerSandbox:
			sb.mu.Unlock()
			c.rejectOverLimit()
		default:
			sb.conns[c] = struct{}{}
			sb.wg.Add(1)
			sb.mu.Unlock()
			go c.serve()
		}
	}
}

func (sb *sandbox) removeConn(c *conn) {
	sb.mu.Lock()
	delete(sb.conns, c)
	sb.mu.Unlock()
}

// addFlow registers an established upstream connection. pol is the policy
// the decision was made under; if the policy has been replaced since, the
// flow is re-evaluated under the current one, so an Update can never be
// outrun by a request that was already in the pipeline.
func (sb *sandbox) addFlow(f *flow, pol *policy) (Reason, bool) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.closed {
		return ReasonDetached, false
	}
	if cur := sb.policy.Load(); cur != pol && !cur.permitsEstablished(f.t, f.ip) {
		return ReasonPolicyChanged, false
	}
	sb.flows[f] = struct{}{}
	sb.allowed++ // counted when established, so live tunnels show up
	return "", true
}

func (sb *sandbox) removeFlow(f *flow) {
	sb.mu.Lock()
	if _, ok := sb.flows[f]; ok {
		delete(sb.flows, f)
		sb.bytesUp += f.up.Load()
		sb.bytesDown += f.down.Load()
	}
	sb.mu.Unlock()
}

func (sb *sandbox) shutdown() error {
	sb.mu.Lock()
	sb.closed = true
	conns := make([]*conn, 0, len(sb.conns))
	for c := range sb.conns {
		conns = append(conns, c)
	}
	flows := make([]*flow, 0, len(sb.flows))
	for f := range sb.flows {
		flows = append(flows, f)
	}
	sb.mu.Unlock()

	sb.cancel()
	err := sb.ln.Close()
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	for _, f := range flows {
		f.kill(ReasonDetached)
	}
	for _, c := range conns {
		c.raw.Close()
	}
	// The listener unlinks its socket on Close; remove a leftover socket
	// (never anything else) in case that failed.
	if fi, lerr := os.Lstat(sb.path); lerr == nil && fi.Mode()&fs.ModeSocket != 0 {
		if rerr := os.Remove(sb.path); rerr != nil && err == nil {
			err = rerr
		}
	}
	done := make(chan struct{})
	go func() { sb.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(detachWait):
		if err == nil {
			err = fmt.Errorf("egress: detach %s: connection handlers still draining after %s", sb.id, detachWait)
		}
	}
	return err
}

func (sb *sandbox) snapshot() Stats {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	st := Stats{
		Allowed:     sb.allowed,
		Denied:      make(map[Reason]uint64, len(sb.denied)),
		Errors:      make(map[Reason]uint64, len(sb.errs)),
		BytesUp:     sb.bytesUp,
		BytesDown:   sb.bytesDown,
		ActiveConns: len(sb.conns),
	}
	for k, v := range sb.denied {
		st.Denied[k] = v
	}
	for k, v := range sb.errs {
		st.Errors[k] = v
	}
	for f := range sb.flows {
		st.BytesUp += f.up.Load()
		st.BytesDown += f.down.Load()
	}
	return st
}

// record counts, reports and logs the decision for a request that never
// got an upstream connection.
func (sb *sandbox) record(d Decision) { sb.emit(d, false) }

// emit counts, reports and logs one decision. established says the
// decision belongs to a flow that addFlow already counted as allowed.
func (sb *sandbox) emit(d Decision, established bool) {
	d.Time = time.Now()
	d.SandboxID = sb.id
	sb.mu.Lock()
	if established && d.Verdict != VerdictAllowed {
		sb.allowed-- // it turned into a refusal or failure after all
	}
	switch d.Verdict {
	case VerdictAllowed:
		if !established {
			sb.allowed++
		}
	case VerdictDenied:
		sb.denied[d.Reason]++
	default:
		sb.errs[d.Reason]++
	}
	sb.mu.Unlock()
	if cb := sb.m.cfg.OnDecision; cb != nil {
		cb(d)
	}
	level := slog.LevelInfo
	if d.Verdict == VerdictDenied {
		level = slog.LevelWarn
	}
	log := sb.m.cfg.Logger
	if !log.Enabled(context.Background(), level) {
		return
	}
	ip := ""
	if d.ResolvedIP.IsValid() {
		ip = d.ResolvedIP.String()
	}
	log.LogAttrs(context.Background(), level, "egress decision",
		slog.String("sandbox_id", d.SandboxID),
		slog.String("protocol", d.Protocol),
		slog.String("host", d.Host),
		slog.Int("port", int(d.Port)),
		slog.String("resolved_ip", ip),
		slog.String("verdict", string(d.Verdict)),
		slog.String("reason", string(d.Reason)),
		slog.String("detail", d.Detail),
		slog.Int64("bytes_up", d.BytesUp),
		slog.Int64("bytes_down", d.BytesDown),
		slog.Duration("duration", d.Duration),
	)
}

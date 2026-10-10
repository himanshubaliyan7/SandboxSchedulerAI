package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
)

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// manager is the registry of processes.
type manager struct {
	cfg    Config
	reaper *reaper

	mu         sync.Mutex
	procs      map[string]*Process
	order      []*Process      // start order
	exited     []*Process      // exit order, oldest first
	reserved   map[string]bool // tags of processes being started
	sessionEnv []string        // set by Init: proxy variables, then InitRequest.Env
}

func newManager(cfg Config, r *reaper) *manager {
	return &manager{cfg: cfg, reaper: r, procs: make(map[string]*Process), reserved: make(map[string]bool)}
}

func (m *manager) setSessionEnv(env []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionEnv = env
}

// get finds a process by id or, failing that, the most recently started
// process carrying that tag.
func (m *manager) get(idOrTag string) (*Process, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.procs[idOrTag]; ok {
		return p, nil
	}
	for i := len(m.order) - 1; i >= 0; i-- {
		if m.order[i].tag == idOrTag && idOrTag != "" {
			return m.order[i], nil
		}
	}
	return nil, &apiError{http.StatusNotFound, "not_found", "no such process: " + idOrTag}
}

func (m *manager) list() []agentapi.ProcessInfo {
	m.mu.Lock()
	ps := append([]*Process(nil), m.order...)
	m.mu.Unlock()
	out := make([]agentapi.ProcessInfo, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.info())
	}
	return out
}

// killAll SIGKILLs every running process group.
func (m *manager) killAll() {
	m.mu.Lock()
	ps := append([]*Process(nil), m.order...)
	m.mu.Unlock()
	for _, p := range ps {
		p.kill()
	}
}

// mergeEnv concatenates KEY=VALUE lists; later entries override earlier
// ones. The result is sorted so that it is deterministic.
func mergeEnv(lists ...[]string) []string {
	vals := make(map[string]string)
	for _, l := range lists {
		for _, kv := range l {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				continue
			}
			vals[k] = v
		}
	}
	out := make([]string, 0, len(vals))
	for k, v := range vals {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func getenv(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// lookPath resolves cmd the way execvp would, against the PATH of the
// process being started (not the agent's own) and its working directory.
func lookPath(cmd, pathEnv, dir string) (string, error) {
	check := func(path string) error {
		full := path
		if !filepath.IsAbs(full) && dir != "" {
			full = filepath.Join(dir, full)
		}
		fi, err := os.Stat(full)
		if err != nil {
			return err
		}
		if fi.IsDir() || fi.Mode()&0o111 == 0 {
			return syscall.EACCES
		}
		return nil
	}
	if strings.Contains(cmd, "/") {
		if err := check(cmd); err != nil {
			return "", err
		}
		return cmd, nil
	}
	var firstErr error
	for _, d := range filepath.SplitList(pathEnv) {
		if d == "" {
			d = "."
		}
		path := filepath.Join(d, cmd)
		if !filepath.IsAbs(path) {
			path = "./" + path
		}
		err := check(path)
		if err == nil {
			return path, nil
		}
		if firstErr == nil && !errors.Is(err, os.ErrNotExist) {
			firstErr = err
		}
	}
	if firstErr != nil {
		return "", firstErr
	}
	return "", os.ErrNotExist
}

func startError(cmd string, err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return &apiError{http.StatusBadRequest, "command_not_found", "command not found: " + cmd}
	case errors.Is(err, os.ErrPermission):
		return &apiError{http.StatusForbidden, "permission_denied", "cannot execute " + cmd + ": " + err.Error()}
	case errors.Is(err, syscall.ENOEXEC):
		return &apiError{http.StatusBadRequest, "exec_format", "cannot execute " + cmd + ": " + err.Error()}
	}
	return &apiError{http.StatusInternalServerError, "start_failed", "cannot start " + cmd + ": " + err.Error()}
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on Linux
	}
	return "p" + hex.EncodeToString(b[:])
}

// start launches a process as described by req.
func (m *manager) start(req agentapi.StartRequest) (*Process, error) {
	if req.Cmd == "" {
		return nil, &apiError{http.StatusBadRequest, "bad_request", "cmd is required"}
	}
	if req.TimeoutMs < 0 {
		return nil, &apiError{http.StatusBadRequest, "bad_request", "timeout_ms must not be negative"}
	}
	if req.PTY != nil && (req.PTY.Cols == 0 || req.PTY.Rows == 0) {
		return nil, &apiError{http.StatusBadRequest, "bad_request", "pty cols and rows must be positive"}
	}

	// Working directory: the request's, else the workspace if it exists.
	cwd := req.Cwd
	if cwd != "" && !filepath.IsAbs(cwd) {
		cwd = filepath.Join(m.cfg.Workspace, cwd)
	}
	if cwd == "" {
		if fi, err := os.Stat(m.cfg.Workspace); err == nil && fi.IsDir() {
			cwd = m.cfg.Workspace
		}
	} else if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return nil, &apiError{http.StatusBadRequest, "bad_cwd", "working directory is not a directory: " + cwd}
	}

	// Environment: base < session (proxy, then InitRequest.Env) < request.
	m.mu.Lock()
	session := m.sessionEnv
	m.mu.Unlock()
	var ptyEnv []string
	if req.PTY != nil {
		// Overrides the agent's own TERM, which describes nothing useful;
		// the session and the request can still override it.
		ptyEnv = []string{"TERM=xterm-256color"}
	}
	env := mergeEnv([]string{"PATH=" + defaultPath}, m.cfg.BaseEnv, ptyEnv, session, envList(req.Env))
	if cwd != "" {
		env = mergeEnv(env, []string{"PWD=" + cwd})
	}

	pathEnv, _ := getenv(env, "PATH")
	path, err := lookPath(req.Cmd, pathEnv, cwd)
	if err != nil {
		return nil, startError(req.Cmd, err)
	}

	p := &Process{
		id:         newID(),
		tag:        req.Tag,
		cmd:        req.Cmd,
		args:       append([]string(nil), req.Args...),
		isPTY:      req.PTY != nil,
		started:    time.Now().UTC(),
		drainGrace: m.cfg.DrainGrace,
		oomBefore:  readOOMKills(),
		onDone:     m.onDone,
		ring:       newRing(m.cfg.RingBytes),
		changed:    make(chan struct{}),
		flows:      make(map[*flowConn]struct{}),
		pumpsDone:  make(chan struct{}),
	}

	// Files for the child (closed in the parent after the fork) and the
	// parent's ends.
	var childFiles [3]*os.File
	var toClose []*os.File // child ends
	var mine []*os.File    // parent ends, closed if the start fails
	fail := func(err error) (*Process, error) {
		for _, f := range append(toClose, mine...) {
			_ = f.Close()
		}
		return nil, err
	}
	type out struct {
		f  *os.File
		ch agentapi.EventType
	}
	var outs []out
	sys := &syscall.SysProcAttr{Setsid: true}

	if req.PTY != nil {
		ptm, pts, err := pty.Open()
		if err != nil {
			return fail(&apiError{http.StatusInternalServerError, "pty_failed", "cannot allocate a pty: " + err.Error()})
		}
		toClose = append(toClose, pts)
		mine = append(mine, ptm)
		if err := pty.Setsize(ptm, &pty.Winsize{Cols: req.PTY.Cols, Rows: req.PTY.Rows}); err != nil {
			return fail(&apiError{http.StatusInternalServerError, "pty_failed", "cannot size the pty: " + err.Error()})
		}
		childFiles = [3]*os.File{pts, pts, pts}
		sys.Setctty = true
		sys.Ctty = 0 // fd in the child
		p.ptm = ptm
		p.stdin = ptm
		outs = []out{{ptm, agentapi.EventPTY}}
	} else {
		if req.Stdin {
			r, w, err := os.Pipe()
			if err != nil {
				return fail(startError(req.Cmd, err))
			}
			toClose = append(toClose, r)
			mine = append(mine, w)
			childFiles[0] = r
			p.stdin = w
		} else {
			null, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
			if err != nil {
				return fail(startError(req.Cmd, err))
			}
			toClose = append(toClose, null)
			childFiles[0] = null
		}
		for i, ch := range []agentapi.EventType{agentapi.EventStdout, agentapi.EventStderr} {
			r, w, err := os.Pipe()
			if err != nil {
				return fail(startError(req.Cmd, err))
			}
			toClose = append(toClose, w)
			mine = append(mine, r)
			childFiles[i+1] = w
			outs = append(outs, out{r, ch})
		}
	}

	// Reserve the tag before the fork so that a duplicate tag never
	// starts a process.
	if req.Tag != "" {
		m.mu.Lock()
		inUse := m.reserved[req.Tag]
		for _, q := range m.order {
			if q.tag == req.Tag && !q.exited() {
				inUse = true
			}
		}
		if inUse {
			m.mu.Unlock()
			return fail(&apiError{http.StatusConflict, "tag_in_use", "a live process already has tag " + req.Tag})
		}
		m.reserved[req.Tag] = true
		m.mu.Unlock()
	}

	for _, o := range outs {
		p.outputs = append(p.outputs, o.f)
	}
	p.pumps.Add(len(outs))

	// Fd() puts the child's ends back into blocking mode.
	fds := []uintptr{childFiles[0].Fd(), childFiles[1].Fd(), childFiles[2].Fd()}
	argv := append([]string{req.Cmd}, req.Args...)
	attr := &syscall.ProcAttr{Dir: cwd, Env: env, Files: fds, Sys: sys}
	_, err = m.reaper.start(func() (int, error) {
		pid, err := syscall.ForkExec(path, argv, attr)
		if err == nil {
			// Set before the process is visible to anyone else.
			p.pid = pid
		}
		return pid, err
	}, p.onReaped)
	runtime.KeepAlive(childFiles)
	m.mu.Lock()
	delete(m.reserved, req.Tag)
	if err == nil {
		m.procs[p.id] = p
		m.order = append(m.order, p)
	}
	m.mu.Unlock()
	if err != nil {
		return fail(startError(req.Cmd, err))
	}
	for _, f := range toClose {
		_ = f.Close()
	}

	for _, o := range outs {
		go p.pump(o.f, o.ch)
	}
	go func() {
		p.pumps.Wait()
		close(p.pumpsDone)
	}()

	if req.TimeoutMs > 0 {
		p.mu.Lock()
		if !p.reaped {
			p.timer = time.AfterFunc(time.Duration(req.TimeoutMs)*time.Millisecond, p.onTimeout)
		}
		p.mu.Unlock()
	}
	return p, nil
}

// onDone applies the retention limits once a process is done.
func (m *manager) onDone(p *Process) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exited = append(m.exited, p)

	if max := m.cfg.MaxExited; max > 0 {
		for len(m.exited) > max {
			old := m.exited[0]
			m.exited = m.exited[1:]
			delete(m.procs, old.id)
			for i, q := range m.order {
				if q == old {
					m.order = append(m.order[:i], m.order[i+1:]...)
					break
				}
			}
		}
	}
	if budget := m.cfg.MaxRetainedBytes; budget > 0 {
		var total int64
		for _, q := range m.exited {
			q.mu.Lock()
			total += int64(q.ring.retained())
			q.mu.Unlock()
		}
		for _, q := range m.exited {
			if total <= budget {
				break
			}
			q.mu.Lock()
			total -= int64(q.ring.retained())
			q.ring.release()
			q.notifyLocked()
			q.mu.Unlock()
		}
	}
}

// readOOMKills returns the oom_kill counter of the agent's cgroup, or -1
// when it cannot be read (no cgroup v2 mount, or the root cgroup).
func readOOMKills() int64 {
	b, err := os.ReadFile("/sys/fs/cgroup/memory.events")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "oom_kill "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return -1
			}
			return n
		}
	}
	return -1
}

package podman

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/runtime"
)

// Leftover is the host side of a sandbox container that Podman no longer
// has: its conmon process, and whatever conmon started.
//
// How one comes to exist. Starting a container is three steps for Libpod:
// spawn conmon, which runs `crun create` (the container's first process
// then waits to be started), then `crun start`, then record the new state
// in the database. If the Podman service dies in between (it was seen to:
// a SIGSEGV in libsubid under concurrent creates, which Backend.createMu
// now prevents, but a service can also be killed), the database still
// says "configured". Removing a container in that state, even with
// force, kills nothing, because as far as Libpod knows nothing was
// started. The row and the storage go, and conmon stays for ever with a
// process parked in the sandbox's cgroup scope. Two such pairs were found
// on the development machine a day after the crash that made them.
//
// Nothing in the Libpod API can see them any more, so they are found the
// way an operator would find them: in the process table.
type Leftover struct {
	ConmonPID   int
	ContainerID string
	SandboxID   string
	Age         time.Duration
}

// leftoverGrace is how long a conmon may be without its container before
// it counts as left over. A normal removal has conmon gone within
// milliseconds of the container; the grace keeps a pass that runs in that
// moment from seeing a leftover that is not one.
const leftoverGrace = 30 * time.Second

var containerIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Leftovers lists the conmon processes of this user that were started for
// a sandbox container (name "sbx-...") Podman does not have any more, and
// that are older than grace. It does not say whose sandbox each was: the
// caller decides that from Leftover.SandboxID before reaping.
func (b *Backend) Leftovers(ctx context.Context, grace time.Duration) ([]Leftover, error) {
	candidates, err := scanConmons(b.procRoot(), os.Getuid(), time.Now())
	if err != nil || len(candidates) == 0 {
		return nil, err
	}
	var out []Leftover
	for _, l := range candidates {
		if l.Age < grace {
			continue
		}
		// One exact question per candidate (there are normally none): a
		// listing could miss a container that is being created right now.
		_, err := b.c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(l.ContainerID)+"/exists", nil, nil, nil,
			http.StatusNoContent)
		switch {
		case err == nil:
			continue // Podman has it: an ordinary, live conmon
		case IsStatus(err, http.StatusNotFound):
			out = append(out, l)
		default:
			return out, fmt.Errorf("podman: check container %s: %w", l.ContainerID, err)
		}
	}
	return out, nil
}

// ReapLeftovers implements runtime.LeftoverReaper: it kills the leftovers
// whose sandbox the caller owns, the processes conmon started first and
// then conmon, and removes the OCI runtime's state directory.
func (b *Backend) ReapLeftovers(ctx context.Context, owned func(sandboxID string) bool) (int, error) {
	return b.reapLeftovers(ctx, leftoverGrace, owned)
}

func (b *Backend) reapLeftovers(ctx context.Context, grace time.Duration, owned func(sandboxID string) bool) (int, error) {
	// Cheap first: with no conmon at all whose container is unknown to
	// this pass there is nothing to ask Podman.
	left, err := b.Leftovers(ctx, grace)
	n := 0
	for _, l := range left {
		if !owned(l.SandboxID) {
			continue
		}
		if kerr := killLeftover(b.procRoot(), l); kerr != nil {
			err = kerr
			continue
		}
		n++
	}
	return n, err
}

var _ runtime.LeftoverReaper = (*Backend)(nil)

func (b *Backend) procRoot() string {
	if b.cfg.ProcRoot != "" {
		return b.cfg.ProcRoot
	}
	return "/proc"
}

// scanConmons returns every conmon of uid that was started for a
// container named like a sandbox's. It reads only what an unprivileged
// user can read about its own processes.
func scanConmons(procRoot string, uid int, now time.Time) ([]Leftover, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("podman: read %s: %w", procRoot, err)
	}
	var out []Leftover
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		comm, err := os.ReadFile(filepath.Join(dir, "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "conmon" {
			continue
		}
		fi, err := os.Stat(dir)
		if err != nil {
			continue // it ended while we were looking
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue
		}
		cid, name := conmonArgs(bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0}))
		const prefix = "sbx-"
		if !containerIDRE.MatchString(cid) || !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
			continue
		}
		out = append(out, Leftover{ConmonPID: pid, ContainerID: cid, SandboxID: strings.TrimPrefix(name, prefix), Age: now.Sub(fi.ModTime())})
	}
	return out, nil
}

// conmonArgs returns the container id (-c) and name (-n) of a conmon
// command line. Parsing stops at the first argument that belongs to
// something else (--exit-command-arg and friends repeat free text).
func conmonArgs(argv [][]byte) (cid, name string) {
	for i := 1; i+1 < len(argv); i++ {
		switch string(argv[i]) {
		case "-c", "--cid":
			if cid == "" {
				cid = string(argv[i+1])
			}
		case "-n", "--name":
			if name == "" {
				name = string(argv[i+1])
			}
		case "--exit-command":
			return cid, name
		}
	}
	return cid, name
}

// killLeftover ends a leftover: first the processes conmon started (the
// container's parked first process), then conmon itself. Each pid is
// checked again right before the signal, so that a pid reused since the
// scan is never hit.
func killLeftover(procRoot string, l Leftover) error {
	still := func() bool {
		cur, _ := scanConmons(procRoot, os.Getuid(), time.Now())
		for _, c := range cur {
			if c.ConmonPID == l.ConmonPID && c.ContainerID == l.ContainerID {
				return true
			}
		}
		return false
	}
	if !still() {
		return nil
	}
	for _, child := range childrenOf(procRoot, l.ConmonPID) {
		_ = syscall.Kill(child, syscall.SIGKILL)
	}
	if !still() {
		return nil // conmon ended with its child, as it does
	}
	if err := syscall.Kill(l.ConmonPID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("podman: kill leftover conmon %d of container %s: %w", l.ConmonPID, l.ContainerID, err)
	}
	// The OCI runtime's record of the container that never started.
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		_ = os.RemoveAll(filepath.Join(dir, "crun", l.ContainerID))
	}
	return nil
}

// childrenOf returns the pids whose parent is ppid.
func childrenOf(procRoot string, ppid int) []int {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			continue
		}
		// "pid (comm) state ppid ...": comm may contain spaces and parentheses.
		i := bytes.LastIndexByte(raw, ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(raw[i+1:]))
		if len(fields) >= 2 {
			if p, err := strconv.Atoi(fields[1]); err == nil && p == ppid {
				out = append(out, pid)
			}
		}
	}
	return out
}

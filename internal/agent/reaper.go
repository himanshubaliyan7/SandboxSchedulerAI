package agent

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// reaper collects the exit status of every child.
//
// The agent is PID 1 of the sandbox (or a child subreaper), so orphaned
// descendants are re-parented to it and must be reaped or they stay
// zombies forever. Reaping them needs wait4(-1), which would also collect
// (and so steal) the status of processes the agent started itself. The
// reaper therefore is the ONLY code that ever waits for a child:
//
//   - fork and registration of the new pid happen under mu, and so does
//     every wait4(-1) call, so a child can never be reaped before its pid
//     is in the table, however quickly it exits;
//   - a reaped pid found in the table has its status delivered to the
//     process that owns it; any other pid was an adopted orphan and its
//     status is discarded.
//
// With all == false (the agent is embedded in a process that has other
// children, e.g. a test binary) wait4(-1) is never used; each started
// child gets a goroutine blocked in wait4(pid) instead and orphans are
// not reaped.
type reaper struct {
	all bool

	mu    sync.Mutex
	procs map[int]func(syscall.WaitStatus)
}

func newReaper(all bool) *reaper {
	return &reaper{all: all, procs: make(map[int]func(syscall.WaitStatus))}
}

// start runs fork (which must create exactly one child and return its
// pid) and arranges for onExit to be called once with the child's wait
// status. onExit is called from another goroutine and may be called
// before start returns.
func (r *reaper) start(fork func() (int, error), onExit func(syscall.WaitStatus)) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pid, err := fork()
	if err != nil {
		return 0, err
	}
	if r.all {
		r.procs[pid] = onExit
		return pid, nil
	}
	go func() {
		var ws syscall.WaitStatus
		for {
			_, err := syscall.Wait4(pid, &ws, 0, nil)
			if err == syscall.EINTR {
				continue
			}
			break
		}
		onExit(ws)
	}()
	return pid, nil
}

// run reaps children until ctx is done. It only does anything when the
// reaper was created with all == true. ready is closed once SIGCHLD is
// being delivered, i.e. once it is safe to start children.
func (r *reaper) run(ctx context.Context, ready chan<- struct{}) {
	if !r.all {
		close(ready)
		<-ctx.Done()
		return
	}
	// SIGCHLD is level-ish for our purposes: the channel coalesces
	// signals, and every wake-up reaps until no exited child is left.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGCHLD)
	defer signal.Stop(sigc)
	close(ready)
	for {
		r.reapAll()
		select {
		case <-ctx.Done():
			return
		case <-sigc:
		}
	}
}

func (r *reaper) reapAll() {
	for {
		var ws syscall.WaitStatus
		r.mu.Lock()
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		var cb func(syscall.WaitStatus)
		if err == nil && pid > 0 {
			cb = r.procs[pid]
			delete(r.procs, pid)
		}
		r.mu.Unlock()
		if err == syscall.EINTR {
			continue
		}
		if err != nil || pid <= 0 {
			return // ECHILD: no children; 0: none has exited yet
		}
		if cb != nil {
			cb(ws)
		}
	}
}

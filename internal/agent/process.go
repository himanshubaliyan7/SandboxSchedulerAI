package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
)

// Process is one process started by the agent, together with its replay
// buffer. It outlives every client connection and is kept after exit.
//
// Lifecycle: running -> reaped (the kernel reported the exit status) ->
// done (all output has been read or the drain grace expired; the exit
// status is published and no more output is ever appended).
type Process struct {
	id      string
	tag     string
	cmd     string
	args    []string
	isPTY   bool
	started time.Time
	pid     int // also the process group and session id

	drainGrace time.Duration
	oomBefore  int64
	onDone     func(*Process)

	// stdinMu serialises writers to stdin and guards stdin itself.
	stdinMu sync.Mutex
	stdin   *os.File // pipe write end, or the PTY master; nil once closed

	ptm       *os.File   // PTY master (PTY mode)
	outputs   []*os.File // read ends pumped into the ring
	pumps     sync.WaitGroup
	pumpsDone chan struct{}

	mu       sync.Mutex
	ring     *ring
	reaped   bool
	timedOut bool
	timer    *time.Timer
	exit     *agentapi.ExitStatus // non-nil once done
	changed  chan struct{}        // closed and replaced on every state change
	waiters  bool
	flows    map[*flowConn]struct{}
}

// flowConn is the flow-control state of one attached WebSocket.
//
// Rule: a connection takes part in flow control from its first AckFrame
// on. From then, whenever the bytes the process has produced beyond the
// connection's last acknowledged offset exceed FlowHighWatermark the
// agent stops reading the process's output (so the process blocks in
// write(2)) until that number drops below FlowLowWatermark. A connection
// that never ACKs never pauses the process; it is an ordinary ring-buffer
// reader that is truncated if it falls too far behind.
//
// "Produced minus acknowledged" is at least "sent minus acknowledged", so
// this is slightly stricter than the contract's wording and also bounds
// what is queued between the ring and a client that has stopped reading.
type flowConn struct {
	acked   int64
	enabled bool
	over    bool
}

// notifyLocked wakes everything waiting for a state change.
func (p *Process) notifyLocked() {
	if p.waiters {
		close(p.changed)
		p.changed = make(chan struct{})
		p.waiters = false
	}
}

// waitLocked returns a channel closed on the next state change.
func (p *Process) waitLocked() <-chan struct{} {
	p.waiters = true
	return p.changed
}

func (p *Process) info() agentapi.ProcessInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	pi := agentapi.ProcessInfo{
		ID:         p.id,
		Tag:        p.tag,
		Cmd:        p.cmd,
		Args:       p.args,
		PTY:        p.isPTY,
		StartedAt:  p.started,
		NextOffset: p.ring.next,
	}
	if p.exit != nil {
		e := *p.exit
		pi.Exit = &e
	}
	return pi
}

func (p *Process) exited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit != nil
}

// --- output --------------------------------------------------------------

// pump copies one output file into the ring until EOF, honouring the
// flow-control gate before every read.
func (p *Process) pump(f *os.File, ch agentapi.EventType) {
	defer p.pumps.Done()
	buf := make([]byte, agentapi.MaxFrameBytes)
	for {
		p.waitGate()
		n, err := f.Read(buf)
		if n > 0 {
			p.append(ch, buf[:n])
		}
		if err != nil {
			return // EOF, EIO on PTY hang-up, or the drain deadline
		}
	}
}

// waitGate blocks while flow control has the process paused.
func (p *Process) waitGate() {
	for {
		p.mu.Lock()
		if !p.pausedLocked() {
			p.mu.Unlock()
			return
		}
		ch := p.waitLocked()
		p.mu.Unlock()
		<-ch
	}
}

// pausedLocked reports whether output reads are suspended. A reaped
// process is never paused, so its remaining output always drains.
func (p *Process) pausedLocked() bool {
	if p.reaped {
		return false
	}
	for fc := range p.flows {
		if fc.over {
			return true
		}
	}
	return false
}

func (p *Process) append(ch agentapi.EventType, b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exit != nil {
		return // sealed
	}
	p.ring.write(ch, b)
	for fc := range p.flows {
		p.updateFlowLocked(fc)
	}
	p.notifyLocked()
}

func (p *Process) updateFlowLocked(fc *flowConn) {
	if !fc.enabled {
		return
	}
	unacked := p.ring.next - fc.acked
	switch {
	case unacked > agentapi.FlowHighWatermark:
		fc.over = true
	case unacked < agentapi.FlowLowWatermark:
		fc.over = false
	}
}

// poll is the non-blocking read used by every consumer. It returns, in
// order of preference: output at cursor (n > 0), the exit status if the
// process is done and cursor is at the end, or a channel to wait on.
// off is where the returned data (or the cursor) really is; dropped > 0
// means [off-dropped, off) was requested but is no longer buffered.
func (p *Process) poll(cursor int64, dst []byte) (ch agentapi.EventType, n int, off, dropped int64, exit *agentapi.ExitStatus, wait <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, n, off, dropped = p.ring.read(cursor, dst)
	if n > 0 {
		return
	}
	if p.exit != nil {
		e := *p.exit
		exit = &e
		return
	}
	wait = p.waitLocked()
	return
}

// attachFlow registers a WebSocket reader starting at offset from and
// returns its flow state, the offset it really starts at and how many
// requested bytes are no longer available.
func (p *Process) attachFlow(from int64) (fc *flowConn, start, dropped int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	start = from
	if start > p.ring.next {
		start = p.ring.next
	}
	if start < p.ring.base {
		dropped = p.ring.base - start
		start = p.ring.base
	}
	fc = &flowConn{acked: start}
	p.flows[fc] = struct{}{}
	return
}

func (p *Process) detachFlow(fc *flowConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.flows, fc)
	p.notifyLocked()
}

// ack records that the client has processed output up to off. limit is
// the offset the agent has sent so far; a client cannot acknowledge more.
func (p *Process) ack(fc *flowConn, off, limit int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if off > limit {
		off = limit
	}
	if off > fc.acked {
		fc.acked = off
	}
	fc.enabled = true
	p.updateFlowLocked(fc)
	p.notifyLocked()
}

// skipAck moves the acknowledged offset past bytes that were dropped and
// so can never be acknowledged by the client.
func (p *Process) skipAck(fc *flowConn, off int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if off > fc.acked {
		fc.acked = off
		p.updateFlowLocked(fc)
		p.notifyLocked()
	}
}

// --- input and control ---------------------------------------------------

var (
	errExited      = &apiError{http.StatusConflict, "process_exited", "process has exited"}
	errStdinClosed = &apiError{http.StatusConflict, "stdin_closed", "stdin is not open (start the process with stdin=true or a pty)"}
	errNotPTY      = &apiError{http.StatusConflict, "not_a_pty", "process does not run on a pty"}
	errIsPTY       = &apiError{http.StatusConflict, "is_a_pty", "close-stdin applies to pipe mode only; send Ctrl+D (0x04) to a pty"}
)

// writeStdin writes b to the process's stdin (or PTY). It blocks while
// the process is not reading, until ctx is done.
func (p *Process) writeStdin(ctx context.Context, b []byte) error {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	w := p.stdin
	if w == nil {
		if p.exited() {
			return errExited
		}
		return errStdinClosed
	}
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = w.SetWriteDeadline(time.Unix(1, 0))
		close(fired)
	})
	_, err := w.Write(b)
	if !stop() {
		<-fired
		_ = w.SetWriteDeadline(time.Time{})
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p.exited() || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
			return errExited
		}
		return err
	}
	return nil
}

func (p *Process) closeStdin() error {
	if p.isPTY {
		return errIsPTY
	}
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.stdin == nil {
		if p.exited() {
			return errExited
		}
		return nil // already closed, or never open: nothing to do
	}
	err := p.stdin.Close()
	p.stdin = nil
	return err
}

func (p *Process) resize(cols, rows uint16) error {
	if !p.isPTY {
		return errNotPTY
	}
	if cols == 0 || rows == 0 {
		return &apiError{http.StatusBadRequest, "bad_request", "cols and rows must be positive"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exit != nil {
		return errExited
	}
	return pty.Setsize(p.ptm, &pty.Winsize{Cols: cols, Rows: rows})
}

// signal sends sig to the whole process group.
func (p *Process) signal(sig syscall.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped {
		// The group id may be reused once its leader is reaped.
		return errExited
	}
	return p.killLocked(sig)
}

func (p *Process) killLocked(sig syscall.Signal) error {
	if p.pid <= 1 {
		return errExited // never signal group 0 (ourselves) or -1 (everyone)
	}
	err := syscall.Kill(-p.pid, sig)
	if err == syscall.ESRCH {
		// The group is not there yet or any more; try the leader.
		err = syscall.Kill(p.pid, sig)
	}
	return err
}

// kill SIGKILLs the process group if the process is still running.
func (p *Process) kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.reaped {
		_ = p.killLocked(syscall.SIGKILL)
	}
}

func (p *Process) onTimeout() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped {
		return
	}
	p.timedOut = true
	_ = p.killLocked(syscall.SIGKILL)
}

// --- exit ----------------------------------------------------------------

// onReaped is called by the reaper with the process's wait status.
func (p *Process) onReaped(ws syscall.WaitStatus) {
	p.mu.Lock()
	p.reaped = true
	if p.timer != nil {
		p.timer.Stop()
	}
	timedOut := p.timedOut
	p.notifyLocked() // opens the flow-control gate
	p.mu.Unlock()

	go p.finish(ws, timedOut)
}

// finish drains the remaining output, then publishes the exit status.
func (p *Process) finish(ws syscall.WaitStatus, timedOut bool) {
	grace := time.NewTimer(p.drainGrace)
	select {
	case <-p.pumpsDone:
		grace.Stop()
	case <-grace.C:
		// A descendant still holds the output open. Stop reading; what it
		// writes from now on is lost (and it gets EPIPE/EIO).
		for _, f := range p.outputs {
			_ = f.SetReadDeadline(time.Unix(1, 0))
		}
		t := time.NewTimer(time.Second)
		select {
		case <-p.pumpsDone:
			t.Stop()
		case <-t.C:
		}
	}

	status := exitStatus(ws, timedOut)
	if status.Reason == agentapi.ExitKilled && ws.Signal() == syscall.SIGKILL && p.oomBefore >= 0 {
		if after := readOOMKills(); after > p.oomBefore {
			status.Reason = agentapi.ExitOOM
		}
	}

	p.mu.Lock()
	p.exit = &status
	p.notifyLocked()
	p.mu.Unlock()

	p.stdinMu.Lock()
	if p.stdin != nil && p.stdin != p.ptm {
		_ = p.stdin.Close()
	}
	p.stdin = nil
	p.stdinMu.Unlock()
	for _, f := range p.outputs {
		_ = f.Close() // includes the PTY master
	}
	if p.onDone != nil {
		p.onDone(p)
	}
}

func exitStatus(ws syscall.WaitStatus, timedOut bool) agentapi.ExitStatus {
	if ws.Signaled() {
		sig := ws.Signal()
		st := agentapi.ExitStatus{
			ExitCode: 128 + int(sig),
			Signal:   signalName(sig),
			Reason:   agentapi.ExitKilled,
		}
		if timedOut {
			st.Reason = agentapi.ExitTimeout
		}
		return st
	}
	return agentapi.ExitStatus{ExitCode: ws.ExitStatus(), Reason: agentapi.ExitExited}
}

func signalName(sig syscall.Signal) string {
	if n := unix.SignalName(sig); n != "" {
		return n
	}
	return "SIG" + strconv.Itoa(int(sig))
}

var signalsByName = map[string]syscall.Signal{
	"SIGINT":   syscall.SIGINT,
	"SIGTERM":  syscall.SIGTERM,
	"SIGKILL":  syscall.SIGKILL,
	"SIGHUP":   syscall.SIGHUP,
	"SIGQUIT":  syscall.SIGQUIT,
	"SIGUSR1":  syscall.SIGUSR1,
	"SIGUSR2":  syscall.SIGUSR2,
	"SIGSTOP":  syscall.SIGSTOP,
	"SIGCONT":  syscall.SIGCONT,
	"SIGWINCH": syscall.SIGWINCH,
}

func parseSignal(name string) (syscall.Signal, error) {
	if sig, ok := signalsByName[name]; ok {
		return sig, nil
	}
	if sig, ok := signalsByName["SIG"+name]; ok {
		return sig, nil
	}
	return 0, &apiError{http.StatusBadRequest, "bad_signal", "unsupported signal " + name}
}

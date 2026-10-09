package agent

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
)

// These tests run the agent as a subprocess that is a child subreaper,
// which is how orphans reach it when it is PID 1 of a sandbox.

func zombiesOf(pid int) []child {
	var z []child
	for _, c := range childrenOf(pid) {
		if c.state == "Z" {
			z = append(z, c)
		}
	}
	return z
}

func TestReapsOrphanedZombies(t *testing.T) {
	h := startAgent(t, opts{subprocess: true})

	// The shell forks a grandchild, detaches it and exits 7 at once. The
	// grandchild is re-parented to the agent and exits later.
	scripts := []string{
		"(sleep 0.4 >/dev/null 2>&1 &) ; exit 7",
		"sleep 0.4 >/dev/null 2>&1 & sleep 0.4 >/dev/null 2>&1 & exit 7",
		"(exec sh -c 'sleep 0.4 & sleep 0.4 & wait' >/dev/null 2>&1 &) ; exit 7",
	}
	begin := time.Now()
	for _, s := range scripts {
		pi := h.start(sh(s))
		r := h.collect(pi.ID, 0)
		wantExit(t, r.exit, 7, "", agentapi.ExitExited)
	}
	if d := time.Since(begin); d > 300*time.Millisecond {
		t.Fatalf("starting took %v; the orphans may already be gone", d)
	}

	// The orphans really are the agent's children now.
	var adopted int
	for _, c := range childrenOf(h.pid) {
		if c.comm == "sleep" || c.comm == "sh" {
			adopted++
		}
	}
	if adopted < 4 {
		t.Fatalf("only %d orphans were adopted by the agent (pid %d): %+v", adopted, h.pid, childrenOf(h.pid))
	}

	// Once they exit they must be reaped, not left as zombies.
	deadline := time.Now().Add(10 * time.Second)
	for {
		kids := childrenOf(h.pid)
		if len(kids) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent still has children: %+v (zombies: %+v)", kids, zombiesOf(h.pid))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And its own processes still report the right status afterwards.
	r := h.collect(h.start(sh("exit 42")).ID, 0)
	wantExit(t, r.exit, 42, "", agentapi.ExitExited)
}

// Many short processes at once, each leaving an orphan behind: wait4(-1)
// must never swallow the status of a process the agent started.
func TestReaperDoesNotStealExitStatus(t *testing.T) {
	h := startAgent(t, opts{subprocess: true})
	const n = 60
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code := 1 + i%120
			var req agentapi.StartRequest
			switch i % 3 {
			case 0:
				req = sh(fmt.Sprintf("exit %d", code))
			case 1: // leaves an orphan that exits a little later
				req = sh(fmt.Sprintf("(sleep 0.0%d >/dev/null 2>&1 &) ; echo %d; exit %d", i%10, code, code))
			default: // dies from a signal
				req = sh("kill -TERM $$")
			}
			pi, err := h.c.Start(testCtx(t), req)
			if err != nil {
				errs <- err.Error()
				return
			}
			st := h.waitExit(pi.ID)
			if i%3 == 2 {
				if st.Reason != agentapi.ExitKilled || st.Signal != "SIGTERM" {
					errs <- fmt.Sprintf("process %d: %+v, want killed by SIGTERM", i, st)
				}
			} else if st.ExitCode != code || st.Reason != agentapi.ExitExited {
				errs <- fmt.Sprintf("process %d: %+v, want exit code %d", i, st, code)
			}
		}()
	}
	wg.Wait()
	close(errs)
	var all []string
	for e := range errs {
		all = append(all, e)
	}
	if len(all) > 0 {
		t.Fatalf("%d wrong statuses:\n%s", len(all), strings.Join(all, "\n"))
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(childrenOf(h.pid)) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("agent still has children: %+v", childrenOf(h.pid))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

package podman

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// oomFileName is the file conmon creates in its working directory when
// the container it watches is OOM-killed.
const oomFileName = "oom"

const (
	// An OOM kill is attributed to a container whose init process exited
	// with SIGKILL when conmon reported an OOM event at most oomBefore
	// before, or oomAfter after, the container's recorded end. Measured on
	// Podman 4.9.3 with conmon 2.1.10, the file appears 2 to 4 ms before
	// the recorded end; the window is wide so that a loaded host does not
	// turn an OOM kill into an unexplained death.
	oomBefore = 2 * time.Second
	oomAfter  = 1 * time.Second
	// oomSettle is how long after a container's end its conmon is given to
	// report the OOM event before the death is judged not to be one.
	oomSettle = 300 * time.Millisecond
	// oomPollInterval is how often the working directory is looked at.
	// The interval only bounds how long the single file name stays taken;
	// the time of the event is the file's own timestamp.
	oomPollInterval = 50 * time.Millisecond
	oomKeep         = time.Hour
	oomMaxEvents    = 4096
)

// oomEvidence turns conmon's "oom" files into per-container evidence.
//
// conmon 2.1 watches the memory.events of its container's cgroup and, on
// an oom_kill, creates an empty file named "oom" in its working directory
// (and in --persist-dir, which Podman 4.9 does not pass, which is also
// why Podman's own State.OOMKilled stays false there). Every conmon of a
// managed service inherits the service's private working directory, so
// all of them create the same file: it says that some sandbox of this
// service was OOM-killed at the file's timestamp, not which one. The
// watcher therefore consumes the file as soon as it appears, recording
// its timestamp, and an exited container counts as OOM-killed when its
// init process was ended by SIGKILL and such an event lies within a few
// seconds of its recorded end.
//
// What that buys over the exit code alone (see killedFromOutside): a
// sandbox whose process is killed with SIGKILL from the host is no longer
// reported as oom_killed, unless another sandbox of the same service was
// OOM-killed within the same few seconds. What it costs: an OOM kill that
// conmon fails to report would be recorded as instance_lost (with exit
// code 137) instead; none was seen in testing.
type oomEvidence struct {
	paths []string // the "oom" file of each service working directory

	mu     sync.Mutex
	events []time.Time

	stop chan struct{}
	done chan struct{}
}

func newOOMEvidence(dirs []string) *oomEvidence {
	e := &oomEvidence{stop: make(chan struct{}), done: make(chan struct{})}
	for _, dir := range dirs {
		e.paths = append(e.paths, filepath.Join(dir, oomFileName))
	}
	e.poll()
	go func() {
		defer close(e.done)
		t := time.NewTicker(oomPollInterval)
		defer t.Stop()
		for {
			select {
			case <-e.stop:
				return
			case <-t.C:
				e.poll()
			}
		}
	}()
	return e
}

func (e *oomEvidence) close() {
	select {
	case <-e.stop:
	default:
		close(e.stop)
	}
	<-e.done
}

// poll consumes the files that are there.
func (e *oomEvidence) poll() {
	for _, path := range e.paths {
		e.consume(path)
	}
}

// consume takes one file. Only an empty regular file is taken for
// conmon's; anything else is left alone and ignored.
func (e *oomEvidence) consume(path string) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 {
		return
	}
	at := fi.ModTime()
	// Free the name first: conmon opens it with O_CREAT and no O_TRUNC, so
	// while the file exists a second OOM kill leaves no new timestamp.
	if err := os.Remove(path); err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cut := time.Now().Add(-oomKeep)
	i := 0
	for i < len(e.events) && (e.events[i].Before(cut) || len(e.events)-i >= oomMaxEvents) {
		i++
	}
	e.events = append(e.events[i:], at)
}

// near reports whether an OOM event lies within the attribution window
// around end, widened by slack on both sides (for an end that is only
// known to the second).
func (e *oomEvidence) near(end time.Time, slack time.Duration) bool {
	e.poll()
	e.mu.Lock()
	defer e.mu.Unlock()
	lo, hi := end.Add(-oomBefore-slack), end.Add(oomAfter+slack)
	for i := len(e.events) - 1; i >= 0; i-- {
		if at := e.events[i]; !at.Before(lo) && !at.After(hi) {
			return true
		}
	}
	return false
}

// killed reports whether a container that ended at end, by SIGKILL, was
// OOM-killed. A container that ended only just now is given oomSettle for
// its conmon to report.
func (e *oomEvidence) killed(end time.Time, slack time.Duration) bool {
	for {
		if e.near(end, slack) {
			return true
		}
		if end.IsZero() || time.Since(end) > oomSettle+slack {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

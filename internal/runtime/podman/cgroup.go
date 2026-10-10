package podman

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/himanshubaliyan7/warmbox/internal/runtime"
)

// ErrNotRunning is returned by Stats for an instance that exists but has
// no cgroup any more because its processes are gone. It is the contract's
// runtime.ErrNotRunning, so callers above the backend can test for it.
var ErrNotRunning = runtime.ErrNotRunning

// Stats implements runtime.Backend. It asks Podman only for the
// container's cgroup path and then reads the cgroup v2 files directly:
// that is cheaper than Podman's stats endpoint and exposes the counters
// that endpoint does not (oom_kill, memory.high events, pids.max hits,
// CPU throttling), which are what turn a "silent kill" into a reason.
//
// Limitation, observed on rootless Podman 4.9.3: once the last process of
// a container is gone, systemd removes its scope and the counters with
// it, and Podman's own State.OOMKilled stays false even when the cause
// was the OOM killer. Because memory.oom.group=1 kills the whole sandbox
// at once, a sandbox that died of OOM is reported here as ErrNotRunning,
// not as OOMKills > 0. State.OOMKilled is still honoured for Podman
// versions that set it. Callers that get ErrNotRunning should Inspect the
// instance: its ExitCode and OOMKilled say how it ended (see inspect).
func (b *Backend) Stats(ctx context.Context, instanceID string) (runtime.Stats, error) {
	_, st, err := b.inspect(ctx, instanceID)
	if err != nil {
		return runtime.Stats{}, err
	}
	var stats runtime.Stats
	if st.CgroupPath == "" {
		err = ErrNotRunning
	} else if stats, err = readCgroupStats(statsDir(b.cfg.CgroupRoot, st.CgroupPath)); errors.Is(err, fs.ErrNotExist) {
		err = fmt.Errorf("%w: %w", ErrNotRunning, err)
	}
	if err != nil {
		if st.OOMKilled {
			return runtime.Stats{OOMKills: 1}, nil
		}
		return runtime.Stats{}, fmt.Errorf("podman: stats for %s (state %q): %w", instanceID, st.Status, err)
	}
	if st.OOMKilled && stats.OOMKills == 0 {
		stats.OOMKills = 1
	}
	return stats, nil
}

// statsDir maps the cgroup path Podman reports to the directory to read.
//
// With the systemd cgroup manager, Podman reports the libpod-<id>.scope
// unit, but crun puts the container's processes in a "container"
// sub-cgroup of it and writes the limits there (observed on Podman 4.9.3
// with crun 1.14: pids.max, memory.swap.max, memory.high and
// memory.oom.group exist only on the leaf, while the scope keeps
// systemd's defaults for them). Limit events are counted where the limit
// is, so the leaf is the directory to read whenever it exists. A path
// that already names the leaf, or a cgroupfs-managed path with no leaf,
// is used as reported.
func statsDir(root, cgroupPath string) string {
	dir := filepath.Join(root, filepath.Clean("/"+cgroupPath))
	if strings.HasSuffix(filepath.Base(dir), ".scope") {
		leaf := filepath.Join(dir, "container")
		if fi, err := os.Stat(leaf); err == nil && fi.IsDir() {
			return leaf
		}
	}
	return dir
}

// readCgroupStats reads one cgroup v2 directory. A missing directory is an
// error; a missing file or key (a controller that is not enabled) leaves
// the corresponding fields zero.
func readCgroupStats(dir string) (runtime.Stats, error) {
	if fi, err := os.Stat(dir); err != nil {
		return runtime.Stats{}, err
	} else if !fi.IsDir() {
		return runtime.Stats{}, fmt.Errorf("%s is not a directory", dir)
	}
	var s runtime.Stats
	var errs []error
	note := func(err error) {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}

	v, err := readInt(filepath.Join(dir, "memory.current"))
	note(err)
	s.MemoryBytes = v

	kv, err := readFlatKeyed(filepath.Join(dir, "memory.events"))
	note(err)
	s.OOMKills = kv["oom_kill"]
	s.MemoryHighHits = kv["high"]

	kv, err = readFlatKeyed(filepath.Join(dir, "cpu.stat"))
	note(err)
	s.CPUUsageUsec = kv["usage_usec"]
	s.NrThrottled = kv["nr_throttled"]
	s.ThrottledUsec = kv["throttled_usec"]

	v, err = readInt(filepath.Join(dir, "pids.current"))
	note(err)
	s.PidsCurrent = v

	kv, err = readFlatKeyed(filepath.Join(dir, "pids.events"))
	note(err)
	s.PidsLimitHits = kv["max"]

	return s, errors.Join(errs...)
}

// readInt reads a single-value cgroup file. "max" reads as 0.
func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	str := strings.TrimSpace(string(b))
	if str == "max" {
		return 0, nil
	}
	v, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// readFlatKeyed reads a "key value" per line cgroup file.
func readFlatKeyed(path string) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		if v, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			out[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

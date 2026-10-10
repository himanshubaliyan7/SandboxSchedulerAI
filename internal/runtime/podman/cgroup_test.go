package podman

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/himanshubaliyan7/warmbox/internal/runtime"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

var cgroupFixture = map[string]string{
	"memory.current": "104857600\n",
	"memory.events":  "low 0\nhigh 12\nmax 3\noom 2\noom_kill 1\noom_group_kill 1\n",
	"cpu.stat":       "usage_usec 5000000\nuser_usec 4000000\nsystem_usec 1000000\nnr_periods 100\nnr_throttled 7\nthrottled_usec 250000\nnr_bursts 0\nburst_usec 0\n",
	"pids.current":   "17\n",
	"pids.events":    "max 4\n",
}

func TestReadCgroupStats(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, cgroupFixture)
	got, err := readCgroupStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := runtime.Stats{
		MemoryBytes: 104857600, OOMKills: 1, CPUUsageUsec: 5000000, NrThrottled: 7,
		ThrottledUsec: 250000, PidsCurrent: 17, PidsLimitHits: 4, MemoryHighHits: 12,
	}
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestReadCgroupStatsPartialAndMissing(t *testing.T) {
	// A controller that is not enabled has no files: zeros, no error.
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"memory.current": "42\n", "cpu.stat": "usage_usec 9\n"})
	got, err := readCgroupStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != (runtime.Stats{MemoryBytes: 42, CPUUsageUsec: 9}) {
		t.Errorf("got %+v", got)
	}
	// Garbage in a single-value file is an error, not a silent zero.
	writeFiles(t, dir, map[string]string{"pids.current": "lots\n"})
	if _, err := readCgroupStats(dir); err == nil {
		t.Error("unparsable pids.current accepted")
	}
	if _, err := readCgroupStats(filepath.Join(dir, "gone")); err == nil {
		t.Error("missing cgroup directory accepted")
	}
}

func TestStatsDir(t *testing.T) {
	root := t.TempDir()
	scope := "/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-abc.scope"
	// No leaf yet: the reported path is used.
	if got := statsDir(root, scope); got != filepath.Join(root, scope) {
		t.Errorf("without leaf: %s", got)
	}
	// crun's "container" leaf holds the limits and their events.
	if err := os.MkdirAll(filepath.Join(root, scope, "container"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := statsDir(root, scope); got != filepath.Join(root, scope, "container") {
		t.Errorf("with leaf: %s", got)
	}
	if got := statsDir(root, scope+"/container"); got != filepath.Join(root, scope, "container") {
		t.Errorf("leaf reported directly: %s", got)
	}
	// A path cannot climb out of the cgroup root.
	if got := statsDir(root, "../../etc"); got != filepath.Join(root, "etc") {
		t.Errorf("traversal: %s", got)
	}
}

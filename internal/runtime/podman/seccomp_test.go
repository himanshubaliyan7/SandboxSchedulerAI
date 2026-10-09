package podman

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// These tests check the profile's structure. That it loads and behaves as
// intended under crun is checked by TestIntegrationPythonUnderSeccomp.
func loadProfile(t *testing.T) seccompDoc {
	t.Helper()
	var doc seccompDoc
	if err := json.Unmarshal(SeccompProfile(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestSeccompProfileIsDenyByDefault(t *testing.T) {
	doc := loadProfile(t)
	if doc.DefaultAction != "SCMP_ACT_ERRNO" || doc.DefaultErrnoRet == nil || *doc.DefaultErrnoRet != 38 {
		t.Errorf("default action %q errno %v, want SCMP_ACT_ERRNO/38 (ENOSYS)", doc.DefaultAction, doc.DefaultErrnoRet)
	}
	if len(doc.ArchMap) < 10 {
		t.Error("archMap missing")
	}
	for _, r := range doc.Syscalls {
		if len(r.Includes.Caps) > 0 || len(r.Excludes.Caps) > 0 {
			t.Errorf("capability-conditional rule left in the profile: %v", r.Names)
		}
	}
}

func TestSeccompProfileBlocks(t *testing.T) {
	doc := loadProfile(t)
	unconditional := map[string]bool{} // allowed with no argument filter
	filtered := map[string][]seccompRule{}
	errno := map[string]uint{}
	for _, r := range doc.Syscalls {
		for _, n := range r.Names {
			switch {
			case r.Action == "SCMP_ACT_ALLOW" && len(r.Args) == 0:
				unconditional[n] = true
			case r.Action == "SCMP_ACT_ALLOW":
				filtered[n] = append(filtered[n], r)
			case r.Action == "SCMP_ACT_ERRNO" && r.ErrnoRet != nil:
				errno[n] = *r.ErrnoRet
			}
		}
	}
	for _, n := range []string{
		"unshare", "clone", "clone3", "setns", "socket", "socketcall",
		"bpf", "userfaultfd", "io_uring_setup", "io_uring_enter", "io_uring_register",
		"keyctl", "add_key", "request_key", "ptrace", "process_vm_readv", "process_vm_writev",
		"mount", "umount", "umount2", "mount_setattr", "move_mount", "open_tree", "fsopen", "fsconfig", "fsmount", "fspick", "pivot_root", "chroot",
		"kexec_load", "kexec_file_load", "init_module", "finit_module", "delete_module",
		"perf_event_open", "open_by_handle_at", "name_to_handle_at",
	} {
		if unconditional[n] {
			t.Errorf("%s is allowed unconditionally", n)
		}
	}
	for n, want := range map[string]uint{"clone3": 38, "io_uring_setup": 38, "socketcall": 38, "bpf": 1, "mount": 1, "ptrace": 1, "keyctl": 1, "userfaultfd": 1, "setns": 1, "init_module": 1} {
		if errno[n] != want {
			t.Errorf("%s: errno %d, want %d", n, errno[n], want)
		}
	}

	// socket(): exactly AF_UNIX, AF_INET, AF_INET6, compared on arg 0.
	fams := map[uint64]bool{}
	for _, r := range filtered["socket"] {
		if len(r.Args) != 1 || r.Args[0].Index != 0 || r.Args[0].Op != "SCMP_CMP_EQ" {
			t.Errorf("unexpected socket rule: %+v", r)
			continue
		}
		fams[r.Args[0].Value] = true
	}
	if len(fams) != 3 || !fams[1] || !fams[2] || !fams[10] {
		t.Errorf("allowed socket families %v, want {1, 2, 10}", fams)
	}

	// clone and unshare: allowed only when no CLONE_NEW* bit is set.
	const newuser, newnet, newns = 0x10000000, 0x40000000, 0x00020000
	for _, n := range []string{"clone", "unshare"} {
		if len(filtered[n]) == 0 {
			t.Errorf("%s has no rule; plain fork/threads would break", n)
		}
		for _, r := range filtered[n] {
			a := r.Args[0]
			if len(r.Args) != 1 || a.Op != "SCMP_CMP_MASKED_EQ" || a.ValueTwo != 0 {
				t.Errorf("%s: unexpected rule %+v", n, r)
				continue
			}
			for _, bit := range []uint64{newuser, newnet, newns, 0x20000000, 0x08000000, 0x04000000, 0x02000000} {
				if a.Value&bit == 0 {
					t.Errorf("%s: mask %#x does not cover namespace flag %#x", n, a.Value, bit)
				}
			}
			// The flags argument is 0 everywhere except s390, where it is 1.
			s390 := len(r.Includes.Arches) > 0
			if (a.Index == 1) != s390 || a.Index > 1 {
				t.Errorf("%s: arg index %d with arches %v", n, a.Index, r.Includes.Arches)
			}
		}
	}

	// The things a Python/Bash workload cannot live without stay allowed.
	for _, n := range []string{"read", "write", "openat", "execve", "mmap", "futex", "fork", "vfork", "wait4", "pipe2",
		"socketpair", "connect", "bind", "epoll_wait", "getrandom", "ioctl", "rt_sigaction", "set_robust_list", "rseq",
		"memfd_create", "pidfd_open", "prctl", "seccomp", "statx", "getdents64", "copy_file_range"} {
		if !unconditional[n] {
			t.Errorf("%s is not allowed", n)
		}
	}
}

func TestWriteSeccompProfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	p1, err := WriteSeccompProfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p1) || filepath.Dir(p1) != dir {
		t.Errorf("path %q not inside %q", p1, dir)
	}
	got, err := os.ReadFile(p1)
	if err != nil || !bytes.Equal(got, SeccompProfile()) {
		t.Fatalf("written profile differs (err %v)", err)
	}
	fi, _ := os.Stat(p1)
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, want 0644", fi.Mode().Perm())
	}
	// Idempotent, repairs a corrupted file, leaves no temp files behind.
	if err := os.WriteFile(p1, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	p2, err := WriteSeccompProfile(dir)
	if err != nil || p2 != p1 {
		t.Fatalf("second write: %q, %v", p2, err)
	}
	if got, _ := os.ReadFile(p1); !bytes.Equal(got, SeccompProfile()) {
		t.Error("corrupted profile was not rewritten")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want 1", len(entries))
	}
	// The result is accepted as Config.SeccompProfilePath.
	if _, err := New(Config{SocketPath: "/s", SeccompProfilePath: p1}); err != nil {
		t.Error(err)
	}
}

package podman

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// The hardened seccomp profile.
//
// It is generated, not hand-written: seccomp_gen_test.go takes the
// containers/common default profile (testdata/seccomp-upstream.json, a
// deny-by-default allowlist that answers ENOSYS to anything unlisted) and
// subtracts from it. Run `go test -run TestSeccompProfileUpToDate
// -update-seccomp` to regenerate after changing the rules or refreshing
// the upstream file. What is removed, and why:
//
//   - Namespace creation. unshare and clone are allowed only without any
//     CLONE_NEW* flag; setns is denied; clone3 returns ENOSYS because its
//     flags live in a struct that seccomp cannot read, and ENOSYS makes
//     glibc fall back to clone. A nested user namespace is how a process
//     with no capabilities gets CAP_NET_ADMIN and CAP_SYS_ADMIN over new
//     namespaces, which is the entry point of most recent kernel escapes.
//   - The mount API (mount, umount*, the fsopen family, open_tree,
//     move_mount, mount_setattr, pivot_root, chroot).
//   - bpf, perf_event_open, userfaultfd, io_uring_* (ENOSYS, so runtimes
//     fall back to epoll), the keyring calls, ptrace and
//     process_vm_readv/writev, kcmp, pidfd_getfd, the file-handle calls,
//     kexec, module loading, modify_ldt, and the administrative calls
//     that need a capability the sandbox never has.
//   - socket() is allowed for AF_UNIX, AF_INET and AF_INET6 only. That
//     removes AF_NETLINK, AF_PACKET, AF_ALG, AF_RXRPC, AF_VSOCK and the
//     rest; socketcall (the 32-bit multiplexer that would bypass the
//     argument filter) returns ENOSYS.
//
// Known consequences: debuggers and strace do not work; anything that
// builds its own sandbox from namespaces (bubblewrap, Chromium's sandbox)
// does not work; getifaddrs() and `ip` fail because they use netlink.
//
// It cannot close kernel bugs reachable through syscalls CPython needs
// (AF_UNIX, futex, ordinary file I/O). Only a non-shared-kernel runtime
// does that.
//
//go:embed seccomp/sbx-seccomp.json
var seccompProfile []byte

// SeccompProfile returns the embedded hardened profile (JSON).
func SeccompProfile() []byte { return append([]byte(nil), seccompProfile...) }

// WriteSeccompProfile writes the embedded profile into dir and returns its
// path, for use as Config.SeccompProfilePath. The file name carries a
// content hash, so a changed profile gets a new path and containers
// created earlier keep referring to the file they were created with. The
// write is atomic and idempotent.
func WriteSeccompProfile(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(seccompProfile)
	path := filepath.Join(abs, "sbx-seccomp-"+hex.EncodeToString(sum[:6])+".json")
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(seccompProfile) {
		return path, nil
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(abs, ".sbx-seccomp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(seccompProfile); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("podman: install seccomp profile: %w", err)
	}
	return path, nil
}

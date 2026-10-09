package podman

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"testing"
)

var updateSeccomp = flag.Bool("update-seccomp", false, "regenerate seccomp/sbx-seccomp.json from testdata/seccomp-upstream.json")

type seccompArg struct {
	Index    uint   `json:"index"`
	Value    uint64 `json:"value"`
	ValueTwo uint64 `json:"valueTwo"`
	Op       string `json:"op"`
}

type seccompFilter struct {
	Caps   []string `json:"caps,omitempty"`
	Arches []string `json:"arches,omitempty"`
}

type seccompRule struct {
	Names    []string      `json:"names"`
	Action   string        `json:"action"`
	Args     []seccompArg  `json:"args"`
	Comment  string        `json:"comment"`
	Includes seccompFilter `json:"includes"`
	Excludes seccompFilter `json:"excludes"`
	ErrnoRet *uint         `json:"errnoRet,omitempty"`
	Errno    string        `json:"errno,omitempty"`
}

type seccompDoc struct {
	DefaultAction   string          `json:"defaultAction"`
	DefaultErrnoRet *uint           `json:"defaultErrnoRet,omitempty"`
	DefaultErrno    string          `json:"defaultErrno,omitempty"`
	ArchMap         json.RawMessage `json:"archMap"`
	Syscalls        []seccompRule   `json:"syscalls"`
}

const (
	// Every CLONE_NEW* flag that clone(2) accepts: NEWNS, NEWCGROUP,
	// NEWUTS, NEWIPC, NEWUSER, NEWPID, NEWNET.
	cloneNSMask = 0x00020000 | 0x02000000 | 0x04000000 | 0x08000000 | 0x10000000 | 0x20000000 | 0x40000000
	// unshare(2) additionally accepts CLONE_NEWTIME (0x80).
	unshareNSMask = cloneNSMask | 0x80
)

// seccompDenyEPERM is removed from every allow rule and answered EPERM.
var seccompDenyEPERM = []string{
	// namespaces and the mount API
	"setns", "mount", "umount", "umount2", "mount_setattr", "move_mount", "open_tree", "open_tree_attr",
	"fsopen", "fsconfig", "fsmount", "fspick", "pivot_root", "chroot",
	// kernel attack surface with no use in a Python/Bash workload
	"bpf", "perf_event_open", "userfaultfd", "keyctl", "add_key", "request_key",
	"kexec_load", "kexec_file_load", "init_module", "finit_module", "delete_module", "query_module", "create_module",
	"name_to_handle_at", "open_by_handle_at", "lookup_dcookie", "modify_ldt",
	// reaching into other processes
	"ptrace", "process_vm_readv", "process_vm_writev", "kcmp", "process_madvise", "pidfd_getfd",
	// administrative calls gated on capabilities the sandbox never has
	"quotactl", "quotactl_fd", "sethostname", "setdomainname", "acct", "ioperm", "iopl",
	"clock_settime", "clock_settime64", "settimeofday", "stime", "vhangup",
	"swapon", "swapoff", "reboot", "syslog", "fanotify_init",
}

// seccompDenyENOSYS is removed from every allow rule and answered ENOSYS,
// the errno that makes callers take their fallback path.
var seccompDenyENOSYS = []string{"clone3", "io_uring_setup", "io_uring_enter", "io_uring_register", "socketcall"}

// Allowed socket(2) address families: AF_UNIX, AF_INET, AF_INET6.
var seccompSocketFamilies = []uint64{1, 2, 10}

func uintp(v uint) *uint { return &v }

// buildSeccompProfile derives the hardened profile from the upstream one.
func buildSeccompProfile(upstream []byte) ([]byte, error) {
	var doc seccompDoc
	if err := json.Unmarshal(upstream, &doc); err != nil {
		return nil, err
	}
	if doc.DefaultAction != "SCMP_ACT_ERRNO" {
		return nil, fmt.Errorf("upstream profile is not deny-by-default (defaultAction %q)", doc.DefaultAction)
	}
	special := map[string]bool{"clone": true, "unshare": true, "socket": true}
	enosys := map[string]bool{}
	for _, n := range seccompDenyENOSYS {
		enosys[n] = true
	}
	eperm := map[string]bool{}
	for _, n := range seccompDenyEPERM {
		eperm[n] = true
	}

	var rules []seccompRule
	for _, r := range doc.Syscalls {
		// Capability-conditional rules: the sandbox has no capabilities,
		// so "includes caps" rules never apply and are dropped, and the
		// names of "excludes caps" deny rules are denied unconditionally.
		if len(r.Includes.Caps) > 0 {
			continue
		}
		if len(r.Excludes.Caps) > 0 || r.Action != "SCMP_ACT_ALLOW" {
			for _, n := range r.Names {
				if !special[n] && !enosys[n] {
					eperm[n] = true
				}
			}
			continue
		}
		rules = append(rules, r)
	}
	// Strip everything denied or specially handled from the allow rules.
	kept := rules[:0]
	for _, r := range rules {
		var names []string
		for _, n := range r.Names {
			if !eperm[n] && !enosys[n] && !special[n] {
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			continue
		}
		r.Names = names
		kept = append(kept, r)
	}

	sorted := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for n := range m {
			out = append(out, n)
		}
		sort.Strings(out)
		return out
	}
	masked := func(index uint, mask uint64) []seccompArg {
		return []seccompArg{{Index: index, Value: mask, ValueTwo: 0, Op: "SCMP_CMP_MASKED_EQ"}}
	}
	s390 := []string{"s390", "s390x"}
	out := []seccompRule{
		{Names: sorted(eperm), Action: "SCMP_ACT_ERRNO", ErrnoRet: uintp(1), Errno: "EPERM",
			Comment: "sbx: denied outright"},
		{Names: sorted(enosys), Action: "SCMP_ACT_ERRNO", ErrnoRet: uintp(38), Errno: "ENOSYS",
			Comment: "sbx: denied as 'not implemented' so callers fall back (clone3 -> clone, io_uring -> epoll)"},
		{Names: []string{"clone"}, Action: "SCMP_ACT_ALLOW", Args: masked(0, cloneNSMask),
			Excludes: seccompFilter{Arches: s390}, Comment: "sbx: clone without any CLONE_NEW* flag"},
		{Names: []string{"clone"}, Action: "SCMP_ACT_ALLOW", Args: masked(1, cloneNSMask),
			Includes: seccompFilter{Arches: s390}, Comment: "sbx: s390 passes clone flags as the second argument"},
		{Names: []string{"unshare"}, Action: "SCMP_ACT_ALLOW", Args: masked(0, unshareNSMask),
			Comment: "sbx: unshare without any CLONE_NEW* flag"},
	}
	for _, fam := range seccompSocketFamilies {
		out = append(out, seccompRule{Names: []string{"socket"}, Action: "SCMP_ACT_ALLOW",
			Args:    []seccompArg{{Index: 0, Value: fam, Op: "SCMP_CMP_EQ"}},
			Comment: "sbx: AF_UNIX, AF_INET and AF_INET6 only"})
	}
	doc.Syscalls = append(out, kept...)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "\t")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TestSeccompProfileUpToDate keeps the embedded profile reproducible from
// the upstream default plus the rules above.
func TestSeccompProfileUpToDate(t *testing.T) {
	upstream, err := os.ReadFile("testdata/seccomp-upstream.json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := buildSeccompProfile(upstream)
	if err != nil {
		t.Fatal(err)
	}
	if *updateSeccomp {
		if err := os.WriteFile("seccomp/sbx-seccomp.json", want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("seccomp/sbx-seccomp.json regenerated; rebuild to embed it")
		return
	}
	if !bytes.Equal(want, seccompProfile) {
		t.Fatal("seccomp/sbx-seccomp.json is stale; run: go test -run TestSeccompProfileUpToDate -update-seccomp")
	}
}

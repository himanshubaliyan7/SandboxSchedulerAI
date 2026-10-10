package agent

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
	"github.com/himanshubaliyan7/warmbox/internal/agentclient"
)

func TestFiles(t *testing.T) {
	h := startAgent(t, opts{})
	ctx := testCtx(t)

	// Multi-megabyte round trip, relative path, parents created.
	big := make([]byte, 5<<20+123)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	// iotest-style reader without a Len method forces a streamed body.
	if err := h.c.WriteFile(ctx, "a/b/big.bin", 0o640, io.MultiReader(bytes.NewReader(big[:1<<20]), bytes.NewReader(big[1<<20:]))); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(h.workspace, "a/b/big.bin"))
	if err != nil || !bytes.Equal(onDisk, big) {
		t.Fatalf("file on disk differs (err %v, %d bytes)", err, len(onDisk))
	}
	var back bytes.Buffer
	n, err := h.c.ReadFile(ctx, "a/b/big.bin", &back)
	if err != nil || n != int64(len(big)) || !bytes.Equal(back.Bytes(), big) {
		t.Fatalf("read back %d bytes, err %v", n, err)
	}
	body, size, err := h.c.OpenFile(ctx, filepath.Join(h.workspace, "a/b/big.bin"))
	if err != nil || size != int64(len(big)) {
		t.Fatalf("OpenFile size %d err %v", size, err)
	}
	body.Close()

	// Default mode, overwrite, empty file.
	if err := h.c.WriteFile(ctx, "a/b/small.txt", 0, strings.NewReader("one")); err != nil {
		t.Fatal(err)
	}
	if err := h.c.WriteFile(ctx, "a/b/small.txt", 0, strings.NewReader("two!")); err != nil {
		t.Fatal(err)
	}
	if err := h.c.WriteFile(ctx, "a/b/empty", 0o600, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if created, err := h.c.Mkdir(ctx, "a/b/dir/deep"); err != nil || !created {
		t.Fatalf("mkdir with missing parents: created %v, err %v", created, err)
	}
	if created, err := h.c.Mkdir(ctx, "a/b/dir/deep"); err != nil || created {
		t.Fatalf("mkdir of an existing directory: created %v, err %v", created, err)
	}
	if created, err := h.c.Mkdir(ctx, "a/b/dir"); err != nil || created {
		t.Fatalf("mkdir of an existing parent: created %v, err %v", created, err)
	}

	ls, err := h.c.ListFiles(ctx, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		name  string
		size  int64
		mode  uint32
		isDir bool
	}
	var got []row
	for _, fi := range ls {
		if fi.Path != filepath.Join(h.workspace, "a/b", fi.Name) || fi.ModTime.IsZero() {
			t.Errorf("entry %+v", fi)
		}
		r := row{fi.Name, fi.Size, fi.Mode, fi.IsDir}
		if fi.IsDir {
			r.size, r.mode = 0, 0
		}
		got = append(got, r)
	}
	want := []row{{"big.bin", int64(len(big)), 0o640, false}, {"dir", 0, 0, true}, {"empty", 0, 0o600, false}, {"small.txt", 4, 0o644, false}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("list = %v, want %v (no temp files left)", got, want)
	}

	// A process sees what was written.
	r := h.collect(h.start(agentapi.StartRequest{Cmd: "cat", Args: []string{"a/b/small.txt"}}).ID, 0)
	if string(r.stdout) != "two!" {
		t.Fatalf("cat = %q", r.stdout)
	}

	// Errors.
	if _, err := h.c.ReadFile(ctx, "missing.txt", io.Discard); !agentclient.IsNotFound(err) {
		t.Errorf("read missing: %v, want 404", err)
	}
	if _, err := h.c.ListFiles(ctx, "missing-dir"); !agentclient.IsNotFound(err) {
		t.Errorf("list missing: %v, want 404", err)
	}
	if err := h.c.DeleteFile(ctx, "missing.txt"); !agentclient.IsNotFound(err) {
		t.Errorf("delete missing: %v, want 404", err)
	}
	if _, err := h.c.ReadFile(ctx, "a/b", io.Discard); !agentclient.IsConflict(err) {
		t.Errorf("read a directory: %v, want 409", err)
	}
	if err := h.c.WriteFile(ctx, "a/b", 0, strings.NewReader("x")); !agentclient.IsConflict(err) {
		t.Errorf("write over a directory: %v, want 409", err)
	}
	if _, err := h.c.ListFiles(ctx, "a/b/small.txt"); !agentclient.IsConflict(err) {
		t.Errorf("list a file: %v, want 409", err)
	}
	if _, err := h.c.Mkdir(ctx, "a/b/small.txt/x"); !agentclient.IsConflict(err) {
		t.Errorf("mkdir under a file: %v, want 409", err)
	}
	if _, err := h.c.ReadFile(ctx, "", io.Discard); agentclient.StatusOf(err) != http.StatusBadRequest {
		t.Errorf("empty path: %v, want 400", err)
	}
	if os.Getuid() != 0 {
		ro := filepath.Join(h.dir, "ro")
		if err := os.Mkdir(ro, 0o555); err != nil {
			t.Fatal(err)
		}
		if err := h.c.WriteFile(ctx, filepath.Join(ro, "f"), 0, strings.NewReader("x")); agentclient.StatusOf(err) != http.StatusForbidden {
			t.Errorf("write into a read-only directory: %v, want 403", err)
		}
	}

	// Delete a file, then a whole tree.
	if err := h.c.DeleteFile(ctx, "a/b/small.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.ReadFile(ctx, "a/b/small.txt", io.Discard); !agentclient.IsNotFound(err) {
		t.Errorf("read deleted: %v, want 404", err)
	}
	if err := h.c.DeleteFile(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if ls, err := h.c.ListFiles(ctx, "."); err != nil || len(ls) != 0 {
		t.Fatalf("workspace after delete: %v, %v", ls, err)
	}
}

func TestInitEnvAndGeneration(t *testing.T) {
	h := startAgent(t, opts{})
	ctx := testCtx(t)
	req := agentapi.InitRequest{SandboxID: "sb1", Generation: 7, Env: map[string]string{"SESSION": "s1", "BOTH": "init"}}
	if err := h.c.Init(ctx, req); err != nil {
		t.Fatal(err)
	}
	hl, err := h.c.Health(ctx)
	if err != nil || !hl.Initialized {
		t.Fatalf("health = %+v, %v", hl, err)
	}
	// A retry of the same init is accepted.
	if err := h.c.Init(ctx, req); err != nil {
		t.Fatalf("same init again: %v", err)
	}
	// A different generation is not, and changes nothing.
	other := agentapi.InitRequest{SandboxID: "sb1", Generation: 8, Env: map[string]string{"SESSION": "s2"}}
	err = h.c.Init(ctx, other)
	var ae *agentclient.Error
	if !asError(err, &ae) || ae.Status != http.StatusConflict || ae.Code != "already_initialized" {
		t.Fatalf("init with another generation: err = %v, want 409 already_initialized", err)
	}
	if err := h.c.Init(ctx, agentapi.InitRequest{SandboxID: "sb2", Generation: 7}); !agentclient.IsConflict(err) {
		t.Fatalf("init with another sandbox id: err = %v, want 409", err)
	}

	sreq := sh("echo $SESSION/$BOTH/$HTTP_PROXY.")
	sreq.Env = map[string]string{"BOTH": "start"}
	r := h.collect(h.start(sreq).ID, 0)
	if string(r.stdout) != "s1/start/.\n" {
		t.Fatalf("env = %q, want session env, request override, no proxy", r.stdout)
	}
}

func TestEgressBridge(t *testing.T) {
	h := startAgent(t, opts{})
	ctx := testCtx(t)

	// A fake egress proxy on the Unix socket next to the agent socket.
	ln, err := net.Listen("unix", filepath.Join(h.dir, "egress.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						fmt.Fprintf(c, "eof\n") // half-close reached us
						return
					}
					fmt.Fprintf(c, "proxy saw: %s", line)
				}
			}()
		}
	}()

	if err := h.c.Init(ctx, agentapi.InitRequest{SandboxID: "sb", Generation: 1, EgressProxy: true}); err != nil {
		t.Fatal(err)
	}
	r := h.collect(h.start(agentapi.StartRequest{Cmd: "env"}).ID, 0)
	env := map[string]string{}
	for _, line := range strings.Split(string(r.stdout), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	proxy := env["HTTP_PROXY"]
	if !strings.HasPrefix(proxy, "http://127.0.0.1:") {
		t.Fatalf("HTTP_PROXY = %q", proxy)
	}
	for _, k := range []string{"http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if env[k] != proxy {
			t.Errorf("%s = %q, want %q", k, env[k], proxy)
		}
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		if !strings.Contains(env[k], "localhost") || !strings.Contains(env[k], "127.0.0.1") {
			t.Errorf("%s = %q", k, env[k])
		}
	}

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxy, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	for _, msg := range []string{"CONNECT example.com:443 HTTP/1.1\n", "second line\n"} {
		if _, err := io.WriteString(conn, msg); err != nil {
			t.Fatal(err)
		}
		line, err := br.ReadString('\n')
		if err != nil || line != "proxy saw: "+msg {
			t.Fatalf("through the bridge: %q, %v", line, err)
		}
	}
	// Half-close travels through, and the reply still comes back.
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(br)
	if err != nil || string(rest) != "eof\n" {
		t.Fatalf("after half-close: %q, %v", rest, err)
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	dir, err := os.MkdirTemp("", "sbx")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "agent.sock")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Config{SocketPath: sock})
	if ln, err := a.Listen(); err == nil {
		ln.Close()
		t.Fatal("Listen succeeded while another listener is live")
	}
	old.SetUnlinkOnClose(false)
	old.Close() // leaves a dead socket file behind
	if _, err := os.Stat(sock); err != nil {
		t.Fatal(err)
	}
	ln, err := a.Listen()
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	ln.Close()

	// Anything that is not a socket is left alone.
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ln, err := New(Config{SocketPath: file}).Listen(); err == nil {
		ln.Close()
		t.Fatal("Listen replaced a regular file")
	}
}

// The ring against a trivial model: an unbounded log.
func TestRingModel(t *testing.T) {
	rng := mrand.New(mrand.NewSource(1))
	chans := []agentapi.EventType{agentapi.EventStdout, agentapi.EventStderr}
	for _, limit := range []int{16, 100, 5000, 70000} {
		r := newRing(limit)
		var log []byte
		var who []agentapi.EventType
		for step := 0; step < 2000; step++ {
			n := rng.Intn(40)
			if rng.Intn(50) == 0 {
				n = rng.Intn(3 * limit)
			}
			p := make([]byte, n)
			rng.Read(p)
			ch := chans[rng.Intn(2)]
			r.write(ch, p)
			log = append(log, p...)
			for range p {
				who = append(who, ch)
			}
			if r.next != int64(len(log)) || r.size > limit || len(r.buf) > limit || len(r.segs) > r.maxSegs || r.next-r.base != int64(r.size) {
				t.Fatalf("limit %d step %d: next %d size %d cap %d segs %d base %d", limit, step, r.next, r.size, len(r.buf), len(r.segs), r.base)
			}
			// Read everything from a random offset and compare.
			from := int64(rng.Intn(len(log) + 2))
			cursor := from
			dst := make([]byte, 1+rng.Intn(64+limit/8))
			first := true
			for {
				ch, n, off, dropped := r.read(cursor, dst)
				if first {
					wantDropped := r.base - from
					if wantDropped < 0 {
						wantDropped = 0
					}
					if dropped != wantDropped {
						t.Fatalf("dropped = %d, want %d", dropped, wantDropped)
					}
					first = false
				} else if dropped != 0 {
					t.Fatalf("dropped %d mid-read", dropped)
				}
				if n == 0 {
					if off != r.next {
						t.Fatalf("read ended at %d, want %d", off, r.next)
					}
					break
				}
				if !bytes.Equal(dst[:n], log[off:off+int64(n)]) {
					t.Fatalf("limit %d: bytes at %d differ", limit, off)
				}
				for i := off; i < off+int64(n); i++ {
					if who[i] != ch {
						t.Fatalf("limit %d: byte %d reported as %s, was %s", limit, i, ch, who[i])
					}
				}
				cursor = off + int64(n)
			}
		}
		r.release()
		if _, n, off, dropped := r.read(0, make([]byte, 8)); n != 0 || off != r.next || dropped != r.next {
			t.Fatalf("after release: n %d off %d dropped %d", n, off, dropped)
		}
	}
}

// Channels alternating byte by byte must not grow the segment list
// without bound.
func TestRingSegmentBound(t *testing.T) {
	r := newRing(1 << 20)
	for i := 0; i < 200000; i++ {
		ch := agentapi.EventStdout
		if i%2 == 1 {
			ch = agentapi.EventStderr
		}
		r.write(ch, []byte{byte(i)})
	}
	if len(r.segs) > r.maxSegs || cap(r.segs) > 4*r.maxSegs {
		t.Fatalf("segments: len %d cap %d, max %d", len(r.segs), cap(r.segs), r.maxSegs)
	}
	if r.next != 200000 || r.next-r.base != int64(r.size) || r.size == 0 {
		t.Fatalf("next %d base %d size %d", r.next, r.base, r.size)
	}
}

// Exited processes do not accumulate output without bound.
func TestRetentionLimits(t *testing.T) {
	if os.Getenv("SBX_AGENT_TEST_MODE") == "subprocess" {
		t.Skip("needs in-process configuration")
	}
	dir, err := os.MkdirTemp("", "sbx")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	m := newManager(Config{Workspace: dir, MaxExited: 3, MaxRetainedBytes: 10000, RingBytes: 1 << 20}.withDefaults(), newReaper(false))
	var ids []string
	for i := 0; i < 5; i++ {
		p, err := m.start(sh("head -c 6000 /dev/zero"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.id)
		for !p.exited() {
			time.Sleep(5 * time.Millisecond)
		}
	}
	time.Sleep(50 * time.Millisecond) // onDone runs just after the exit is published
	if got := m.list(); len(got) != 3 || got[0].ID != ids[2] {
		t.Fatalf("remembered %d processes, want the last 3", len(got))
	}
	if _, err := m.get(ids[0]); err == nil {
		t.Fatal("oldest exited process is still known")
	}
	var held int
	for _, id := range ids[2:] {
		p, err := m.get(id)
		if err != nil {
			t.Fatal(err)
		}
		p.mu.Lock()
		held += p.ring.retained()
		if p.ring.next != 6000 {
			t.Fatalf("NextOffset = %d", p.ring.next)
		}
		p.mu.Unlock()
	}
	if held > 10000 || held == 0 {
		t.Fatalf("exited processes hold %d bytes, budget 10000", held)
	}
}

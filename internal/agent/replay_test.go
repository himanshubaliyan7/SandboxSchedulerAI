package agent

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
)

func TestReplayAfterDisconnect(t *testing.T) {
	h := startAgent(t, opts{})
	pi := h.start(sh("printf AAAA; sleep 0.4; printf BBBB; sleep 0.4; printf CCCC >&2"))

	// Read the first chunk only, then drop the connection.
	ctx, cancel := context.WithCancel(testCtx(t))
	var got []byte
	var next int64
	for ev, err := range h.c.Stream(ctx, pi.ID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == agentapi.EventStdout {
			got = append(got, ev.Data...)
			next = ev.Offset + int64(len(ev.Data))
			break
		}
	}
	cancel()
	if string(got) != "AAAA" || next != 4 {
		t.Fatalf("first read = %q next %d", got, next)
	}

	// Reconnect: exactly the missing bytes, on the right channels.
	r := h.collect(pi.ID, next)
	if string(r.stdout) != "BBBB" || string(r.stderr) != "CCCC" {
		t.Fatalf("resumed stdout %q stderr %q", r.stdout, r.stderr)
	}
	if len(r.truncated) != 0 || r.exitOffset != 12 {
		t.Fatalf("truncated %v exit offset %d", r.truncated, r.exitOffset)
	}

	// After exit the whole output can still be replayed, from any offset.
	r = h.collect(pi.ID, 0)
	if string(r.all) != "AAAABBBBCCCC" {
		t.Fatalf("full replay = %q", r.all)
	}
	r = h.collect(pi.ID, 6)
	if string(r.all) != "BBCCCC" || r.events[0].Offset != 6 {
		t.Fatalf("replay from 6 = %q", r.all)
	}
	r = h.collect(pi.ID, 12)
	if len(r.all) != 0 || len(r.events) != 1 {
		t.Fatalf("replay from the end = %+v", r.events)
	}
	// Beyond the end is treated as the end.
	r = h.collect(pi.ID, 12)
	if len(r.all) != 0 {
		t.Fatalf("replay beyond the end = %+v", r.events)
	}
}

func TestReplayTruncated(t *testing.T) {
	const ringBytes = 100
	h := startAgent(t, opts{ring: ringBytes})
	// 1000 bytes: 0123456789 repeated.
	pi := h.start(sh("i=0; while [ $i -lt 100 ]; do printf 0123456789; i=$((i+1)); done"))
	h.waitExit(pi.ID)

	r := h.collect(pi.ID, 10)
	if len(r.truncated) != 1 {
		t.Fatalf("got %d truncated events, want 1: %+v", len(r.truncated), r.events)
	}
	tr := r.truncated[0]
	if r.events[0].Type != agentapi.EventTruncated || tr.Dropped != 1000-ringBytes-10 || tr.Offset != 1000-ringBytes {
		t.Fatalf("truncated = %+v, want first, dropped %d, offset %d", tr, 1000-ringBytes-10, 1000-ringBytes)
	}
	if want := bytes.Repeat([]byte("0123456789"), ringBytes/10); !bytes.Equal(r.all, want) {
		t.Fatalf("data after truncation = %q", r.all)
	}
	if r.exitOffset != 1000 {
		t.Fatalf("exit offset = %d", r.exitOffset)
	}
	// A from still inside the ring is lossless.
	r = h.collect(pi.ID, 950)
	if len(r.truncated) != 0 || len(r.all) != 50 {
		t.Fatalf("from 950: truncated %v, %d bytes", r.truncated, len(r.all))
	}
}

// A reader that does not read must not hold the process up; it falls
// behind and is told how much it lost.
func TestSlowStreamReaderDoesNotBlockProcess(t *testing.T) {
	h := startAgent(t, opts{ring: 64 << 10})
	const total = 8 << 20
	pi := h.start(sh("sleep 0.3; head -c 8388608 /dev/zero"))

	// Attach while the process is still silent, read one event (none
	// yet, so just start the request) and then stop reading.
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	pause := make(chan struct{})
	type out struct {
		bytes, dropped int64
		exit           *agentapi.ExitStatus
	}
	done := make(chan out, 1)
	go func() {
		var o out
		first := true
		for ev, err := range h.c.Stream(ctx, pi.ID, 0) {
			if err != nil {
				t.Errorf("slow stream: %v", err)
				break
			}
			if first && len(ev.Data) > 0 {
				first = false
				<-pause // stall with the response half read
			}
			o.bytes += int64(len(ev.Data))
			o.dropped += ev.Dropped
			if ev.Type == agentapi.EventExit {
				o.exit = ev.Exit
			}
		}
		done <- o
	}()

	// The process finishes although that reader is stalled...
	st := h.waitExit(pi.ID)
	wantExit(t, &st, 0, "", agentapi.ExitExited)
	// ...and a second reader is served meanwhile.
	r := h.collect(pi.ID, total-10)
	if len(r.all) != 10 {
		t.Fatalf("second reader got %d bytes", len(r.all))
	}

	close(pause)
	select {
	case o := <-done:
		if o.exit == nil || o.dropped == 0 || o.bytes+o.dropped != total {
			t.Fatalf("slow reader: %d bytes + %d dropped (want sum %d, some dropped), exit %v", o.bytes, o.dropped, total, o.exit)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("slow reader never finished")
	}
}

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
)

func (h *harness) nextOffset(id string) int64 {
	h.t.Helper()
	pi, err := h.c.Get(testCtx(h.t), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return pi.NextOffset
}

// checkBounded asserts what "memory stays bounded" means for the agent.
func (h *harness) checkBounded(id string, ringBytes int) {
	h.t.Helper()
	if h.a != nil {
		p, err := h.a.procs.get(id)
		if err != nil {
			h.t.Fatal(err)
		}
		p.mu.Lock()
		held, segs, size := p.ring.retained(), len(p.ring.segs), p.ring.size
		p.mu.Unlock()
		if held > ringBytes || size > ringBytes || segs > ringBytes/64+64 {
			h.t.Fatalf("ring holds %d bytes (%d used, %d segments), limit %d", held, size, segs, ringBytes)
		}
		return
	}
	if kb := rssKB(h.t, h.pid); kb > 400<<10 {
		h.t.Fatalf("agent RSS is %d kB", kb)
	}
}

// `yes` on a PTY with a client that acknowledges slowly: the agent stops
// reading the PTY, so `yes` blocks and produces only what the client has
// acknowledged plus the high watermark; Ctrl+C still gets through at once.
func TestBackpressureYesSlowAckAndCtrlC(t *testing.T) {
	h := startAgent(t, opts{})
	ctx := testCtx(t)
	pi := h.start(ptyReq("yes"))
	ws, err := h.c.DialWS(ctx, pi.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	// slack: the pump may read one more chunk per watermark crossing, and
	// an ACK and the Get that samples NextOffset are not atomic.
	const slack = 4 * agentapi.MaxFrameBytes

	// The stall is observed through NextOffset rather than by timing out
	// a Recv, because a cancelled read closes a coder/websocket
	// connection.
	var acked int64
	begin := time.Now()
	var rounds int
	for time.Since(begin) < 2*time.Second {
		// Consume up to a quarter megabyte, acknowledging in small steps
		// (smaller than the low watermark), slowly.
		target := ws.Offset() + 256<<10
		for ws.Offset() < target {
			f, err := ws.Recv(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if f.Status != nil {
				t.Fatalf("yes ended early: %+v", f.Status)
			}
			if ws.Offset()-acked >= 8<<10 {
				acked = ws.Offset()
				if err := ws.Ack(ctx, acked); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Millisecond) // a slow consumer
			}
		}
		// Now stop acknowledging and stop reading. Production must stop
		// within the high watermark of the last acknowledgement.
		time.Sleep(150 * time.Millisecond)
		a := h.nextOffset(pi.ID)
		time.Sleep(150 * time.Millisecond)
		b := h.nextOffset(pi.ID)
		if a != b {
			t.Fatalf("round %d: process still producing while unacknowledged: %d -> %d (acked %d)", rounds, a, b, acked)
		}
		if lag := b - acked; lag > agentapi.FlowHighWatermark+slack || lag < agentapi.FlowLowWatermark {
			t.Fatalf("round %d: %d bytes produced beyond the last ack, want within (%d, %d]", rounds, lag,
				agentapi.FlowLowWatermark, agentapi.FlowHighWatermark+slack)
		}
		h.checkBounded(pi.ID, agentapi.DefaultRingBytes)
		rounds++
	}
	if rounds < 2 {
		t.Fatalf("only %d rounds completed", rounds)
	}
	total := h.nextOffset(pi.ID)
	// Unthrottled, `yes` writes hundreds of megabytes in this time.
	if total > acked+agentapi.FlowHighWatermark+slack {
		t.Fatalf("produced %d bytes with %d acknowledged", total, acked)
	}
	t.Logf("%d rounds, %d bytes produced, %d acknowledged", rounds, total, acked)

	// Ctrl+C while paused and with nobody acknowledging.
	sent := time.Now()
	if err := ws.Write(ctx, []byte{0x03}); err != nil {
		t.Fatal(err)
	}
	st, _ := wsStatus(t, ws, 3*time.Second)
	wantExit(t, st.Exit, 128+2, "SIGINT", agentapi.ExitKilled)
	t.Logf("Ctrl+C to exit status: %v", time.Since(sent))
}

// A client that never acknowledges does not pause the process: `yes`
// runs free, the ring drops old bytes, memory stays bounded, and both
// Ctrl+C and an HTTP signal still work even though the client is not
// reading its socket at all.
func TestYesWithoutAcksStaysBoundedAndInterruptible(t *testing.T) {
	const ringBytes = 256 << 10
	for _, how := range []string{"ctrl-c", "http-signal"} {
		t.Run(how, func(t *testing.T) {
			h := startAgent(t, opts{ring: ringBytes})
			ctx := testCtx(t)
			pi := h.start(ptyReq("yes"))
			ws, err := h.c.DialWS(ctx, pi.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()

			deadline := time.Now().Add(10 * time.Second)
			for h.nextOffset(pi.ID) < 8*ringBytes {
				if time.Now().After(deadline) {
					t.Fatalf("yes produced only %d bytes: it is being held back", h.nextOffset(pi.ID))
				}
				time.Sleep(20 * time.Millisecond)
			}
			h.checkBounded(pi.ID, ringBytes)

			sent := time.Now()
			if how == "ctrl-c" {
				err = ws.Write(ctx, []byte{0x03})
			} else {
				err = h.c.Signal(ctx, pi.ID, "SIGINT")
			}
			if err != nil {
				t.Fatal(err)
			}
			wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			for {
				got, err := h.c.Get(wctx, pi.ID)
				if err != nil {
					t.Fatalf("waiting for exit: %v", err)
				}
				if got.Exit != nil {
					wantExit(t, got.Exit, 128+2, "SIGINT", agentapi.ExitKilled)
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Logf("%s to exit: %v", how, time.Since(sent))
			h.checkBounded(pi.ID, ringBytes)

			// The stalled client now reads: it lost data (OffsetFrame
			// with Dropped) and still gets the exit status.
			var dropped int64
			rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
			defer rcancel()
			for {
				f, err := ws.Recv(rctx)
				if err != nil {
					t.Fatal(err)
				}
				if f.Channel == agentapi.ChannelOffset {
					dropped += f.Dropped
				}
				if f.Status != nil {
					wantExit(t, f.Status.Exit, 128+2, "SIGINT", agentapi.ExitKilled)
					break
				}
			}
			if dropped == 0 {
				t.Errorf("stalled client was not told about dropped output")
			}
		})
	}
}

// Disconnecting a flow-controlled client releases the process.
func TestFlowControlReleasedOnDisconnect(t *testing.T) {
	h := startAgent(t, opts{})
	ctx := testCtx(t)
	pi := h.start(ptyReq("yes"))
	ws, err := h.c.DialWS(ctx, pi.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Ack(ctx, 0); err != nil {
		t.Fatal(err)
	}
	// Paused within the high watermark of offset 0.
	time.Sleep(300 * time.Millisecond)
	a := h.nextOffset(pi.ID)
	time.Sleep(200 * time.Millisecond)
	if b := h.nextOffset(pi.ID); a != b || b > agentapi.FlowHighWatermark+4*agentapi.MaxFrameBytes {
		t.Fatalf("not paused: %d -> %d", a, b)
	}
	ws.Close()
	deadline := time.Now().Add(5 * time.Second)
	for h.nextOffset(pi.ID) < a+(4<<20) {
		if time.Now().After(deadline) {
			t.Fatalf("process still paused after the client left: offset %d", h.nextOffset(pi.ID))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := h.c.Signal(ctx, pi.ID, "SIGKILL"); err != nil {
		t.Fatal(err)
	}
	h.waitExit(pi.ID)
}

// AutoAck keeps a fast consumer flowing without stalls.
func TestAutoAckFlows(t *testing.T) {
	h := startAgent(t, opts{})
	ctx := testCtx(t)
	const total = 4 << 20
	pi := h.start(ptyReq("sh", "-c", "stty -opost; head -c 4194304 /dev/zero"))
	ws, err := h.c.DialWS(ctx, pi.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.AutoAck = true
	var n, dropped int64
	for {
		f, err := ws.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n += int64(len(f.Data))
		dropped += f.Dropped
		if f.Status != nil {
			wantExit(t, f.Status.Exit, 0, "", agentapi.ExitExited)
			break
		}
	}
	// Lossless: flow control never lets the client fall out of the ring.
	if n != total || dropped != 0 {
		t.Fatalf("got %d bytes, %d dropped, want %d and 0", n, dropped, total)
	}
}

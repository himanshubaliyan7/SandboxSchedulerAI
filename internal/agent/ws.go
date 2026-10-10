package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
)

// wsSession is one WebSocket attached to a process.
//
// Three goroutines serve it and none of them can hold up another:
//
//   - the writer (the handler goroutine) copies output from the ring
//     buffer to the socket; it may block for as long as the client does
//     not read;
//   - the reader handles client frames; it never writes to the socket and
//     never blocks on the process, so a signal, resize or ACK is acted on
//     at once even while the writer is blocked;
//   - the stdin goroutine writes queued input to the process, which may
//     block while the process is not reading.
type wsSession struct {
	p    *Process
	conn *websocket.Conn
	fc   *flowConn
	sent atomic.Int64 // offset up to which output has been written

	wake      chan struct{} // writer: pongs or a fatal error are pending
	stdinWake chan struct{}

	mu         sync.Mutex
	pongs      [][]byte
	stdinQ     [][]byte
	stdinBytes int
	fatal      *agentapi.Error
}

const maxPendingPongs = 16

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// handleWS serves GET /processes/{id}/ws?from=N.
func (a *Agent) handleWS(w http.ResponseWriter, r *http.Request, p *Process) {
	from, err := parseFrom(r)
	if err != nil {
		writeError(w, err)
		return
	}
	// The agent is reachable only through the host, which authenticates
	// and checks origins; there is nothing to check here.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return // Accept has written the error response
	}
	defer conn.CloseNow()
	// One frame: the channel byte and at most MaxClientPayloadBytes.
	conn.SetReadLimit(agentapi.MaxClientFrameBytes)

	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()

	fc, start, dropped := p.attachFlow(from)
	defer p.detachFlow(fc)
	s := &wsSession{
		p:         p,
		conn:      conn,
		fc:        fc,
		wake:      make(chan struct{}, 1),
		stdinWake: make(chan struct{}, 1),
	}
	s.sent.Store(start)

	go func() {
		defer cancel()
		s.readLoop(ctx)
	}()
	go s.stdinLoop(ctx)

	s.writeLoop(ctx, start, dropped)
}

func (s *wsSession) writeJSONFrame(ctx context.Context, ch byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageBinary, append([]byte{ch}, b...))
}

func (s *wsSession) writeLoop(ctx context.Context, start, dropped int64) {
	if s.writeJSONFrame(ctx, agentapi.ChannelOffset, agentapi.OffsetFrame{Offset: start, Dropped: dropped}) != nil {
		return
	}
	cursor := start
	frame := make([]byte, 1+agentapi.MaxFrameBytes)
	for {
		// Control traffic first, so it is not starved by output.
		s.mu.Lock()
		pongs, fatal := s.pongs, s.fatal
		s.pongs = nil
		s.mu.Unlock()
		for _, payload := range pongs {
			if s.conn.Write(ctx, websocket.MessageBinary, append([]byte{agentapi.ChannelPong}, payload...)) != nil {
				return
			}
		}
		if fatal != nil {
			_ = s.writeJSONFrame(ctx, agentapi.ChannelStatus, agentapi.StatusFrame{Error: fatal})
			_ = s.conn.Close(websocket.StatusNormalClosure, "")
			return
		}

		ch, n, off, lost, exit, wait := s.p.poll(cursor, frame[1:])
		if lost > 0 {
			// The client fell behind the ring buffer.
			s.p.skipAck(s.fc, off)
			if s.writeJSONFrame(ctx, agentapi.ChannelOffset, agentapi.OffsetFrame{Offset: off, Dropped: lost}) != nil {
				return
			}
		}
		cursor = off
		if n > 0 {
			frame[0] = agentapi.ChannelStdout // stdout and PTY output
			if ch == agentapi.EventStderr {
				frame[0] = agentapi.ChannelStderr
			}
			// Published before the write so that an ACK for these bytes,
			// which can arrive before Write returns, is not clamped.
			s.sent.Store(cursor + int64(n))
			if s.conn.Write(ctx, websocket.MessageBinary, frame[:1+n]) != nil {
				return
			}
			cursor += int64(n)
			continue
		}
		if exit != nil {
			_ = s.writeJSONFrame(ctx, agentapi.ChannelStatus, agentapi.StatusFrame{Exit: exit})
			_ = s.conn.Close(websocket.StatusNormalClosure, "")
			return
		}
		select {
		case <-wait:
		case <-s.wake:
		case <-ctx.Done():
			return
		}
	}
}

func (s *wsSession) fail(code, msg string) {
	s.mu.Lock()
	if s.fatal == nil {
		s.fatal = &agentapi.Error{Code: code, Message: msg}
	}
	s.mu.Unlock()
	poke(s.wake)
}

// readLoop handles client frames until the connection ends.
//
// Failures of an individual stdin, resize or signal frame (typically
// because the process has just exited) are not reported: the status
// frame that follows tells the client what happened. A malformed frame
// ends the session with an error status.
func (s *wsSession) readLoop(ctx context.Context) {
	for {
		typ, data, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary || len(data) == 0 {
			s.fail("bad_frame", "frames must be binary with a one-byte channel prefix")
			continue
		}
		payload := data[1:]
		switch data[0] {
		case agentapi.ChannelStdin:
			if len(payload) == 0 {
				continue
			}
			s.mu.Lock()
			over := s.stdinBytes+len(payload) > maxWSStdinQueue
			if !over {
				s.stdinQ = append(s.stdinQ, payload)
				s.stdinBytes += len(payload)
			}
			s.mu.Unlock()
			if over {
				s.fail("stdin_overflow", "more than 1 MiB of stdin is queued and the process is not reading it")
				continue
			}
			poke(s.stdinWake)
		case agentapi.ChannelResize:
			var req agentapi.ResizeRequest
			if json.Unmarshal(payload, &req) != nil {
				s.fail("bad_frame", "invalid resize frame")
				continue
			}
			_ = s.p.resize(req.Cols, req.Rows)
		case agentapi.ChannelSignal:
			var req agentapi.SignalRequest
			if json.Unmarshal(payload, &req) != nil {
				s.fail("bad_frame", "invalid signal frame")
				continue
			}
			sig, err := parseSignal(req.Signal)
			if err != nil {
				s.fail("bad_signal", err.Error())
				continue
			}
			_ = s.p.signal(sig)
		case agentapi.ChannelAck:
			var req agentapi.AckFrame
			if json.Unmarshal(payload, &req) != nil {
				s.fail("bad_frame", "invalid ack frame")
				continue
			}
			s.p.ack(s.fc, req.Offset, s.sent.Load())
		case agentapi.ChannelPing:
			s.mu.Lock()
			if len(s.pongs) < maxPendingPongs {
				s.pongs = append(s.pongs, payload)
			}
			s.mu.Unlock()
			poke(s.wake)
		case agentapi.ChannelPong:
		default:
			s.fail("bad_frame", "unknown client channel")
		}
	}
}

func (s *wsSession) stdinLoop(ctx context.Context) {
	for {
		s.mu.Lock()
		var b []byte
		if len(s.stdinQ) > 0 {
			b = s.stdinQ[0]
			s.stdinQ = s.stdinQ[1:]
		}
		s.mu.Unlock()
		if b == nil {
			select {
			case <-s.stdinWake:
				continue
			case <-ctx.Done():
				return
			}
		}
		_ = s.p.writeStdin(ctx, b)
		s.mu.Lock()
		s.stdinBytes -= len(b)
		s.mu.Unlock()
	}
}

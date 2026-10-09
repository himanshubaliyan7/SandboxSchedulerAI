package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/coder/websocket"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
)

// Frame is one frame received from the agent on a WS.
type Frame struct {
	// Channel is agentapi.ChannelStdout, ChannelStderr, ChannelOffset,
	// ChannelStatus or ChannelPong.
	Channel byte
	// Data is the output (ChannelStdout, ChannelStderr) or the echoed
	// payload (ChannelPong).
	Data []byte
	// Offset is the stream offset of Data[0] for output frames, and the
	// offset output resumes at for a ChannelOffset frame.
	Offset int64
	// Dropped is set on a ChannelOffset frame: that many bytes before
	// Offset fell out of the agent's buffer and are lost.
	Dropped int64
	// Status is set on the final ChannelStatus frame.
	Status *agentapi.StatusFrame
}

// WS is an attached process WebSocket. Recv must be called from one
// goroutine at a time; the send methods may be called concurrently with
// it and with each other.
//
// Flow control: the agent pauses the process when it has produced more
// than agentapi.FlowHighWatermark bytes beyond the last acknowledged
// offset and resumes below agentapi.FlowLowWatermark, starting from the
// first acknowledgement a connection sends. So a client that uses flow
// control must acknowledge in steps smaller than FlowLowWatermark (or
// whenever it has processed everything it received); otherwise the
// process can stay paused. Either call Ack yourself, or set AutoAck to
// have Recv acknowledge everything it has returned.
type WS struct {
	// Start is the offset the agent started sending from, and how many
	// bytes before it had already been dropped.
	Start agentapi.OffsetFrame
	// AutoAck makes Recv acknowledge output as it is returned, which
	// suits a consumer whose processing is done by the time it calls
	// Recv again. Set it before the first Recv.
	AutoAck bool

	conn *websocket.Conn

	mu      sync.Mutex // guards the fields below (Recv vs Offset)
	next    int64      // offset of the next output byte
	acked   int64
	done    bool
	lastErr error
}

// autoAckStep is how much output Recv returns between automatic
// acknowledgements; it must stay below agentapi.FlowLowWatermark.
const autoAckStep = 4096

// wsReadLimit bounds a frame from the agent (a channel byte plus at most
// MaxFrameBytes, or a small JSON document).
const wsReadLimit = agentapi.MaxFrameBytes + 4096

// DialWS attaches to a process over WebSocket, starting at offset from.
// It returns after the agent's first OffsetFrame, available as Start.
// ctx bounds the dial only.
func (c *Client) DialWS(ctx context.Context, id string, from int64) (*WS, error) {
	u := procURL(id, "/ws?from="+strconv.FormatInt(from, 10))
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.http})
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 {
			// The body is still readable when the handshake was refused.
			return nil, responseError(resp)
		}
		return nil, err
	}
	conn.SetReadLimit(wsReadLimit)
	w := &WS{conn: conn}
	f, err := w.Recv(ctx)
	if err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	if f.Channel != agentapi.ChannelOffset {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("agent: expected an offset frame first, got channel %d", f.Channel)
	}
	w.Start = agentapi.OffsetFrame{Offset: f.Offset, Dropped: f.Dropped}
	return w, nil
}

// Offset is the offset of the next output byte Recv will return; pass it
// as from to resume after a disconnect.
func (w *WS) Offset() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.next
}

// Recv returns the next frame. After the ChannelStatus frame (the
// process exited, or the agent ended the session with an error) it
// returns io.EOF. Pings from the agent are answered and not returned.
func (w *WS) Recv(ctx context.Context) (Frame, error) {
	for {
		w.mu.Lock()
		done, lastErr := w.done, w.lastErr
		w.mu.Unlock()
		if lastErr != nil {
			return Frame{}, lastErr
		}
		if done {
			return Frame{}, io.EOF
		}
		typ, data, err := w.conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				err = io.EOF
			}
			w.mu.Lock()
			w.lastErr = err
			w.mu.Unlock()
			return Frame{}, err
		}
		if typ != websocket.MessageBinary || len(data) == 0 {
			return Frame{}, errors.New("agent: malformed frame")
		}
		f := Frame{Channel: data[0]}
		payload := data[1:]
		switch f.Channel {
		case agentapi.ChannelStdout, agentapi.ChannelStderr:
			f.Data = payload
			w.mu.Lock()
			f.Offset = w.next
			w.next += int64(len(payload))
			ackTo := int64(-1)
			if w.AutoAck && w.next-w.acked >= autoAckStep {
				ackTo = w.next
				w.acked = ackTo
			}
			w.mu.Unlock()
			if ackTo >= 0 {
				if err := w.Ack(ctx, ackTo); err != nil {
					return Frame{}, err
				}
			}
			return f, nil
		case agentapi.ChannelOffset:
			var of agentapi.OffsetFrame
			if err := json.Unmarshal(payload, &of); err != nil {
				return Frame{}, fmt.Errorf("agent: invalid offset frame: %w", err)
			}
			f.Offset, f.Dropped = of.Offset, of.Dropped
			w.mu.Lock()
			w.next = of.Offset
			if w.acked < of.Offset {
				w.acked = of.Offset
			}
			w.mu.Unlock()
			return f, nil
		case agentapi.ChannelStatus:
			var st agentapi.StatusFrame
			if err := json.Unmarshal(payload, &st); err != nil {
				return Frame{}, fmt.Errorf("agent: invalid status frame: %w", err)
			}
			f.Status = &st
			w.mu.Lock()
			w.done = true
			w.mu.Unlock()
			return f, nil
		case agentapi.ChannelPing:
			if err := w.send(ctx, agentapi.ChannelPong, payload); err != nil {
				return Frame{}, err
			}
		case agentapi.ChannelPong:
			f.Data = payload
			return f, nil
		default:
			// Unknown channels are skipped for forward compatibility.
		}
	}
}

func (w *WS) send(ctx context.Context, ch byte, payload []byte) error {
	b := make([]byte, 1+len(payload))
	b[0] = ch
	copy(b[1:], payload)
	return w.conn.Write(ctx, websocket.MessageBinary, b)
}

func (w *WS) sendJSON(ctx context.Context, ch byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.send(ctx, ch, b)
}

// Write sends input bytes to the process (its stdin, or the PTY: 0x03 is
// Ctrl+C, 0x04 is Ctrl+D).
func (w *WS) Write(ctx context.Context, p []byte) error {
	return w.send(ctx, agentapi.ChannelStdin, p)
}

// Resize resizes the PTY.
func (w *WS) Resize(ctx context.Context, cols, rows uint16) error {
	return w.sendJSON(ctx, agentapi.ChannelResize, agentapi.ResizeRequest{Cols: cols, Rows: rows})
}

// Signal sends a signal (e.g. "SIGINT") to the process group.
func (w *WS) Signal(ctx context.Context, signal string) error {
	return w.sendJSON(ctx, agentapi.ChannelSignal, agentapi.SignalRequest{Signal: signal})
}

// Ack tells the agent that output up to offset has been processed. The
// first Ack on a connection turns flow control on for it.
func (w *WS) Ack(ctx context.Context, offset int64) error {
	w.mu.Lock()
	if offset > w.acked {
		w.acked = offset
	}
	w.mu.Unlock()
	return w.sendJSON(ctx, agentapi.ChannelAck, agentapi.AckFrame{Offset: offset})
}

// Ping sends an application-level ping; the agent echoes the payload in
// a ChannelPong frame, which Recv returns.
func (w *WS) Ping(ctx context.Context, payload []byte) error {
	return w.send(ctx, agentapi.ChannelPing, payload)
}

// Close detaches. The process keeps running and can be reattached with
// DialWS and Offset.
func (w *WS) Close() error {
	return w.conn.CloseNow()
}

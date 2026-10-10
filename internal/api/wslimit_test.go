package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
	"github.com/himanshubaliyan7/warmbox/internal/api"
)

// A client frame of exactly the documented maximum (one channel byte plus
// agentapi.MaxClientPayloadBytes of stdin) must travel through the gateway
// and the agent to the process; one byte more must be refused with the
// standard "message too big" close.
func TestWebSocketClientFrameLimit(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	base := "/v1/sandboxes/" + sb.ID + "/commands"
	wsURL := "ws" + strings.TrimPrefix(h.srv.URL, "http") + base
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := func() api.CommandView {
		var cmd api.CommandView
		// head ends after exactly one full-size payload, so no EOF has to
		// be signalled (which could overtake the frame).
		body := sh("head -c " + strconv.Itoa(agentapi.MaxClientPayloadBytes) + " | sha256sum")
		body["stdin"] = true
		h.want(h.do("POST", base, body, withToken(sb.AccessToken)), 201, "").json(t, &cmd)
		return cmd
	}
	dial := func(pid string) *websocket.Conn {
		c, _, err := websocket.Dial(ctx, wsURL+"/"+pid+"/ws?token="+sb.AccessToken, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		c.SetReadLimit(wsFrameLimit)
		wsRecv(t, ctx, c, agentapi.ChannelOffset)
		return c
	}

	t.Run("exactly the limit", func(t *testing.T) {
		cmd := start()
		c := dial(cmd.Pid)
		defer c.CloseNow()
		payload := bytes.Repeat([]byte("x"), agentapi.MaxClientPayloadBytes)
		frame := append([]byte{agentapi.ChannelStdin}, payload...)
		if len(frame) != agentapi.MaxClientFrameBytes {
			t.Fatalf("frame is %d bytes, the limit is %d", len(frame), agentapi.MaxClientFrameBytes)
		}
		if err := c.Write(ctx, websocket.MessageBinary, frame); err != nil {
			t.Fatalf("write: %v", err)
		}
		sum := sha256.Sum256(payload)
		if out := wsUntil(t, ctx, c, "-"); !strings.HasPrefix(out, hex.EncodeToString(sum[:])) {
			t.Fatalf("the process did not receive the frame intact: %q", out)
		}
	})

	t.Run("one byte over", func(t *testing.T) {
		cmd := start()
		c := dial(cmd.Pid)
		defer c.CloseNow()
		frame := append([]byte{agentapi.ChannelStdin}, bytes.Repeat([]byte("x"), agentapi.MaxClientPayloadBytes+1)...)
		_ = c.Write(ctx, websocket.MessageBinary, frame) // the server may close before the write completes
		for {
			_, _, err := c.Read(ctx)
			if err == nil {
				continue
			}
			if got := websocket.CloseStatus(err); got != websocket.StatusMessageTooBig {
				t.Fatalf("close status %d (%v), want %d", got, err, websocket.StatusMessageTooBig)
			}
			break
		}
		// The command is unharmed and can still be reached.
		h.want(h.do("GET", base+"/"+cmd.Pid, nil, withToken(sb.AccessToken)), 200, "")
	})
}

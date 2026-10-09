package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
)

// wsClientReadLimit bounds one frame from the client: the agent accepts
// at most 1 MiB per frame, plus the channel byte.
const wsClientReadLimit = 1<<20 + 1

// wsWriteTimeout bounds one write to the client. A client that reads
// nothing for this long is disconnected; the process keeps running and
// the client can reattach with from=<offset>.
const wsWriteTimeout = 60 * time.Second

// handleWS attaches a WebSocket to a command: GET
// /v1/sandboxes/{id}/commands/{pid}/ws?from=N.
//
// The gateway is a relay. Every binary frame from the client goes to the
// agent's WebSocket for the process unchanged, and every frame from the
// agent goes to the client unchanged, so the wire format is exactly
// agentapi's: a one-byte channel followed by the payload (see the
// Channel* constants). Each direction is a loop of "read one frame, write
// it": there is no queue in the gateway, a peer that stops reading stops
// the other side's writes through TCP, and the protocol's own ACK flow
// control (ChannelAck) is end to end between client and agent.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request, sess *session) error {
	from, err := parseFrom(r)
	if err != nil {
		return err
	}
	pid := r.PathValue("pid")

	// Dial the agent before upgrading, so an unknown process is still a
	// plain HTTP 404.
	dctx, dcancel := s.opCtx(r)
	agentConn, err := sess.client.DialRawWS(dctx, pid, from)
	dcancel()
	if err != nil {
		return s.agentFailure(sess, err, CodeProcessNotFound)
	}
	defer agentConn.CloseNow()

	// Authentication is by token, never by ambient browser credentials,
	// so there is no cross-site request to defend against and any Origin
	// is accepted. Compression is off: the payload is already a relay.
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{WSSubprotocol}, InsecureSkipVerify: true,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil // Accept has answered
	}
	defer client.CloseNow()
	client.SetReadLimit(wsClientReadLimit)

	ctx, cancel := context.WithCancel(s.baseCtx)
	defer cancel()
	stopOnSession := context.AfterFunc(sess.ctx, cancel)
	defer stopOnSession()
	s.m.streamStarted("ws")
	defer s.m.streamEnded("ws")

	// client -> agent
	clientDone := make(chan error, 1)
	go func() {
		defer cancel() // without a client there is nothing to relay to
		for {
			typ, data, err := client.Read(ctx)
			if err != nil {
				clientDone <- err
				return
			}
			if typ == websocket.MessageBinary && len(data) > 0 {
				switch data[0] {
				case agentapi.ChannelStdin, agentapi.ChannelResize, agentapi.ChannelSignal:
					s.touch(sess)
				}
			}
			if err := agentConn.Write(ctx, typ, data); err != nil {
				clientDone <- err
				return
			}
		}
	}()

	// Keep intermediaries from idling the connection out and notice a
	// dead client.
	go func() {
		t := time.NewTicker(s.cfg.WSPingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, s.cfg.WSPingInterval)
				err := client.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// agent -> client
	var agentErr error
	for {
		typ, data, err := agentConn.Read(ctx)
		if err != nil {
			agentErr = err
			break
		}
		if typ == websocket.MessageBinary && len(data) > 1 &&
			(data[0] == agentapi.ChannelStdout || data[0] == agentapi.ChannelStderr) {
			s.touch(sess)
		}
		wctx, wcancel := context.WithTimeout(ctx, wsWriteTimeout)
		err = client.Write(wctx, typ, data)
		wcancel()
		if err != nil {
			return nil // the client is gone or stalled; the process keeps running
		}
	}

	switch {
	case websocket.CloseStatus(agentErr) == websocket.StatusNormalClosure:
		// The agent sent its status frame (already relayed) and closed.
		_ = client.Close(websocket.StatusNormalClosure, "")
	case ctx.Err() != nil:
		select {
		case <-clientDone: // the client hung up first
		default:
			reason := "sandbox terminated"
			if s.baseCtx.Err() != nil {
				reason = "server shutting down"
			}
			_ = client.Close(websocket.StatusGoingAway, reason)
		}
	default:
		// The agent connection broke: say why in the protocol's own
		// terminal frame, then close.
		apiErr := toError(s.agentFailure(sess, agentErr, CodeProcessNotFound))
		frame, _ := json.Marshal(agentapi.StatusFrame{Error: &agentapi.Error{Code: apiErr.Code, Message: apiErr.Message}})
		wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = client.Write(wctx, websocket.MessageBinary, append([]byte{agentapi.ChannelStatus}, frame...))
		wcancel()
		_ = client.Close(websocket.StatusGoingAway, apiErr.Code)
	}
	return nil
}

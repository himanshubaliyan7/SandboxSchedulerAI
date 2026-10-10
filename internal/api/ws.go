package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
)

// wsHandshakeErrors is the query parameter that selects how a refused
// handshake is reported: "http" (default), the usual status and error
// envelope; or "close", for clients that cannot read those (browsers):
// the upgrade is accepted and the connection closed at once with an
// application close code (agentapi.Close*).
const wsHandshakeErrors = "handshake_errors"

func wsCloseErrors(r *http.Request) bool {
	return r.URL.Query().Get(wsHandshakeErrors) == "close" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func wsAcceptOptions() *websocket.AcceptOptions {
	// Authentication is by token, never by ambient browser credentials,
	// so there is no cross-site request to defend against; when CORS is
	// configured the Origin has already been checked against the same
	// list (cors.go), and otherwise any Origin is accepted. Compression
	// is off: the payload is already a relay.
	return &websocket.AcceptOptions{
		Subprotocols: []string{WSSubprotocol}, InsecureSkipVerify: true,
		CompressionMode: websocket.CompressionDisabled,
	}
}

// wsEnd ends a session for a reason of the gateway's: a status frame
// naming the error, then a close whose code is agentapi.CloseCodeFor the
// error's code and whose reason is that code.
func wsEnd(c *websocket.Conn, e *Error) {
	frame, _ := json.Marshal(agentapi.StatusFrame{Error: &agentapi.Error{Code: e.Code, Message: e.Message, Details: e.Details}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageBinary, append([]byte{agentapi.ChannelStatus}, frame...))
	_ = c.Close(websocket.StatusCode(agentapi.CloseCodeFor(e.Code)), e.Code)
}

// wsRefuse reports a handshake that failed with err to a client that
// asked for handshake_errors=close.
func (s *Server) wsRefuse(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return
	}
	e := toError(err)
	if e.Status >= 500 {
		s.log.Warn("request failed", "request_id", requestID(r), "path", r.URL.Path, "status", e.Status, "code", e.Code, "err", err)
	}
	c, aerr := websocket.Accept(w, r, wsAcceptOptions())
	if aerr != nil {
		return // not a valid upgrade request; Accept has answered
	}
	defer c.CloseNow()
	wsEnd(c, e)
}

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
//
// How a session ends is part of the contract; see agentapi.Close*.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request, sess *session) error {
	from, err := parseFrom(r)
	if err != nil {
		return err
	}
	switch r.URL.Query().Get(wsHandshakeErrors) {
	case "", "http", "close":
	default:
		return badRequest("%s must be http or close", wsHandshakeErrors)
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

	client, err := websocket.Accept(w, r, wsAcceptOptions())
	if err != nil {
		return nil // Accept has answered
	}
	defer client.CloseNow()
	// The same limit the agent applies: a frame that passes here passes
	// there. A larger one is answered with close code 1009.
	client.SetReadLimit(agentapi.MaxClientFrameBytes)

	// ctx ends the relay: when the session is dropped, the server closes
	// or the client goes away. Operations on the client connection run on
	// connCtx instead, which lives until the handler returns: this
	// WebSocket library closes a connection whose read or write is
	// cancelled, and the client must still be there to receive the status
	// frame and the close code that say why the relay ended.
	ctx, cancel := context.WithCancel(s.baseCtx)
	defer cancel()
	stopOnSession := context.AfterFunc(sess.ctx, cancel)
	defer stopOnSession()
	connCtx, connCancel := context.WithCancel(context.Background())
	defer connCancel()
	s.m.streamStarted("ws")
	defer s.m.streamEnded("ws")

	// client -> agent
	clientDone := make(chan error, 1)
	go func() {
		defer cancel() // without a client there is nothing to relay to
		for {
			typ, data, err := client.Read(connCtx)
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
				pctx, pcancel := context.WithTimeout(connCtx, s.cfg.WSPingInterval)
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
		wctx, wcancel := context.WithTimeout(connCtx, wsWriteTimeout)
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
		case <-clientDone: // the client hung up first, or sent a frame that was refused
		default:
			if s.baseCtx.Err() != nil {
				wsEnd(client, &Error{Code: CodeShuttingDown, Message: "the server is shutting down"})
			} else {
				wsEnd(client, s.sessionEnded(sess))
			}
		}
	default:
		// The agent connection broke: sandbox_terminated if the sandbox
		// died, agent_unavailable (reattach) if only the link did.
		wsEnd(client, toError(s.agentFailure(sess, agentErr, CodeProcessNotFound)))
	}
	return nil
}

// sessionEnded describes why a session was dropped under an open
// WebSocket: the sandbox is no longer running.
func (s *Server) sessionEnded(sess *session) *Error {
	e := errf(http.StatusConflict, CodeSandboxTerminated, "sandbox %s has terminated", sess.id)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseCtx), 2*time.Second)
	defer cancel()
	// The session is dropped a moment before the row says why (a delete
	// drops it first and fences the sandbox next), so a row that still
	// looks alive is read again for a short while. The row is this
	// sandbox's whatever its generation says by now: ids are never reused.
	for attempt := 0; ; attempt++ {
		acc, err := s.store.GetSandboxAccess(ctx, sess.id)
		if err != nil {
			break
		}
		if isLive(acc.Sandbox.State) && attempt < lostCheckAttempts {
			select {
			case <-ctx.Done():
			case <-time.After(lostCheckInterval / 4):
				continue
			}
			break
		}
		if !isLive(acc.Sandbox.State) {
			e.Details = map[string]any{"state": publicState(acc.Sandbox.State)}
			if acc.Sandbox.Reason != "" {
				e.Details["reason"] = string(acc.Sandbox.Reason)
				e.Message += ": " + string(acc.Sandbox.Reason)
			}
		}
		break
	}
	return e
}

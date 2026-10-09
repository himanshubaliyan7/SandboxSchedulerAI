package agent

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
)

// handleStream serves GET /processes/{id}/stream?from=N: replay from N,
// then live output, then one exit event.
//
// The reader pulls from the ring buffer at its own pace. It never holds
// the process back: if it is slower than the process for long enough, the
// bytes it has not read yet fall out of the ring and it gets a truncated
// event carrying how many were dropped, then continues from the oldest
// byte still buffered. A from beyond the current end is treated as the
// current end.
func (a *Agent) handleStream(w http.ResponseWriter, r *http.Request, p *Process) {
	from, err := parseFrom(r)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	ctx := r.Context()

	cursor := from
	buf := make([]byte, agentapi.MaxFrameBytes)
	for {
		ch, n, off, dropped, exit, wait := p.poll(cursor, buf)
		if dropped > 0 {
			if enc.Encode(agentapi.Event{Type: agentapi.EventTruncated, Offset: off, Dropped: dropped}) != nil {
				return
			}
		}
		cursor = off
		if n > 0 {
			if enc.Encode(agentapi.Event{Type: ch, Offset: off, Data: buf[:n]}) != nil {
				return
			}
			cursor += int64(n)
			continue
		}
		if exit != nil {
			_ = enc.Encode(agentapi.Event{Type: agentapi.EventExit, Offset: cursor, Exit: exit})
			_ = rc.Flush()
			return
		}
		// Caught up: push what is buffered, then wait.
		if rc.Flush() != nil {
			return
		}
		idle := time.NewTimer(a.cfg.KeepaliveInterval)
		select {
		case <-wait:
			idle.Stop()
		case <-ctx.Done():
			idle.Stop()
			return
		case <-idle.C:
			if enc.Encode(agentapi.Event{Type: agentapi.EventKeepalive, Offset: cursor}) != nil {
				return
			}
		}
	}
}

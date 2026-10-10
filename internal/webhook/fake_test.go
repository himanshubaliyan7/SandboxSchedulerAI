package webhook

import (
	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/store"
	"github.com/himanshubaliyan7/warmbox/internal/webhook/webhooktest"
)

// The worker and the service are tested over webhooktest.MemStore.
var (
	_ Store        = (*webhooktest.MemStore)(nil)
	_ ServiceStore = (*webhooktest.MemStore)(nil)
)

// Event constructors, as the store and the API's recorder write them.

func evClaimed(sb string) store.Event {
	return store.Event{SandboxID: sb, Type: store.EventClaimed, FromState: core.StateWarm, ToState: core.StateAssigned}
}

func evTransition(sb string, from, to core.State, reason core.TerminationReason) store.Event {
	return store.Event{SandboxID: sb, Type: store.EventTransition, FromState: from, ToState: to, Reason: reason}
}

func evCommand(sb string, n int) store.Event {
	return store.Event{SandboxID: sb, Type: internalCommandFinished, Data: map[string]any{"pid": n, "cmd": "true", "exit_code": 0, "reason": "exited"}}
}

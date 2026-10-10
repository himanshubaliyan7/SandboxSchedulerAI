package webhook

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// EnvelopeVersion is the version of the payload format. It changes only
// with an incompatible change; new fields and new event types do not
// change it, so receivers must ignore what they do not know.
const EnvelopeVersion = 1

// The public event types. They are the contract; the names the event log
// uses internally ("claimed", "transition", RECYCLING, ...) never leave
// the server.
const (
	// TypeSandboxCreated: a sandbox was assigned to the tenant
	// (POST /v1/sandboxes took it from the pool). It is not running yet.
	TypeSandboxCreated = "sandbox.created"
	// TypeSandboxRunning: the session was set up; the sandbox accepts
	// commands.
	TypeSandboxRunning = "sandbox.running"
	// TypeSandboxTerminated: the sandbox stopped being the tenant's, for
	// the reason in data.reason. Sent once per sandbox.
	TypeSandboxTerminated = "sandbox.terminated"
	// TypeSandboxDestroyed: the container and its storage are gone.
	TypeSandboxDestroyed = "sandbox.destroyed"
	// TypeCommandStarted and TypeCommandFinished: the execution record of
	// a command started through the API.
	TypeCommandStarted  = "command.started"
	TypeCommandFinished = "command.finished"
	// TypeTest is sent only by POST /v1/webhooks/{id}/test. It cannot be
	// subscribed to and is sent whatever the subscription says.
	TypeTest = "webhook.test"

	// AllEvents is the subscription that matches every event type,
	// including types added later.
	AllEvents = "*"
)

// EventTypes lists the types a webhook can subscribe to.
func EventTypes() []string {
	return []string{TypeSandboxCreated, TypeSandboxRunning, TypeSandboxTerminated, TypeSandboxDestroyed,
		TypeCommandStarted, TypeCommandFinished}
}

// The event-log types of the API's execution recorder (api.EventCommand*;
// a test in the api package keeps the two in step).
const (
	internalCommandStarted  = "command_started"
	internalCommandFinished = "command_finished"
)

// Envelope is the body of a webhook request.
type Envelope struct {
	// ID identifies the event. It is the same on every attempt and for
	// every webhook that receives the event, and is also sent as the
	// Webhook-Id header: receivers deduplicate on it.
	ID string `json:"id"`
	// Type is one of the Type* constants.
	Type string `json:"type"`
	// Version is EnvelopeVersion.
	Version int `json:"version"`
	// CreatedAt is when the event happened, not when it was sent.
	CreatedAt time.Time `json:"created_at"`
	// SandboxID and TemplateID are absent from a webhook.test event.
	// TemplateID is also absent once the sandbox's record has been
	// garbage-collected (seven days after it was destroyed by default).
	SandboxID  string `json:"sandbox_id,omitempty"`
	TemplateID string `json:"template_id,omitempty"`
	// Data depends on Type; see docs/webhooks.md.
	Data map[string]any `json:"data"`
}

// EventID is the public id of an event-log row.
func EventID(id int64) string { return "evt_" + strconv.FormatInt(id, 10) }

func newTestEventID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "evt_test_" + hex.EncodeToString(b)
}

// publicState is the API's spelling of a lifecycle state (api.publicState):
// lower case, with the unreachable PAUSED reported as running.
func publicState(s core.State) string {
	if s == core.StatePaused {
		s = core.StateRunning
	}
	return strings.ToLower(string(s))
}

func owned(s core.State) bool {
	return s == core.StateAssigned || s == core.StateRunning || s == core.StatePaused
}

// publicType maps an event-log row to its public type. ok is false for
// rows that are not part of the public contract: steps of the teardown
// after the sandbox was already reported as terminated, pause and resume
// (no API reaches them), and types this version does not know.
func publicType(e store.Event) (typ string, ok bool) {
	switch e.Type {
	case internalCommandStarted:
		return TypeCommandStarted, true
	case internalCommandFinished:
		return TypeCommandFinished, true
	case store.EventClaimed:
		return TypeSandboxCreated, true
	}
	// Every other state change is judged by its edge, whatever type the
	// writer gave the row.
	switch {
	case e.FromState == core.StateAssigned && e.ToState == core.StateRunning:
		return TypeSandboxRunning, true
	case owned(e.FromState) && (e.ToState == core.StateRecycling || e.ToState == core.StateFailed || e.ToState == core.StateUnknown):
		return TypeSandboxTerminated, true
	case e.FromState == core.StateRecycling && e.ToState == core.StateDestroyed:
		return TypeSandboxDestroyed, true
	}
	return "", false
}

// commandKeys are the fields of an execution record that are published.
// The record never holds the command's environment; listing the fields
// keeps it that way whatever is added to the record later.
var commandKeys = []string{"pid", "cmd", "args", "args_truncated", "tag", "pty", "cwd", "timeout_ms", "stdin",
	"exit_code", "reason", "signal", "duration_ms", "stdout_bytes", "stderr_bytes", "uncounted_bytes", "sandbox_reason"}

// Translate builds the public envelope of an event-log row. ok is false
// when the row is not published (see publicType).
//
// What is copied is chosen field by field: the state, the termination
// reason, the sandbox's metadata and the execution record. The access
// token, the network policy, the environment, host and instance ids and
// the free-form data of internal transitions are not.
func Translate(e store.WebhookEvent) (env Envelope, ok bool) {
	typ, ok := publicType(e.Event)
	if !ok {
		return Envelope{}, false
	}
	data := map[string]any{}
	switch typ {
	case TypeCommandStarted, TypeCommandFinished:
		for _, k := range commandKeys {
			if v, has := e.Data[k]; has {
				data[k] = v
			}
		}
	default:
		data["state"] = publicState(e.ToState)
		if e.Reason != "" {
			data["reason"] = string(e.Reason)
		}
	}
	if len(e.Metadata) > 0 {
		md := make(map[string]any, len(e.Metadata))
		for k, v := range e.Metadata {
			md[k] = v
		}
		data["metadata"] = md
	}
	return Envelope{ID: EventID(e.ID), Type: typ, Version: EnvelopeVersion, CreatedAt: e.At.UTC(),
		SandboxID: e.SandboxID, TemplateID: e.TemplateID, Data: data}, true
}

// testEnvelope is the webhook.test event.
func testEnvelope(webhookID string, now time.Time) Envelope {
	return Envelope{ID: newTestEventID(), Type: TypeTest, Version: EnvelopeVersion, CreatedAt: now.UTC(),
		Data: map[string]any{"webhook_id": webhookID, "message": "This is a test event from Warmbox."}}
}

// Marshal encodes the envelope as the request body.
func (e Envelope) Marshal() ([]byte, error) {
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	return json.Marshal(e)
}

// NormalizeEventTypes validates a subscription and returns it sorted and
// without duplicates. An empty list means every event; so does a list
// that contains "*", which is then returned alone.
func NormalizeEventTypes(in []string) ([]string, error) {
	if len(in) == 0 {
		return []string{AllEvents}, nil
	}
	known := map[string]bool{}
	for _, t := range EventTypes() {
		known[t] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range in {
		t = strings.TrimSpace(t)
		switch {
		case t == AllEvents:
			return []string{AllEvents}, nil
		case !known[t]:
			return nil, fmt.Errorf("unknown event type %q (known: %s, or \"*\" for all)", t, strings.Join(EventTypes(), ", "))
		case !seen[t]:
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Subscribed reports whether a webhook with these event types receives typ.
func Subscribed(eventTypes []string, typ string) bool {
	for _, t := range eventTypes {
		if t == AllEvents || t == typ {
			return true
		}
	}
	return false
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/agentclient"
	"github.com/himanshubaliyan7/warmbox/internal/pool"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// Stable error codes of the public API. Codes the in-sandbox agent
// produces for a command or file request are passed through unchanged
// (see agentCodes in the OpenAPI description).
const (
	CodeBadRequest         = "bad_request"
	CodeUnauthorized       = "unauthorized"
	CodeNotFound           = "not_found"
	CodeTemplateNotFound   = "template_not_found"
	CodeProcessNotFound    = "process_not_found"
	CodeFileNotFound       = "file_not_found"
	CodeMethodNotAllowed   = "method_not_allowed"
	CodeConflict           = "conflict"
	CodeSandboxNotRunning  = "sandbox_not_running"
	CodeSandboxTerminated  = "sandbox_terminated"
	CodeNetworkNotAllowed  = "network_not_allowed"
	CodeRuntimeNotEnabled  = "runtime_not_enabled"
	CodeNetworkNotAttached = "network_not_attached"
	CodeInvalidTimeout     = "invalid_timeout"
	CodePayloadTooLarge    = "payload_too_large"
	CodeRateLimited        = "rate_limited"
	CodeQuotaExceeded      = "quota_exceeded"
	CodePoolEmpty          = "pool_empty"
	CodeShuttingDown       = "shutting_down"
	CodeAgentUnavailable   = "agent_unavailable"
	CodeTimeout            = "timeout"
	CodeNotImplemented     = "not_implemented"
	CodeOriginNotAllowed   = "origin_not_allowed"
	CodeAPIKeyNotAllowed   = "api_key_not_allowed"
	CodeIdempotencyKeyUsed = "idempotency_key_reused"
	CodeInternal           = "internal"
)

// Error is an API error: an HTTP status, a stable code and a message that
// is safe to show to the caller.
type Error struct {
	Status     int
	Code       string
	Message    string
	Details    map[string]any
	RetryAfter time.Duration // sent as Retry-After when positive
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) *Error {
	return errf(http.StatusBadRequest, CodeBadRequest, format, args...)
}

var errUnauthorized = &Error{Status: http.StatusUnauthorized, Code: CodeUnauthorized,
	Message: "missing or invalid credentials"}

func errSandboxNotFound(id string) *Error {
	return errf(http.StatusNotFound, CodeNotFound, "sandbox %q not found", id)
}

// ErrorBody is the "error" member of the JSON error envelope, and the
// payload of an NDJSON error event.
type ErrorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

type errorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// toError maps any error to an API error. Unknown errors become a 500
// with a generic message; the detail goes to the log, not the client.
func toError(err error) *Error {
	var (
		ae   *Error
		age  *agentclient.Error
		maxb *http.MaxBytesError
	)
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.As(err, &maxb):
		return errf(http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "request body exceeds %d bytes", maxb.Limit)
	case errors.As(err, &age):
		return &Error{Status: age.Status, Code: age.Code, Message: age.Message}
	case errors.Is(err, pool.ErrPoolEmpty):
		return &Error{Status: http.StatusServiceUnavailable, Code: CodePoolEmpty,
			Message: "no warm sandbox is available for this template right now", RetryAfter: time.Second}
	case errors.Is(err, pool.ErrQuotaExceeded):
		return &Error{Status: http.StatusTooManyRequests, Code: CodeQuotaExceeded,
			Message: "the tenant's concurrent sandbox quota is exhausted; delete a sandbox first", RetryAfter: 5 * time.Second}
	case errors.Is(err, pool.ErrNetworkNotAllowed):
		return errf(http.StatusBadRequest, CodeNetworkNotAllowed, "the template does not allow the requested network mode")
	case errors.Is(err, pool.ErrRuntimeNotEnabled):
		return errf(http.StatusServiceUnavailable, CodeRuntimeNotEnabled,
			"the template names an isolation runtime this server has not enabled; the operator must enable it (--runtimes) or change the template")
	case errors.Is(err, pool.ErrStopped):
		return &Error{Status: http.StatusServiceUnavailable, Code: CodeShuttingDown,
			Message: "the server is shutting down", RetryAfter: 5 * time.Second}
	case errors.Is(err, store.ErrNotFound):
		return errf(http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, store.ErrStaleGeneration), errors.Is(err, store.ErrInvalidTransition):
		return errf(http.StatusConflict, CodeConflict, "the sandbox changed state while the request was being served")
	case errors.Is(err, context.DeadlineExceeded):
		return errf(http.StatusGatewayTimeout, CodeTimeout, "the request timed out")
	}
	return errf(http.StatusInternalServerError, CodeInternal, "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the error envelope. requestID may be empty.
func writeError(w http.ResponseWriter, requestID string, e *Error) {
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	if e.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sbx"`)
	}
	writeJSON(w, e.Status, errorEnvelope{Error: ErrorBody{
		Code: e.Code, Message: e.Message, RequestID: requestID, Details: e.Details,
	}})
}

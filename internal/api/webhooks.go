package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/store"
	"github.com/himanshubaliyan7/warmbox/internal/webhook"
)

// Webhooks is the management side of lifecycle webhooks.
// *webhook.Service satisfies it. Every method is scoped to one tenant:
// another tenant's webhook is store.ErrNotFound.
type Webhooks interface {
	Create(ctx context.Context, tenantID string, p webhook.CreateParams) (store.Webhook, error)
	List(ctx context.Context, tenantID string) ([]store.Webhook, error)
	Get(ctx context.Context, tenantID, id string) (store.Webhook, error)
	Update(ctx context.Context, tenantID, id string, p webhook.UpdateParams) (store.Webhook, error)
	Delete(ctx context.Context, tenantID, id string) error
	RotateSecret(ctx context.Context, tenantID, id string, keepPrevious time.Duration) (store.Webhook, error)
	Test(ctx context.Context, tenantID, id string) (webhook.TestResult, error)
	Deliveries(ctx context.Context, tenantID, id string, limit int) ([]store.WebhookDelivery, error)
}

var _ Webhooks = (*webhook.Service)(nil)

// Error codes of the webhook routes.
const (
	CodeInvalidWebhookURL    = "invalid_webhook_url"
	CodeWebhookURLNotAllowed = "webhook_url_not_allowed"
	CodeWebhookLimitExceeded = "webhook_limit_exceeded"
)

// maxDeliveriesLimit caps ?limit= of the deliveries route.
const maxDeliveriesLimit = 500

// webhookRoutes registers the webhook routes. They are control plane: a
// tenant API key only. A sandbox access token is not a credential here,
// so code running in a sandbox cannot point the server at a URL.
func (s *Server) webhookRoutes() {
	s.tenant("POST /v1/webhooks", classDefault, s.handleCreateWebhook)
	s.tenant("GET /v1/webhooks", classDefault, s.handleListWebhooks)
	s.tenant("GET /v1/webhooks/{id}", classDefault, s.handleGetWebhook)
	s.tenant("PATCH /v1/webhooks/{id}", classDefault, s.handleUpdateWebhook)
	s.tenant("DELETE /v1/webhooks/{id}", classDefault, s.handleDeleteWebhook)
	s.tenant("POST /v1/webhooks/{id}/rotate-secret", classDefault, s.handleRotateWebhookSecret)
	s.tenant("POST /v1/webhooks/{id}/test", classDefault, s.handleTestWebhook)
	s.tenant("GET /v1/webhooks/{id}/deliveries", classDefault, s.handleWebhookDeliveries)
}

// WebhookView is the public view of a webhook.
type WebhookView struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	EventTypes  []string `json:"event_types"`
	Description string   `json:"description"`
	Enabled     bool     `json:"enabled"`
	// DisabledReason is "user", "failing" or "endpoint_gone" while the
	// webhook is disabled.
	DisabledReason string     `json:"disabled_reason,omitempty"`
	DisabledAt     *time.Time `json:"disabled_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`

	// Secret is present only in the responses of POST /v1/webhooks and
	// POST /v1/webhooks/{id}/rotate-secret.
	Secret string `json:"secret,omitempty"`
	// PreviousSecretExpiresAt is set while a rotated-out secret still signs.
	PreviousSecretExpiresAt *time.Time `json:"previous_secret_expires_at,omitempty"`

	// Delivery state.
	ConsecutiveFailures int        `json:"consecutive_failures"`
	FailingSince        *time.Time `json:"failing_since,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt       *time.Time `json:"last_failure_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	NextAttemptAt       *time.Time `json:"next_attempt_at,omitempty"`
	DroppedEvents       int64      `json:"dropped_events"`
}

// webhookView renders a row. The secret is included only when withSecret
// is set; the previous secret never is.
func webhookView(w store.Webhook, withSecret bool) WebhookView {
	v := WebhookView{
		ID: w.ID, URL: w.URL, EventTypes: w.EventTypes, Description: w.Description, Enabled: w.Enabled,
		DisabledReason: w.DisabledReason, DisabledAt: w.DisabledAt, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
		ConsecutiveFailures: w.Attempts, FailingSince: w.FailingSince, LastSuccessAt: w.LastSuccessAt,
		LastFailureAt: w.LastFailureAt, LastError: w.LastError, DroppedEvents: w.DroppedEvents,
	}
	if v.EventTypes == nil {
		v.EventTypes = []string{}
	}
	if withSecret {
		v.Secret = w.Secret
	}
	if w.PreviousSecret != "" && w.PreviousSecretExpiresAt != nil && w.PreviousSecretExpiresAt.After(time.Now()) {
		v.PreviousSecretExpiresAt = w.PreviousSecretExpiresAt
	}
	if w.Enabled && w.Attempts > 0 {
		next := w.NextAttemptAt
		v.NextAttemptAt = &next
	}
	return v
}

// webhooks returns the service, or the 501 of a server that runs without it.
func (s *Server) webhooks() (Webhooks, error) {
	if s.cfg.Webhooks == nil {
		return nil, errf(http.StatusNotImplemented, CodeNotImplemented, "webhooks are not enabled on this server")
	}
	return s.cfg.Webhooks, nil
}

// webhookError maps the errors of the service to API errors.
func webhookError(err error, id string) error {
	var inv *webhook.InvalidError
	switch {
	case errors.As(err, &inv):
		code := CodeBadRequest
		switch inv.Code {
		case webhook.CodeInvalidURL:
			code = CodeInvalidWebhookURL
		case webhook.CodeDestinationNotAllowed:
			code = CodeWebhookURLNotAllowed
		}
		return errf(http.StatusBadRequest, code, "%s", inv.Message)
	case errors.Is(err, webhook.ErrLimit):
		return errf(http.StatusConflict, CodeWebhookLimitExceeded, "this tenant already has the maximum number of webhooks; delete one first")
	case errors.Is(err, store.ErrNotFound):
		return errf(http.StatusNotFound, CodeNotFound, "webhook %q not found", id)
	case errors.Is(err, webhook.ErrNoSender):
		return errf(http.StatusNotImplemented, CodeNotImplemented, "this server does not send webhooks")
	}
	return err
}

// CreateWebhookRequest is the body of POST /v1/webhooks.
type CreateWebhookRequest struct {
	URL string `json:"url"`
	// EventTypes is the subscription; omitted or empty means every event.
	EventTypes  []string `json:"event_types,omitempty"`
	Description string   `json:"description,omitempty"`
	// Enabled defaults to true.
	Enabled *bool `json:"enabled,omitempty"`
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	svc, err := s.webhooks()
	if err != nil {
		return err
	}
	var req CreateWebhookRequest
	if err := s.decodeJSON(w, r, &req, false); err != nil {
		return err
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	wh, err := svc.Create(ctx, tenant.ID, webhook.CreateParams{URL: req.URL, EventTypes: req.EventTypes,
		Description: req.Description, Disabled: req.Enabled != nil && !*req.Enabled})
	if err != nil {
		return webhookError(err, "")
	}
	s.log.Info("webhook created", "request_id", requestID(r), "tenant", tenant.ID, "webhook", wh.ID, "url", webhook.LogURL(wh.URL))
	writeJSON(w, http.StatusCreated, webhookView(wh, true))
	return nil
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	svc, err := s.webhooks()
	if err != nil {
		return err
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	list, err := svc.List(ctx, tenant.ID)
	if err != nil {
		return webhookError(err, "")
	}
	out := make([]WebhookView, 0, len(list))
	for _, wh := range list {
		out = append(out, webhookView(wh, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out})
	return nil
}

func (s *Server) handleGetWebhook(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	svc, err := s.webhooks()
	if err != nil {
		return err
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	id := r.PathValue("id")
	wh, err := svc.Get(ctx, tenant.ID, id)
	if err != nil {
		return webhookError(err, id)
	}
	writeJSON(w, http.StatusOK, webhookView(wh, false))
	return nil
}

// UpdateWebhookRequest is the body of PATCH /v1/webhooks/{id}. Members
// that are absent keep their value.
type UpdateWebhookRequest struct {
	URL         *string   `json:"url,omitempty"`
	EventTypes  *[]string `json:"event_types,omitempty"`
	Description *string   `json:"description,omitempty"`
	Enabled     *bool     `json:"enabled,omitempty"`
}

func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	svc, err := s.webhooks()
	if err != nil {
		return err
	}
	var req UpdateWebhookRequest
	if err := s.decodeJSON(w, r, &req, false); err != nil {
		return err
	}
	if req.URL == nil && req.EventTypes == nil && req.Description == nil && req.Enabled == nil {
		return badRequest("nothing to change: give at least one of url, event_types, description, enabled")
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	id := r.PathValue("id")
	wh, err := svc.Update(ctx, tenant.ID, id, webhook.UpdateParams{URL: req.URL, EventTypes: req.EventTypes,
		Description: req.Description, Enabled: req.Enabled})
	if err != nil {
		return webhookError(err, id)
	}
	s.log.Info("webhook updated", "request_id", requestID(r), "tenant", tenant.ID, "webhook", wh.ID, "url", webhook.LogURL(wh.URL), "enabled", wh.Enabled)
	writeJSON(w, http.StatusOK, webhookView(wh, false))
	return nil
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	svc, err := s.webhooks()
	if err != nil {
		return err
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	id := r.PathValue("id")
	if err := svc.Delete(ctx, tenant.ID, id); err != nil {
		return webhookError(err, id)
	}
	s.log.Info("webhook deleted", "request_id", requestID(r), "tenant", tenant.ID, "webhook", id)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// RotateWebhookSecretRequest is the optional body of
// POST /v1/webhooks/{id}/rotate-secret.
type RotateWebhookSecretRequest struct {
	// KeepPreviousSeconds is how long the old secret keeps signing next
	// to the new one. Default 86400 (24 hours); 0 ends it at once; at
	// most 604800 (7 days).
	KeepPreviousSeconds *int64 `json:"keep_previous_seconds,omitempty"`
}

func (s *Server) handleRotateWebhookSecret(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	svc, err := s.webhooks()
	if err != nil {
		return err
	}
	var req RotateWebhookSecretRequest
	if err := s.decodeJSON(w, r, &req, true); err != nil {
		return err
	}
	keep := webhook.DefaultRotationGrace
	if req.KeepPreviousSeconds != nil {
		if *req.KeepPreviousSeconds < 0 || *req.KeepPreviousSeconds > int64(webhook.MaxRotationGrace/time.Second) {
			return badRequest("keep_previous_seconds must be between 0 and %d", int64(webhook.MaxRotationGrace/time.Second))
		}
		keep = time.Duration(*req.KeepPreviousSeconds) * time.Second
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	id := r.PathValue("id")
	wh, err := svc.RotateSecret(ctx, tenant.ID, id, keep)
	if err != nil {
		return webhookError(err, id)
	}
	s.log.Info("webhook secret rotated", "request_id", requestID(r), "tenant", tenant.ID, "webhook", wh.ID, "keep_previous", keep)
	writeJSON(w, http.StatusOK, webhookView(wh, true))
	return nil
}

// WebhookTestView is the response of POST /v1/webhooks/{id}/test.
type WebhookTestView struct {
	// EventID is the id of the webhook.test event that was sent.
	EventID   string `json:"event_id"`
	Delivered bool   `json:"delivered"`
	// Status is the endpoint's HTTP status; absent when none arrived.
	Status     int    `json:"status,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	ErrorCode  string `json:"error_code,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	svc, err := s.webhooks()
	if err != nil {
		return err
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	id := r.PathValue("id")
	res, err := svc.Test(ctx, tenant.ID, id)
	if err != nil {
		return webhookError(err, id)
	}
	// The request to this API succeeded whatever the endpoint answered:
	// the answer is the result, not an error of this call.
	writeJSON(w, http.StatusOK, WebhookTestView{EventID: res.EventID, Delivered: res.Delivered, Status: res.Status,
		DurationMs: res.DurationMs, ErrorCode: res.ErrorCode, Error: res.Error})
	return nil
}

// WebhookDeliveryView is one row of a webhook's delivery log.
type WebhookDeliveryView struct {
	ID int64 `json:"id"`
	// EventID is the id of the event ("evt_..."), as sent in Webhook-Id.
	// A "dropped" row that stands for a run of expired events carries the
	// id of the last of them.
	EventID   string `json:"event_id,omitempty"`
	EventType string `json:"event_type,omitempty"`
	Attempt   int    `json:"attempt"`
	// Status is "succeeded", "failed" (it will be retried) or "dropped"
	// (the event was given up on).
	Status         string    `json:"status"`
	ResponseStatus int       `json:"response_status,omitempty"`
	DurationMs     int       `json:"duration_ms"`
	ErrorCode      string    `json:"error_code,omitempty"`
	Error          string    `json:"error,omitempty"`
	DroppedEvents  int       `json:"dropped_events,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

func (s *Server) handleWebhookDeliveries(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	svc, err := s.webhooks()
	if err != nil {
		return err
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxDeliveriesLimit {
			return badRequest("limit must be between 1 and %d", maxDeliveriesLimit)
		}
		limit = n
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	id := r.PathValue("id")
	rows, err := svc.Deliveries(ctx, tenant.ID, id, limit)
	if err != nil {
		return webhookError(err, id)
	}
	out := make([]WebhookDeliveryView, 0, len(rows))
	for _, d := range rows {
		v := WebhookDeliveryView{ID: d.ID, EventID: d.MessageID, EventType: d.EventType, Attempt: d.Attempt, Status: d.Status,
			ResponseStatus: d.ResponseStatus, DurationMs: d.DurationMs, ErrorCode: d.ErrorCode, Error: d.Error,
			DroppedEvents: d.DroppedEvents, CreatedAt: d.CreatedAt}
		if v.EventID == "" && d.EventID != 0 {
			v.EventID = webhook.EventID(d.EventID)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
	return nil
}

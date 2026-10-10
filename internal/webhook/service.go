package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// Limits and defaults of the management operations.
const (
	// DefaultMaxPerTenant is how many webhooks a tenant may have.
	DefaultMaxPerTenant = 10
	// MaxDescriptionLength bounds a webhook's description, in bytes.
	MaxDescriptionLength = 512
	// DefaultRotationGrace is how long the previous secret keeps signing
	// after a rotation, and MaxRotationGrace the longest a caller may ask
	// for.
	DefaultRotationGrace = 24 * time.Hour
	MaxRotationGrace     = 7 * 24 * time.Hour
	// IDPrefix starts every webhook id.
	IDPrefix = "wh_"
)

// ErrLimit: the tenant has as many webhooks as it may have.
var ErrLimit = errors.New("webhook: the tenant's webhook limit is reached")

// InvalidError is a request the service refuses: a bad URL, an unknown
// event type, a description that is too long. Message is safe to show.
type InvalidError struct {
	// Code is CodeInvalidURL, CodeDestinationNotAllowed or "invalid".
	Code    string
	Message string
}

func (e *InvalidError) Error() string { return e.Message }

func invalid(format string, args ...any) *InvalidError {
	return &InvalidError{Code: "invalid", Message: fmt.Sprintf(format, args...)}
}

// ServiceStore is the part of the state store the service needs.
// *store.Store satisfies it.
type ServiceStore interface {
	CreateWebhook(ctx context.Context, n store.NewWebhook, maxPerTenant int) (store.Webhook, error)
	GetWebhook(ctx context.Context, tenantID, id string) (store.Webhook, error)
	ListWebhooks(ctx context.Context, tenantID string) ([]store.Webhook, error)
	UpdateWebhook(ctx context.Context, tenantID, id string, u store.WebhookUpdate) (store.Webhook, error)
	RotateWebhookSecret(ctx context.Context, tenantID, id, newSecret string, keepPrevious time.Duration) (store.Webhook, error)
	DeleteWebhook(ctx context.Context, tenantID, id string) error
	AppendWebhookDelivery(ctx context.Context, d store.WebhookDelivery) error
	ListWebhookDeliveries(ctx context.Context, webhookID string, limit int) ([]store.WebhookDelivery, error)
}

var _ ServiceStore = (*store.Store)(nil)

// ServiceConfig configures a Service. Store and Destinations are required.
type ServiceConfig struct {
	Store        ServiceStore
	Destinations *Destinations
	// Sender sends test events. Nil makes Test fail with ErrNoSender;
	// the admin CLI, which only manages rows, leaves it nil.
	Sender Sender
	Clock  Clock
	// Metrics counts test events. Optional.
	Metrics *Metrics
	// MaxPerTenant is the per-tenant limit. Default DefaultMaxPerTenant;
	// negative means no limit.
	MaxPerTenant int
	// RequestTimeout bounds a test event. Default DefaultRequestTimeout.
	RequestTimeout time.Duration
}

// ErrNoSender: the service was built without a Sender.
var ErrNoSender = errors.New("webhook: this service cannot send")

// Service is the management side of webhooks: what the API routes and
// the admin CLI call. Every method that takes a tenant id only ever sees
// that tenant's webhooks; an empty tenant id (the admin CLI) sees all.
type Service struct {
	cfg ServiceConfig
}

// NewService validates cfg and returns a Service.
func NewService(cfg ServiceConfig) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("webhook: ServiceConfig.Store is required")
	case cfg.Destinations == nil:
		return nil, errors.New("webhook: ServiceConfig.Destinations is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = RealClock{}
	}
	if cfg.MaxPerTenant == 0 {
		cfg.MaxPerTenant = DefaultMaxPerTenant
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	return &Service{cfg: cfg}, nil
}

// MaxPerTenant is the limit Create enforces; zero means none.
func (s *Service) MaxPerTenant() int {
	if s.cfg.MaxPerTenant < 0 {
		return 0
	}
	return s.cfg.MaxPerTenant
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return IDPrefix + hex.EncodeToString(b)
}

func checkDescription(d string) error {
	if len(d) > MaxDescriptionLength {
		return invalid("description is longer than %d bytes", MaxDescriptionLength)
	}
	if !utf8.ValidString(d) {
		return invalid("description is not valid UTF-8")
	}
	for _, r := range d {
		if r < ' ' && r != '\t' {
			return invalid("description contains a control character")
		}
	}
	return nil
}

// checkURL validates a URL for storing and returns its normalised form.
func (s *Service) checkURL(ctx context.Context, raw string) (string, error) {
	u, err := s.cfg.Destinations.Check(ctx, raw)
	if err != nil {
		if de, ok := asDestinationError(err); ok {
			return "", &InvalidError{Code: de.Code, Message: de.Message}
		}
		return "", err
	}
	return u, nil
}

// CreateParams describes a webhook to create.
type CreateParams struct {
	URL string
	// EventTypes is the subscription; empty means every event.
	EventTypes  []string
	Description string
	// Disabled creates the webhook switched off.
	Disabled bool
}

// Create validates p and stores a webhook with a freshly generated
// secret. The returned row is the only place the caller gets the secret
// from without asking for it again by rotating.
//
// Errors: *InvalidError, ErrLimit, store.ErrNotFound (no such tenant).
func (s *Service) Create(ctx context.Context, tenantID string, p CreateParams) (store.Webhook, error) {
	if tenantID == "" {
		return store.Webhook{}, invalid("a tenant is required")
	}
	types, err := NormalizeEventTypes(p.EventTypes)
	if err != nil {
		return store.Webhook{}, invalid("%v", err)
	}
	if err := checkDescription(p.Description); err != nil {
		return store.Webhook{}, err
	}
	u, err := s.checkURL(ctx, p.URL)
	if err != nil {
		return store.Webhook{}, err
	}
	secret, err := NewSecret()
	if err != nil {
		return store.Webhook{}, err
	}
	w, err := s.cfg.Store.CreateWebhook(ctx, store.NewWebhook{ID: newID(), TenantID: tenantID, URL: u, Secret: secret,
		EventTypes: types, Description: p.Description, Disabled: p.Disabled}, s.MaxPerTenant())
	if errors.Is(err, store.ErrWebhookLimit) {
		return store.Webhook{}, fmt.Errorf("%w (%d)", ErrLimit, s.MaxPerTenant())
	}
	return w, err
}

// Get returns one webhook of the tenant.
func (s *Service) Get(ctx context.Context, tenantID, id string) (store.Webhook, error) {
	return s.cfg.Store.GetWebhook(ctx, tenantID, id)
}

// List returns the tenant's webhooks, oldest first.
func (s *Service) List(ctx context.Context, tenantID string) ([]store.Webhook, error) {
	return s.cfg.Store.ListWebhooks(ctx, tenantID)
}

// UpdateParams changes a webhook; nil fields keep the stored value.
type UpdateParams struct {
	URL         *string
	EventTypes  *[]string
	Description *string
	Enabled     *bool
}

// Update validates and applies p. See store.UpdateWebhook for what
// enabling and changing the URL do to the delivery state.
func (s *Service) Update(ctx context.Context, tenantID, id string, p UpdateParams) (store.Webhook, error) {
	var u store.WebhookUpdate
	if p.EventTypes != nil {
		types, err := NormalizeEventTypes(*p.EventTypes)
		if err != nil {
			return store.Webhook{}, invalid("%v", err)
		}
		u.EventTypes = types
	}
	if p.Description != nil {
		if err := checkDescription(*p.Description); err != nil {
			return store.Webhook{}, err
		}
		u.Description = p.Description
	}
	u.Enabled = p.Enabled
	if p.URL != nil {
		// Look the webhook up first, so that a caller cannot use the URL
		// check (which resolves a name) on a webhook that is not theirs.
		if _, err := s.cfg.Store.GetWebhook(ctx, tenantID, id); err != nil {
			return store.Webhook{}, err
		}
		norm, err := s.checkURL(ctx, *p.URL)
		if err != nil {
			return store.Webhook{}, err
		}
		u.URL = &norm
	}
	return s.cfg.Store.UpdateWebhook(ctx, tenantID, id, u)
}

// Delete removes a webhook and its delivery log.
func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	return s.cfg.Store.DeleteWebhook(ctx, tenantID, id)
}

// RotateSecret replaces the secret with a new one, which is in the
// returned row. The previous secret keeps signing, next to the new one,
// for keepPrevious: zero or negative means it stops at once.
func (s *Service) RotateSecret(ctx context.Context, tenantID, id string, keepPrevious time.Duration) (store.Webhook, error) {
	if keepPrevious > MaxRotationGrace {
		return store.Webhook{}, invalid("the previous secret can be kept for at most %s", MaxRotationGrace)
	}
	if keepPrevious < 0 {
		keepPrevious = 0
	}
	secret, err := NewSecret()
	if err != nil {
		return store.Webhook{}, err
	}
	return s.cfg.Store.RotateWebhookSecret(ctx, tenantID, id, secret, keepPrevious)
}

// TestResult is what the endpoint did with a test event.
type TestResult struct {
	// EventID is the id of the webhook.test event that was sent.
	EventID   string
	Delivered bool
	// Status is the endpoint's HTTP status; zero when none arrived.
	Status     int
	DurationMs int64
	// ErrorCode and Error are empty when Delivered.
	ErrorCode string
	Error     string
}

// Test sends a webhook.test event to the webhook's URL now, signed like
// any other, and reports what came back. It works on a disabled webhook
// too, does not touch the cursor or the retry state, is not retried, and
// leaves one row in the delivery log.
func (s *Service) Test(ctx context.Context, tenantID, id string) (TestResult, error) {
	if s.cfg.Sender == nil {
		return TestResult{}, ErrNoSender
	}
	wh, err := s.cfg.Store.GetWebhook(ctx, tenantID, id)
	if err != nil {
		return TestResult{}, err
	}
	now := s.cfg.Clock.Now()
	env := testEnvelope(wh.ID, now)
	body, err := env.Marshal()
	if err != nil {
		return TestResult{}, err
	}
	secrets := []string{wh.Secret}
	if wh.PreviousSecret != "" && wh.PreviousSecretExpiresAt != nil && wh.PreviousSecretExpiresAt.After(now) {
		secrets = append(secrets, wh.PreviousSecret)
	}
	header, err := signatureHeaders(env.ID, now, body, secrets...)
	if err != nil {
		return TestResult{}, err
	}
	header.Set(HeaderEventType, env.Type)
	header.Set(HeaderAttempt, strconv.Itoa(1))
	sctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	res := s.cfg.Sender.Send(sctx, Request{URL: wh.URL, Header: header, Body: body})
	cancel()
	if err := ctx.Err(); err != nil {
		return TestResult{}, err // the caller went away; nothing to report to
	}
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.test(res)
	}
	status := store.DeliveryFailed
	if res.OK() {
		status = store.DeliverySucceeded
	}
	// The log row is a convenience; failing to write it does not change
	// what happened at the endpoint.
	lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	_ = s.cfg.Store.AppendWebhookDelivery(lctx, store.WebhookDelivery{WebhookID: wh.ID, MessageID: env.ID, EventType: env.Type,
		Attempt: 1, Status: status, ResponseStatus: res.Status, DurationMs: int(res.Duration.Milliseconds()),
		ErrorCode: res.Code, Error: res.Error})
	lcancel()
	return TestResult{EventID: env.ID, Delivered: res.OK(), Status: res.Status, DurationMs: res.Duration.Milliseconds(),
		ErrorCode: res.Code, Error: res.Error}, nil
}

// Deliveries returns the newest rows of a webhook's delivery log.
func (s *Service) Deliveries(ctx context.Context, tenantID, id string, limit int) ([]store.WebhookDelivery, error) {
	if _, err := s.cfg.Store.GetWebhook(ctx, tenantID, id); err != nil {
		return nil, err
	}
	return s.cfg.Store.ListWebhookDeliveries(ctx, id, limit)
}

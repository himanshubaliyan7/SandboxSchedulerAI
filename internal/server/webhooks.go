package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/api"
	"github.com/himanshubaliyan7/warmbox/internal/store"
	"github.com/himanshubaliyan7/warmbox/internal/webhook"
)

// WebhookConfig configures lifecycle webhooks (internal/webhook). The
// zero value is "on, with the defaults".
type WebhookConfig struct {
	// Disabled turns the feature off: no worker runs and the
	// /v1/webhooks routes answer 501.
	Disabled bool

	// AllowHTTP accepts http:// webhook URLs. Development only.
	AllowHTTP bool
	// AllowLoopback accepts webhook URLs that point at this machine's
	// loopback interface. Development only: on a shared server it lets a
	// tenant reach every service that listens on 127.0.0.1.
	AllowLoopback bool
	// PermitPrivateCIDRs opens sub-ranges of RFC 1918, CGNAT or ULA space
	// to webhooks, for receivers inside the operator's network. It is
	// separate from EgressPermitPrivateCIDRs on purpose: what a sandbox
	// may reach and what the server itself may call are different grants.
	PermitPrivateCIDRs []string

	// MaxPerTenant is how many webhooks a tenant may have. Default 10;
	// negative means no limit.
	MaxPerTenant int
	// The rest take the webhook package's defaults when zero.
	RequestTimeout time.Duration // one attempt; default 10s
	Concurrency    int           // webhooks delivered to at once; default 8
	PollInterval   time.Duration // default 1s
	RetryBase      time.Duration // default 5s
	RetryMax       time.Duration // default 15m
	Retention      time.Duration // default 24h; negative: never drop by age
	DisableAfter   time.Duration // default 72h; negative: never auto-disable
}

// drainBudget bounds the last round of deliveries during a shutdown.
const drainBudget = 5 * time.Second

// webhooks is the running webhook subsystem; nil when it is off.
type webhooks struct {
	svc     *webhook.Service
	worker  *webhook.Worker
	metrics *webhook.Metrics
}

// startWebhooks builds the destination policy, the management service
// and the delivery worker, and starts the worker.
func startWebhooks(cfg WebhookConfig, st *store.Store, log *slog.Logger) (*webhooks, error) {
	if cfg.Disabled {
		log.Info("webhooks are disabled")
		return nil, nil
	}
	dest, err := webhook.NewDestinations(webhook.DestinationConfig{
		AllowHTTP: cfg.AllowHTTP, AllowLoopback: cfg.AllowLoopback, PermitPrivateCIDRs: cfg.PermitPrivateCIDRs,
	})
	if err != nil {
		return nil, fmt.Errorf("webhooks: %w", err)
	}
	if cfg.AllowHTTP {
		log.Warn("webhook URLs may use plain http (--webhook-allow-http): payloads and signatures travel unencrypted; development only")
	}
	if cfg.AllowLoopback {
		log.Warn("webhook URLs may point at this machine's loopback interface (--webhook-allow-loopback): " +
			"any tenant can make sbxd send requests to local services; development only")
	}
	if len(cfg.PermitPrivateCIDRs) > 0 {
		log.Info("webhooks may be delivered into private ranges", "cidrs", cfg.PermitPrivateCIDRs)
	}
	m := webhook.NewMetrics()
	sender := webhook.NewHTTPSender(dest, cfg.RequestTimeout)
	svc, err := webhook.NewService(webhook.ServiceConfig{Store: st, Destinations: dest, Sender: sender, Metrics: m,
		MaxPerTenant: cfg.MaxPerTenant, RequestTimeout: cfg.RequestTimeout})
	if err != nil {
		return nil, err
	}
	worker, err := webhook.NewWorker(webhook.Config{
		Store: st, Sender: sender, Logger: log, Metrics: m,
		PollInterval: cfg.PollInterval, Concurrency: cfg.Concurrency, RequestTimeout: cfg.RequestTimeout,
		RetryBase: cfg.RetryBase, RetryMax: cfg.RetryMax, Retention: cfg.Retention, DisableAfter: cfg.DisableAfter,
	})
	if err != nil {
		return nil, err
	}
	worker.Start()
	return &webhooks{svc: svc, worker: worker, metrics: m}, nil
}

// service is what the API serves the webhook routes from; nil (the
// interface, not a typed nil) when webhooks are off.
func (w *webhooks) service() api.Webhooks {
	if w == nil {
		return nil
	}
	return w.svc
}

func (w *webhooks) writeMetrics(out io.Writer) {
	if w != nil {
		w.metrics.Write(out)
	}
}

// stop sends what the shutdown itself produced (for a short while), then
// stops the worker, letting attempts in flight finish while ctx lasts.
// What is left is delivered by the next start, or by another server on
// the same database: the cursor is in the store.
func (w *webhooks) stop(ctx context.Context) {
	if w == nil {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, drainBudget)
	w.worker.Drain(dctx)
	cancel()
	w.worker.Stop(ctx)
}

// abort stops the worker without the final round of deliveries. Run
// defers it for the paths that return early; after stop it does nothing.
func (w *webhooks) abort() {
	if w == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	w.worker.Stop(ctx)
}

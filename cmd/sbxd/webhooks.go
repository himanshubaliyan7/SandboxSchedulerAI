package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/server"
	"github.com/himanshubaliyan7/warmbox/internal/store"
	"github.com/himanshubaliyan7/warmbox/internal/webhook"
)

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// webhookFlags registers the serve flags of lifecycle webhooks. The
// returned function is called after the flags were parsed and fills in
// the settings that are not plain values.
func webhookFlags(fs *flag.FlagSet, cfg *server.WebhookConfig) (finish func()) {
	enabled := fs.Bool("webhooks", envBool("SBX_WEBHOOKS", true),
		"deliver lifecycle webhooks; false stops the delivery worker and makes the /v1/webhooks routes answer 501 [SBX_WEBHOOKS]")
	fs.BoolVar(&cfg.AllowHTTP, "webhook-allow-http", envBool("SBX_WEBHOOK_ALLOW_HTTP", false),
		"accept http:// webhook URLs; development only, payloads and signatures travel unencrypted [SBX_WEBHOOK_ALLOW_HTTP]")
	fs.BoolVar(&cfg.AllowLoopback, "webhook-allow-loopback", envBool("SBX_WEBHOOK_ALLOW_LOOPBACK", false),
		"accept webhook URLs on this machine's loopback interface; development only, never on a server other people "+
			"create webhooks on [SBX_WEBHOOK_ALLOW_LOOPBACK]")
	permit := fs.String("webhook-permit-private-cidrs", envStr("SBX_WEBHOOK_PERMIT_PRIVATE_CIDRS", ""),
		"comma-separated private ranges (inside RFC 1918, CGNAT or ULA space) webhooks may be delivered into, for receivers "+
			"on your own network; everything private is refused by default [SBX_WEBHOOK_PERMIT_PRIVATE_CIDRS]")
	fs.IntVar(&cfg.MaxPerTenant, "webhook-max-per-tenant", envInt("SBX_WEBHOOK_MAX_PER_TENANT", 0),
		"webhooks a tenant may have (default 10, negative for no limit) [SBX_WEBHOOK_MAX_PER_TENANT]")
	fs.DurationVar(&cfg.RequestTimeout, "webhook-timeout", envDuration("SBX_WEBHOOK_TIMEOUT", 0),
		"how long one delivery attempt may take, lookup to response (default 10s) [SBX_WEBHOOK_TIMEOUT]")
	fs.IntVar(&cfg.Concurrency, "webhook-concurrency", envInt("SBX_WEBHOOK_CONCURRENCY", 0),
		"webhooks delivered to at the same time; one webhook never takes more than one slot (default 8) [SBX_WEBHOOK_CONCURRENCY]")
	fs.DurationVar(&cfg.PollInterval, "webhook-poll-interval", envDuration("SBX_WEBHOOK_POLL_INTERVAL", 0),
		"how often the worker looks for new events; bounds the delay of a first attempt (default 1s) [SBX_WEBHOOK_POLL_INTERVAL]")
	fs.DurationVar(&cfg.RetryBase, "webhook-retry-base", envDuration("SBX_WEBHOOK_RETRY_BASE", 0),
		"delay after the first failed attempt; it doubles with each further failure (default 5s) [SBX_WEBHOOK_RETRY_BASE]")
	fs.DurationVar(&cfg.RetryMax, "webhook-retry-max", envDuration("SBX_WEBHOOK_RETRY_MAX", 0),
		"largest delay between two attempts (default 15m) [SBX_WEBHOOK_RETRY_MAX]")
	fs.DurationVar(&cfg.Retention, "webhook-retention", envDuration("SBX_WEBHOOK_RETENTION", 0),
		"an undelivered event older than this is dropped and recorded as a gap (default 24h, negative to never drop) [SBX_WEBHOOK_RETENTION]")
	fs.DurationVar(&cfg.DisableAfter, "webhook-disable-after", envDuration("SBX_WEBHOOK_DISABLE_AFTER", 0),
		"disable a webhook whose deliveries have failed for this long (default 72h, negative to never disable) [SBX_WEBHOOK_DISABLE_AFTER]")
	return func() {
		cfg.Disabled = !*enabled
		cfg.PermitPrivateCIDRs = splitList(*permit)
	}
}

// --- admin: webhooks ---------------------------------------------------------

type webhookJSON struct {
	ID             string     `json:"id"`
	TenantID       string     `json:"tenant_id"`
	URL            string     `json:"url"`
	EventTypes     []string   `json:"event_types"`
	Description    string     `json:"description,omitempty"`
	Enabled        bool       `json:"enabled"`
	DisabledReason string     `json:"disabled_reason,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	Failures       int        `json:"consecutive_failures"`
	FailingSince   *time.Time `json:"failing_since,omitempty"`
	LastSuccessAt  *time.Time `json:"last_success_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	DroppedEvents  int64      `json:"dropped_events"`
	// Secret is present only in the output of "webhook create".
	Secret string `json:"secret,omitempty"`
}

func webhookOut(w store.Webhook, withSecret bool) webhookJSON {
	out := webhookJSON{ID: w.ID, TenantID: w.TenantID, URL: w.URL, EventTypes: w.EventTypes, Description: w.Description,
		Enabled: w.Enabled, DisabledReason: w.DisabledReason, CreatedAt: w.CreatedAt, Failures: w.Attempts,
		FailingSince: w.FailingSince, LastSuccessAt: w.LastSuccessAt, LastError: w.LastError, DroppedEvents: w.DroppedEvents}
	if withSecret {
		out.Secret = w.Secret
	}
	return out
}

func adminWebhookList(a *adminFlags, args []string) error {
	tenant := a.fs.String("tenant", "", "list only this tenant's webhooks (default: every tenant's)")
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()
	hooks, err := st.ListWebhooks(ctx, *tenant)
	if err != nil {
		return err
	}
	if *a.asJSON {
		out := make([]webhookJSON, 0, len(hooks))
		for _, w := range hooks {
			out = append(out, webhookOut(w, false))
		}
		return a.printJSON(out)
	}
	a.table("ID\tTENANT\tSTATE\tEVENTS\tFAILURES\tDROPPED\tURL", func(tw io.Writer) {
		for _, w := range hooks {
			state := "enabled"
			switch {
			case !w.Enabled:
				state = "disabled (" + w.DisabledReason + ")"
			case w.FailingSince != nil:
				state = "failing since " + w.FailingSince.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\n", w.ID, w.TenantID, state, strings.Join(w.EventTypes, ","), w.Attempts, w.DroppedEvents, w.URL)
		}
	})
	return nil
}

func adminWebhookCreate(a *adminFlags, args []string) error {
	tenant := a.fs.String("tenant", "", "tenant id (required)")
	url := a.fs.String("url", "", "endpoint URL (required)")
	events := a.fs.String("events", "", "comma-separated event types (default: all); known: "+strings.Join(webhook.EventTypes(), ", "))
	desc := a.fs.String("description", "", "free-form description")
	disabled := a.fs.Bool("disabled", false, "create the webhook switched off")
	allowHTTP := a.fs.Bool("allow-http", false, "accept an http:// URL (the server must run with --webhook-allow-http to deliver to it)")
	allowLoopback := a.fs.Bool("allow-loopback", false, "accept a loopback URL (the server must run with --webhook-allow-loopback)")
	permit := a.fs.String("permit-private-cidrs", "", "comma-separated private ranges the URL may resolve into (the server needs the same --webhook-permit-private-cidrs)")
	noLimit := a.fs.Bool("no-limit", false, "ignore the per-tenant limit of "+fmt.Sprint(webhook.DefaultMaxPerTenant))
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()
	if *tenant == "" || *url == "" {
		return errors.New("--tenant and --url are required")
	}
	// The same checks as the API applies. The running server validates
	// again, with its own flags, every time it delivers.
	dest, err := webhook.NewDestinations(webhook.DestinationConfig{AllowHTTP: *allowHTTP, AllowLoopback: *allowLoopback,
		PermitPrivateCIDRs: splitList(*permit)})
	if err != nil {
		return err
	}
	limit := 0
	if *noLimit {
		limit = -1
	}
	svc, err := webhook.NewService(webhook.ServiceConfig{Store: st, Destinations: dest, MaxPerTenant: limit})
	if err != nil {
		return err
	}
	w, err := svc.Create(ctx, *tenant, webhook.CreateParams{URL: *url, EventTypes: splitList(*events), Description: *desc, Disabled: *disabled})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no such tenant %q", *tenant)
		}
		return err
	}
	if *a.asJSON {
		return a.printJSON(webhookOut(w, true))
	}
	fmt.Fprintf(a.stdout, "webhook: %s\ntenant:  %s\nurl:     %s\nevents:  %s\nsecret:  %s\n\n"+
		"This is the only time the secret is shown by this command. It is stored in the database\n"+
		"(the server signs with it); the tenant can replace it with POST /v1/webhooks/%s/rotate-secret.\n",
		w.ID, w.TenantID, w.URL, strings.Join(w.EventTypes, ","), w.Secret, w.ID)
	return nil
}

func adminWebhookDelete(a *adminFlags, args []string) error {
	id := a.fs.String("id", "", "webhook id, as printed by webhook create or webhook list (required)")
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()
	if *id == "" {
		return errors.New("--id is required")
	}
	if err := st.DeleteWebhook(ctx, "", *id); err != nil {
		return err
	}
	if *a.asJSON {
		return a.printJSON(map[string]string{"deleted": *id})
	}
	fmt.Fprintf(a.stdout, "webhook %s deleted (its undelivered events will not be sent)\n", *id)
	return nil
}

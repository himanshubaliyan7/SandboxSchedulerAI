// Package webhook delivers sandbox lifecycle events to endpoints that
// tenants configure: signed HTTPS POSTs, in order, at least once.
//
// # Where the events come from
//
// Every state change of a sandbox and every command it ran is already a
// row of the events table, appended in the same transaction as the change
// itself (store.Transition, store.Claim, the API's execution recorder).
// That table is the outbox. Producing an event costs nothing extra and
// cannot be forgotten: there is no second write that could fail after the
// first one committed.
//
// # Delivery model
//
// Each webhook has a cursor into its tenant's part of the log. A Worker
// polls for webhooks that are due and have events after their cursor,
// leases each one (FOR UPDATE SKIP LOCKED, so that two sbxd processes on
// one database never work on the same webhook), reads the next events in
// order and sends them one at a time. After each event it records, in one
// transaction, the attempt and the new cursor. So:
//
//   - Ordering: a webhook receives its events in log order, one request
//     at a time. A failed event is retried before any later event is
//     sent; it blocks the webhook, not the other webhooks.
//   - At least once: the cursor moves only after the endpoint answered
//     2xx. A crash between the answer and the commit, or a lease that
//     ran out, sends the event again. Receivers deduplicate on the
//     Webhook-Id header.
//   - Never concurrently: a lease is longer than the longest turn a
//     worker takes, and every write is fenced by the lease token.
//
// # What happens when an endpoint is down
//
// Failed attempts are retried with exponential backoff (5s doubling to
// 15m, with jitter). The backlog is bounded by age, not by count: an
// event that is older than Retention (24h) when its turn comes is not
// sent; the cursor moves past it, the webhook's dropped_events counter
// grows and a "dropped" row in the delivery log records the gap. A
// webhook that has failed continuously for DisableAfter (72h) is
// disabled; so is one whose endpoint answers 410 Gone. An event that the
// endpoint keeps rejecting with a 4xx is dropped after MaxRejections
// attempts, so one event the receiver cannot digest does not hold up the
// rest for a day.
//
// # Signing and safety
//
// Requests are signed as the Standard Webhooks specification describes
// (sign.go). URLs are tenant input and are treated as hostile: see url.go
// for the SSRF rules, which reuse the egress proxy's deny set.
package webhook

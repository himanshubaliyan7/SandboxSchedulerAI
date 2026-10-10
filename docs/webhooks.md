# Webhooks

A webhook is an HTTPS endpoint of yours that Warmbox calls when something
happens to one of your sandboxes: it was created, it started running, a
command finished, it terminated. Each call is a signed JSON `POST`.

- [Quick start](#quick-start)
- [Event catalogue](#event-catalogue)
- [Verifying a request](#verifying-a-request)
- [Delivery guarantees](#delivery-guarantees)
- [Retries](#retries)
- [When an endpoint stays down](#when-an-endpoint-stays-down)
- [Managing webhooks](#managing-webhooks)
- [Security](#security)
- [For operators](#for-operators)

## Quick start

Create a webhook with your tenant API key. The answer contains the signing
secret; it is not shown again.

```sh
curl -sS -X POST http://127.0.0.1:8080/v1/webhooks \
  -H "X-API-Key: $SBX_API_KEY" -H 'Content-Type: application/json' \
  -d '{"url": "https://hooks.example.com/warmbox", "event_types": ["sandbox.terminated", "command.finished"]}'
```

```json
{
  "id": "wh_5f1c0a9e7b3d4c2a8e6f0b1d",
  "url": "https://hooks.example.com/warmbox",
  "event_types": ["command.finished", "sandbox.terminated"],
  "description": "",
  "enabled": true,
  "created_at": "2026-10-10T12:00:00.000000Z",
  "updated_at": "2026-10-10T12:00:00.000000Z",
  "consecutive_failures": 0,
  "dropped_events": 0,
  "secret": "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
}
```

Then check that your endpoint is reachable and verifies signatures:

```sh
curl -sS -X POST http://127.0.0.1:8080/v1/webhooks/wh_5f1c0a9e7b3d4c2a8e6f0b1d/test -H "X-API-Key: $SBX_API_KEY"
# {"event_id":"evt_test_3f9a…","delivered":true,"status":200,"duration_ms":41}
```

Only events that occur after a webhook was created are delivered to it.

## Event catalogue

Every request body is one envelope:

| Member | |
|---|---|
| `id` | Identifies the event. The same on every attempt and in the `Webhook-Id` header; the same for every webhook that receives the event. Treat it as opaque. |
| `type` | One of the types below. |
| `version` | `1`. It changes only with an incompatible change. New members and new event types do not change it: ignore what you do not know. |
| `created_at` | When the event happened, RFC 3339, UTC. Not when it was sent. |
| `sandbox_id` | The sandbox. Absent from `webhook.test`. |
| `template_id` | Its template. Absent from `webhook.test`, and from events delivered after the sandbox's record was garbage-collected (seven days after it was destroyed, by default). |
| `data` | Depends on `type`. |

`data.metadata` is the metadata the sandbox was created with, whenever it has
any; it is read when the event is sent. The envelope never contains the
sandbox's access token, its environment, its network policy, or the server's
host and container ids.

| Type | When | `data` |
|---|---|---|
| `sandbox.created` | A sandbox was assigned to you (`POST /v1/sandboxes` took it from the pool). It is not running yet. | `state` (`assigned`), `metadata` |
| `sandbox.running` | The session was set up; the sandbox accepts commands. | `state` (`running`), `metadata` |
| `sandbox.terminated` | The sandbox stopped being yours. Sent once per sandbox, whatever ended it. | `state` (`recycling`, or `failed` when the container died), `reason`, `metadata` |
| `sandbox.destroyed` | The container and its storage are gone. | `state` (`destroyed`), `reason`, `metadata` |
| `command.started` | A command was started through the API. | `pid`, `cmd`, `args`, `args_truncated`, `tag`, `pty`, `cwd`, `timeout_ms`, `stdin`, `metadata` |
| `command.finished` | It ended, whether or not a client was attached. | `pid`, `cmd`, `args`, `tag`, `pty`, `exit_code`, `reason`, `signal`, `duration_ms`, `stdout_bytes`, `stderr_bytes`, `uncounted_bytes`, `sandbox_reason`, `metadata` |
| `webhook.test` | Only when you call `POST /v1/webhooks/{id}/test`. Sent whatever the subscription says; cannot be subscribed to. | `webhook_id`, `message` |

`reason` of a sandbox event is why it terminated: `released` (you deleted
it), `idle_timeout`, `max_lifetime`, `oom_killed`, `instance_lost`,
`host_drain` (the server was stopped), `deadline_exceeded`, `error`. `reason`
of `command.finished` is how the command ended: `exited`, `killed`, `timeout`,
`oom`, or `sandbox_terminated` when the sandbox went away under it (then there
is no `exit_code`, and `sandbox_reason` says why the sandbox ended).

A webhook subscribes to a list of these types, or to `"*"` (the default),
which also covers types added later.

### Examples

```json
{
  "id": "evt_1041",
  "type": "sandbox.created",
  "version": 1,
  "created_at": "2026-10-10T12:00:03.118204Z",
  "sandbox_id": "sb-4bf9d106833ea7fc",
  "template_id": "python",
  "data": { "state": "assigned", "metadata": { "job": "42" } }
}
```

```json
{
  "id": "evt_1045",
  "type": "command.finished",
  "version": 1,
  "created_at": "2026-10-10T12:00:04.902113Z",
  "sandbox_id": "sb-4bf9d106833ea7fc",
  "template_id": "python",
  "data": {
    "pid": 3, "cmd": "python3", "args": ["-c", "print(6*7)"], "pty": false,
    "exit_code": 0, "reason": "exited", "duration_ms": 38,
    "stdout_bytes": 3, "stderr_bytes": 0,
    "metadata": { "job": "42" }
  }
}
```

```json
{
  "id": "evt_1046",
  "type": "sandbox.terminated",
  "version": 1,
  "created_at": "2026-10-10T12:10:04.000310Z",
  "sandbox_id": "sb-4bf9d106833ea7fc",
  "template_id": "python",
  "data": { "state": "recycling", "reason": "idle_timeout", "metadata": { "job": "42" } }
}
```

```json
{
  "id": "evt_test_3f9a1c0e5b7d2a4c6e8f0a1b",
  "type": "webhook.test",
  "version": 1,
  "created_at": "2026-10-10T12:00:00.512000Z",
  "data": { "webhook_id": "wh_5f1c0a9e7b3d4c2a8e6f0b1d", "message": "This is a test event from Warmbox." }
}
```

## Verifying a request

Anyone who learns your URL can post to it. Verify every request before you
act on it.

Requests are signed as the [Standard Webhooks](https://www.standardwebhooks.com)
specification describes, so its libraries (for Python, JavaScript, Go, Ruby,
Java, Rust, PHP, C#, Elixir) verify Warmbox webhooks as they are.

| Header | |
|---|---|
| `Webhook-Id` | The event id (`id` of the body). |
| `Webhook-Timestamp` | Seconds since the epoch, when this attempt was made. |
| `Webhook-Signature` | `v1,<base64>`: HMAC-SHA256 over `<Webhook-Id>.<Webhook-Timestamp>.<raw body>`. The key is the base64-decoded part of the secret after `whsec_`. During a [secret rotation](#rotating-the-secret) there are two, separated by a space. |
| `Warmbox-Event-Type`, `Warmbox-Delivery-Attempt` | Conveniences for routing and logging. Not signed: do not base decisions on them. |

Three rules:

1. Verify the **raw body**, the bytes as they arrived. Parsing the JSON and
   serialising it again changes them.
2. Refuse a timestamp more than five minutes from your clock. The signature
   covers it, so an old request cannot be replayed with a fresh one.
3. Compare signatures in constant time.

The SDK helpers do all three and need no client object.

**Python**

```python
from sbx import verify_webhook, WebhookVerificationError

@app.post("/warmbox")                      # Flask; any framework works
def warmbox():
    try:
        event = verify_webhook(request.get_data(), request.headers, WEBHOOK_SECRET)
    except WebhookVerificationError:
        return "", 400
    if event.type == "sandbox.terminated":
        print(event.sandbox_id, event.data["reason"])
    return "", 204
```

**TypeScript**

```ts
import { verifyWebhook, WebhookVerificationError } from 'sbx-sdk'

// Any runtime with the Fetch API and WebCrypto: Node 20+, Deno, Bun, Workers.
export async function handler(request: Request): Promise<Response> {
  let event
  try {
    event = await verifyWebhook(await request.text(), request.headers, WEBHOOK_SECRET)
  } catch (e) {
    if (e instanceof WebhookVerificationError) return new Response(null, { status: 400 })
    throw e
  }
  if (event.type === 'sandbox.terminated') console.log(event.sandboxId, event.data.reason)
  return new Response(null, { status: 204 })
}
```

With Express, mount `express.raw({ type: 'application/json' })` on the route
and pass `req.body` (a `Buffer`) and `req.headers`.

**Go** (no dependency; this is the whole algorithm)

```go
func verify(secret string, h http.Header, body []byte) error {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return err
	}
	id, ts := h.Get("Webhook-Id"), h.Get("Webhook-Timestamp")
	sent, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("bad timestamp")
	}
	if d := time.Since(time.Unix(sent, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return errors.New("timestamp outside the tolerance")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, sig := range strings.Fields(h.Get("Webhook-Signature")) {
		if enc, ok := strings.CutPrefix(sig, "v1,"); ok {
			if got, err := base64.StdEncoding.DecodeString(enc); err == nil && hmac.Equal(got, want) {
				return nil
			}
		}
	}
	return errors.New("no signature matches")
}
```

A vector to test your own implementation against (it is the one the
Standard Webhooks libraries use): secret
`whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw`, id `msg_p5jXN8AQM9LWM0D4loKWxJek`,
timestamp `1614265330`, body `{"test": 2432232314}` gives
`v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=`.

### Where Warmbox departs from Standard Webhooks

- The specification suggests a payload of `type`, `timestamp` and `data`.
  Warmbox's envelope has `id`, `version`, `created_at`, `sandbox_id` and
  `template_id` at the top level and calls the event time `created_at`.
  Verification is unaffected: it never looks inside the body.
- Only the symmetric scheme (`v1`, HMAC-SHA256) is offered, not the
  asymmetric `v1a`.
- The two `Warmbox-*` headers are additions.

## Delivery guarantees

**In order.** Each webhook receives its events one request at a time, in the
order of the server's event log. A later event is never sent while an earlier
one is still being retried. For one sandbox that is the order things happened
in: `sandbox.created`, `sandbox.running`, its commands, `sandbox.terminated`,
`sandbox.destroyed`. Two caveats: events of *different* sandboxes that were
written in the same instant may arrive in either order, and a
`command.finished` written while its sandbox was being torn down may arrive
after `sandbox.terminated`. Order by `created_at` if that matters to you.

**At least once.** An event is marked delivered only after your endpoint
answered `2xx`. If the server stops, or loses its database connection, between
your answer and that mark, the event is sent again. Deduplicate on
`Webhook-Id`: process an id once, and answer `2xx` to its repeats.

**Not concurrently.** A webhook never has two requests in flight, including
when several `sbxd` processes share the database.

**Not instantly.** Events are picked up by polling, every second by default,
and an event is only sent once every database transaction that started before
it has finished (that is what makes the order stable). Expect a second or
two; a long-running write transaction on the same Postgres cluster delays
delivery for as long as it runs.

**Answer quickly.** A request times out after 10 seconds. Acknowledge with a
`2xx` and do slow work afterwards. Only the status is looked at; the response
body is discarded after at most 64 KiB, and redirects are not followed.

## Retries

| Your endpoint | What happens |
|---|---|
| `2xx` | Delivered. The next event is sent. |
| `5xx`, `408`, `425`, `429`, a timeout, a refused or failed connection, a TLS error, a `3xx` | Retried, with the delays below, until it succeeds or the event [expires](#when-an-endpoint-stays-down). |
| another `4xx` | Your endpoint rejected this event. It is retried on the same schedule, at most 10 attempts (about 35 minutes); then the event is dropped and the next one is sent, so that one event you cannot process does not hold up the rest for a day. |
| `410 Gone` | The webhook is disabled at once (`disabled_reason: endpoint_gone`). |

The delay after the n-th consecutive failure is 5 seconds doubled n-1 times,
capped at 15 minutes, with ±20% of jitter:

| Failed attempts | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 and more |
|---|---|---|---|---|---|---|---|---|---|
| Wait before the next | 5s | 10s | 20s | 40s | 80s | 160s | 320s | 640s | 15m |

A `Retry-After` header on a failure is honoured when it asks for more than
that, up to the 15-minute cap. A success resets the schedule. Every attempt is
a new request with a new `Webhook-Timestamp` and signature and the same
`Webhook-Id`.

## When an endpoint stays down

The backlog is bounded by age.

- **After 24 hours an undelivered event is dropped.** When its turn comes and
  it is older than that, it is not sent. The webhook's `dropped_events`
  counter grows, and the delivery log gets a row with `status: "dropped"`,
  `error_code: "retention_expired"` and how many events it stands for. Newer
  events are still delivered. An endpoint that was down for a day therefore
  comes back to the last 24 hours of events, in order, and a recorded gap
  before them.
- **After 72 hours of continuous failure the webhook is disabled**
  (`enabled: false`, `disabled_reason: "failing"`): three days in which
  attempts were made and none succeeded.
- **A disabled webhook receives nothing, and does not catch up.** Enabling it
  (`PATCH` with `{"enabled": true}`) starts it at the present with a clean
  failure state, whoever disabled it. Events from the disabled period are
  never delivered.

After any gap, reconcile from the API: `GET /v1/sandboxes` lists your
sandboxes with their state and termination reason (pass `state=all` to
include the ones that ended), and `GET /v1/sandboxes/{id}/events` is a
sandbox's full event log.

To see what is going on, read the webhook (`consecutive_failures`,
`failing_since`, `last_error`, `next_attempt_at`, `dropped_events`) and its
delivery log:

```sh
curl -sS "http://127.0.0.1:8080/v1/webhooks/$ID/deliveries?limit=20" -H "X-API-Key: $SBX_API_KEY"
```

```json
{"deliveries": [
  {"id": 812, "event_id": "evt_1046", "event_type": "sandbox.terminated", "attempt": 3, "status": "succeeded", "response_status": 200, "duration_ms": 37, "created_at": "2026-10-10T12:10:19Z"},
  {"id": 811, "event_id": "evt_1046", "event_type": "sandbox.terminated", "attempt": 2, "status": "failed", "response_status": 503, "duration_ms": 12, "error_code": "http_status", "error": "the endpoint answered 503", "created_at": "2026-10-10T12:10:09Z"}
]}
```

`error_code` is one of `http_status`, `timeout`, `connection_failed`,
`tls_error`, `redirect`, `invalid_url`, `destination_not_allowed`, and for
dropped events `retention_expired` or `rejected`. The log keeps seven days.

## Managing webhooks

All routes need the tenant API key. A sandbox access token is refused, so
code running in a sandbox cannot create or change a webhook. A tenant may
have 10 webhooks unless the operator changed that.

| | |
|---|---|
| `POST /v1/webhooks` | Create. Body: `url`, optional `event_types`, `description`, `enabled`. Returns the secret. |
| `GET /v1/webhooks` | List. |
| `GET /v1/webhooks/{id}` | Configuration and delivery state. |
| `PATCH /v1/webhooks/{id}` | Change `url`, `event_types`, `description`, `enabled`. |
| `DELETE /v1/webhooks/{id}` | Delete, with its delivery log. |
| `POST /v1/webhooks/{id}/rotate-secret` | New secret; returns it. |
| `POST /v1/webhooks/{id}/test` | Send a `webhook.test` event now and report the answer. |
| `GET /v1/webhooks/{id}/deliveries` | Recent attempts. |

The schemas are in [`api/openapi.yaml`](../api/openapi.yaml). Both SDKs wrap
these routes:

```python
from sbx import Webhooks

with Webhooks() as webhooks:                       # AsyncWebhooks for asyncio
    hook = webhooks.create("https://hooks.example.com/warmbox", event_types=["sandbox.terminated"])
    save(hook.secret)                              # shown this once
    print(webhooks.test(hook.id).delivered)
    for d in webhooks.deliveries(hook.id, limit=20):
        print(d.status, d.event_type, d.response_status, d.error)
```

```ts
import { Webhooks } from 'sbx-sdk'

const webhooks = new Webhooks()
const hook = await webhooks.create('https://hooks.example.com/warmbox', { eventTypes: ['sandbox.terminated'] })
save(hook.secret) // shown this once
console.log((await webhooks.test(hook.webhookId)).delivered)
```

### Rotating the secret

`POST /v1/webhooks/{id}/rotate-secret` returns a new secret. For the next 24
hours (`{"keep_previous_seconds": n}` changes that; `0` for not at all, at
most 7 days) every request carries two signatures, one per secret, and a
verifier accepts the request if either matches. So: rotate, deploy the new
secret to your receiver, done; nothing is rejected in between. If the old
secret leaked, rotate with `keep_previous_seconds: 0`.

## Security

**The secret is stored so that it can be read back.** An API key is only
ever compared, so Warmbox keeps a hash of it. A webhook secret is an HMAC key
the server has to use, so it is in the database in the clear (`webhooks.secret`),
readable by whoever can read the database and present in its backups. The API
returns it at creation and rotation only. It protects your endpoint from
forged events; it grants nothing on the Warmbox side.

**Where a webhook may point.** The URL is yours to choose and the server
sits inside someone's network, so it is checked when you store it and again on
every delivery:

- `https` only, no user name or password, no fragment; the host is a DNS name
  or a canonical IP literal (`127.1`, `0x7f.0.0.1`, `2130706433` are refused).
- The server resolves the name itself and refuses loopback, link-local,
  private (RFC 1918, CGNAT, ULA), cloud metadata (`169.254.169.254` and its
  relatives), multicast, reserved and documentation ranges, IPv4 addresses
  wrapped in IPv6 (IPv4-mapped, NAT64, 6to4, Teredo), and its own interface
  addresses. These are the rules of the sandbox egress proxy, from the same
  code.
- It connects to exactly the address it checked. The name is resolved once
  per delivery, so a DNS answer that changes afterwards (DNS rebinding)
  changes nothing.
- Redirects are not followed, proxy environment variables are ignored, and
  certificates are verified against the system's roots.

A URL that is refused answers `400` with `invalid_webhook_url` or
`webhook_url_not_allowed`. "The host does not resolve" and "the host resolves
to an address that is not allowed" are deliberately the same answer, so the
API cannot be used to map a private network's names.

**On your side:** verify every request, enforce the timestamp tolerance,
deduplicate on `Webhook-Id`, and serve the endpoint over TLS with a
certificate from a public authority.

## For operators

Webhooks are on by default. The worker runs inside `sbxd`; several servers on
one database share the work and never deliver to the same webhook at the same
time (each claims webhooks with `FOR UPDATE SKIP LOCKED` and a lease).

| Flag (`sbxd serve`) | Default | |
|---|---|---|
| `--webhooks` | `true` | `false` stops the worker; the routes answer `501`. |
| `--webhook-allow-http` | `false` | Accept `http://` URLs. Development only. |
| `--webhook-allow-loopback` | `false` | Accept URLs on the server's loopback interface, for a receiver on your own machine. Never on a shared server: it lets any tenant reach whatever listens on `127.0.0.1`. |
| `--webhook-permit-private-cidrs` | none | Private ranges webhooks may be delivered into (on-premises receivers), e.g. `10.20.0.0/16`. Must lie inside RFC 1918, CGNAT or ULA space; metadata and the host's own addresses stay refused. Independent of `--egress-permit-private-cidrs`. |
| `--webhook-max-per-tenant` | `10` | Negative for no limit. |
| `--webhook-timeout` | `10s` | One attempt, DNS lookup to response. |
| `--webhook-concurrency` | `8` | Webhooks delivered to at once. One webhook uses one slot at most, and gives it up after 30 seconds if it still has a backlog, so a slow endpoint cannot starve the others. |
| `--webhook-poll-interval` | `1s` | |
| `--webhook-retry-base`, `--webhook-retry-max` | `5s`, `15m` | The backoff. |
| `--webhook-retention` | `24h` | Age at which an undelivered event is dropped. Negative: never. |
| `--webhook-disable-after` | `72h` | Negative: never auto-disable. |

Each flag has an environment variable (`SBX_WEBHOOK_TIMEOUT` and so on); see
`sbxd serve -h`.

```sh
sbxd admin webhook list [--tenant acme]
sbxd admin webhook create --tenant acme --url https://hooks.example.com/warmbox --events sandbox.terminated
sbxd admin webhook delete --id wh_5f1c0a9e7b3d4c2a8e6f0b1d
```

`webhook create` prints the secret once and applies the same URL rules as the
API; `--allow-http`, `--allow-loopback` and `--permit-private-cidrs` relax them
for that one command (the server needs the matching flag to deliver).

**Metrics** (`GET /metrics`):

| Metric | |
|---|---|
| `sbx_webhook_deliveries_total{outcome}` | Attempts: `succeeded`, `failed`. |
| `sbx_webhook_delivery_failures_total{code}` | Failed attempts by error code. |
| `sbx_webhook_delivery_duration_seconds` | Histogram of attempt durations. |
| `sbx_webhook_dropped_events_total{reason}` | `retention_expired`, `rejected`. |
| `sbx_webhook_auto_disabled_total{reason}` | `failing`, `endpoint_gone`. |
| `sbx_webhook_backlog_events` | Events after the cursors of the enabled webhooks. It counts events the subscription will filter out too, and each webhook up to 10000. |
| `sbx_webhook_lag_seconds` | Age of the oldest such event. The one to alert on. |
| `sbx_webhooks{state}` | `enabled`, `disabled`, `failing`. |
| `sbx_webhook_in_flight`, `sbx_webhook_lease_lost_total`, `sbx_webhook_test_events_total{outcome}`, `sbx_webhook_last_poll_timestamp_seconds`, `sbx_webhook_stats_timestamp_seconds` | |

The counters are per process; the backlog, lag and state gauges describe the
whole database and are refreshed every 15 seconds.

**How it is stored.** There is no outbox table: the `events` table, which
every state change already writes in its own transaction, is the outbox. A
webhook row holds a cursor into it. Migration `0003_webhooks` adds a `txid`
column to `events` (filled by a column default, so no code that writes events
changed) because the cursor is a position in `(txid, id)` order and events are
only read below the oldest running transaction; a cursor on the id alone could
skip an event whose transaction committed late. This needs Postgres 13 or
newer.

Two consequences worth knowing:

- A write transaction left open on the same Postgres cluster, in any
  database, holds deliveries back until it ends. `sbx_webhook_lag_seconds`
  shows it.
- After restoring the database from a logical dump (`pg_dump`), the
  transaction counter starts again while the rows keep their old values. The
  worker notices cursors that point past the counter and moves them to the
  present, logging a warning; events between the dump and that moment are not
  delivered. `pg_upgrade` and physical backups keep the counter and are not
  affected.

During a graceful stop the server spends up to five seconds delivering the
events of the shutdown itself (`sandbox.terminated` with reason `host_drain`)
and lets requests in flight finish within `--shutdown-timeout`. Whatever is
left is delivered by the next start.

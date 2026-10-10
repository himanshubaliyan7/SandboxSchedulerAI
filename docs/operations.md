# Operating notes

- **Host id.** Every container carries an `sbx.host_id` label and a server only
  ever touches containers and database rows with its own id; containers of another
  id are left alone, and `sbx.managed` containers with no id (made by an older
  version) are left alone with a warning. The default id is
  `<hostname>-u<uid>-<hash of the run dir>`; set `--host-id` / `SBX_HOST_ID` to pin
  it. It must stay the same across restarts, and each server needs its own
  `--run-dir` (a second server on a run dir in use refuses to start). Several
  servers can then share one Podman user.
- **Density ceiling.** Each running container holds one kernel key of the user's
  `kernel.keys.maxkeys` quota (200 by default), so about 190 sandboxes per user
  before creates fail with `Disk quota exceeded`. The Libpod API has no
  per-container switch; set `keyring = false` under `[containers]` in
  `~/.config/containers/containers.conf` and restart the Podman service, or raise
  the sysctl as root. `sbxd preflight` and server start-up report the real ceiling
  against the templates' `max_total`, and the same for inotify instances.
- **Metrics.** `/metrics` is unauthenticated on the API listener, which is only
  allowed while `--listen` is a loopback address: otherwise `sbxd serve` refuses to
  start unless `--metrics-token` (bearer token) or `--metrics-listen` (separate
  listener, route removed from the API listener) is set.
- **Delete.** `DELETE /v1/sandboxes/{id}` answers once the sandbox is fenced and
  destroys the container in the background; `?wait=true` waits for it.
- **Shutdown.** On SIGTERM or SIGINT `sbxd serve` closes the API (open streams and
  WebSockets end with `shutting_down`), then stops the refill loop and waits for the
  creates in flight, and only then touches a sandbox, so a stop never starts a
  container or leaves one half made. What remains is decided by `--shutdown-policy`:

  | Policy | Tenant sandboxes | Warm sandboxes | Containers of this host id after the stop |
  |---|---|---|---|
  | `drain` (default) | released, reason `host_drain` | **left running** | exactly the warm pool |
  | `leave` | left running, unreachable | left running | warm and tenant sandboxes |
  | `destroy` | destroyed, reason `host_drain` | destroyed | none |

  Leaving the warm pool is deliberate: a warm sandbox holds no tenant data, and the
  next start re-adopts it after one readiness probe, so a restart does not pay for a
  cold pool. It also means that after a default stop `podman ps` still shows
  `warm-target` containers per template. They are not leaked: they are labelled with
  the host id, recorded in the database, and adopted or reaped by the next start.
  With `leave`, the next start recycles the tenant sandboxes (`host_drain`). To stop a
  server for good use `--shutdown-policy destroy`, or afterwards
  `sbxd admin host destroy --run-dir <its run dir>` (add `--host-id` if it was set),
  which removes the containers, run dirs and live sandbox rows of a host id without
  starting the server and refuses to run while that server is up. Neither ever touches
  a container of another host id. A second signal during the stop kills the process.
- **Browsers (CORS).** Off by default. `--cors-allowed-origins https://app.example.com`
  (comma-separated exact origins, or `*`) lets pages on those origins call the
  data-plane routes (commands, files, the PTY WebSocket) with a sandbox access token.
  The control plane never takes part, and a tenant API key is refused from a browser
  origin (`403 api_key_not_allowed`) unless `--cors-allow-api-key` is also given. With
  the flag set, a request or WebSocket handshake from any other origin is refused.
  Details are in `api/openapi.yaml` under "Browsers and CORS".
- **Retrying a command start.** A start that carries `Idempotency-Key` runs at most
  once per sandbox and key; a repeat answers for the first command. See "Not knowing
  whether a command started" in `api/openapi.yaml`.
- **`oom` file.** When a sandbox is OOM-killed, conmon creates an empty file named
  `oom` in the working directory of the Podman service (the user's home for the
  systemd-activated service). Podman 4.9 has no option for it; a systemd drop-in
  with `WorkingDirectory=` for `podman.service` moves it.

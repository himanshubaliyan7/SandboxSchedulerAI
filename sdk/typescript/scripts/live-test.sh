#!/usr/bin/env bash
# Runs the live integration tests against a real sbxd built from this
# repository, following the README quick start.
#
#   scripts/live-test.sh            # build, start Postgres + sbxd, test, clean up
#   scripts/live-test.sh --keep-pg  # leave the Postgres container running afterwards
#
# Two sbxd servers of one user must not run at the same time (they would
# reconcile each other's containers away), so everything that touches Podman
# runs under an exclusive flock, and the slow steps (compiling, starting
# Postgres) run before the lock is taken.
#
# Environment (all optional):
#   SBX_PODMAN_LOCK   lock file shared by everyone using this Podman user
#   SBX_LIVE_PORT     API port               (default 18082)
#   SBX_LIVE_PG_PORT  Postgres host port     (default 55435)
#   SBX_LIVE_IMAGE    sandbox image, already pulled (default docker.io/library/python:3.12-alpine)
#   GO                go binary              (default: go on PATH, else ~/.local/opt/go/bin/go)
set -euo pipefail

SDK_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
REPO_DIR=$(cd "$SDK_DIR/../.." && pwd)
LOCK=${SBX_PODMAN_LOCK:-/tmp/claude-1000/-home-user-warmbox/00dddaa2-61cf-4bfd-a377-ebe4c5ac26b7/scratchpad/podman.lock}
PORT=${SBX_LIVE_PORT:-18082}
PG_PORT=${SBX_LIVE_PG_PORT:-55435}
PG_NAME=sbx-ts-pg
IMAGE=${SBX_LIVE_IMAGE:-docker.io/library/python:3.12-alpine}
HOST_ID=sbxts
WORK=${SBX_LIVE_WORK:-$SDK_DIR/.live}
RUN_DIR=${XDG_RUNTIME_DIR:-/tmp}/sbxts
export SBX_DATABASE_URL="postgres://postgres:sbx@127.0.0.1:$PG_PORT/postgres?sslmode=disable"

log() { printf '\n== %s\n' "$*" >&2; }

# ---------------------------------------------------------------------------
# The part that runs while holding the lock: keep it short.
# ---------------------------------------------------------------------------
locked() {
  local bin=$WORK/bin server_pid="" status=0
  local started=$SECONDS

  stop_server() {
    if [[ -n $server_pid ]] && kill -0 "$server_pid" 2>/dev/null; then
      log "stopping sbxd (SIGTERM)"
      kill -TERM "$server_pid"
      for _ in $(seq 1 450); do kill -0 "$server_pid" 2>/dev/null || break; sleep 0.1; done
      if kill -0 "$server_pid" 2>/dev/null; then
        echo "sbxd did not stop within 45 s; killing it" >&2
        kill -KILL "$server_pid" || true
      fi
      wait "$server_pid" 2>/dev/null || true
    fi
    server_pid=""
    # Only containers labelled with this script's host id are ever touched.
    local left
    left=$(podman ps -a -q --filter "label=sbx.host_id=$HOST_ID")
    if [[ -n $left ]]; then
      echo "removing $(wc -w <<<"$left") leftover container(s) of host id $HOST_ID" >&2
      # shellcheck disable=SC2086
      podman rm -f -t 0 $left >/dev/null || true
    fi
    left=$(podman ps -a -q --filter "label=sbx.host_id=$HOST_ID")
    if [[ -n $left ]]; then
      echo "CLEANUP FAILED: containers of host id $HOST_ID remain: $left" >&2
      status=1
    else
      echo "cleanup: no containers of host id $HOST_ID remain" >&2
    fi
    rm -rf "$RUN_DIR"
  }
  trap stop_server EXIT

  log "preparing tenants, keys and the template"
  mkdir -p "$RUN_DIR"
  "$bin/sbxd" admin tenant create --id acme --max-concurrent 20 --rate 200 --burst 400 >/dev/null
  "$bin/sbxd" admin tenant create --id other --max-concurrent 5 --rate 50 --burst 100 >/dev/null
  local key other
  key=$("$bin/sbxd" admin key create --tenant acme --json | node -e 'process.stdout.write(JSON.parse(require("fs").readFileSync(0,"utf8")).key)')
  other=$("$bin/sbxd" admin key create --tenant other --json | node -e 'process.stdout.write(JSON.parse(require("fs").readFileSync(0,"utf8")).key)')
  "$bin/sbxd" admin template apply --id python --image "$IMAGE" \
    --memory 256m --workdir-size 64m --tmp-size 32m --network allowlist --warm-target 3 >/dev/null

  log "starting sbxd on 127.0.0.1:$PORT"
  "$bin/sbxd" serve --listen "127.0.0.1:$PORT" --run-dir "$RUN_DIR" --host-id "$HOST_ID" \
    --claim-wait 20s >"$WORK/sbxd.log" 2>&1 &
  server_pid=$!
  local ready=""
  for _ in $(seq 1 300); do
    if ! kill -0 "$server_pid" 2>/dev/null; then break; fi
    if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/readyz" 2>/dev/null; then ready=1; break; fi
    sleep 0.2
  done
  if [[ -z $ready ]]; then
    echo "sbxd did not become ready; log tail:" >&2
    tail -n 40 "$WORK/sbxd.log" >&2 || true
    return 1
  fi

  log "running the live tests"
  # The keys reach the tests through the environment and are never printed.
  (
    cd "$SDK_DIR"
    SBX_LIVE_BASE_URL="http://127.0.0.1:$PORT" SBX_LIVE_API_KEY="$key" SBX_LIVE_OTHER_API_KEY="$other" \
      npx vitest run test/live --reporter=verbose
  ) || status=$?

  if [[ $status -ne 0 ]]; then
    echo "live tests failed; sbxd log tail:" >&2
    tail -n 30 "$WORK/sbxd.log" >&2 || true
  fi
  trap - EXIT
  stop_server
  log "lock held for $((SECONDS - started)) s"
  return "$status"
}

if [[ ${1:-} == --locked ]]; then
  locked
  exit $?
fi

KEEP_PG=""
[[ ${1:-} == --keep-pg ]] && KEEP_PG=1

# ---------------------------------------------------------------------------
# Slow preparation, without the lock.
# ---------------------------------------------------------------------------
GO=${GO:-$(command -v go || echo "$HOME/.local/opt/go/bin/go")}
mkdir -p "$WORK/bin"

log "building sbxd and sbx-agent into $WORK/bin"
(
  cd "$REPO_DIR"
  "$GO" build -trimpath -o "$WORK/bin/sbxd" ./cmd/sbxd
  # The agent must be static: it is bind-mounted into the sandbox image.
  CGO_ENABLED=0 "$GO" build -trimpath -ldflags '-s -w' -o "$WORK/bin/sbx-agent" ./cmd/sbx-agent
)

[[ -d $SDK_DIR/node_modules ]] || (cd "$SDK_DIR" && npm ci)

log "starting a fresh Postgres ($PG_NAME on 127.0.0.1:$PG_PORT)"
docker rm -f "$PG_NAME" >/dev/null 2>&1 || true
docker run -d --rm --name "$PG_NAME" -e POSTGRES_PASSWORD=sbx -p "127.0.0.1:$PG_PORT:5432" postgres:17-alpine >/dev/null
cleanup_pg() {
  if [[ -z $KEEP_PG ]]; then
    docker rm -f "$PG_NAME" >/dev/null 2>&1 || true
    echo "cleanup: removed Postgres container $PG_NAME" >&2
  fi
}
trap cleanup_pg EXIT
for _ in $(seq 1 150); do
  # Postgres restarts once during initialisation: wait for the real one, on TCP.
  if docker exec "$PG_NAME" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 &&
    docker exec "$PG_NAME" psql -h 127.0.0.1 -U postgres -c 'select 1' >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

log "waiting for the Podman lock ($LOCK)"
status=0
flock "$LOCK" "$0" --locked || status=$?
exit "$status"

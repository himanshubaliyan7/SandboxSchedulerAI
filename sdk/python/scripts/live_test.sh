#!/usr/bin/env bash
# Run the live integration tests against a real sbxd built from this repository.
#
# What it does, in order:
#   1. builds sbxd and the static sbx-agent into a temporary directory;
#   2. makes sure a private Postgres is running (container "sbx-py-pg", port 55434);
#   3. takes the Podman lock and, while holding it: creates two tenants,
#      two API keys and a "python" template, starts the server with
#      --shutdown-policy destroy, runs `pytest -m live`, stops the server with
#      SIGTERM and fails if any of its containers remain.
#
# The lock only keeps two runs of this script apart (they share a host id,
# a run dir and a port). Servers are scoped by host id, so nothing else
# needs it.
#
# The lock is held only for step 3. Extra arguments go to pytest, e.g.
#   scripts/live_test.sh -k pty -x
#
# Environment:
#   SBX_PODMAN_LOCK   lock file (default $XDG_RUNTIME_DIR/sbx-podman.lock, else /tmp/sbx-podman.lock)
#   SBX_LIVE_LISTEN   server address            (default 127.0.0.1:18081)
#   SBX_LIVE_PG_PORT  port of the Postgres      (default 55434)
#   SBX_LIVE_KEEP_PG  set to 1 to leave the Postgres container running
#   SBX_LIVE_EXAMPLES set to 1 to also run the scripts in examples/ against the server
#   SBX_LIVE_LOG_COPY write the server log (credentials redacted) to this file
#   GO                go binary                 (default: go from PATH or ~/.local/opt/go/bin)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
LOCK="${SBX_PODMAN_LOCK:-${XDG_RUNTIME_DIR:-/tmp}/sbx-podman.lock}"
LISTEN="${SBX_LIVE_LISTEN:-127.0.0.1:18081}"
PG_PORT="${SBX_LIVE_PG_PORT:-55434}"
PG_NAME="sbx-py-pg"
HOST_ID="sbxpy"
IMAGE="docker.io/library/python:3.12-alpine"
PYTHON="${PYTHON:-$HERE/.venv/bin/python}"
export PATH="$HOME/.local/opt/go/bin:$PATH"
GO="${GO:-go}"

if [[ "${1:-}" != "--locked" ]]; then
    WORK="$(mktemp -d "${TMPDIR:-/tmp}/sbxpy-live.XXXXXX")"
    trap 'rm -rf "$WORK"' EXIT
    echo "== building sbxd and sbx-agent into $WORK"
    (cd "$REPO" && CGO_ENABLED=0 "$GO" build -trimpath -ldflags '-s -w' -o "$WORK/sbx-agent" ./cmd/sbx-agent)
    (cd "$REPO" && "$GO" build -trimpath -o "$WORK/sbxd" ./cmd/sbxd)

    if ! docker ps --format '{{.Names}}' | grep -qx "$PG_NAME"; then
        echo "== starting Postgres ($PG_NAME on 127.0.0.1:$PG_PORT)"
        docker run -d --rm --name "$PG_NAME" -e POSTGRES_PASSWORD=sbx -p "127.0.0.1:$PG_PORT:5432" postgres:17-alpine >/dev/null
    fi
    for _ in $(seq 1 60); do
        docker exec "$PG_NAME" pg_isready -q -U postgres 2>/dev/null && break
        sleep 0.5
    done

    echo "== waiting for the Podman lock ($LOCK)"
    status=0
    SBX_LIVE_WORK="$WORK" flock "$LOCK" timeout --signal=TERM --kill-after=30 600 "$0" --locked "$@" || status=$?
    if [[ "${SBX_LIVE_KEEP_PG:-0}" != "1" ]]; then
        docker rm -f "$PG_NAME" >/dev/null 2>&1 || true
        echo "== removed Postgres container $PG_NAME"
    fi
    exit "$status"
fi

# ---- everything below runs while the Podman lock is held ----------------------
shift
WORK="$SBX_LIVE_WORK"
export SBX_DATABASE_URL="postgres://postgres:sbx@127.0.0.1:$PG_PORT/postgres?sslmode=disable"
export SBX_RUN_DIR="${XDG_RUNTIME_DIR:-/tmp}/sbxpy"
export SBX_HOST_ID="$HOST_ID"
SBXD="$WORK/sbxd"
SERVER_PID=""
LEFTOVER=0

mine() { podman ps -a --filter "label=sbx.host_id=$HOST_ID" --format '{{.ID}}'; }

cleanup() {
    if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
        echo "== stopping sbxd (SIGTERM)"
        kill -TERM "$SERVER_PID"
        for _ in $(seq 1 90); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.5; done
        kill -KILL "$SERVER_PID" 2>/dev/null || true
    fi
    # The server runs with --shutdown-policy destroy, so a graceful stop
    # leaves nothing of this host id. Anything that is left is a bug: it is
    # reported (and fails the run), then removed; the label makes sure only
    # this script's containers are touched.
    left="$(mine)"
    if [[ -n "$left" ]]; then
        LEFTOVER="$(echo "$left" | wc -l)"
        echo "== ERROR: $LEFTOVER container(s) left after a stop with --shutdown-policy destroy; removing them"
        # shellcheck disable=SC2086
        podman rm -f -t 0 $left >/dev/null
    fi
    echo "== containers with sbx.host_id=$HOST_ID after cleanup: $(mine | wc -l)"
    rm -rf "$SBX_RUN_DIR"
}
trap cleanup EXIT

key() { "$SBXD" admin key create --tenant "$1" --json | "$PYTHON" -c 'import json,sys; print(json.load(sys.stdin)["key"])'; }

mkdir -p "$SBX_RUN_DIR"
"$SBXD" preflight >"$WORK/preflight.log" 2>&1 || { cat "$WORK/preflight.log"; echo "preflight failed"; exit 1; }
"$SBXD" admin tenant create --id sbxpy-a --max-concurrent 20 --rate 200 --burst 400 >/dev/null 2>&1 || true
"$SBXD" admin tenant create --id sbxpy-b --max-concurrent 5 --rate 200 --burst 400 >/dev/null 2>&1 || true
KEY_A="$(key sbxpy-a)"
KEY_B="$(key sbxpy-b)"
"$SBXD" admin template apply --id python --image "$IMAGE" --memory 256m --workdir-size 64m --tmp-size 32m \
    --network allowlist --warm-target 3 >/dev/null

echo "== starting sbxd on $LISTEN (host id $HOST_ID, run dir $SBX_RUN_DIR)"
"$SBXD" serve --listen "$LISTEN" --agent-binary "$WORK/sbx-agent" --shutdown-policy destroy \
    --cors-allowed-origins "http://app.sbx.test" >"$WORK/sbxd.log" 2>&1 &
SERVER_PID=$!
ready=0
for _ in $(seq 1 120); do
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then break; fi
    if curl -fsS "http://$LISTEN/readyz" >/dev/null 2>&1; then ready=1; break; fi
    sleep 0.25
done
if [[ "$ready" != 1 ]]; then
    echo "sbxd did not become ready:"; tail -n 40 "$WORK/sbxd.log"; exit 1
fi

status=0
(cd "$HERE" && SBX_LIVE_BASE_URL="http://$LISTEN" SBX_LIVE_API_KEY="$KEY_A" SBX_LIVE_OTHER_API_KEY="$KEY_B" \
    SBX_LIVE_TEMPLATE=python "$PYTHON" -m pytest -m live tests/live -v -rs -p no:cacheprovider "$@") || status=$?
if [[ "$status" == 0 && "${SBX_LIVE_EXAMPLES:-0}" == "1" ]]; then
    export SBX_BASE_URL="http://$LISTEN" SBX_API_KEY="$KEY_A" SBX_TEMPLATE=python
    echo "== examples/run_code.py"
    "$PYTHON" "$HERE/examples/run_code.py" || status=$?
    echo "== examples/process_file.py"
    "$PYTHON" "$HERE/examples/process_file.py" || status=$?
    echo "== examples/interactive_pty.py (scripted input)"
    printf 'echo "cols x rows: $(stty size)"\nexit 3\n' | "$PYTHON" "$HERE/examples/interactive_pty.py" || [[ $? == 3 ]] || status=1
    unset SBX_BASE_URL SBX_API_KEY SBX_TEMPLATE
fi
if [[ "$status" != 0 ]]; then
    echo "== last lines of the server log"; tail -n 40 "$WORK/sbxd.log" | sed -E 's/sb[xt]_[A-Za-z0-9_]+/<redacted>/g'
fi
if [[ -n "${SBX_LIVE_LOG_COPY:-}" ]]; then
    sed -E 's/sb[xt]_[A-Za-z0-9_]+/<redacted>/g' "$WORK/sbxd.log" >"$SBX_LIVE_LOG_COPY" || true
fi
# Stop the server now, so that what it leaves behind counts towards the result.
trap - EXIT
cleanup
if [[ "$LEFTOVER" != 0 ]]; then status=1; fi
exit "$status"

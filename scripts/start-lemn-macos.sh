#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_DIR="${LEMN_RUN_DIR:-$ROOT_DIR/.lemn-run}"
LOG_DIR="$RUN_DIR/logs"
PID_DIR="$RUN_DIR/pids"
VENV_DIR="$ROOT_DIR/layarouter/.venv"

mkdir -p "$LOG_DIR" "$PID_DIR"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

http_up() {
  local url="$1"
  curl -fsS -o /dev/null "$url" >/dev/null 2>&1
}

wait_for_http() {
  local name="$1"
  local url="$2"
  local attempts="${3:-60}"

  for ((i = 1; i <= attempts; i++)); do
    if http_up "$url"; then
      echo "$name is up at $url"
      return 0
    fi
    sleep 1
  done

  echo "Timed out waiting for $name at $url" >&2
  exit 1
}

start_process() {
  local name="$1"
  local command="$2"
  local pid_file="$PID_DIR/$name.pid"
  local log_file="$LOG_DIR/$name.log"

  if [[ -f "$pid_file" ]]; then
    local existing_pid
    existing_pid="$(cat "$pid_file")"
    if kill -0 "$existing_pid" >/dev/null 2>&1; then
      echo "$name already running with PID $existing_pid"
      return 0
    fi
    rm -f "$pid_file"
  fi

  echo "Starting $name"
  (
    cd "$ROOT_DIR"
    exec bash -lc "$command"
  ) >>"$log_file" 2>&1 &
  echo $! > "$pid_file"
}

write_env_file() {
  cat > "$RUN_DIR/lemn.env" <<EOF
export LEMN_SHARED_SECRET='$LEMN_SHARED_SECRET'
export LEMN_LAYA_URL='$LEMN_LAYA_URL'
export LEMN_LAYA_MEMORY_URL='$LEMN_LAYA_MEMORY_URL'
export LAYA_MODEL_REPO='$LAYA_MODEL_REPO'
export USE_TF='$USE_TF'
export LEMN_POSTGRES_DSN='${LEMN_POSTGRES_DSN:-}'
EOF
}

build_go_binaries() {
  echo "Building Go binaries"
  (
    cd "$ROOT_DIR/lemnd"
    go build -o bin/daemon  ./cmd/daemon
    go build -o bin/router  ./cmd/router
    go build -o bin/lemn    ./cmd/lemn
    go build -o bin/labeler ./cmd/labeler
    go build -o bin/export  ./cmd/export
  )
}

prepare_python_env() {
  echo "Preparing Python environment"
  if [[ ! -x "$VENV_DIR/bin/python3" ]]; then
    python3 -m venv "$VENV_DIR"
    "$VENV_DIR/bin/pip" install -r "$ROOT_DIR/layarouter/requirements.txt"
    return
  fi

  if [[ "${LEMN_PIP_SYNC:-0}" == "1" ]]; then
    "$VENV_DIR/bin/pip" install -r "$ROOT_DIR/layarouter/requirements.txt"
  fi
}

require_cmd bash
require_cmd curl
require_cmd go
require_cmd openssl
require_cmd python3

export LEMN_SHARED_SECRET="${LEMN_SHARED_SECRET:-$(openssl rand -hex 32)}"
export LAYA_MODEL_REPO="${LAYA_MODEL_REPO:-convaiinnovations/laya-typed-decisions}"
export LEMN_LAYA_URL="${LEMN_LAYA_URL:-http://127.0.0.1:8002/classify}"
export LEMN_LAYA_MEMORY_URL="${LEMN_LAYA_MEMORY_URL:-http://127.0.0.1:8002/memory-worthiness}"
export USE_TF="${USE_TF:-0}"

if [[ "${LEMN_SKIP_BUILD:-0}" != "1" ]]; then
  build_go_binaries
fi

prepare_python_env
write_env_file

start_process laya-service "source '$VENV_DIR/bin/activate' && cd '$ROOT_DIR/layarouter' && uvicorn server:app --host 127.0.0.1 --port 8002"
start_process router "cd '$ROOT_DIR/lemnd' && ./bin/router"
start_process daemon "cd '$ROOT_DIR/lemnd' && ./bin/daemon"

wait_for_http laya-service "http://127.0.0.1:8002/healthz"
wait_for_http router "http://127.0.0.1:8090/healthz"
wait_for_http daemon "http://127.0.0.1:8080/healthz"

cat <<EOF

LEMN is up.

Shared secret:
  $LEMN_SHARED_SECRET

Runtime env file:
  $RUN_DIR/lemn.env

Logs:
  $LOG_DIR

Stop everything with:
  $ROOT_DIR/scripts/stop-lemn-macos.sh

Model backends should already be running in your UI.
EOF
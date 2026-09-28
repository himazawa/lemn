#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_DIR="${LEMN_RUN_DIR:-$ROOT_DIR/.lemn-run}"
PID_DIR="$RUN_DIR/pids"

if [[ ! -d "$PID_DIR" ]]; then
  echo "No PID directory found at $PID_DIR"
  exit 0
fi

for pid_file in "$PID_DIR"/*.pid; do
  [[ -e "$pid_file" ]] || continue
  name="$(basename "$pid_file" .pid)"
  pid="$(cat "$pid_file")"

  if kill -0 "$pid" >/dev/null 2>&1; then
    echo "Stopping $name ($pid)"
    kill "$pid" >/dev/null 2>&1 || true
  else
    echo "$name already stopped"
  fi

  rm -f "$pid_file"
done

echo "All tracked LEMN processes have been signaled to stop."
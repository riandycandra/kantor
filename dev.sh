#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKEND_PID=""
FRONTEND_PID=""

cleanup() {
  local exit_code=$?
  trap - EXIT INT TERM

  if [[ -n "$FRONTEND_PID" ]] && kill -0 "$FRONTEND_PID" 2>/dev/null; then
    kill "$FRONTEND_PID" 2>/dev/null || true
  fi
  if [[ -n "$BACKEND_PID" ]] && kill -0 "$BACKEND_PID" 2>/dev/null; then
    kill "$BACKEND_PID" 2>/dev/null || true
  fi

  wait 2>/dev/null || true
  exit "$exit_code"
}

trap cleanup EXIT INT TERM

for cmd in go npm; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Error: '$cmd' is required but not installed or not in PATH." >&2
    exit 1
  fi
done

if [[ ! -d "$ROOT_DIR/frontend/node_modules" ]]; then
  echo "Installing frontend dependencies..."
  (cd "$ROOT_DIR/frontend" && npm install)
fi

if [[ ! -f "$ROOT_DIR/frontend/.env" && -z "${VITE_API_BASE_URL:-}" ]]; then
  export VITE_API_BASE_URL="http://localhost:8080/api/v1"
fi

echo "Starting backend (go run ./cmd/server)..."
(cd "$ROOT_DIR/backend" && go run ./cmd/server) &
BACKEND_PID=$!

echo "Starting frontend (npm run dev)..."
(cd "$ROOT_DIR/frontend" && npm run dev) &
FRONTEND_PID=$!

echo "Dev services running. Press Ctrl+C to stop both."
wait "$BACKEND_PID" "$FRONTEND_PID"


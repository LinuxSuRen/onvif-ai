#!/bin/bash
set -e

cd "$(dirname "$0")"

# Load .env if exists
if [ -f .env ]; then
    export $(grep -v '^#' .env | grep -v '^$' | xargs)
fi

cleanup() {
    echo ""
    echo "Shutting down..."
    kill $GO_PID $VUE_PID 2>/dev/null
    wait $GO_PID $VUE_PID 2>/dev/null
    exit 0
}
trap cleanup SIGINT SIGTERM

echo "=== Starting ONVIF AI ==="

# Start Go backend
echo "[1/2] Starting Go backend on :${PORT:-8080}..."
go run ./cmd/server &
GO_PID=$!

# Wait briefly for backend to start
sleep 1

# Start Vue frontend
echo "[2/2] Starting Vue frontend on :5173..."
cd web && npm run dev &
VUE_PID=$!

echo ""
echo "=================================="
echo "  Backend:  http://localhost:${PORT:-8080}"
echo "  Frontend: http://localhost:5173"
echo "  Press Ctrl+C to stop"
echo "=================================="

wait

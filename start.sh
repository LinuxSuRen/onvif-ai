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
    kill $GO_PID 2>/dev/null
    wait $GO_PID 2>/dev/null
    exit 0
}
trap cleanup SIGINT SIGTERM

echo "=== Starting ONVIF AI ==="

# 前端已嵌入后端二进制（go:embed），无需单独启动；
# 仅前端开发时另开终端执行 make web-dev（:5173，带热更新）
echo "Starting server on :${PORT:-8080}..."
go run ./cmd/server &
GO_PID=$!

echo ""
echo "=================================="
echo "  Open: http://localhost:${PORT:-8080}"
echo "  Press Ctrl+C to stop"
echo "=================================="

wait

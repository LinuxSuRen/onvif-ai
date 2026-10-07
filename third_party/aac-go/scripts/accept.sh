#!/usr/bin/env bash
# Acceptance gate.
# Runs the hermetic test suite, then verifies both directions against an
# external reference decoder: our encoder's streams must be accepted and
# meet SNR gates, and committed real-world vectors must decode to match
# the reference decoder's output.
#
# Oracle (test oracle: an independent reference decoder used to judge
# correctness): ffmpeg (any platform / CI, default) or afconvert (macOS
# cross-check), auto-detected. Both are external CLI tools used only for verification —
# never dependencies of the library.
# Override with ORACLE=afconvert scripts/accept.sh
# Usage: scripts/accept.sh [case1,case2|all]
set -euo pipefail
cd "$(dirname "$0")/.."

CASES="${1:-all}"
ORACLE="${ORACLE:-auto}"

echo "== go vet =="
go vet ./...

echo "== go test (hermetic) =="
go test ./...

echo "== encode acceptance (oracle) =="
go run -tags accept ./scripts/accept -oracle "$ORACLE" -cases "$CASES" -out testdata/accept

echo "== real-world decode acceptance (oracle) =="
go run -tags accept ./scripts/accept -oracle "$ORACLE" -realworld testdata/realworld

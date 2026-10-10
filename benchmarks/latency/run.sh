#!/usr/bin/env bash
# Reproduce the recall and write latency benchmark (see REPORT.md).
#
#   benchmarks/latency/run.sh                  # full run: N = 1k, 10k, 50k
#   benchmarks/latency/run.sh --sizes 1000     # one size, for a quick check
#
# Builds the grimoire binary from this worktree, checks the Python token
# estimator against the Go source, then drives a real server over HTTP and
# writes results.json and REPORT.md next to this script. Scratch vaults and
# logs go to benchmarks/latency/.work (git-ignored).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(git -C "$HERE" rev-parse --show-toplevel)"
export PATH="$PATH:/usr/local/go/bin"
WORK="${LATENCY_WORK:-$HERE/.work}"
mkdir -p "$WORK"
go -C "$REPO/go" build -o "$WORK/grimoire" ./cmd/grimoire
python3 "$HERE/check_tokens.py" "$WORK"
python3 "$HERE/bench.py" --binary "$WORK/grimoire" --work "$WORK" --out "$HERE" "$@"

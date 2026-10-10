#!/bin/bash
# Run a harness command while holding the shared AMD GPU research lease, the same way
# /home/admin/homelab-api/eval/capability/servers/run-under-lease.sh does for its evals
# (that script is hardwired to run.py, so this reuses its lease mechanism for our commands).
# usage: run_under_lease.sh <command...>
set -u
LEASE=/mnt/bulk/inference-research/floor/research-lease.sh
LOCK=/mnt/bulk/inference-research/floor/.machine-leases/host-lxc105.lock
until flock --nonblock "$LOCK" true; do echo "waiting for GPU lease"; sleep 60; done
exec "$LEASE" "$@"

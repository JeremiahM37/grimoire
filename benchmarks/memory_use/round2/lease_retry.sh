#!/bin/bash
# usage: lease_retry.sh script.py -- reruns (resumable scripts) until exit 0, waiting on the lease
LEASE=/mnt/bulk/inference-research/floor/research-lease.sh
cd "$(dirname "$0")"
until $LEASE python3 "$@"; do echo "rc=$? retry in 30s"; sleep 30; done

#!/bin/bash
# usage: lease.sh script.py args...  -- waits for the shared GPU lease, then runs it.
LEASE=/mnt/bulk/inference-research/floor/research-lease.sh
cd "$(dirname "$0")"
while true; do
  $LEASE python3 "$@"; rc=$?
  [ $rc -ne 75 ] && exit $rc
  echo "lease busy, waiting"; sleep 60
done

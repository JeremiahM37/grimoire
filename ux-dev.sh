#!/bin/bash
# UX dev loop: rebuild frontend (+go if asked) and (re)start a scratch server on :$PORT
set -e
PORT=${UX_PORT:-$PORT}
VAULT=/mnt/bulk/grimoire-ux-vault-$PORT
[ -d $VAULT ] || rsync -a --exclude .grimoire /mnt/bulk/grimoire-ux-vault/ $VAULT/
cd "$(dirname "$0")"
export PATH=$PATH:/usr/local/go/bin
(cd frontend && npm run build >/tmp/ux-build-$PORT.log 2>&1) || { tail -30 /tmp/ux-build-$PORT.log; exit 1; }
[ "$1" = go ] && (cd go && go build -o grimoire ./cmd/grimoire)
[ -f /tmp/ux-server-$PORT.pid ] && kill $(cat /tmp/ux-server-$PORT.pid) 2>/dev/null || true
fuser -k $PORT/tcp 2>/dev/null || true
sleep 0.5
GRIMOIRE_VAULT=$VAULT GRIMOIRE_PORT=$PORT GRIMOIRE_HOST=127.0.0.1 GRIMOIRE_NO_WATCHER=1 GRIMOIRE_WEB_DIR=./web nohup ./go/grimoire >/tmp/ux-server-$PORT.log 2>&1 &
echo $! >/tmp/ux-server-$PORT.pid
for i in $(seq 30); do curl -sf http://127.0.0.1:$PORT/api/health >/dev/null && echo "ready http://127.0.0.1:$PORT" && exit 0; sleep 0.5; done
tail -20 /tmp/ux-server-$PORT.log; exit 1

#!/usr/bin/env bash
# dev.sh — source-mode start/stop helper (the docker compose equivalent).
# Usage: ./dev.sh {start|stop|restart|status|logs|key}
set -uo pipefail
cd "$(dirname "$0")"

BIN=./chatglm2api
LOG=./server.log
PIDFILE=./.server.pid

alive() { [[ -f $PIDFILE ]] && kill -0 "$(cat $PIDFILE)" 2>/dev/null; }

case "${1:-status}" in
  start)
    if alive; then echo "already running (PID $(cat $PIDFILE))"; exit 0; fi
    if [[ ! -x $BIN ]] || find . \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$BIN" -print -quit | grep -q .; then
        if ! command -v go >/dev/null 2>&1; then
            echo "go is required to build $BIN (use docker compose for container deployments)" >&2
            exit 1
        fi
        echo "build $BIN ..."
        CGO_ENABLED=0 go build -o "$BIN" ./cmd/chatglm2api || {
            echo "build failed; not starting (refusing to run a stale binary)" >&2
            exit 1
        }
    fi
    nohup "$BIN" -config config.json > "$LOG" 2>&1 &
    echo $! > "$PIDFILE"
    sleep 2
    if alive; then echo "started (PID $(cat $PIDFILE))"; else echo "start failed, see $LOG"; tail -20 "$LOG"; exit 1; fi
    ;;
  stop)
    if alive; then kill "$(cat $PIDFILE)" && echo "stopped"; else echo "not running"; fi
    rm -f "$PIDFILE"
    ;;
  restart)
    "$0" stop; sleep 1; "$0" start
    ;;
  status)
    if alive; then
      KEY=$(python3 -c "import json;print(json.load(open('config.json')).get('api_key',''))" 2>/dev/null)
      echo "running (PID $(cat $PIDFILE))"
      curl -s http://127.0.0.1:7988/healthz -w "\n" 2>/dev/null
      curl -s http://127.0.0.1:7988/status -H "Authorization: Bearer $KEY" 2>/dev/null \
        | python3 -c 'import json,sys; p=json.load(sys.stdin)["pool"]; print(f"stock={p[\"stock\"]}/{p[\"warm_size\"]} total={p[\"total\"]} in_flight={p[\"in_flight\"]}")' 2>/dev/null || echo "status unavailable"
    else
      echo "not running"
    fi
    ;;
  logs)    tail -f "$LOG" ;;
  key)     python3 -c "import json;print(json.load(open('config.json')).get('api_key',''))" ;;
  *)       echo "usage: $0 {start|stop|restart|status|logs|key}"; exit 1 ;;
esac

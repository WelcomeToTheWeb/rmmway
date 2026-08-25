#!/usr/bin/env bash
# wait-for-health.sh — block until every service in docker-compose reports
# "healthy". Fails fast (with container logs) if a container exits or keeps
# restarting, instead of silently timing out. Used by `make health`/`make dev`.
set -euo pipefail

TIMEOUT="${TIMEOUT:-120}"
DEADLINE=$(( $(date +%s) + TIMEOUT ))

fail() { echo "ERROR: $*" >&2; exit 1; }

services=(timescale nats redis minio meilisearch loki)

echo "==> Waiting for services to become healthy (timeout ${TIMEOUT}s)"

declare -A seen restarts
while :; do
  now=$(date +%s)
  (( now > DEADLINE )) && fail "timed out after ${TIMEOUT}s waiting for services (see 'docker compose ps')"

  state=$(docker compose ps -a --format '{{.Name}} {{.State}} {{.Health}}' 2>/dev/null || true)

  for svc in "${services[@]}"; do
    # Compose names containers <project>-<service> (here rmmway-<service>);
    # match the "<svc>" line by exact name suffix.
    line=""
    while read -r name st health; do
      [[ "$name" == *-$svc || "$name" == "$svc" ]] && line="$st $health" && break
    done <<< "$state"
    state_word=${line%% *}
    health_word=${line#* }
    [[ -z "$line" ]] && state_word="missing"

    case "$state_word" in
      exited|dead)
        echo "==> $svc container EXITED — logs:" >&2
        docker logs --tail 30 "rmmway-$svc" >&2 2>/dev/null || true
        fail "service $svc exited; see logs above" ;;
      restarting)
        restarts[$svc]=1
        ;;
      running)
        case "$health_word" in
          *"healthy"*) seen[$svc]=1 ;;
        esac
        ;;
    esac
    # created / paused / missing: still starting, keep waiting
  done

  for svc in "${!restarts[@]}"; do
    echo "    $svc: restarting (check 'docker logs rmmway-$svc')"
  done

  ok_count=0
  for svc in "${services[@]}"; do [[ -n "${seen[$svc]:-}" ]] && ok_count=$((ok_count+1)); done

  if (( ok_count == ${#services[@]} )); then
    echo "==> All ${#services[@]} services healthy"
    exit 0
  fi

  echo "    ${ok_count}/${#services[@]} healthy"
  sleep 2
done

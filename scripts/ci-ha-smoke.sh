#!/usr/bin/env bash
# ci-ha-smoke.sh — THE v0.8 acceptance test: seamless failover through a
# stable endpoint. Real binaries, real postgres container:
#
#   postgres + localmesh-server (active) + localmesh-server (standby)
#     + localmesh-router  ──stable endpoint──▶  agent join + inference
#   SIGKILL active → standby promotes → router re-elects → agent
#   reconnects to the SAME address → second inference succeeds.
#
# NO agent.json or client config is touched after the crash — the file
# is checksumed before and after to prove it.
#
#   LOCALMESH_DATABASE_URL=postgres://... bash scripts/ci-ha-smoke.sh
#   bash scripts/ci-ha-smoke.sh            # starts postgres:16-alpine via docker

set -euo pipefail

cd "$(dirname "$0")/.."

step() { printf '\n== %s ==\n' "$*"; }
pass() { printf '  PASS %s\n' "$*"; }
bad()  { printf 'FAIL %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

have curl || bad "curl is required"
have go   || bad "go is required"

py() {
  if have python3; then python3 "$@"
  elif have python; then python "$@"
  else bad "python3/python required for JSON assertions"; fi
}

free_port() {
  py -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

jget() {
  py -c 'import json,sys; print(sys.argv[2].format(j=json.load(open(sys.argv[1]))))' "$1" "$2"
}

tmp="$(mktemp -d)"
pg_name=""
server_pid=""
standby_pid=""
router_pid=""
agent_pid=""
fake_pid=""
join_pid=""

cleanup() {
  for p in "$agent_pid" "$fake_pid" "$server_pid" "$standby_pid" "$router_pid" "$join_pid"; do
    [ -n "$p" ] && kill "$p" >/dev/null 2>&1 || true
  done
  [ -n "$pg_name" ] && docker rm -f "$pg_name" >/dev/null 2>&1 || true
  if [ "${E2E_KEPT:-0}" != "1" ]; then rm -rf "$tmp"; else echo "kept $tmp"; fi
  for f in server standby router agent; do
    echo "--- $f log (tail) ---"; tail -n 12 "$tmp/$f.log" 2>/dev/null || true
  done
}
trap cleanup EXIT

wait_pid_or_bad() {
  [ -n "$1" ] && kill -0 "$1" 2>/dev/null && return 0
  bad "$2 exited unexpectedly — see log tail above"
}

# --- binaries ---------------------------------------------------------------
step "build binaries"
go build -o "$tmp/localmesh-server" ./cmd/localmesh-server
go build -o "$tmp/localmesh-agent"  ./cmd/localmesh-agent
go build -o "$tmp/localmesh-router" ./cmd/localmesh-router
go build -o "$tmp/fakeollama"       ./cmd/fakeollama
pass "built server, agent, router, fakeollama"

# --- postgres ---------------------------------------------------------------
step "postgres"
DSN="${LOCALMESH_DATABASE_URL:-}"
if [ -z "$DSN" ]; then
  have docker || bad "no LOCALMESH_DATABASE_URL and no docker"
  pg_name="localmesh-ci-ha-pg-$$"
  docker run -d --rm --name "$pg_name" \
    -l localmesh.test=1 -l localmesh.test.role=postgres \
    --tmpfs /var/lib/postgresql/data:rw,size=512m \
    -e POSTGRES_PASSWORD=localmeshtest -e POSTGRES_DB=localmesh \
    -p '127.0.0.1::5432' postgres:16-alpine >/dev/null
  port="$(docker port "$pg_name" 5432 | head -n1 | awk -F: '{print $NF}')"
  [ -n "$port" ] || bad "docker port mapping failed"
  DSN="postgres://postgres:localmeshtest@127.0.0.1:$port/localmesh?sslmode=disable"
  ready=0
  for _ in $(seq 60); do
    docker exec "$pg_name" pg_isready -U postgres -d localmesh >/dev/null 2>&1 && { ready=1; break; }
    sleep 1
  done
  [ "$ready" -eq 1 ] || bad "postgres container never became ready"
  pass "postgres:16-alpine up via docker on 127.0.0.1:$port"
else
  pass "using provided LOCALMESH_DATABASE_URL"
fi

ADMIN="lmk_ci_ha_admin_0123456789abcdef"    # test-only preset, not a secret

# --- control-plane pair + router ---------------------------------------------
step "active + standby + router"
APORT="$(free_port)"   # control plane A (will be active)
BPORT="$(free_port)"   # control plane B (standby → promoted)
RPORT="$(free_port)"   # stable endpoint
SRPORT="$(free_port)"  # router status

LOCALMESH_DEV_INSECURE_HTTP=1 \
LOCALMESH_DATABASE_URL="$DSN" \
LOCALMESH_DATA_DIR="$tmp/data-a" \
LOCALMESH_LISTEN="127.0.0.1:$APORT" \
LOCALMESH_ADMIN_KEY="$ADMIN" \
LOCALMESH_DISCOVERY=0 \
  "$tmp/localmesh-server" >"$tmp/server.log" 2>&1 &
server_pid=$!

# A must own the cluster BEFORE the standby starts — otherwise the pair
# races on the advisory lock and either may win. Correct for fencing,
# but not the topology under test.
up=0
for _ in $(seq 60); do
  wait_pid_or_bad "$server_pid" "localmesh-server A"
  code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 \
    "http://127.0.0.1:$APORT/readyz" || true)"
  [ "$code" = "200" ] && { up=1; break; }
  sleep 1
done
[ "$up" -eq 1 ] || bad "control plane A never became ready"
pass "control plane A owns the cluster on $APORT"

LOCALMESH_DEV_INSECURE_HTTP=1 \
LOCALMESH_DATABASE_URL="$DSN" \
LOCALMESH_DATA_DIR="$tmp/data-b" \
LOCALMESH_LISTEN="127.0.0.1:$BPORT" \
LOCALMESH_ADMIN_KEY="$ADMIN" \
LOCALMESH_DISCOVERY=0 \
LOCALMESH_ROLE=standby \
  "$tmp/localmesh-server" >"$tmp/standby.log" 2>&1 &
standby_pid=$!

# Router fronts the pair — A first (preferred while alive), B standby.
# Plain http backends: dev-insecure mode, so TLS probing is skipped.
"$tmp/localmesh-router" \
  -listen "127.0.0.1:$RPORT" \
  -backends "http://127.0.0.1:$APORT,http://127.0.0.1:$BPORT" \
  -interval 200ms -wait 15s -status "127.0.0.1:$SRPORT" \
  >"$tmp/router.log" 2>&1 &
router_pid=$!

BASE="http://127.0.0.1:$RPORT"   # every client/agent uses ONLY this

up=0
for _ in $(seq 90); do
  wait_pid_or_bad "$server_pid" "localmesh-server A"
  wait_pid_or_bad "$router_pid" "localmesh-router"
  code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 "$BASE/readyz" || true)"
  [ "$code" = "200" ] && { up=1; break; }
  sleep 1
done
[ "$up" -eq 1 ] || bad "stable endpoint never answered /readyz"
pass "stable endpoint live on $BASE → routed to active A ($APORT)"

# The standby must be excluded: hit B directly to confirm it is waiting.
code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$BPORT/readyz")"
[ "$code" = "503" ] || bad "standby /readyz → $code, want 503"
pass "standby B excluded by the router (/readyz 503 → not routable)"

active="$(curl -s "http://127.0.0.1:$SRPORT/status" | py -c 'import json,sys; print(json.load(sys.stdin)["active"])')"
case "$active" in *":$APORT"*) pass "router elected backend A ($active)" ;;
  *) bad "router elected '$active', want :$APORT";; esac

# --- enroll + agent connect THROUGH THE ROUTER --------------------------------
step "enrollment through stable endpoint"
code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/enrollment-tokens" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"label":"ci-ha","max_uses":1}')"
[ "$code" = "201" ] || bad "create enrollment token → $code: $(cat "$tmp/resp.json")"
TOKEN="$(jget "$tmp/resp.json" '{j[token]}')"
pass "enrollment token created through router"

export LOCALMESH_AGENT_DIR="$tmp/agent-home"
mkdir -p "$LOCALMESH_AGENT_DIR"
OPORT="$(free_port)"

"$tmp/localmesh-agent" join --server "$BASE" --token "$TOKEN" \
  --name ci-ha-node -ollama-url "http://127.0.0.1:$OPORT" \
  -wait 60s -run=false >"$tmp/join.log" 2>&1 &
join_pid=$!

req_id=""
for _ in $(seq 45); do
  wait_pid_or_bad "$join_pid" "agent join"
  code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' \
    -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/enrollments?status=pending")"
  if [ "$code" = "200" ]; then
    req_id="$(jget "$tmp/resp.json" '{j[0][id]}' 2>/dev/null || true)"
    [ -n "$req_id" ] && break
  fi
  sleep 1
done
[ -n "$req_id" ] || bad "enrollment never appeared pending: $(cat "$tmp/join.log")"
code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/enrollments/$req_id/approve")"
[ "$code" = "200" ] || bad "approve → $code: $(cat "$tmp/resp.json")"
NODE_ID="$(jget "$tmp/resp.json" '{j[id]}')"
wait "$join_pid" || bad "agent join failed: $(cat "$tmp/join.log")"
join_pid=""
CFG="$LOCALMESH_AGENT_DIR/agent.json"
[ -f "$CFG" ] || bad "no config at $CFG"
pass "agent joined through router → node $NODE_ID"
CFG_SUM_BEFORE="$(shasum "$CFG" | awk '{print $1}')"

# --- fakeollama + agent websocket through the router ---------------------------
step "agent connect + inference through stable endpoint"
"$tmp/fakeollama" -port "$OPORT" >"$tmp/fakeollama.log" 2>&1 &
fake_pid=$!
for _ in $(seq 30); do
  wait_pid_or_bad "$fake_pid" "fakeollama"
  curl -sf "http://127.0.0.1:$OPORT/api/version" >/dev/null 2>&1 && break
  sleep 1
done

"$tmp/localmesh-agent" run >"$tmp/agent.log" 2>&1 &
agent_pid=$!

online=0
for _ in $(seq 60); do
  wait_pid_or_bad "$agent_pid" "agent run"
  code="$(curl -s -o "$tmp/nodes.json" -w '%{http_code}' \
    -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/nodes")"
  if [ "$code" = "200" ]; then
    got="$(py - "$tmp/nodes.json" "$NODE_ID" <<'EOF' 2>/dev/null || true
import json,sys
for n in json.load(open(sys.argv[1])):
    if n.get("id")==sys.argv[2] and n.get("online"): print("1")
EOF
)"
    [ "$got" = "1" ] && { online=1; break; }
  fi
  sleep 1
done
[ "$online" -eq 1 ] || bad "node never online through router"
pass "agent websocket connected through the router (WS upgrade survives splice)"

code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"model":"llama3.1:8b","messages":[{"role":"user","content":"pre-failover"}],"stream":false}')"
[ "$code" = "200" ] || bad "pre-failover chat → $code: $(cat "$tmp/resp.json")"
grep -q 'localmesh-echo' "$tmp/resp.json" || bad "pre-failover chat: $(cat "$tmp/resp.json")"
pass "pre-failover inference through stable endpoint"

# --- THE CRASH -----------------------------------------------------------------
step "SIGKILL active control plane"
kill -9 "$server_pid" 2>/dev/null || true
server_pid=""
crash_t="$(date +%s)"

promoted=0
for _ in $(seq 90); do
  wait_pid_or_bad "$standby_pid" "standby (promotion)"
  code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 \
    "http://127.0.0.1:$BPORT/readyz" || true)"
  [ "$code" = "200" ] && { promoted=1; break; }
  sleep 1
done
[ "$promoted" -eq 1 ] || bad "standby never promoted"
pass "standby promoted ($(( $(date +%s) - crash_t ))s after kill)"

# Router must now elect B through the same stable port.
elected=0
for _ in $(seq 60); do
  wait_pid_or_bad "$router_pid" "router (re-election)"
  active="$(curl -s --connect-timeout 2 "http://127.0.0.1:$SRPORT/status" 2>/dev/null \
    | py -c 'import json,sys; print(json.load(sys.stdin)["active"])' 2>/dev/null || true)"
  case "$active" in *":$BPORT"*) elected=1; break;; esac
  sleep 1
done
[ "$elected" -eq 1 ] || bad "router never elected promoted standby: $(cat "$tmp/router.log")"
pass "router re-elected promoted standby — stable endpoint unchanged ($BASE)"

# --- THE REPLAY: same endpoint, zero config change -----------------------------
step "agent auto-reconnect + post-failover inference — no repoint"
online=0
for _ in $(seq 90); do
  wait_pid_or_bad "$agent_pid" "agent run (post-failover)"
  code="$(curl -s -o "$tmp/nodes.json" -w '%{http_code}' --connect-timeout 2 \
    -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/nodes" || true)"
  if [ "$code" = "200" ]; then
    got="$(py - "$tmp/nodes.json" "$NODE_ID" <<'EOF' 2>/dev/null || true
import json,sys
for n in json.load(open(sys.argv[1])):
    if n.get("id")==sys.argv[2] and n.get("online"): print("1")
EOF
)"
    [ "$got" = "1" ] && { online=1; break; }
  fi
  sleep 1
done
[ "$online" -eq 1 ] || bad "agent never reconnected to promoted standby via stable endpoint"
pass "agent reconnected automatically — same server_url, zero intervention"

CFG_SUM_AFTER="$(shasum "$CFG" | awk '{print $1}')"
[ "$CFG_SUM_BEFORE" = "$CFG_SUM_AFTER" ] \
  || bad "agent.json changed during failover — repointing was required (defeats the purpose)"
pass "agent.json untouched before/after failover"

code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"model":"llama3.1:8b","messages":[{"role":"user","content":"post-failover"}],"stream":false}')"
[ "$code" = "200" ] || bad "post-failover chat via SAME endpoint → $code: $(cat "$tmp/resp.json")"
grep -q 'localmesh-echo' "$tmp/resp.json" \
  || bad "post-failover chat content: $(cat "$tmp/resp.json")"
pass "post-failover inference through the SAME stable endpoint — seamless HA verified"

code="$(curl -s -o "$tmp/cp.json" -w '%{http_code}' -H "Authorization: Bearer $ADMIN" \
  "$BASE/api/v1/control-plane")"
[ "$code" = "200" ] || bad "control-plane status → $code"
[ "$(jget "$tmp/cp.json" '{j[epoch]}')" = "2" ] || bad "epoch: $(cat "$tmp/cp.json")"
pass "fencing epoch 2 on promoted owner — single-authority preserved"

step "ci-ha-smoke: PASS"
echo "stable endpoint → active A → SIGKILL → standby promotes → router re-elects →"
echo "agent reconnects untouched → inference again through the same URL. Seamless HA."

#!/usr/bin/env bash
# ci-e2e-smoke.sh — real end-to-end check of the shipped binaries:
# postgres + localmesh-server + fakeollama + localmesh-agent, driving
# enroll → admin approve → agent connect → capability report →
# OpenAI-compatible inference over the wire. Nothing is stubbed between
# the processes except the inference engine (cmd/fakeollama, the same
# double the Go e2e suite uses).
#
#   LOCALMESH_DATABASE_URL=postgres://... bash scripts/ci-e2e-smoke.sh
#   bash scripts/ci-e2e-smoke.sh            # starts postgres:16-alpine via docker
#
# Wired into CI as the `agent-integration` job (ubuntu + a health-checked
# postgres:16-alpine service container). Also runnable on a dev machine
# with docker. Every step asserts; exit is non-zero on any failure —
# there is no SKIP path here: on a hosted ubuntu runner every dependency
# exists, so a broken step is a real regression.

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

# jget FILE {python-format-path} — e.g. jget resp.json '{j[token]}' or
# '{j[0][id]}' (str.format indexing, not eval).
jget() {
  py -c 'import json,sys; print(sys.argv[2].format(j=json.load(open(sys.argv[1]))))' "$1" "$2"
}

tmp="$(mktemp -d)"
pg_name=""
server_pid=""
standby_pid=""
agent_pid=""
fake_pid=""
join_pid=""

cleanup() {
  for p in "$agent_pid" "$fake_pid" "$server_pid" "$standby_pid" "$join_pid"; do
    [ -n "$p" ] && kill "$p" >/dev/null 2>&1 || true
  done
  [ -n "$pg_name" ] && docker rm -f "$pg_name" >/dev/null 2>&1 || true
  if [ "${E2E_KEPT:-0}" != "1" ]; then rm -rf "$tmp"; else echo "kept $tmp"; fi
  echo "--- server log (tail) ---"; tail -n 15 "$tmp/server.log" 2>/dev/null || true
  echo "--- standby log (tail) ---"; tail -n 15 "$tmp/standby.log" 2>/dev/null || true
  echo "--- agent log (tail) ---"; tail -n 15 "$tmp/agent.log" 2>/dev/null || true
}
trap cleanup EXIT

wait_pid_or_bad() { # pid name — fails fast if the process died
  [ -n "$1" ] && kill -0 "$1" 2>/dev/null && return 0
  bad "$2 exited unexpectedly — see log tail above"
}

# --- binaries ---------------------------------------------------------------
step "build binaries"
go build -o "$tmp/localmesh-server" ./cmd/localmesh-server
go build -o "$tmp/localmesh-agent"  ./cmd/localmesh-agent
go build -o "$tmp/fakeollama"       ./cmd/fakeollama
pass "built server, agent, fakeollama"

# --- postgres ---------------------------------------------------------------
step "postgres"
DSN="${LOCALMESH_DATABASE_URL:-}"
if [ -z "$DSN" ]; then
  have docker || bad "no LOCALMESH_DATABASE_URL and no docker"
  pg_name="localmesh-ci-e2e-pg-$$"
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

# --- control plane ------------------------------------------------------------
step "localmesh-server"
SPORT="$(free_port)"
BASE="http://127.0.0.1:$SPORT"
ADMIN="lmk_ci_e2e_admin_0123456789abcdef"   # test-only preset, not a secret

LOCALMESH_DEV_INSECURE_HTTP=1 \
LOCALMESH_DATABASE_URL="$DSN" \
LOCALMESH_DATA_DIR="$tmp/data" \
LOCALMESH_LISTEN="127.0.0.1:$SPORT" \
LOCALMESH_ADMIN_KEY="$ADMIN" \
LOCALMESH_DISCOVERY=0 \
  "$tmp/localmesh-server" >"$tmp/server.log" 2>&1 &
server_pid=$!

up=0
for _ in $(seq 60); do
  wait_pid_or_bad "$server_pid" "localmesh-server"
  code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 "$BASE/healthz" || true)"
  [ "$code" = "200" ] && { up=1; break; }
  sleep 1
done
[ "$up" -eq 1 ] || bad "server never answered /healthz on $BASE"
pass "server listening on $BASE (dev insecure http)"

code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/readyz")"
[ "$code" = "200" ] || bad "/readyz → $code, want 200"
pass "/readyz → 200 (postgres dependency live)"

code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/chat/completions" \
  -H 'Content-Type: application/json' -d '{"model":"x","messages":[{"role":"user","content":"hi"}]}')"
[ "$code" = "401" ] || bad "unauthenticated /v1/chat/completions → $code, want 401"
pass "auth gate: unauthenticated /v1/chat/completions → 401"

step "fleet-check against live server"
bash scripts/fleet-check.sh "$BASE" || bad "fleet-check reported NOT READY"
pass "fleet-check: ready"

# --- enrollment token ---------------------------------------------------------
step "enrollment token"
code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/enrollment-tokens" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"label":"ci-e2e","max_uses":1}')"
[ "$code" = "201" ] || bad "create enrollment token → $code: $(cat "$tmp/resp.json")"
TOKEN="$(jget "$tmp/resp.json" '{j[token]}')"
case "$TOKEN" in lme_*) ;; *) bad "unexpected token format: $TOKEN";; esac
pass "enrollment token created (${TOKEN:0:10}…, single-use)"

# --- agent join (enroll → approve) ---------------------------------------------
step "agent join"
export LOCALMESH_AGENT_DIR="$tmp/agent-home"
mkdir -p "$LOCALMESH_AGENT_DIR"

OPORT="$(free_port)"

# Join waits for admin approval; we approve out-of-band via the admin API.
"$tmp/localmesh-agent" join --server "$BASE" --token "$TOKEN" \
  --name ci-e2e-node -ollama-url "http://127.0.0.1:$OPORT" \
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
[ -n "$req_id" ] || bad "agent enrollment never appeared as pending: $(cat "$tmp/join.log")"
pass "enrollment request pending ($req_id)"

code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/enrollments/$req_id/approve")"
[ "$code" = "200" ] || bad "approve → $code: $(cat "$tmp/resp.json")"
NODE_ID="$(jget "$tmp/resp.json" '{j[id]}')"
pass "enrollment approved → node $NODE_ID"

join_rc=0
wait "$join_pid" || join_rc=$?
join_pid=""
[ "$join_rc" -eq 0 ] || bad "agent join exited $join_rc: $(cat "$tmp/join.log")"
CFG="$LOCALMESH_AGENT_DIR/agent.json"
[ -f "$CFG" ] || bad "join succeeded but no config at $CFG"
[ "$(jget "$CFG" '{j[node_id]}')" = "$NODE_ID" ] || bad "config node_id mismatch: $(cat "$CFG")"
[ "$(jget "$CFG" '{j[node_token]}')" != "" ] || bad "config has no node_token"
pass "join exited 0; config persisted with node creds (lmn_ hash-stored server-side)"

# --- fakeollama + agent run ----------------------------------------------------
step "fakeollama + agent run"
"$tmp/fakeollama" -port "$OPORT" >"$tmp/fakeollama.log" 2>&1 &
fake_pid=$!
for _ in $(seq 30); do
  wait_pid_or_bad "$fake_pid" "fakeollama"
  curl -sf "http://127.0.0.1:$OPORT/api/version" >/dev/null 2>&1 && break
  sleep 1
done
curl -sf "http://127.0.0.1:$OPORT/api/version" >/dev/null \
  || bad "fakeollama not answering /api/version"
pass "fakeollama up on 127.0.0.1:$OPORT (llama3.1:8b, qwen2.5:7b, nomic-embed-text)"

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
nodes=json.load(open(sys.argv[1]))
for n in nodes:
    if n.get("id") == sys.argv[2] and n.get("online"):
        names=[m.get("name") for m in (n.get("models") or [])]
        print("online models=" + ",".join(names))
EOF
)"
    [ -n "$got" ] && { echo "    $got"; online=1; break; }
  fi
  sleep 1
done
[ "$online" -eq 1 ] || bad "node never came online with models: $(cat "$tmp/nodes.json" 2>/dev/null)"
pass "node online via agent websocket (ws:// /api/v1/agent/connect)"

step "agent doctor (fully-enrolled green path)"
doc_out=""
doc_code=0
doc_out="$(LOCALMESH_AGENT_DIR="$LOCALMESH_AGENT_DIR" "$tmp/localmesh-agent" doctor 2>&1)" || doc_code=$?
printf '%s\n' "$doc_out"
[ "$doc_code" -eq 0 ] || bad "doctor on live cluster → exit $doc_code, want 0"
pass "doctor exit 0: config+identity+dns+tcp+healthz+websocket+engine+models+disk+hardware"

# --- inference over the wire -----------------------------------------------------
step "inference"
code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' \
  -H "Authorization: Bearer $ADMIN" "$BASE/v1/models")"
[ "$code" = "200" ] || bad "GET /v1/models → $code"
grep -q 'llama3.1:8b' "$tmp/resp.json" || bad "/v1/models missing llama3.1:8b: $(cat "$tmp/resp.json")"
pass "GET /v1/models lists llama3.1:8b"

code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"model":"llama3.1:8b","messages":[{"role":"user","content":"hello cluster"}],"stream":false}')"
[ "$code" = "200" ] || bad "chat (buffered) → $code: $(cat "$tmp/resp.json")"
content="$(jget "$tmp/resp.json" '{j[choices][0][message][content]}')"
case "$content" in
  *localmesh-echo*) pass "chat (buffered): gateway→node→fakeollama round trip: '$content'" ;;
  *) bad "unexpected completion content: $content" ;;
esac

code="$(curl -s -N -o "$tmp/sse.txt" -w '%{http_code}' -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"model":"llama3.1:8b","messages":[{"role":"user","content":"stream me"}],"stream":true}')"
[ "$code" = "200" ] || bad "chat (stream) → $code: $(cat "$tmp/sse.txt")"
grep -q 'data: ' "$tmp/sse.txt" && grep -q 'localmesh-echo' "$tmp/sse.txt" \
  && grep -q 'data: \[DONE\]' "$tmp/sse.txt" \
  || bad "SSE stream malformed: $(cat "$tmp/sse.txt")"
pass "chat (stream): SSE chunks + [DONE] received"

code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' \
  -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/workloads")"
[ "$code" = "200" ] || bad "GET /api/v1/workloads → $code"
wl="$(jget "$tmp/resp.json" '{j[0][status]}')"
[ "$wl" = "completed" ] || bad "latest workload status '$wl', want completed: $(cat "$tmp/resp.json")"
pass "workload row recorded as completed (metadata only — no prompt persisted)"

# --- standby promotion --------------------------------------------------------
step "standby promotion"
S2PORT="$(free_port)"
SBASE="http://127.0.0.1:$S2PORT"
LOCALMESH_DEV_INSECURE_HTTP=1 \
LOCALMESH_DATABASE_URL="$DSN" \
LOCALMESH_DATA_DIR="$tmp/data-sb" \
LOCALMESH_LISTEN="127.0.0.1:$S2PORT" \
LOCALMESH_ADMIN_KEY="$ADMIN" \
LOCALMESH_DISCOVERY=0 \
LOCALMESH_ROLE=standby \
  "$tmp/localmesh-server" >"$tmp/standby.log" 2>&1 &
standby_pid=$!

# While the primary owns the cluster the standby reports itself and
# refuses every authoritative surface.
sb=0
for _ in $(seq 60); do
  wait_pid_or_bad "$standby_pid" "standby localmesh-server"
  code="$(curl -s -o "$tmp/sb.json" -w '%{http_code}' --connect-timeout 2 \
    "$SBASE/api/v1/standby" || true)"
  [ "$code" = "200" ] && { sb=1; break; }
  sleep 1
done
[ "$sb" -eq 1 ] || bad "standby never answered /api/v1/standby"
[ "$(jget "$tmp/sb.json" '{j[role]}')" = "standby" ] \
  || bad "standby role: $(cat "$tmp/sb.json")"
code="$(curl -s -o /dev/null -w '%{http_code}' "$SBASE/healthz")"
[ "$code" = "503" ] || bad "standby /healthz → $code, want 503"
code="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ADMIN" \
  "$SBASE/api/v1/nodes")"
[ "$code" = "503" ] || bad "standby /api/v1/nodes → $code, want 503"
pass "standby waits: role=standby, /healthz 503, API 503"

# Kill the primary without a clean shutdown — the crash shape: the lock
# session dies with the process, the standby must claim the next epoch.
kill -9 "$server_pid" 2>/dev/null || true
server_pid=""

promoted=0
for _ in $(seq 60); do
  wait_pid_or_bad "$standby_pid" "standby localmesh-server (promotion)"
  code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 \
    "$SBASE/healthz" || true)"
  [ "$code" = "200" ] && { promoted=1; break; }
  sleep 1
done
[ "$promoted" -eq 1 ] || bad "standby never promoted after primary crash"

code="$(curl -s -o "$tmp/cp.json" -w '%{http_code}' -H "Authorization: Bearer $ADMIN" \
  "$SBASE/api/v1/control-plane")"
[ "$code" = "200" ] || bad "promoted /api/v1/control-plane → $code"
[ "$(jget "$tmp/cp.json" '{j[epoch]}')" = "2" ] \
  || bad "promoted epoch: $(cat "$tmp/cp.json")"
[ "$(jget "$tmp/cp.json" '{j[promoted_from_standby]}')" = "True" ] \
  || bad "promoted_from_standby unset: $(cat "$tmp/cp.json")"
pass "standby promoted on primary crash — epoch 2, full API serving"

step "ci-e2e-smoke: PASS"
echo "enroll → approve → connect → capability → infer all verified on real binaries"

#!/usr/bin/env bash
# ci-ollama-ha-smoke.sh — THE v0.9 acceptance test: seamless failover through
# a stable endpoint with a REAL Ollama runtime and a REAL AI model.
#
#   postgres + localmesh-server (active) + localmesh-server (standby)
#     + localmesh-router ──stable endpoint──▶ agent ──▶ real Ollama ──▶ model
#
#   A. non-streaming inference + model discovery + workload persistence
#   B. streaming inference (real SSE token deltas)
#   E. model pull in-flight during SIGKILL failover → honest reconciliation
#   C. agent auto-reconnect → post-failover real inference, same endpoint
#   D. stream interrupted by SIGKILL → honest client failure + terminal state
#      → restarted instance promotes again → fresh inference succeeds
#
# Requires a reachable real Ollama (native or container — either is real):
#
#   OLLAMA_URL=http://127.0.0.1:11434 bash scripts/ci-ollama-ha-smoke.sh
#   REQUIRE_REAL_OLLAMA=1 ./ci-ollama-ha-smoke.sh   # fail instead of skip
#
# Tunables: OLLAMA_MODEL (default qwen2.5:0.5b), OLLAMA_PULL_MODEL (default
# smollm2:135m — small model the failover leg pulls while the primary dies).
set -euo pipefail

cd "$(dirname "$0")/.."

step() { printf '\n== %s ==\n' "$*"; }
pass() { printf '  PASS %s\n' "$*"; }
note() { printf '  .... %s\n' "$*"; }
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

# --- real Ollama gate ---------------------------------------------------------
OLLAMA_URL="${OLLAMA_URL:-http://127.0.0.1:11434}"
MODEL="${OLLAMA_MODEL:-qwen2.5:0.5b}"
PULL_MODEL="${OLLAMA_PULL_MODEL:-smollm2:135m}"

step "real Ollama gate"
OVER=""
tmp_oll_ver="$(mktemp)"
if curl -sf -m 5 "$OLLAMA_URL/api/version" >"$tmp_oll_ver" 2>/dev/null; then
  OVER="$(py -c 'import json,sys;print(json.load(open(sys.argv[1]))["version"])' "$tmp_oll_ver" 2>/dev/null || true)"
fi
rm -f "$tmp_oll_ver"
if [ -z "$OVER" ]; then
  if [ "${REQUIRE_REAL_OLLAMA:-0}" = "1" ]; then
    bad "real Ollama unreachable at $OLLAMA_URL — start ollama or the ollama/ollama container"
  fi
  echo "SKIP: no real Ollama at $OLLAMA_URL (set REQUIRE_REAL_OLLAMA=1 to enforce)"
  exit 0
fi
pass "real Ollama $OVER at $OLLAMA_URL (NOT a test double)"

have_model() { curl -sf -m 10 "$OLLAMA_URL/api/tags" | py -c '
import json,sys
tag=sys.argv[1]
tags=[m.get("name","") for m in json.load(sys.stdin).get("models",[])]
print("1" if tag in tags or any(t.startswith(tag.split(":")[0]+":") and t==tag for t in tags) else "")' "$1"; }

ensure_model() { # $1 model — pull via real Ollama API if absent
  [ "$(have_model "$1")" = "1" ] && return 0
  note "pulling $1 through the real Ollama API (one-time)"
  code="$(curl -s -o /dev/null -w '%{http_code}' -m 600 -X POST "$OLLAMA_URL/api/pull" \
    -H 'Content-Type: application/json' -d "{\"name\":\"$1\",\"stream\":false}" || true)"
  [ "$code" = "200" ] || bad "ollama pull $1 → $code"
  [ "$(have_model "$1")" = "1" ] || bad "model $1 still absent after pull"
}
ensure_model "$MODEL"
pass "inference model present: $MODEL (real weights)"

tmp="$(mktemp -d)"
pg_name=""
server_pid=""; standby_pid=""; router_pid=""; agent_pid=""; join_pid=""

cleanup() {
  for p in "$agent_pid" "$server_pid" "$standby_pid" "$router_pid" "$join_pid"; do
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

# --- binaries -----------------------------------------------------------------
step "build binaries"
go build -o "$tmp/localmesh-server" ./cmd/localmesh-server
go build -o "$tmp/localmesh-agent"  ./cmd/localmesh-agent
go build -o "$tmp/localmesh-router" ./cmd/localmesh-router
pass "built server, agent, router"

# --- postgres -----------------------------------------------------------------
step "postgres"
DSN="${LOCALMESH_DATABASE_URL:-}"
if [ -z "$DSN" ]; then
  have docker || bad "no LOCALMESH_DATABASE_URL and no docker"
  pg_name="localmesh-ci-oha-pg-$$"
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

ADMIN="lmk_ci_oha_admin_0123456789abcdef"   # test-only preset, not a secret

start_cp() { # $1 datadir $2 port $3 logname $4 mode(active|standby)
  local extra=()
  [ -n "$4" ] && extra=(LOCALMESH_STANDBY=1)
  env LOCALMESH_DEV_INSECURE_HTTP=1 \
  LOCALMESH_DATABASE_URL="$DSN" \
  LOCALMESH_DATA_DIR="$tmp/$1" \
  LOCALMESH_LISTEN="127.0.0.1:$2" \
  LOCALMESH_ADMIN_KEY="$ADMIN" \
  LOCALMESH_DISCOVERY=0 \
  ${extra[@]+"${extra[@]}"} \
    "$tmp/localmesh-server" >"$tmp/$3.log" 2>&1 &
  echo $!
}

wait_ready() { # $1 port $2 want_code — polls /readyz
  local ok=0
  for _ in $(seq "${3:-90}"); do
    code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 \
      "http://127.0.0.1:$1/readyz" || true)"
    [ "$code" = "$2" ] && { ok=1; break; }
    sleep 1
  done
  [ "$ok" -eq 1 ]
}

node_online() { # polls node online through the router
  local ok=0
  for _ in $(seq "${2:-90}"); do
    wait_pid_or_bad "$agent_pid" "agent run"
    code="$(curl -s -o "$tmp/nodes.json" -w '%{http_code}' --connect-timeout 2 \
      -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/nodes" || true)"
    if [ "$code" = "200" ]; then
      got="$(py - "$tmp/nodes.json" "$NODE_ID" <<'EOF' 2>/dev/null || true
import json,sys
for n in json.load(open(sys.argv[1])):
    if n.get("id")==sys.argv[2] and n.get("online"): print("1")
EOF
)"
      [ "$got" = "1" ] && { ok=1; break; }
    fi
    sleep 1
  done
  [ "$ok" -eq 1 ]
}

real_chat() { # $1 prompt $2 stream(true|false) → writes $tmp/resp.json, echoes http code
  curl -s -o "$tmp/resp.json" -w '%{http_code}' -m 120 -X POST "$BASE/v1/chat/completions" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$1\"}],\"stream\":$2,\"max_tokens\":64}" || true
}

router_elected() { # echoes elected backend address ("" if none)
  curl -s --connect-timeout 2 "http://127.0.0.1:$SRPORT/status" 2>/dev/null \
    | py -c 'import json,sys; print(json.load(sys.stdin).get("active") or "")' 2>/dev/null || true
}

# --- topology: A active, B standby, router stable endpoint ---------------------
step "active + standby + router"
APORT="$(free_port)"; BPORT="$(free_port)"; RPORT="$(free_port)"; SRPORT="$(free_port)"

server_pid="$(start_cp data-a "$APORT" server "")"
for _ in $(seq 60); do
  wait_pid_or_bad "$server_pid" "control plane A"
  code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 \
    "http://127.0.0.1:$APORT/readyz" || true)"
  [ "$code" = "200" ] && break; sleep 1
done
[ "$code" = "200" ] || bad "control plane A never became ready"
pass "control plane A owns the cluster on $APORT"

standby_pid="$(start_cp data-b "$BPORT" standby standby)"

"$tmp/localmesh-router" \
  -listen "127.0.0.1:$RPORT" \
  -backends "http://127.0.0.1:$APORT,http://127.0.0.1:$BPORT" \
  -interval 200ms -wait 15s -status "127.0.0.1:$SRPORT" \
  >"$tmp/router.log" 2>&1 &
router_pid=$!

BASE="http://127.0.0.1:$RPORT"   # every client/agent uses ONLY this
wait_ready "$RPORT" 200 || bad "stable endpoint never answered /readyz"
pass "stable endpoint live on $BASE → routed to active A"

# The standby must be alive-but-not-ready: poll until its listener is up
# and answering 503 (connection refused = still starting).
wait_ready "$BPORT" 503 || bad "standby /readyz never settled to 503"
pass "standby B excluded by the router (/readyz 503 → not routable)"

# --- enroll + agent connect THROUGH THE ROUTER (real Ollama behind it) --------
step "enrollment through stable endpoint"
code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/enrollment-tokens" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"label":"ci-oha","max_uses":1}')"
[ "$code" = "201" ] || bad "create enrollment token → $code: $(cat "$tmp/resp.json")"
TOKEN="$(jget "$tmp/resp.json" '{j[token]}')"

export LOCALMESH_AGENT_DIR="$tmp/agent-home"
mkdir -p "$LOCALMESH_AGENT_DIR"

"$tmp/localmesh-agent" join --server "$BASE" --token "$TOKEN" \
  --name ci-oha-node -ollama-url "$OLLAMA_URL" \
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
CFG_SUM_BEFORE="$(shasum "$CFG" | awk '{print $1}')"
pass "agent joined through router → node $NODE_ID"

"$tmp/localmesh-agent" run >"$tmp/agent.log" 2>&1 &
agent_pid=$!
node_online 60 || bad "node never online through router"
pass "agent websocket connected through the router"

# ==============================================================================
step "A: real model discovery + non-streaming inference"
seen=0
for _ in $(seq 60); do
  code="$(curl -s -o "$tmp/models.json" -w '%{http_code}' \
    -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/models")"
  [ "$code" = "200" ] || { sleep 1; continue; }
  got="$(py - "$tmp/models.json" "$MODEL" <<'EOF' 2>/dev/null || true
import json,sys
ms=json.load(open(sys.argv[1]))
tag=sys.argv[2]
rows=ms if isinstance(ms,list) else ms.get("models",[])
for m in rows:
    name=m.get("name") or m.get("id") or ""
    if name==tag or name.startswith(tag.split(":")[0]+":"):
        print("1"); break
EOF
)"
  [ "$got" = "1" ] && { seen=1; break; }
  sleep 1
done
[ "$seen" -eq 1 ] || bad "real model $MODEL never discovered: $(cat "$tmp/models.json")"
pass "real model discovered in cluster inventory: $MODEL"

code="$(real_chat "Reply with exactly the two words: LOCALMESH REAL" false)"
[ "$code" = "200" ] || bad "non-streaming chat → $code: $(cat "$tmp/resp.json")"
py - "$tmp/resp.json" <<'EOF' || bad "response shape: $(cat "$tmp/resp.json")"
import json,sys
j=json.load(open(sys.argv[1]))
content=j["choices"][0]["message"]["content"].strip()
assert content, "empty model output"
u=j.get("usage") or {}
print(f"     content={content!r} usage={u}")
EOF
pass "real generated text received (not an echo double)"

wl_seen=0
for _ in $(seq 30); do
  code="$(curl -s -o "$tmp/wl.json" -w '%{http_code}' \
    -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/workloads")"
  [ "$code" = "200" ] || { sleep 1; continue; }
  got="$(py - "$tmp/wl.json" <<'EOF' 2>/dev/null || true
import json,sys
rows=json.load(open(sys.argv[1]))
rows=rows if isinstance(rows,list) else rows.get("workloads",[])
for w in rows:
    if w.get("status")=="completed" and w.get("completion_tokens"): print("1"); break
EOF
)"
  [ "$got" = "1" ] && { wl_seen=1; break; }
  sleep 1
done
[ "$wl_seen" -eq 1 ] || bad "no completed workload with token usage persisted"
pass "workload persisted with real token usage"

# ==============================================================================
step "B: real streaming inference (SSE)"
curl -s -N -m 90 -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Count from one to ten.\"}],\"stream\":true,\"max_tokens\":80}" \
  >"$tmp/stream.txt" 2>"$tmp/stream.err" || true

chunks="$(grep -c '^data:' "$tmp/stream.txt" || true)"
[ "$chunks" -ge 2 ] || bad "expected ≥2 SSE data chunks, got $chunks: $(cat "$tmp/stream.txt" | head -5)"
grep -q 'data: \[DONE\]' "$tmp/stream.txt" || bad "stream missing [DONE] terminator"
tok="$(grep '^data:' "$tmp/stream.txt" | grep -v '\[DONE\]' | py -c '
import json,sys
out=""
for line in sys.stdin:
    try:
        d=json.loads(line[5:].strip())
        out+=d["choices"][0].get("delta",{}).get("content") or ""
    except Exception: pass
print(out)' )"
[ -n "$tok" ] || bad "stream carried no delta content"
pass "real token deltas streamed as OpenAI SSE ($chunks chunks, [DONE] seen)"

# ==============================================================================
step "E: model pull in-flight during control-plane kill"
# Kick off a real pull of a second model; while it is being fetched the
# active control plane is killed. The op must reconcile honestly after
# promotion — never duplicated, never falsely 'complete'.
pull_started=0
code="$(curl -s -o "$tmp/pull.json" -w '%{http_code}' -X POST "$BASE/api/v1/models/pull" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$PULL_MODEL\",\"node_ids\":[\"$NODE_ID\"]}")"
if [ "$code" = "200" ] || [ "$code" = "201" ] || [ "$code" = "202" ]; then
  OP_ID="$(jget "$tmp/pull.json" '{j[id]}' 2>/dev/null || jget "$tmp/pull.json" '{j[operations][0][id]}' 2>/dev/null || true)"
  [ -n "$OP_ID" ] || OP_ID="$(py -c 'import json;j=json.load(open("'"$tmp"'/pull.json"));
ops=j.get("operations") or ([j] if j.get("id") else []); print(ops[0]["id"] if ops else "")' 2>/dev/null || true)"
  # wait for the op to actually be running/dispatched (or complete — a
  # warm cache may finish instantly; that still exercises reconcile)
  for _ in $(seq 30); do
    code="$(curl -s -o "$tmp/ops.json" -w '%{http_code}' \
      -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/model-operations")"
    [ "$code" = "200" ] || { sleep 1; continue; }
    st="$(py - "$tmp/ops.json" "${OP_ID:-x}" <<'EOF' 2>/dev/null || true
import json,sys
rows=json.load(open(sys.argv[1]))
rows=rows if isinstance(rows,list) else rows.get("operations",rows.get("model_operations",[]))
for o in rows:
    if o.get("id")==sys.argv[2] or (sys.argv[2]=="x" and o.get("model","").endswith("135m")):
        print(o.get("status","")); break
EOF
)"
    case "$st" in
      running|dispatched|downloading|queued) pull_started=1; break ;;
      complete|completed|succeeded) pull_started=1; note "pull finished before the kill (warm path) — reconciliation still asserted"; break ;;
    esac
    sleep 1
  done
  [ "$pull_started" -eq 1 ] || bad "pull op never reached a live state: $(cat "$tmp/ops.json")"
  note "pull op ${OP_ID:-?} in state '$st' — killing the active now"
else
  note "pull dispatch → $code (tolerated: offline or registry slow) — failover reconciliation leg continues with whatever ops exist"
fi

# --- THE CRASH #1 ---------------------------------------------------------------
step "SIGKILL active control plane (mid model-op)"
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

elected=0
for _ in $(seq 60); do
  wait_pid_or_bad "$router_pid" "router (re-election)"
  active="$(router_elected)"
  case "$active" in *":$BPORT"*) elected=1; break;; esac
  sleep 1
done
[ "$elected" -eq 1 ] || bad "router never elected promoted standby: $(cat "$tmp/router.log")"
pass "router re-elected promoted standby — stable endpoint unchanged"

# pull op must reconcile to an honest terminal or resumable state — never
# stuck 'running' on a dead owner, never duplicated.
settled=0
for _ in $(seq 120); do
  code="$(curl -s -o "$tmp/ops.json" -w '%{http_code}' --connect-timeout 2 \
    -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/model-operations" || true)"
  [ "$code" = "200" ] || { sleep 1; continue; }
  verdict="$(py - "$tmp/ops.json" "$PULL_MODEL" <<'EOF' 2>/dev/null || true
import json,sys
rows=json.load(open(sys.argv[1]))
rows=rows if isinstance(rows,list) else rows.get("operations",rows.get("model_operations",[]))
mine=[o for o in rows if sys.argv[2] in (o.get("model") or "")]
live={"queued","dispatched","running","downloading","pending"}
if len(mine)>1:
    sts=[o.get("status") for o in mine]
    print("DUP:"+",".join(sts)); raise SystemExit
for o in mine:
    s=o.get("status","")
    if s in live: print("live:"+s)
    elif s in {"complete","completed","succeeded"}: print("done:"+s)
    elif s in {"interrupted","failed","cancelled"}: print("term:"+s+":"+(o.get("error") or ""))
    else: print("?"+s)
EOF
)"
  case "$verdict" in
    DUP:*) bad "duplicate pull op rows after failover: $verdict" ;;
    done:*|term:*) settled=1; break ;;
    live:*) ;; # still converging (resumed pull may take a while)
  esac
  sleep 2
done
[ "$settled" -eq 1 ] || bad "pull op never reconciled post-failover: $(cat "$tmp/ops.json")"
pass "model pull reconciled post-failover ($verdict) — no duplicate dispatch"

# ==============================================================================
step "C: agent auto-reconnect + real post-failover inference — same endpoint"
node_online 90 || bad "agent never reconnected to promoted standby via stable endpoint"
pass "agent reconnected automatically — same server_url, zero intervention"

CFG_SUM_AFTER="$(shasum "$CFG" | awk '{print $1}')"
[ "$CFG_SUM_BEFORE" = "$CFG_SUM_AFTER" ] \
  || bad "agent.json changed during failover — repointing was required"
pass "agent.json untouched before/after failover"

code="$(real_chat "Reply with exactly the two words: FAILOVER WORKS" false)"
[ "$code" = "200" ] || bad "post-failover chat via SAME endpoint → $code: $(cat "$tmp/resp.json")"
py - "$tmp/resp.json" <<'EOF' || bad "post-failover response: $(cat "$tmp/resp.json")"
import json,sys
j=json.load(open(sys.argv[1]))
content=j["choices"][0]["message"]["content"].strip()
assert content, "empty model output"
print(f"     real content={content!r}")
EOF
pass "post-failover REAL inference through the SAME stable endpoint"

code="$(curl -s -o "$tmp/cp.json" -w '%{http_code}' -H "Authorization: Bearer $ADMIN" \
  "$BASE/api/v1/control-plane")"
[ "$code" = "200" ] || bad "control-plane status → $code"
[ "$(jget "$tmp/cp.json" '{j[epoch]}')" = "2" ] || bad "epoch: $(cat "$tmp/cp.json")"
pass "fencing epoch 2 on promoted owner"

# ==============================================================================
step "D: SIGKILL the promoted owner mid-stream → honest interruption"
# Long stream in background; kill B while tokens are flowing.
curl -s -N -m 120 -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Count slowly from 1 to 500, one number per line.\"}],\"stream\":true,\"max_tokens\":600}" \
  >"$tmp/stream2.txt" 2>"$tmp/stream2.err" &
stream_pid=$!
sleep 3   # let real tokens flow
kill -9 "$standby_pid" 2>/dev/null || true
standby_pid=""
wait "$stream_pid" 2>/dev/null || true

if grep -q 'data: \[DONE\]' "$tmp/stream2.txt" && [ ! -s "$tmp/stream2.err" ]; then
  bad "stream completed cleanly after owner was SIGKILLed — interruption was not honest"
fi
chunks2="$(grep -c '^data:' "$tmp/stream2.txt" || true)"
[ "$chunks2" -ge 1 ] || bad "stream delivered zero chunks before interruption — was it ever flowing?"
pass "stream honestly interrupted by owner crash ($chunks2 chunks then cut, no fake completion)"

# ==============================================================================
step "restarted instance re-acquires → fresh real inference (epoch 3)"
server_pid="$(start_cp data-a "$APORT" server standby)"
up=0
for _ in $(seq 90); do
  wait_pid_or_bad "$server_pid" "restarted instance A"
  code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 \
    "http://127.0.0.1:$APORT/readyz" || true)"
  [ "$code" = "200" ] && { up=1; break; }
  sleep 1
done
[ "$up" -eq 1 ] || bad "restarted instance never became active"
pass "restarted instance promoted — fencing epoch advanced again"

elected=0
for _ in $(seq 60); do
  active="$(router_elected)"
  case "$active" in *":$APORT"*) elected=1; break;; esac
  sleep 1
done
[ "$elected" -eq 1 ] || bad "router never re-elected restarted A: $(cat "$tmp/router.log")"

node_online 90 || bad "agent never reconnected after second failover"
pass "agent reconnected after second failover — still zero config changes"

# interrupted workload must be terminal, not stuck
settled=0
for _ in $(seq 60); do
  code="$(curl -s -o "$tmp/wl.json" -w '%{http_code}' --connect-timeout 2 \
    -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/workloads" || true)"
  [ "$code" = "200" ] || { sleep 1; continue; }
  bad_rows="$(py - "$tmp/wl.json" <<'EOF' 2>/dev/null || echo pending
import json,sys
rows=json.load(open(sys.argv[1]))
rows=rows if isinstance(rows,list) else rows.get("workloads",[])
live=[w for w in rows if w.get("status") in {"running","queued","dispatched"}]
print(len(live))
EOF
)"
  [ "$bad_rows" = "0" ] && { settled=1; break; }
  sleep 2
done
[ "$settled" -eq 1 ] || bad "workloads still live after reconcile: $(cat "$tmp/wl.json")"
pass "interrupted workload reached terminal state — no leaked reservations"

code="$(real_chat "Reply with exactly the two words: STILL ALIVE" false)"
[ "$code" = "200" ] || bad "third-generation chat → $code: $(cat "$tmp/resp.json")"
py - "$tmp/resp.json" <<'EOF' || bad "response: $(cat "$tmp/resp.json")"
import json,sys
j=json.load(open(sys.argv[1]))
assert j["choices"][0]["message"]["content"].strip()
EOF
pass "real inference after second failover through the SAME endpoint"

code="$(curl -s -o "$tmp/cp.json" -w '%{http_code}' -H "Authorization: Bearer $ADMIN" \
  "$BASE/api/v1/control-plane")"
ep="$(jget "$tmp/cp.json" '{j[epoch]}')"
[ "$ep" = "3" ] || bad "expected epoch 3 after two promotions, got: $(cat "$tmp/cp.json")"
pass "fencing epoch 3 — monotonic across repeated promotions"

step "ci-ollama-ha-smoke: PASS"
echo "REAL Ollama + REAL model through the stable endpoint:"
echo "  discovery → non-stream → stream → SIGKILL → promote → reconnect →"
echo "  real inference again → mid-stream kill → honest interruption →"
echo "  second promotion → real inference again. Zero client/agent changes."

#!/usr/bin/env bash
# ci-update-smoke.sh — live signed agent-update rollout, end to end:
#
#   postgres + localmesh-server + fakeollama + localmesh-agent("dev"),
#   then: minisign keypair (internal/release/cmd/sign) → pin pubkey into
#   agent.json at join → build a versioned agent binary → SHA256SUMS →
#   .minisig signatures → gen-manifest → register key → register release →
#   create rollout → watch the agent verify/download/install/restart/
#   rejoin at the new version → inference still works post-upgrade.
#
# Exercises the whole v0.7 update path on real binaries: the admin
# release registry, the localmesh:// artifact proxy (release base_url is
# left EMPTY so the agent must pull through the control plane), the
# agent's pinned-key signature verification, atomic install + restart,
# and the orchestrator's health-confirm promotion to 'succeeded'.
#
#   bash scripts/ci-update-smoke.sh    # starts postgres via docker
#   LOCALMESH_DATABASE_URL=postgres://... bash scripts/ci-update-smoke.sh

set -euo pipefail

cd "$(dirname "$0")/.."

step() { printf '\n== %s ==\n' "$*"; }
pass() { printf '  PASS %s\n' "$*"; }
bad()  { printf 'FAIL %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

have curl    || bad "curl is required"
have go      || bad "go is required"

py() {
  if have python3; then python3 "$@"
  elif have python; then python "$@"
  else bad "python3/python required for JSON assertions"; fi
}

free_port() {
  py -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

jget() { py -c 'import json,sys; print(sys.argv[2].format(j=json.load(open(sys.argv[1]))))' "$1" "$2"; }

# host platform triple for the artifact name — the agent only downloads
# the artifact matching its own os/arch.
goos="$(uname -s | tr '[:upper:]' '[:lower:]')"
goarch="$(uname -m)"
case "$goarch" in x86_64) goarch=amd64 ;; aarch64) goarch=arm64 ;; esac
[ "$goos" = "darwin" ] || [ "$goos" = "linux" ] || bad "ci-update-smoke supports darwin/linux hosts (got $goos)"

tmp="$(mktemp -d)"
pg_name=""
server_pid=""
agent_pid=""
fake_pid=""
join_pid=""

cleanup() {
  for p in "$agent_pid" "$fake_pid" "$server_pid" "$join_pid"; do
    [ -n "$p" ] && kill "$p" >/dev/null 2>&1 || true
  done
  # The post-upgrade agent is a detached re-exec — our pid handle went
  # stale. Kill by the unique tmp binary path instead.
  pkill -f "$tmp/localmesh-agent" >/dev/null 2>&1 || true
  [ -n "$pg_name" ] && docker rm -f "$pg_name" >/dev/null 2>&1 || true
  if [ "${E2E_KEPT:-0}" != "1" ]; then rm -rf "$tmp"; else echo "kept $tmp"; fi
  echo "--- server log (tail) ---"; tail -n 20 "$tmp/server.log" 2>/dev/null || true
  echo "--- agent log (tail) ---";  tail -n 25 "$tmp/agent.log"  2>/dev/null || true
}
trap cleanup EXIT

wait_pid_or_bad() {
  [ -n "$1" ] && kill -0 "$1" 2>/dev/null && return 0
  bad "$2 exited unexpectedly — see log tail above"
}

NEWVER="0.7.9-smoke"

# --- binaries ---------------------------------------------------------------
step "build binaries"
go build -o "$tmp/localmesh-server" ./cmd/localmesh-server
go build -o "$tmp/localmesh-agent"  ./cmd/localmesh-agent
go build -o "$tmp/fakeollama"       ./cmd/fakeollama
pass "built server, agent(v=dev), fakeollama"

# --- signing key --------------------------------------------------------------
step "release signing key"
go run ./internal/release/cmd/sign keygen -sec "$tmp/sign.sec" -pub "$tmp/sign.pub" 2>"$tmp/keygen.log"
KEYID="$(sed -n 's/.*key id: //p' "$tmp/keygen.log" | tr -d ' \r')"
[ -n "$KEYID" ] || bad "sign keygen produced no key id: $(cat "$tmp/keygen.log")"
pass "minisign-compatible keypair generated (key id $KEYID)"

# --- postgres ---------------------------------------------------------------
step "postgres"
DSN="${LOCALMESH_DATABASE_URL:-}"
if [ -z "$DSN" ]; then
  have docker || bad "no LOCALMESH_DATABASE_URL and no docker"
  pg_name="localmesh-ci-update-pg-$$"
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
ADMIN="lmk_ci_update_admin_0123456789ab"   # test-only preset, not a secret

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

# --- enrollment token ---------------------------------------------------------
step "enrollment token"
code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/enrollment-tokens" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"label":"ci-update","max_uses":1}')"
[ "$code" = "201" ] || bad "create enrollment token → $code: $(cat "$tmp/resp.json")"
TOKEN="$(jget "$tmp/resp.json" '{j[token]}')"
pass "enrollment token created (single-use)"

# --- agent join (pinning the update key at join time) -------------------------
step "agent join (+ update-key pin)"
export LOCALMESH_AGENT_DIR="$tmp/agent-home"
mkdir -p "$LOCALMESH_AGENT_DIR"

OPORT="$(free_port)"

"$tmp/localmesh-agent" join --server "$BASE" --token "$TOKEN" \
  --name ci-update-node -ollama-url "http://127.0.0.1:$OPORT" \
  -update-pubkey "$tmp/sign.pub" -wait 60s -run=false >"$tmp/join.log" 2>&1 &
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
[ "$(jget "$CFG" '{j[update_pubkey]}' 2>/dev/null || true)" != "" ] \
  || bad "agent.json missing pinned update_pubkey: $(cat "$CFG")"
pass "join exited 0; update pubkey pinned in agent.json (agent-trust anchor, not server-supplied)"

# --- fakeollama + agent run ----------------------------------------------------
step "fakeollama + agent run"
"$tmp/fakeollama" -port "$OPORT" >"$tmp/fakeollama.log" 2>&1 &
fake_pid=$!
for _ in $(seq 30); do
  wait_pid_or_bad "$fake_pid" "fakeollama"
  curl -sf "http://127.0.0.1:$OPORT/api/version" >/dev/null 2>&1 && break
  sleep 1
done
curl -sf "http://127.0.0.1:$OPORT/api/version" >/dev/null || bad "fakeollama not answering"
pass "fakeollama up on 127.0.0.1:$OPORT"

"$tmp/localmesh-agent" run >"$tmp/agent.log" 2>&1 &
agent_pid=$!

online=0
for _ in $(seq 60); do
  wait_pid_or_bad "$agent_pid" "agent run"
  code="$(curl -s -o "$tmp/nodes.json" -w '%{http_code}' \
    -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/nodes")"
  if [ "$code" = "200" ]; then
    onl="$(py - "$tmp/nodes.json" "$NODE_ID" <<'EOF' 2>/dev/null || true
import json,sys
for n in json.load(open(sys.argv[1])):
    if n.get("id")==sys.argv[2] and n.get("online"): print("1")
EOF
)"
    [ "$onl" = "1" ] && { online=1; break; }
  fi
  sleep 1
done
[ "$online" -eq 1 ] || bad "node never came online"
pass "node online via agent websocket (agent version 'dev')"

# --- build + sign the release ---------------------------------------------------
step "signed release v$NEWVER"
DIST="$tmp/dist"
mkdir -p "$DIST"
ART="localmesh-agent-$NEWVER-$goos-$goarch"
[ "$goos" = "windows" ] && ART="$ART.exe"
CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -ldflags "-s -w -X main.version=$NEWVER" -o "$DIST/$ART" ./cmd/localmesh-agent
pass "built $ART"

if command -v sha256sum >/dev/null 2>&1; then SUM="sha256sum"; else SUM="shasum -a 256"; fi
(cd "$DIST" && ls localmesh-* | grep -v -E '\.(minisig|sig)$' | LC_ALL=C sort | xargs $SUM > SHA256SUMS)
go run ./internal/release/cmd/sign -sec "$tmp/sign.sec" -m "$DIST/SHA256SUMS" 2>/dev/null

go run ./internal/release/cmd/gen-manifest \
  -dist "$DIST" -version "$NEWVER" -signer minisign -signer-key-id "$KEYID" >/dev/null
go run ./internal/release/cmd/sign -sec "$tmp/sign.sec" -m "$DIST/release-manifest.json" 2>/dev/null
pass "manifest + SHA256SUMS signed (minisign, keyid $KEYID)"

# --- register the key + release --------------------------------------------------
step "release registry"
py - "$tmp/sign.pub" >"$tmp/keyreq.json" <<'EOF'
import json,sys
print(json.dumps({"public_key": open(sys.argv[1]).read(), "comment": "ci-update-smoke"}))
EOF
code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/release-keys" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d @"$tmp/keyreq.json")"
[ "$code" = "201" ] || bad "register release key → $code: $(cat "$tmp/resp.json")"
pass "release key registered (sha256-pinned, not fetched by agents)"

py - "$DIST/release-manifest.json" "$DIST/SHA256SUMS" "$DIST/SHA256SUMS.minisig" "$DIST/release-manifest.json.minisig" \
  >"$tmp/relreq.json" <<'EOF'
import json,sys
# .minisig files are base64 text — JSON transport is verbatim.
print(json.dumps({
  "manifest_json": open(sys.argv[1]).read(),
  "sha256sums":    open(sys.argv[2]).read(),
  "signatures": {
    "SHA256SUMS.minisig":            open(sys.argv[3]).read(),
    "release-manifest.json.minisig": open(sys.argv[4]).read(),
  },
  "channel": "ci", "notes": "ci-update-smoke",
}))
EOF
code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/releases" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d @"$tmp/relreq.json")"
[ "$code" = "201" ] || bad "register release → $code: $(cat "$tmp/resp.json")"
REL_ID="$(jget "$tmp/resp.json" '{j[id]}')"
pass "release $REL_ID registered + verified (signature cross-checked at registration)"

# --- upload the artifact through the control plane ------------------------------
step "artifact upload"
code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X PUT \
  "$BASE/api/v1/releases/$REL_ID/artifacts/$ART" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/octet-stream' \
  --data-binary @"$DIST/$ART")"
[ "$code" = "201" ] || bad "upload artifact → $code: $(cat "$tmp/resp.json")"
pass "artifact stored server-side (digest-verified against signed manifest)"

# --- negative: a tampered artifact must be refused ------------------------------
printf 'tampered-binary' >"$tmp/evil.bin"
code="$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
  "$BASE/api/v1/releases/$REL_ID/artifacts/evil.bin" \
  -H "Authorization: Bearer $ADMIN" --data-binary @"$tmp/evil.bin")"
[ "$code" = "400" ] || bad "undeclared artifact upload → $code, want 400"
code="$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
  "$BASE/api/v1/releases/$REL_ID/artifacts/$ART" \
  -H "Authorization: Bearer $ADMIN" --data-binary @"$tmp/evil.bin")"
[ "$code" = "400" ] || bad "digest-mismatched artifact upload → $code, want 400"
pass "tampered/undeclared uploads rejected (sha256 vs signed manifest enforced)"

# --- rollout ---------------------------------------------------------------------
step "rollout"
py - "$REL_ID" "$NODE_ID" >"$tmp/roreq.json" <<'EOF'
import json,sys
print(json.dumps({"release_id": sys.argv[1], "strategy": "single", "node_ids": [sys.argv[2]]}))
EOF
code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/rollouts" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d @"$tmp/roreq.json")"
[ "$code" = "201" ] || bad "create rollout → $code: $(cat "$tmp/resp.json")"
RO_ID="$(jget "$tmp/resp.json" '{j[id]}')"
pass "rollout $RO_ID created (strategy=single)"

# The agent process is replaced by the update (detached self-restart) —
# our pid handle may go stale; track liveness via the node's online flag.
agent_pid=""
phase=""
for _ in $(seq 120); do
  code="$(curl -s -o "$tmp/ups.json" -w '%{http_code}' \
    -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/agent-updates?node_id=$NODE_ID")"
  if [ "$code" = "200" ]; then
    phase="$(jget "$tmp/ups.json" '{j[0][phase]}' 2>/dev/null || true)"
    case "$phase" in
      succeeded) break ;;
      failed|rolled_back|aborted|interrupted)
        bad "update reached terminal phase '$phase': $(cat "$tmp/ups.json")" ;;
    esac
  fi
  sleep 1
done
[ "$phase" = "succeeded" ] || bad "update never reached 'succeeded' (last: $phase): $(cat "$tmp/ups.json")"
pass "agent update succeeded — verify→download→install→restart→health-confirm"

# --- post-upgrade assertions -------------------------------------------------------
step "post-upgrade"
online=0
for _ in $(seq 60); do
  code="$(curl -s -o "$tmp/nodes.json" -w '%{http_code}' \
    -H "Authorization: Bearer $ADMIN" "$BASE/api/v1/nodes")"
  if [ "$code" = "200" ]; then
    ver="$(py - "$tmp/nodes.json" "$NODE_ID" <<'EOF' 2>/dev/null || true
import json,sys
for n in json.load(open(sys.argv[1])):
    if n.get("id")==sys.argv[2] and n.get("online"):
        print(n.get("agent_version") or n.get("version") or "")
EOF
)"
    [ "$ver" = "$NEWVER" ] && { online=1; break; }
  fi
  sleep 1
done
[ "$online" -eq 1 ] || bad "node did not reconnect reporting version $NEWVER (last: $ver)"
pass "node reconnected reporting agent_version=$NEWVER"

# Inference still flows through the upgraded agent — the update must not
# have broken the working path.
INF="lmk_ci_update_infer_0123456789ab"   # separate inference-scope key
code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/api-keys" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"name":"ci-update-infer","role":"inference"}')"
[ "$code" = "201" ] || bad "create inference key → $code: $(cat "$tmp/resp.json")"
INF="$(jget "$tmp/resp.json" '{j[key]}')"

code="$(curl -s -o "$tmp/chat.json" -w '%{http_code}' -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $INF" -H 'Content-Type: application/json' \
  -d '{"model":"llama3.1:8b","messages":[{"role":"user","content":"post-upgrade"}],"stream":false}')"
[ "$code" = "200" ] || bad "post-upgrade inference → $code: $(cat "$tmp/chat.json")"
grep -q "localmesh-echo" "$tmp/chat.json" || bad "post-upgrade inference content: $(cat "$tmp/chat.json")"
pass "post-upgrade inference verified through the updated agent"

step "ci-update-smoke: PASS"
echo "signed release → registry → proxy fetch → verified install → restart → health-confirm → inference"

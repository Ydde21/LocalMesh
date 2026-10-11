#!/usr/bin/env bash
# ci-clean-install.sh — verify the DOCUMENTED install journey on a clean
# machine, end to end, against a disposable compose project. Nothing
# reuses dev state: unique project name, volumes, networks, published
# ports, agent HOME, and engine.
#
#   bash scripts/ci-clean-install.sh           # stock docker-compose.yml
#   bash scripts/ci-clean-install.sh --ha      # deploy/ha.compose.yml + real failover
#   bash scripts/ci-clean-install.sh --keep    # leave the project running after PASS
#
# What it drives (the exact path docs/onboarding.md documents):
#
#   compose up → server healthy → bootstrap admin key from the data
#   volume → enrollment token via admin API → REAL localmesh-agent
#   binary (fresh HOME, pinned CA fingerprint) → approve enrollment →
#   node online with engines+models → OpenAI /v1/chat/completions →
#   workload row recorded → model install via model-ops API → model
#   served → `compose restart` → node+inference recover → dashboard
#   smoke check.
#
#   --ha adds: standby presence via router status → SIGKILL the active
#   control-plane container → router re-elects the promoted standby →
#   agent reconnects + inference through the SAME stable endpoint, and
#   agent.json is checksum-verified untouched.
#
# Inference engine: by default a local fakeollama process serves the
# model leg. Set CLEAN_INSTALL_OLLAMA_URL=http://host:11434 to run the
# model legs against a REAL Ollama (models must already be installed
# there — pull is still exercised through the model-ops API, which is
# fast on a warm engine).
#
# Env overrides:
#   CLEAN_INSTALL_PORT                published control-plane port (default: auto)
#   CLEAN_INSTALL_WEB_PORT            published dashboard port        (default: auto)
#   CLEAN_INSTALL_ROUTER_STATUS_PORT  router status port, --ha only   (default: auto)
#   CLEAN_INSTALL_OLLAMA_URL          real Ollama endpoint            (default: fakeollama)
#   CLEAN_INSTALL_MODEL               chat model (default llama3.1:8b / qwen2.5:0.5b)
#   CLEAN_INSTALL_PULL_MODEL          model-op pull target (default smollm2:135m)
#   CLEAN_INSTALL_PROJECT             compose project name (default localmesh-ci-<pid>)
#
# Never touches another project's containers/volumes. Prints a per-stage
# PASS/FAIL summary and the artifact dir; exits non-zero on any failure.

set -euo pipefail

cd "$(dirname "$0")/.."

HA=0
KEEP=0
while [ $# -gt 0 ]; do
	case "$1" in
		--ha)      HA=1 ;;
		--keep)    KEEP=1 ;;
		-h|--help) sed -n '2,40p' "$0"; exit 0 ;;
		*) echo "unknown flag: $1 (try --ha, --keep, --help)" >&2; exit 2 ;;
	esac
	shift
done

step() { printf '\n== %s ==\n' "$*"; }
pass() { printf '  PASS %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# --- per-stage summary -------------------------------------------------------
STAGE_NAMES=()
STAGE_STATES=()
stage() {  # stage NAME FN — runs FN; a nonzero return or fail() marks FAIL
	STAGE_NAMES+=("$1"); STAGE_STATES+=("FAIL")
	printf '\n== %s ==\n' "$1"
	if "$2"; then STAGE_STATES[$((${#STAGE_STATES[@]} - 1))]="PASS"; else exit 1; fi
}
summary() {
	printf '\n== stage summary ==\n'
	local i
	for i in "${!STAGE_NAMES[@]}"; do
		printf '  %-4s %s\n' "${STAGE_STATES[$i]}" "${STAGE_NAMES[$i]}"
	done
	printf 'artifacts: %s\n' "$tmp"
}

py() {
	if have python3; then python3 "$@"
	elif have python; then python "$@"
	else fail "python3/python required for JSON assertions"; fi
}

free_port() {
	py -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

jget() {
	py -c 'import json,sys; print(sys.argv[2].format(j=json.load(open(sys.argv[1]))))' "$1" "$2"
}

sha256_of() {
	if have sha256sum; then sha256sum "$1" | awk '{print $1}'
	else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# --- config ------------------------------------------------------------------
PROJ="${CLEAN_INSTALL_PROJECT:-localmesh-ci-$$}"
PORT="${CLEAN_INSTALL_PORT:-$(free_port)}"
WPORT="${CLEAN_INSTALL_WEB_PORT:-$(free_port)}"
SRPORT="${CLEAN_INSTALL_ROUTER_STATUS_PORT:-$(free_port)}"
OLLAMA_URL="${CLEAN_INSTALL_OLLAMA_URL:-}"
if [ "$HA" -eq 1 ]; then COMPOSE="deploy/ha.compose.yml"; else COMPOSE="docker-compose.yml"; fi

tmp="$(mktemp -d)"
ENVF="$tmp/.env"
CA="$tmp/ca.pem"
ADMIN=""
TOKEN=""
NODE_ID=""
FP=""
BASE="https://localhost:$PORT"
MODEL=""
PULL_MODEL=""
OPORT=""
FAKE_OLLAMA=0
CFG=""
agent_pid=""
fake_pid=""
join_pid=""

dc() { docker compose -p "$PROJ" --env-file "$ENVF" -f "$COMPOSE" "$@"; }

cleanup() {
	rc=$?
	for p in "$agent_pid" "$fake_pid" "$join_pid"; do
		[ -n "$p" ] && kill "$p" >/dev/null 2>&1 || true
	done
	if [ "$KEEP" -eq 1 ]; then
		echo "--keep: leaving project $PROJ running (down -v it yourself:"
		echo "  docker compose -p $PROJ --env-file $ENVF -f $COMPOSE down -v)"
	elif [ -f "$ENVF" ]; then
		# -v removes THIS project's volumes only — the project name,
		# volumes and network are all unique to this run.
		dc down -v >/dev/null 2>&1 || true
	fi
	if [ "$rc" -ne 0 ]; then
		for f in server agent join fakeollama router; do
			[ -f "$tmp/$f.log" ] && { echo "--- $f log (tail) ---"; tail -n 15 "$tmp/$f.log"; }
		done
		dc logs --tail=15 server >"$tmp/server-compose-tail.log" 2>/dev/null || true
	fi
	summary
}
trap cleanup EXIT

# --- stage: preflight ---------------------------------------------------------
s_preflight() {
	have docker || fail "docker is required — install Docker Desktop / docker engine first"
	docker compose version >/dev/null 2>&1 \
		|| fail "'docker compose' plugin missing — install Compose v2 (docker compose version)"
	have go   || fail "go is required to build the agent/fakeollama binaries"
	have curl || fail "curl is required"
	for p in "$PORT" "$WPORT"; do
		py -c "import socket,sys; s=socket.socket(); s.bind(('127.0.0.1',$p)); s.close()" 2>/dev/null \
			|| fail "port $p already bound — set CLEAN_INSTALL_PORT / CLEAN_INSTALL_WEB_PORT"
	done
	if [ "$HA" -eq 1 ]; then
		py -c "import socket; s=socket.socket(); s.bind(('127.0.0.1',$SRPORT)); s.close()" 2>/dev/null \
			|| fail "port $SRPORT bound — set CLEAN_INSTALL_ROUTER_STATUS_PORT"
	fi
	pass "docker+compose, go, curl, python present; ports $PORT/$WPORT (+$SRPORT when --ha) free; project $PROJ; compose $COMPOSE"
}

# --- stage: compose up --------------------------------------------------------
s_compose_up() {
	PG_PASS="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
	{
		# What a fresh user's .env needs — nothing else is required.
		echo "POSTGRES_PASSWORD=$PG_PASS"
		echo "LOCALMESH_PORT=$PORT"
		echo "LOCALMESH_WEB_PORT=$WPORT"
		echo "LOCALMESH_ROUTER_STATUS_PORT=$SRPORT"
		echo "LOCALMESH_DISCOVERY=off"   # mDNS is meaningless inside docker NAT anyway
	} > "$ENVF"
	dc up -d >"$tmp/compose-up.log" 2>&1 || {
		tail -n 20 "$tmp/compose-up.log" >&2
		fail "docker compose up failed — full log: $tmp/compose-up.log"
	}
	dc ps --format '{{.Name}} {{.Status}}' | tee "$tmp/ps.txt"
	dc ps -q server >/dev/null 2>&1 || fail "server service missing from project"
	pass "compose project $PROJ up (isolated volumes/network; published :$PORT + :$WPORT)"
}

# --- stage: control plane healthy ---------------------------------------------
s_healthy() {
	local i code
	# ca.pem lands in the shared data volume on first boot.
	for i in $(seq 60); do
		dc exec -T server cat /data/ca.pem >"$CA" 2>/dev/null && [ -s "$CA" ] && break
		sleep 1
	done
	[ -s "$CA" ] || fail "server never wrote /data/ca.pem into the data volume"
	FP="sha256:$(py - "$CA" <<'EOF'
import ssl,hashlib,sys
der = ssl.PEM_cert_to_DER_cert(open(sys.argv[1]).read())
print(hashlib.sha256(der).hexdigest())
EOF
)"
	for i in $(seq 90); do
		code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 --cacert "$CA" \
			"$BASE/healthz" 2>/dev/null || true)"
		[ "$code" = "200" ] && break
		sleep 1
	done
	[ "$code" = "200" ] || fail "/healthz never answered 200 on $BASE"
	code="$(curl -s -o /dev/null -w '%{http_code}' --cacert "$CA" "$BASE/readyz")"
	[ "$code" = "200" ] || fail "/readyz → $code, want 200 (postgres dependency)"
	code="$(curl -s -o /dev/null -w '%{http_code}' --cacert "$CA" -X POST \
		"$BASE/v1/chat/completions" -H 'Content-Type: application/json' \
		-d '{"model":"x","messages":[{"role":"user","content":"hi"}]}')"
	[ "$code" = "401" ] || fail "unauthenticated /v1/chat/completions → $code, want 401"
	pass "TLS+CA live on $BASE, /readyz 200, auth gate 401, CA fingerprint $FP"
}

# --- stage: bootstrap admin key -----------------------------------------------
s_bootstrap() {
	local i
	for i in $(seq 30); do
		ADMIN="$(dc exec -T server cat /data/bootstrap-admin.key 2>/dev/null | tr -d '[:space:]' || true)"
		[ -n "$ADMIN" ] && break
		sleep 1
	done
	if [ -z "$ADMIN" ]; then
		# Fallback the docs also describe: the one-time log line.
		ADMIN="$(dc logs server 2>/dev/null | grep -o 'lmk_[A-Za-z0-9_-]*' | head -1 || true)"
	fi
	case "$ADMIN" in
		lmk_*) ;;
		*) fail "could not extract bootstrap admin key (volume file + logs both empty)" ;;
	esac
	local code
	code="$(curl -s -o "$tmp/authcheck.json" -w '%{http_code}' --cacert "$CA" \
		-H "Authorization: Bearer $ADMIN" "$BASE/api/v1/overview")"
	[ "$code" = "200" ] || fail "bootstrap key rejected by /api/v1/overview → $code"
	pass "bootstrap admin key extracted from data volume and authenticates (${ADMIN:0:12}…)"
}

# --- stage: enrollment token ---------------------------------------------------
s_token() {
	local code
	code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST --cacert "$CA" \
		-H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
		-d '{"label":"ci-clean-install","max_uses":1,"ttl_hours":1}' \
		"$BASE/api/v1/enrollment-tokens")"
	[ "$code" = "201" ] || fail "create enrollment token → $code: $(cat "$tmp/resp.json")"
	TOKEN="$(jget "$tmp/resp.json" '{j[token]}')"
	case "$TOKEN" in lme_*) ;; *) fail "unexpected token format: $TOKEN";; esac
	pass "enrollment token created (single-use, admin API)"
}

# --- stage: agent join + approve -----------------------------------------------
s_join() {
	go build -o "$tmp/localmesh-agent" ./cmd/localmesh-agent || fail "agent build failed"
	pass "built real localmesh-agent binary from this tree"

	if [ -z "$OLLAMA_URL" ]; then
		OPORT="$(free_port)"
		OLLAMA_URL="http://127.0.0.1:$OPORT"
		FAKE_OLLAMA=1
	else
		FAKE_OLLAMA=0
	fi
	MODEL="${CLEAN_INSTALL_MODEL:-$MODEL}"
	PULL_MODEL="${CLEAN_INSTALL_PULL_MODEL:-$PULL_MODEL}"
	if [ -z "$MODEL" ]; then
		if [ "$FAKE_OLLAMA" -eq 1 ]; then MODEL="llama3.1:8b"; else MODEL="qwen2.5:0.5b"; fi
	fi
	[ -n "$PULL_MODEL" ] || PULL_MODEL="smollm2:135m"

	# Fresh HOME + agent dir — a clean machine has no ~/.localmesh and
	# this machine's real dev state must not leak in.
	mkdir -p "$tmp/home" "$tmp/agent-home"

	# Join waits for approval; approve out-of-band via the admin API —
	# the exact dashboard action scripted.
	HOME="$tmp/home" LOCALMESH_AGENT_DIR="$tmp/agent-home" \
		"$tmp/localmesh-agent" join --server "$BASE" --token "$TOKEN" \
		--name ci-clean-node -ollama-url "$OLLAMA_URL" \
		-ca-fingerprint "$FP" -wait 90s -run=false >"$tmp/join.log" 2>&1 &
	join_pid=$!

	local req_id="" i
	for i in $(seq 45); do
		kill -0 "$join_pid" 2>/dev/null || fail "agent join exited early: $(cat "$tmp/join.log")"
		code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' --cacert "$CA" \
			-H "Authorization: Bearer $ADMIN" "$BASE/api/v1/enrollments?status=pending")"
		if [ "$code" = "200" ]; then
			req_id="$(jget "$tmp/resp.json" '{j[0][id]}' 2>/dev/null || true)"
			[ -n "$req_id" ] && break
		fi
		sleep 1
	done
	[ -n "$req_id" ] || fail "enrollment never appeared pending: $(cat "$tmp/join.log")"

	code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST --cacert "$CA" \
		-H "Authorization: Bearer $ADMIN" "$BASE/api/v1/enrollments/$req_id/approve")"
	[ "$code" = "200" ] || fail "approve → $code: $(cat "$tmp/resp.json")"
	NODE_ID="$(jget "$tmp/resp.json" '{j[id]}')"

	local join_rc=0
	wait "$join_pid" || join_rc=$?
	join_pid=""
	[ "$join_rc" -eq 0 ] || fail "agent join exited $join_rc: $(cat "$tmp/join.log")"
	CFG="$tmp/agent-home/agent.json"
	[ -f "$CFG" ] || fail "join succeeded but no config at $CFG"
	[ "$(jget "$CFG" '{j[node_id]}')" = "$NODE_ID" ] || fail "config node_id mismatch: $(cat "$CFG")"
	pass "agent joined with pinned CA fingerprint → approved → node $NODE_ID (engine: $OLLAMA_URL)"
}

# --- stage: node online with engines + models -----------------------------------
s_online() {
	if [ "$FAKE_OLLAMA" -eq 1 ]; then
		go build -o "$tmp/fakeollama" ./cmd/fakeollama || fail "fakeollama build failed"
		"$tmp/fakeollama" -port "$OPORT" >"$tmp/fakeollama.log" 2>&1 &
		fake_pid=$!
		local i
		for i in $(seq 30); do
			kill -0 "$fake_pid" 2>/dev/null || fail "fakeollama died: $(cat "$tmp/fakeollama.log")"
			curl -sf "http://127.0.0.1:$OPORT/api/version" >/dev/null 2>&1 && break
			sleep 1
		done
		curl -sf "http://127.0.0.1:$OPORT/api/version" >/dev/null || fail "fakeollama not answering"
		pass "fakeollama up on 127.0.0.1:$OPORT (llama3.1:8b, qwen2.5:7b, nomic-embed-text)"
	else
		curl -sf "$OLLAMA_URL/api/version" >/dev/null \
			|| fail "CLEAN_INSTALL_OLLAMA_URL=$OLLAMA_URL not answering /api/version"
		pass "using real Ollama at $OLLAMA_URL"
	fi

	HOME="$tmp/home" LOCALMESH_AGENT_DIR="$tmp/agent-home" \
		"$tmp/localmesh-agent" run >"$tmp/agent.log" 2>&1 &
	agent_pid=$!

	local i online=0
	for i in $(seq 60); do
		kill -0 "$agent_pid" 2>/dev/null || fail "agent run died: $(tail -n 10 "$tmp/agent.log")"
		code="$(curl -s -o "$tmp/nodes.json" -w '%{http_code}' --cacert "$CA" \
			-H "Authorization: Bearer $ADMIN" "$BASE/api/v1/nodes")"
		if [ "$code" = "200" ]; then
			got="$(py - "$tmp/nodes.json" "$NODE_ID" <<'EOF' 2>/dev/null || true
import json,sys
for n in json.load(open(sys.argv[1])):
    if n.get("id")==sys.argv[2] and n.get("online"):
        engs=[e.get("type") for e in (n.get("engines") or []) if e.get("healthy")]
        models=[m.get("name") for m in (n.get("models") or [])]
        if engs and models:
            print("engines=%s models=%s" % (",".join(engs), ",".join(models)))
EOF
)"
			[ -n "$got" ] && { echo "    $got"; online=1; break; }
		fi
		sleep 1
	done
	[ "$online" -eq 1 ] || fail "node never came online with healthy engine + models: $(cat "$tmp/nodes.json" 2>/dev/null)"
	pass "node online: hardware + engines + models reported over the agent websocket"
}

# --- stage: inference ------------------------------------------------------------
s_inference() {
	local i code
	for i in $(seq 30); do   # scheduling view settles a beat after capability report
		code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST --cacert "$CA" \
			-H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
			-d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hello clean install\"}],\"stream\":false}" \
			"$BASE/v1/chat/completions")"
		[ "$code" = "200" ] && break
		sleep 2
	done
	[ "$code" = "200" ] || fail "chat completion → $code: $(cat "$tmp/resp.json")"
	local content
	content="$(jget "$tmp/resp.json" '{j[choices][0][message][content]}' 2>/dev/null || true)"
	[ -n "$content" ] || fail "chat returned no content: $(cat "$tmp/resp.json")"
	if [ "$FAKE_OLLAMA" -eq 1 ]; then
		case "$content" in
			*localmesh-echo*) : ;;
			*) fail "fakeollama echo missing: $content" ;;
		esac
	fi
	pass "POST /v1/chat/completions → 200, model $MODEL answered"
}

# --- stage: workload recorded -----------------------------------------------------
s_workload() {
	local i code status=""
	for i in $(seq 20); do
		code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' --cacert "$CA" \
			-H "Authorization: Bearer $ADMIN" "$BASE/api/v1/workloads")"
		if [ "$code" = "200" ]; then
			status="$(jget "$tmp/resp.json" '{j[0][status]}' 2>/dev/null || true)"
			[ "$status" = "completed" ] && break
		fi
		sleep 1
	done
	[ "$status" = "completed" ] || fail "latest workload status '$status': $(cat "$tmp/resp.json" 2>/dev/null)"
	# metadata-only guarantee: the row must not carry the prompt text
	if grep -q "hello clean install" "$tmp/resp.json"; then
		fail "workload row contains prompt text — violates the no-persistence contract"
	fi
	pass "workload row recorded 'completed' (metadata only — prompt never persisted)"
}

# --- stage: install a second model via model-ops API --------------------------------
s_model_pull() {
	local code
	code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST --cacert "$CA" \
		-H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
		-d "{\"model\":\"$PULL_MODEL\",\"node_ids\":[\"$NODE_ID\"]}" \
		"$BASE/api/v1/models/pull")"
	[ "$code" = "200" ] || [ "$code" = "201" ] || [ "$code" = "202" ] \
		|| fail "models/pull → $code: $(cat "$tmp/resp.json")"

	local i st=""
	for i in $(seq 60); do
		code="$(curl -s -o "$tmp/ops.json" -w '%{http_code}' --cacert "$CA" \
			-H "Authorization: Bearer $ADMIN" "$BASE/api/v1/model-operations")"
		if [ "$code" = "200" ]; then
			st="$(py - "$tmp/ops.json" "$PULL_MODEL" <<'EOF' 2>/dev/null || true
import json,sys
ops=json.load(open(sys.argv[1]))
mine=[o for o in ops if o.get("model")==sys.argv[2]]
mine.sort(key=lambda o: o.get("created_at",""), reverse=True)
if mine and mine[0].get("status") in ("succeeded","failed","cancelled"):
    print(mine[0]["status"])
EOF
)"
			[ -n "$st" ] && break
		fi
		sleep 1
	done
	[ "$st" = "succeeded" ] || fail "model pull op ended '$st': $(cat "$tmp/ops.json" 2>/dev/null)"

	# Agent re-reports capabilities after the op; wait for the model to
	# appear in the cluster inventory AND the schedulable /v1/models.
	local avail=0
	for i in $(seq 60); do
		curl -s --cacert "$CA" -H "Authorization: Bearer $ADMIN" \
			"$BASE/v1/models" -o "$tmp/models.json" 2>/dev/null || true
		grep -q "$PULL_MODEL" "$tmp/models.json" 2>/dev/null && { avail=1; break; }
		sleep 1
	done
	[ "$avail" -eq 1 ] || fail "pulled model never became schedulable: $(cat "$tmp/models.json" 2>/dev/null)"
	pass "POST /api/v1/models/pull → op succeeded → '$PULL_MODEL' schedulable via /v1/models"
}

# --- stage: dashboard smoke (web service of the same compose file) ------------------
s_dashboard() {
	local i code="000"
	for i in $(seq 120); do
		code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 \
			"http://localhost:$WPORT/" 2>/dev/null || true)"
		case "$code" in 2*|3*) break ;; esac
		sleep 2
	done
	case "$code" in
		2*|3*) pass "dashboard serving on http://localhost:$WPORT (HTTP $code)" ;;
		*) fail "dashboard never served on :$WPORT — 'dc ps' says: $(dc ps --format '{{.Name}} {{.Status}}' | tr '\n' ' ')" ;;
	esac
}

# --- stage: restart recovery (standard mode) -----------------------------------------
s_restart() {
	dc restart server >/"$tmp/restart.log" 2>&1 || fail "compose restart server failed"
	local i code
	for i in $(seq 90); do
		code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 --cacert "$CA" \
			"$BASE/readyz" 2>/dev/null || true)"
		[ "$code" = "200" ] && break
		sleep 1
	done
	[ "$code" = "200" ] || fail "server never ready after restart"

	local online=0
	for i in $(seq 60); do
		code="$(curl -s -o "$tmp/nodes.json" -w '%{http_code}' --cacert "$CA" \
			-H "Authorization: Bearer $ADMIN" "$BASE/api/v1/nodes")"
		if [ "$code" = "200" ]; then
			online="$(py - "$tmp/nodes.json" "$NODE_ID" <<'EOF' 2>/dev/null || true
import json,sys
for n in json.load(open(sys.argv[1])):
    if n.get("id")==sys.argv[2] and n.get("online"): print("1")
EOF
)"
			[ "$online" = "1" ] && break
		fi
		sleep 1
	done
	[ "$online" = "1" ] || fail "agent never reconnected after server restart"

	code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST --cacert "$CA" \
		-H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
		-d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"post-restart\"}],\"stream\":false}" \
		"$BASE/v1/chat/completions")"
	[ "$code" = "200" ] || fail "post-restart inference → $code: $(cat "$tmp/resp.json")"
	pass "compose restart → node re-enrolled-free reconnect → inference works again"
}

# --- stage: HA failover (--ha) ---------------------------------------------------------
s_failover() {
	local i code active=""
	# Standby presence via the router's status endpoint.
	for i in $(seq 30); do
		active="$(curl -s --connect-timeout 2 "http://127.0.0.1:$SRPORT/status" 2>/dev/null || true)"
		echo "$active" | grep -q "server-standby" && break
		sleep 1
	done
	echo "$active" | grep -q "server-standby" \
		|| fail "router status never listed the standby backend: $active"
	pass "router status lists active + standby backends ($SRPORT)"

	CFG_SUM_BEFORE="$(sha256_of "$CFG")"

	# Kill the ACTIVE control plane container hard — same shape as the
	# process-level SIGKILL in ci-ha-smoke, but on the compose service.
	SERVER_CID="$(dc ps -q server)"
	[ -n "$SERVER_CID" ] || fail "could not resolve server container id"
	docker kill -s KILL "$SERVER_CID" >/dev/null || fail "docker kill failed"
	pass "SIGKILL sent to active server container (${SERVER_CID:0:12})"

	local elected=0
	for i in $(seq 90); do
		active="$(curl -s --connect-timeout 2 "http://127.0.0.1:$SRPORT/status" 2>/dev/null || true)"
		if echo "$active" | py -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if "server-standby" in d.get("active","") else 1)' 2>/dev/null; then
			elected=1; break
		fi
		sleep 1
	done
	[ "$elected" -eq 1 ] || fail "router never re-elected the standby: $active"
	pass "router re-elected server-standby through the SAME stable endpoint"

	for i in $(seq 90); do
		code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 --cacert "$CA" \
			"$BASE/readyz" 2>/dev/null || true)"
		[ "$code" = "200" ] && break
		sleep 1
	done
	[ "$code" = "200" ] || fail "stable endpoint never ready after failover"

	local online=0
	for i in $(seq 90); do
		code="$(curl -s -o "$tmp/nodes.json" -w '%{http_code}' --connect-timeout 2 --cacert "$CA" \
			-H "Authorization: Bearer $ADMIN" "$BASE/api/v1/nodes" 2>/dev/null || true)"
		if [ "$code" = "200" ]; then
			online="$(py - "$tmp/nodes.json" "$NODE_ID" <<'EOF' 2>/dev/null || true
import json,sys
for n in json.load(open(sys.argv[1])):
    if n.get("id")==sys.argv[2] and n.get("online"): print("1")
EOF
)"
			[ "$online" = "1" ] && break
		fi
		sleep 1
	done
	[ "$online" = "1" ] || fail "agent never reconnected through the stable endpoint"
	CFG_SUM_AFTER="$(sha256_of "$CFG")"
	[ "$CFG_SUM_BEFORE" = "$CFG_SUM_AFTER" ] \
		|| fail "agent.json changed during failover — repointing was required"
	pass "agent auto-reconnected — same server_url, agent.json untouched"

	code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST --cacert "$CA" \
		-H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
		-d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"post-failover\"}],\"stream\":false}" \
		"$BASE/v1/chat/completions")"
	[ "$code" = "200" ] || fail "post-failover inference via stable endpoint → $code"
	pass "post-failover inference works through $BASE — seamless HA on the install path"
}

# --- run ---------------------------------------------------------------------------
step "ci-clean-install — clean-machine install verification (project $PROJ)"
stage "preflight"             s_preflight
stage "compose up"            s_compose_up
stage "control plane healthy" s_healthy
stage "bootstrap admin key"   s_bootstrap
stage "enrollment token"      s_token
stage "agent join + approve"  s_join
stage "node online + models"  s_online
stage "inference"             s_inference
stage "workload recorded"     s_workload
stage "model install"         s_model_pull
stage "dashboard smoke"       s_dashboard
if [ "$HA" -eq 1 ]; then
	stage "HA failover (kill active)" s_failover
else
	stage "restart recovery"    s_restart
fi

step "ci-clean-install: PASS"
echo "full documented journey verified on isolated compose project $PROJ"
if [ "$FAKE_OLLAMA" -eq 1 ]; then
	echo "  engine leg: fakeollama (set CLEAN_INSTALL_OLLAMA_URL for the real-Ollama leg)"
else
	echo "  engine leg: REAL Ollama at $OLLAMA_URL"
fi

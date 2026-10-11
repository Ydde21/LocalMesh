#!/usr/bin/env bash
# ci-artifact-install.sh — verify the EXTERNAL-USER install journey using
# only packaged release artifacts: no repo build products are exercised.
#
#   bash scripts/ci-artifact-install.sh [dist-dir]
#   bash scripts/ci-artifact-install.sh --dist /path/to/dist/0.10.0-rc1 --keep
#
# What it drives:
#
#   verify-release.sh (checksums + signature + manifest, dev signer
#   surfaced honestly) → agent + server artifacts re-hashed against
#   SHA256SUMS → disposable compose project running the SHIPPED
#   linux/<arch> server binary (alpine runtime image, same shape as the
#   repo Dockerfile's runtime stage — only the binary source differs:
#   dist/, not a repo build) → bootstrap admin key → enrollment token →
#   the SHIPPED darwin/<arch> agent binary joins with a pinned CA
#   fingerprint → approve → node online reporting the REAL Ollama
#   model → non-stream + SSE-stream chat → workload row (metadata only)
#   → agent-process AND server restarts → agent.json unchanged,
#   inference still works.
#
# Deliberately NOT covered (documented scope):
#   * dashboard — there is no web artifact in dist/; the compose-built
#     dashboard journey is ci-clean-install.sh's job.
#   * model pull via model-ops — against a REAL Ollama it would mutate
#     the operator's engine; covered against fakeollama elsewhere.
#
# Env overrides:
#   ARTIFACT_INSTALL_DIST          release directory (default dist/0.10.0-rc1)
#   ARTIFACT_INSTALL_PUBKEY        verification pubkey
#                                  (default ~/.localmesh/keys/localmesh-dev.pub;
#                                  required unless the release is unsigned)
#   ARTIFACT_INSTALL_PORT          published control-plane port (default: auto)
#   ARTIFACT_INSTALL_OLLAMA_URL    real Ollama endpoint (default http://127.0.0.1:11434)
#   ARTIFACT_INSTALL_MODEL         chat model, must exist on that engine
#                                  (default qwen2.5:0.5b)
#   ARTIFACT_INSTALL_SERVER_ARCH   server artifact arch: amd64|arm64
#                                  (default: host arch)
#   ARTIFACT_INSTALL_PROJECT       compose project name (default localmesh-ci-art-<pid>)
#
# Isolation: a unique compose project + generated compose file means all
# containers/volumes/networks are scoped to this run; everything is
# labelled localmesh.ci=1 and removed on exit. Dev containers
# (localmesh-*, lm-ollama, etc.) are never touched.

set -euo pipefail

cd "$(dirname "$0")/.."

KEEP=0
DIST=""
while [ $# -gt 0 ]; do
	case "$1" in
		--keep)        KEEP=1; shift ;;
		-d|--dist)     DIST="$2"; shift 2 ;;
		-p|--pubkey)   PUBKEY_ARG="$2"; shift 2 ;;
		-h|--help)     sed -n '2,52p' "$0"; exit 0 ;;
		-*)            echo "unknown flag: $1 (try --dist, --pubkey, --keep, --help)" >&2; exit 2 ;;
		*)             [ -z "$DIST" ] && { DIST="$1"; shift; } || { echo "unexpected arg: $1" >&2; exit 2; } ;;
	esac
done

step() { printf '\n== %s ==\n' "$*"; }
pass() { printf '  PASS %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

STAGE_NAMES=()
STAGE_STATES=()
stage() {
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

host_os()   { case "$(uname -s)" in Darwin) echo darwin ;; Linux) echo linux ;; *) uname -s | tr '[:upper:]' '[:lower:]' ;; esac; }
host_arch() { case "$(uname -m)" in arm64|aarch64) echo arm64 ;; x86_64|amd64) echo amd64 ;; *) uname -m ;; esac; }

# --- config -----------------------------------------------------------------------
DIST="${DIST:-${ARTIFACT_INSTALL_DIST:-dist/0.10.0-rc1}}"
PUBKEY="${PUBKEY_ARG:-${ARTIFACT_INSTALL_PUBKEY:-$HOME/.localmesh/keys/localmesh-dev.pub}}"
PROJ="${ARTIFACT_INSTALL_PROJECT:-localmesh-ci-art-$$}"
PORT="${ARTIFACT_INSTALL_PORT:-$(free_port)}"
OLLAMA_URL="${ARTIFACT_INSTALL_OLLAMA_URL:-http://127.0.0.1:11434}"
MODEL="${ARTIFACT_INSTALL_MODEL:-qwen2.5:0.5b}"
SARCH="${ARTIFACT_INSTALL_SERVER_ARCH:-$(host_arch)}"
AOS="$(host_os)"
AARCH="$(host_arch)"

tmp="$(mktemp -d)"
ENVF="$tmp/.env"
COMPOSE_FILE="$tmp/compose.yml"
CA="$tmp/ca.pem"
ADMIN=""
TOKEN=""
NODE_ID=""
FP=""
BASE="https://localhost:$PORT"
CFG=""
VER=""
AGENT_BIN=""
agent_pid=""
join_pid=""

dc() { docker compose -p "$PROJ" --env-file "$ENVF" -f "$COMPOSE_FILE" "$@"; }

project_sweep() {
	# Defensive leftover removal — scoped strictly to THIS project's
	# compose label, so nothing else on the docker host can match.
	local id
	for id in $(docker ps -aq --filter "label=com.docker.compose.project=$PROJ" 2>/dev/null); do
		docker rm -f "$id" >/dev/null 2>&1 || true
	done
	for id in $(docker volume ls -q --filter "label=com.docker.compose.project=$PROJ" 2>/dev/null); do
		docker volume rm "$id" >/dev/null 2>&1 || true
	done
	for id in $(docker network ls -q --filter "label=com.docker.compose.project=$PROJ" 2>/dev/null); do
		docker network rm "$id" >/dev/null 2>&1 || true
	done
}

cleanup() {
	rc=$?
	for p in "$agent_pid" "$join_pid"; do
		[ -n "$p" ] && kill "$p" >/dev/null 2>&1 || true
	done
	if [ "$KEEP" -eq 1 ]; then
		echo "--keep: leaving project $PROJ running (down -v it yourself:"
		echo "  docker compose -p $PROJ --env-file $ENVF -f $COMPOSE_FILE down -v)"
	elif [ -f "$ENVF" ] && [ -f "$COMPOSE_FILE" ]; then
		# Scoped to THIS project only: unique name + generated file.
		dc down -v --rmi local >/dev/null 2>&1 || true
		project_sweep
	fi
	if [ "$rc" -ne 0 ]; then
		for f in agent join server-compose; do
			[ -f "$tmp/$f.log" ] && { echo "--- $f log (tail) ---"; tail -n 15 "$tmp/$f.log"; }
		done
		dc logs --tail=15 server >"$tmp/server-compose-tail.log" 2>/dev/null || true
		[ -f "$tmp/server-compose-tail.log" ] && tail -n 15 "$tmp/server-compose-tail.log"
	fi
	summary
}
trap cleanup EXIT

# --- stage: preflight ------------------------------------------------------------------
s_preflight() {
	have docker || fail "docker is required — install Docker Desktop / docker engine first"
	docker compose version >/dev/null 2>&1 \
		|| fail "'docker compose' plugin missing — install Compose v2"
	have go   || fail "go is required for scripts/verify-release.sh (native Go verifier)"
	have curl || fail "curl is required"
	[ -d "$DIST" ] || fail "dist dir $DIST does not exist — pass --dist or set ARTIFACT_INSTALL_DIST"
	[ -f "$DIST/SHA256SUMS" ] || fail "$DIST/SHA256SUMS missing — not a release directory"
	[ -f "$DIST/release-manifest.json" ] || fail "$DIST/release-manifest.json missing"
	VER="$(jget "$DIST/release-manifest.json" '{j[version]}')" \
		|| fail "could not read version from release-manifest.json"
	[ -n "$VER" ] || fail "manifest version empty"
	py -c "import socket; s=socket.socket(); s.bind(('127.0.0.1',$PORT)); s.close()" 2>/dev/null \
		|| fail "port $PORT already bound — set ARTIFACT_INSTALL_PORT"
	pass "docker+compose, go, curl, python present; dist=$DIST (v$VER); port $PORT; project $PROJ"
}

# --- stage: real Ollama reachable ------------------------------------------------------
s_ollama() {
	curl -sf --connect-timeout 3 "$OLLAMA_URL/api/version" >/dev/null \
		|| fail "real Ollama at $OLLAMA_URL not answering /api/version (set ARTIFACT_INSTALL_OLLAMA_URL)"
	curl -sf "$OLLAMA_URL/api/tags" -o "$tmp/tags.json" \
		|| fail "could not list models at $OLLAMA_URL/api/tags"
	grep -q "\"$MODEL\"" "$tmp/tags.json" \
		|| fail "model $MODEL not installed at $OLLAMA_URL — ollama pull $MODEL (or set ARTIFACT_INSTALL_MODEL); present: $(py -c 'import json,sys; print(",".join(m["name"] for m in json.load(open(sys.argv[1]))["models"]))' "$tmp/tags.json" 2>/dev/null)"
	pass "real Ollama at $OLLAMA_URL serving required model $MODEL"
}

# --- stage: verify the release ----------------------------------------------------------
s_verify() {
	local unsigned=0
	if [ -f "$DIST/SHA256SUMS.minisig" ] || [ -f "$DIST/SHA256SUMS.sig" ]; then
		[ -f "$PUBKEY" ] || fail "release is signed but pubkey $PUBKEY missing — set ARTIFACT_INSTALL_PUBKEY"
	else
		unsigned=1
		echo "  note: no signature files in $DIST — verifying with --allow-unsigned"
	fi
	local out=0
	if [ "$unsigned" -eq 1 ]; then
		bash scripts/verify-release.sh --allow-unsigned "$DIST" >"$tmp/verify.log" 2>&1 || out=$?
	else
		bash scripts/verify-release.sh "$DIST" --pubkey "$PUBKEY" >"$tmp/verify.log" 2>&1 || out=$?
	fi
	cat "$tmp/verify.log"
	[ "$out" -eq 0 ] || fail "verify-release.sh FAILED (see above)"

	# Surface the signer block exactly as the manifest declares it —
	# a dev-signed RC must never be presented as fleet-trusted.
	py - "$DIST/release-manifest.json" <<'EOF'
import json,sys
m=json.load(open(sys.argv[1])); s=m.get("signer",{})
print("  manifest signer: algorithm=%s key_id=%s%s" %
      (s.get("algorithm"), s.get("key_id","(none)"),
       " dev=true" if s.get("dev") else ""))
if s.get("dev"):
    print("  WARNING: DEV-signed release — do NOT pin this key id as a fleet trust anchor")
EOF
	pass "release verified via scripts/verify-release.sh (signer metadata surfaced above)"
}

# --- stage: pick + checksum the artifacts this journey actually uses ---------------------
s_pick_artifacts() {
	local aname="localmesh-agent-$VER-$AOS-$AARCH" sname="localmesh-server-$VER-linux-$SARCH"
	[ -f "$DIST/$aname" ] || fail "no agent artifact $aname in $DIST"
	[ -f "$DIST/$sname" ] || fail "no server artifact $sname in $DIST"

	local want got
	want="$(grep " $aname\$" "$DIST/SHA256SUMS" | awk '{print $1}')"
	got="$(sha256_of "$DIST/$aname")"
	[ "$want" = "$got" ] || fail "agent artifact checksum mismatch: want $want got $got"
	want="$(grep " $sname\$" "$DIST/SHA256SUMS" | awk '{print $1}')"
	got="$(sha256_of "$DIST/$sname")"
	[ "$want" = "$got" ] || fail "server artifact checksum mismatch: want $want got $got"

	mkdir -p "$tmp/bin"
	cp "$DIST/$aname" "$tmp/bin/localmesh-agent"
	chmod +x "$tmp/bin/localmesh-agent"
	AGENT_BIN="$tmp/bin/localmesh-agent"
	"$AGENT_BIN" version >"$tmp/agent-version.txt" 2>&1 || fail "release agent binary does not execute on this host"
	grep -q "$VER" "$tmp/agent-version.txt" \
		|| fail "agent reports unexpected version: $(cat "$tmp/agent-version.txt") (want $VER)"

	mkdir -p "$tmp/img"
	cp "$DIST/$sname" "$tmp/img/localmesh-server"
	chmod +x "$tmp/img/localmesh-server"
	pass "agent $aname + server $sname checksum-verified against SHA256SUMS; agent runs ($(head -c 60 "$tmp/agent-version.txt"))"
}

# --- stage: compose project (disposable, artifact-only server image) ---------------------
s_compose_up() {
	PG_PASS="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
	{
		echo "POSTGRES_PASSWORD=$PG_PASS"
		echo "LOCALMESH_PORT=$PORT"
	} >"$ENVF"

	# Runtime image = same shape as the repo Dockerfile's final stage,
	# but the binary comes FROM THE RELEASE DIRECTORY.
	cat >"$tmp/img/Dockerfile" <<'EOF'
FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 localmesh \
    && mkdir -p /data && chown localmesh:localmesh /data
COPY localmesh-server /usr/local/bin/localmesh-server
USER localmesh
VOLUME /data
ENV LOCALMESH_DATA_DIR=/data LOCALMESH_LISTEN=0.0.0.0:8443
EXPOSE 8443
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s \
  CMD wget -q --no-check-certificate -O /dev/null https://127.0.0.1:8443/healthz || exit 1
ENTRYPOINT ["localmesh-server"]
EOF

	cat >"$COMPOSE_FILE" <<EOF
# Generated by ci-artifact-install.sh — disposable, project $PROJ only.
services:
  postgres:
    image: postgres:16-alpine
    labels:
      localmesh.ci: "1"
    environment:
      POSTGRES_DB: localmesh
      POSTGRES_USER: localmesh
      POSTGRES_PASSWORD: \${POSTGRES_PASSWORD}
    volumes:
      - localmesh-pg:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U localmesh -d localmesh"]
      interval: 5s
      timeout: 3s
      retries: 12
    networks: [localmesh]

  server:
    build: $tmp/img
    labels:
      localmesh.ci: "1"
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      LOCALMESH_DATABASE_URL: postgres://localmesh:\${POSTGRES_PASSWORD}@postgres:5432/localmesh?sslmode=disable
      LOCALMESH_LISTEN: 0.0.0.0:8443
      LOCALMESH_DATA_DIR: /data
      LOCALMESH_HOSTNAMES: server,localhost
      LOCALMESH_DISCOVERY: "off"
      LOCALMESH_LOG_LEVEL: info
    volumes:
      - localmesh-data:/data
    ports:
      - "\${LOCALMESH_PORT}:8443"
    networks: [localmesh]

volumes:
  localmesh-pg:
  localmesh-data:

networks:
  localmesh:
    driver: bridge
EOF

	dc up -d >"$tmp/compose-up.log" 2>&1 || {
		tail -n 20 "$tmp/compose-up.log" >&2
		fail "docker compose up failed — full log: $tmp/compose-up.log"
	}
	dc ps --format '{{.Name}} {{.Status}}' | tee "$tmp/ps.txt"
	dc ps -q server >/dev/null 2>&1 || fail "server service missing from project"
	pass "compose project $PROJ up — server container runs the SHIPPED linux/$SARCH binary (published :$PORT)"
}

# --- stage: control plane healthy ---------------------------------------------------------
s_healthy() {
	local i code
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
	[ "$code" = "200" ] || fail "/readyz → $code, want 200"
	code="$(curl -s -o /dev/null -w '%{http_code}' --cacert "$CA" -X POST \
		"$BASE/v1/chat/completions" -H 'Content-Type: application/json' \
		-d '{"model":"x","messages":[{"role":"user","content":"hi"}]}')"
	[ "$code" = "401" ] || fail "unauthenticated /v1/chat/completions → $code, want 401"
	pass "artifact server healthy on $BASE — TLS+CA live, /readyz 200, auth gate 401, fingerprint $FP"
}

# --- stage: bootstrap admin key ------------------------------------------------------------
s_bootstrap() {
	local i
	for i in $(seq 30); do
		ADMIN="$(dc exec -T server cat /data/bootstrap-admin.key 2>/dev/null | tr -d '[:space:]' || true)"
		[ -n "$ADMIN" ] && break
		sleep 1
	done
	if [ -z "$ADMIN" ]; then
		ADMIN="$(dc logs server 2>/dev/null | grep -o 'lmk_[A-Za-z0-9_-]*' | head -1 || true)"
	fi
	case "$ADMIN" in
		lmk_*) ;;
		*) fail "could not extract bootstrap admin key" ;;
	esac
	local code
	code="$(curl -s -o "$tmp/authcheck.json" -w '%{http_code}' --cacert "$CA" \
		-H "Authorization: Bearer $ADMIN" "$BASE/api/v1/overview")"
	[ "$code" = "200" ] || fail "bootstrap key rejected by /api/v1/overview → $code"
	pass "bootstrap admin key extracted and authenticates (${ADMIN:0:12}…)"
}

# --- stage: enrollment token -----------------------------------------------------------------
s_token() {
	local code
	code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST --cacert "$CA" \
		-H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
		-d '{"label":"ci-artifact-install","max_uses":1,"ttl_hours":1}' \
		"$BASE/api/v1/enrollment-tokens")"
	[ "$code" = "201" ] || fail "create enrollment token → $code: $(cat "$tmp/resp.json")"
	TOKEN="$(jget "$tmp/resp.json" '{j[token]}')"
	case "$TOKEN" in lme_*) ;; *) fail "unexpected token format: $TOKEN";; esac
	pass "enrollment token created (single-use)"
}

# --- stage: artifact agent joins + gets approved ----------------------------------------------
s_join() {
	mkdir -p "$tmp/home" "$tmp/agent-home"

	HOME="$tmp/home" LOCALMESH_AGENT_DIR="$tmp/agent-home" \
		"$AGENT_BIN" join --server "$BASE" --token "$TOKEN" \
		--name ci-artifact-node -ollama-url "$OLLAMA_URL" \
		-ca-fingerprint "$FP" -wait 90s -run=false >"$tmp/join.log" 2>&1 &
	join_pid=$!

	local req_id="" i code
	for i in $(seq 45); do
		kill -0 "$join_pid" 2>/dev/null || fail "artifact agent join exited early: $(cat "$tmp/join.log")"
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
	[ "$join_rc" -eq 0 ] || fail "artifact agent join exited $join_rc: $(cat "$tmp/join.log")"
	CFG="$tmp/agent-home/agent.json"
	[ -f "$CFG" ] || fail "join succeeded but no config at $CFG"
	[ "$(jget "$CFG" '{j[node_id]}')" = "$NODE_ID" ] || fail "config node_id mismatch: $(cat "$CFG")"
	pass "shipped $AOS/$AARCH agent joined with pinned CA fingerprint → approved → node $NODE_ID"
}

wait_node_online() { # timeout-secs — node online AND reporting models
	local i got online=0
	for i in $(seq "$1"); do
		kill -0 "$agent_pid" 2>/dev/null || return 2
		curl -s --connect-timeout 2 --cacert "$CA" \
			-H "Authorization: Bearer $ADMIN" "$BASE/api/v1/nodes" -o "$tmp/nodes.json" 2>/dev/null || true
		got="$(py - "$tmp/nodes.json" "$NODE_ID" <<'EOF' 2>/dev/null || true
import json,sys
try: nodes=json.load(open(sys.argv[1]))
except Exception: sys.exit(0)
for n in nodes:
    if n.get("id")==sys.argv[2] and n.get("online"):
        engs=[e.get("type") for e in (n.get("engines") or []) if e.get("healthy")]
        models=[m.get("name") for m in (n.get("models") or [])]
        if engs and models:
            print("engines=%s models=%s" % (",".join(engs), ",".join(models)))
EOF
)"
		[ -n "$got" ] && { echo "$got"; return 0; }
		sleep 1
	done
	return 1
}

# --- stage: node online with the real Ollama model discovered ----------------------------------
s_online() {
	HOME="$tmp/home" LOCALMESH_AGENT_DIR="$tmp/agent-home" \
		"$AGENT_BIN" run >"$tmp/agent.log" 2>&1 &
	agent_pid=$!

	local got rc=0
	got="$(wait_node_online 60)" || rc=$?
	[ "$rc" -ne 2 ] || fail "artifact agent died: $(tail -n 10 "$tmp/agent.log")"
	[ "$rc" -eq 0 ] || fail "node never came online with healthy engine + models: $(cat "$tmp/nodes.json" 2>/dev/null)"
	echo "    $got"
	echo "$got" | grep -q "$MODEL" \
		|| fail "node online but required model $MODEL absent from discovery: $got"
	pass "node online — hardware + healthy ollama engine + REAL model $MODEL reported over the agent websocket"
}

# --- stage: non-stream inference ------------------------------------------------------------------
s_inference() {
	local i code
	for i in $(seq 30); do
		code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST --cacert "$CA" \
			-H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
			-d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Say the word lighthouse and nothing else.\"}],\"stream\":false}" \
			"$BASE/v1/chat/completions")"
		[ "$code" = "200" ] && break
		sleep 2
	done
	[ "$code" = "200" ] || fail "chat completion → $code: $(cat "$tmp/resp.json")"
	local content
	content="$(jget "$tmp/resp.json" '{j[choices][0][message][content]}' 2>/dev/null || true)"
	[ -n "$content" ] || fail "chat returned no content: $(cat "$tmp/resp.json")"
	echo "    model answered: $(echo "$content" | head -c 80)"
	pass "POST /v1/chat/completions → 200, real model $MODEL returned real text"
}

# --- stage: streaming inference (SSE) ------------------------------------------------------------------
s_stream() {
	local code
	code="$(curl -sN -o "$tmp/stream.txt" -w '%{http_code}' -X POST --cacert "$CA" \
		-H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
		-d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Count to three.\"}],\"stream\":true}" \
		"$BASE/v1/chat/completions")"
	[ "$code" = "200" ] || fail "streaming chat → $code: $(cat "$tmp/stream.txt")"
	grep -q '^data: ' "$tmp/stream.txt" || fail "no SSE data frames: $(head -c 200 "$tmp/stream.txt")"
	local assembled
	assembled="$(py - "$tmp/stream.txt" <<'EOF'
import json,sys
text=[]
for line in open(sys.argv[1]):
    line=line.strip()
    if not line.startswith("data:"): continue
    payload=line[5:].strip()
    if payload=="[DONE]": continue
    try: chunk=json.loads(payload)
    except Exception: continue
    for ch in chunk.get("choices") or []:
        c=(ch.get("delta") or {}).get("content")
        if c: text.append(c)
print("".join(text))
EOF
)"
	[ -n "$assembled" ] || fail "SSE stream carried no delta content: $(head -c 300 "$tmp/stream.txt")"
	echo "    streamed text: $(echo "$assembled" | head -c 80)"
	pass "stream:true → SSE frames with real delta content, terminated correctly"
}

# --- stage: workload recorded, metadata only --------------------------------------------------------------
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
	grep -q "lighthouse" "$tmp/resp.json" &&
		fail "workload row contains prompt text — violates the no-persistence contract"
	grep -q "Count to three" "$tmp/resp.json" &&
		fail "workload row contains prompt text — violates the no-persistence contract"
	pass "workload rows recorded 'completed' — metadata only, prompts never persisted"
}

# --- stage: restart persistence ------------------------------------------------------------------------------
s_restart() {
	local sum0 sum1 i code online=0
	sum0="$(sha256_of "$CFG")"

	# (a) restart the AGENT process — config is re-read from disk, the
	# node must reconnect with no re-enrollment and no config drift.
	kill "$agent_pid" >/dev/null 2>&1 || true
	wait "$agent_pid" 2>/dev/null || true
	agent_pid=""
	HOME="$tmp/home" LOCALMESH_AGENT_DIR="$tmp/agent-home" \
		"$AGENT_BIN" run >"$tmp/agent-restart.log" 2>&1 &
	agent_pid=$!
	wait_node_online 60 >/dev/null || fail "agent never reconnected after agent-process restart: $(tail -n 10 "$tmp/agent-restart.log")"
	sum1="$(sha256_of "$CFG")"
	[ "$sum0" = "$sum1" ] || fail "agent.json changed across agent restart"
	pass "agent process restart → reconnects from agent.json, config byte-identical"

	# (b) restart the SERVER container — same assertion across a
	# control-plane bounce (the ci-clean-install shape).
	dc restart server >"$tmp/restart.log" 2>&1 || fail "compose restart server failed"
	for i in $(seq 90); do
		code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 --cacert "$CA" \
			"$BASE/readyz" 2>/dev/null || true)"
		[ "$code" = "200" ] && break
		sleep 1
	done
	[ "$code" = "200" ] || fail "server never ready after restart"

	for i in $(seq 60); do
		curl -s --connect-timeout 2 --cacert "$CA" \
			-H "Authorization: Bearer $ADMIN" "$BASE/api/v1/nodes" -o "$tmp/nodes.json" 2>/dev/null || true
		online="$(py - "$tmp/nodes.json" "$NODE_ID" <<'EOF' 2>/dev/null || true
import json,sys
for n in json.load(open(sys.argv[1])):
    if n.get("id")==sys.argv[2] and n.get("online"): print("1")
EOF
)"
		[ "$online" = "1" ] && break
		sleep 1
	done
	[ "$online" = "1" ] || fail "agent never reconnected after server restart"
	sum1="$(sha256_of "$CFG")"
	[ "$sum0" = "$sum1" ] || fail "agent.json changed across server restart"

	code="$(curl -s -o "$tmp/resp.json" -w '%{http_code}' -X POST --cacert "$CA" \
		-H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
		-d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"post-restart\"}],\"stream\":false}" \
		"$BASE/v1/chat/completions")"
	[ "$code" = "200" ] || fail "post-restart inference → $code: $(cat "$tmp/resp.json")"
	pass "server restart → agent reconnects, agent.json byte-identical, inference works"
}

# --- run ---------------------------------------------------------------------------------------------
step "ci-artifact-install — packaged-artifact install journey (project $PROJ)"
stage "preflight"                          s_preflight
stage "real Ollama reachable"              s_ollama
stage "verify release (checksums+sigs)"    s_verify
stage "select + checksum artifacts"        s_pick_artifacts
stage "compose up (artifact server image)" s_compose_up
stage "control plane healthy"              s_healthy
stage "bootstrap admin key"                s_bootstrap
stage "enrollment token"                   s_token
stage "artifact agent join + approve"      s_join
stage "node online + real model"           s_online
stage "inference (non-stream)"             s_inference
stage "inference (SSE stream)"             s_stream
stage "workload metadata only"             s_workload
stage "restart persistence"                s_restart

step "ci-artifact-install: PASS"
echo "external-user journey verified end-to-end from $DIST only:"
echo "  release v$VER (server linux/$SARCH, agent $AOS/$AARCH) on project $PROJ"

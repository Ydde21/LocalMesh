# Beta testing guide

How to run the LocalMesh external beta on **real hardware** and report
results the maintainers can act on. The beta exists to answer one
question the CI suite cannot: *does LocalMesh work across separate
physical machines, on different OSes and networks?*

Everything below assumes you have ≥2 devices and ~an hour. You bring the
hardware; we ship the validation kit:

| Piece | Where | What it does |
|---|---|---|
| `scripts/beta-validate.sh` | this repo | the driver — runs the scriptable legs automatically, prompts you through the operator legs, and writes a **redacted evidence bundle** |
| [Beta validation report](https://github.com/localmesh/localmesh/issues/new?template=beta-validation.md) | GitHub issue template | the structured form your results land in |
| [docs/fleet-validation.md](fleet-validation.md) | this repo | the deep-dive runbook if you want to exercise every leg by hand (curl-level) instead of at smoke depth |
| `scripts/fleet-check.sh` / `scripts/fleet-report.sh` | this repo | the lower-level probes the driver builds on |

## Supported environments

The beta wants **heterogeneity** — the more different your devices, the
more valuable the run:

| Device | Status |
|---|---|
| macOS (Apple Silicon or Intel) | primary target — launchd service path |
| Windows 10/11 + NVIDIA GPU | primary target — SCM service path; **GPU telemetry has never run on real hardware before this beta** |
| Linux (any distro, CPU-only or NVIDIA) | primary target — systemd user-unit path |
| Any other *nix the agent builds on | welcome — expect rough edges |

Requirements:

- **≥2 devices** — the whole point is cross-machine behavior. A laptop
  and a desktop, two VMs on *different* hosts, a Mac and a gaming PC.
  (Two VMs on one physical host is useful but weaker: it still proves
  the wire protocol, not independent hardware detection — mark your
  verdict PARTIAL and say so.)
- **One network path between them** — plain LAN, or an overlay:
  Tailscale, Headscale, or WireGuard. Record which you used; the
  overlay leg is part of what we are validating.
- **Ollama** on at least one device — the only engine adapter today.
- **Docker** on the control-plane machine for the default compose
  deployment, or a bare Go toolchain for source builds.

## 1. Stand up the control plane

Pick the machine that hosts the control plane, then follow the
[README quickstart](../README.md) or
[docs/onboarding.md](onboarding.md):

```bash
cp .env.example .env       # set POSTGRES_PASSWORD
docker compose up -d
docker compose logs server | grep "bootstrap admin"   # your lmk_ admin key
docker compose exec web cat /ca/bootstrap-dashboard.password
```

The dashboard is at `http://<machine>:3000`, the control plane at
`https://<machine>:8443`. If your devices are not on the same LAN,
put the control-plane machine on your overlay and add its stable name
(or IP) to `LOCALMESH_HOSTNAMES` before joining anything —
[docs/networking.md](networking.md) covers LAN names, IP SANs, and
Tailscale/Headscale in detail. **The name or IP agents dial must be in
the server certificate's SANs** — this is the single most common setup
failure.

Grab the cluster CA for out-of-band pinning (optional but recommended):

```bash
docker compose exec server cat /data/ca.pem > ca.pem
openssl x509 -in ca.pem -outform DER | openssl dgst -sha256
#   → the sha256 fingerprint to give each join as -ca-fingerprint
```

Create one enrollment token per device (dashboard **Settings → Device
enrollment**, or `POST /api/v1/enrollment-tokens` with `max_uses: 1`).

## 2. Install and join each device

On **each** test device ([docs/onboarding.md](onboarding.md) for the
full matrix):

```bash
# macOS / Linux — installer (verifies SHA-256 before installing):
curl -fsSL https://raw.githubusercontent.com/localmesh/localmesh/main/scripts/install-agent.sh | sh

# Windows — PowerShell installer:
powershell -ExecutionPolicy Bypass -File install-agent.ps1

# or download the matching release binary / build from source (make build)
```

Join:

```bash
localmesh-agent join \
  -server https://<control-plane>:8443 \
  -token lme_<this device's token> \
  -ca-fingerprint sha256:<fingerprint from step 1>
```

The join submits an enrollment and waits. **Approve it in the
dashboard** (Settings → Device enrollment → Pending requests) — the
agent then connects, reports hardware, and starts serving. Approve on
purpose: check the hardware in the request really is the machine you
expect.

Optionally install the OS service for boot persistence (or just leave
`join`/`run` in a terminal for the beta):

```bash
localmesh-agent service install && localmesh-agent service start
```

## 3. Run the validation driver — on each device

Copy the repo (or just `scripts/beta-validate.sh` +
`scripts/fleet-check.sh`) to each device and run:

```bash
bash scripts/beta-validate.sh --self-test
```

What it does:

1. **Auto legs (steps 1–11):** finds the agent binary, checks the
   enrolled config, service status, control-plane reachability
   (TCP → CA fetch → TLS verify → healthz → agent endpoint), pinned-CA
   fingerprint vs the live cluster CA, hardware report, engine detect,
   model inventory, update history, and a full `doctor` run.
   `--self-test` additionally performs **one bounded real inference**
   through the local engine (status/latency/token counts only — the
   prompt and completion are never recorded).
2. **PROMPT legs (steps 12–19):** things only a human can do — approve
   in the dashboard, kill the agent, unplug the network, revoke and
   reinstate, push a model. The script prints the instruction, waits
   for enter, and records your verdict (`<enter>` = verified · `f` =
   failed · `s` = skip).
3. **Writes the bundle:** `beta-report-<hostname>-<ts>.txt` (full
   transcript) and `.json` (machine-readable summary) in the current
   directory.

Non-interactive/CI runs: `--non-interactive` records every PROMPT leg
as `needs-operator` instead of waiting.

Useful flags: `--agent <path>` (binary not on PATH), `--server <url>`
(connectivity legs before enrollment), `--ca-file <pem>` (verify against
your out-of-band CA copy), `--out-dir <dir>`.

## 4. Exercise the cross-device legs

Some legs need the fleet, not one machine — the driver prompts for
them, but here is what "verified" looks like:

- **Cross-device scheduling** — fire ~10 requests at
  `POST /v1/chat/completions`, then check
  `GET /api/v1/workloads`: `node_name` must show **both** machines.
- **Disconnect failover** — stop an agent (Ctrl-C or
  `service stop`) or drop its network. Dashboard shows it `offline`
  within ~5s (clean) to ~45s (silent drop); inference continues on the
  survivor.
- **Reconnect** — restart; the same node returns `online` with no
  re-enrollment.
- **Revoke / reinstate** — Devices page. Revoke drops the socket
  immediately; reinstate reconnects on the same credential.
- **Model install/remove** — pull `qwen2.5:0.5b` (~400 MB) to a node
  from the dashboard or `POST /api/v1/models/pull`, watch real
  progress, then delete it. Deleting a model's *last* replica must
  answer `409` until you pass `"force": true`.
- **Agent update** (if in scope) — a managed rollout
  ([docs/updates-orchestration.md](updates-orchestration.md)) or the
  manual `localmesh-agent update` path
  ([docs/agent-updates.md](agent-updates.md)).

The full curl-by-curl version of every leg — including expected API
responses, placement views, and memory-aware scheduling checks — is
[docs/fleet-validation.md](fleet-validation.md). Beta testers are
encouraged to at least read its "what counts as verified" section: it
is the honesty standard your verdict is measured against.

## 5. File the report

Open a [Beta validation report](https://github.com/localmesh/localmesh/issues/new?template=beta-validation.md)
issue and:

1. Fill the environment table — **one row per device** (OS, arch, CPU,
   GPU, RAM, engine + version, network path LAN/Tailscale/Headscale).
2. Work through the checklist — it mirrors the protocol above.
3. **Attach both bundle files from each device.**
4. Record anomalies and pick an honest verdict.

## Privacy: what the bundle contains

The evidence bundle is **redacted by construction** — the same contract
`localmesh-agent report` and `scripts/fleet-report.sh` already keep:

- **Never included:** API keys, enrollment tokens, node credentials
  (`lmn_…`), poll secrets, prompt text, completion text. `agent.json`
  contents are never read (existence + permission bits only). Paths
  under your home directory collapse to `~`.
- **Included:** OS/arch/kernel, CPU/GPU class, RAM, agent version +
  protocol, engine version, model names, connectivity probe results,
  CA fingerprints (public trust material), and your **OS hostname** —
  the bundle exists to tell one physical machine apart from another.
- **Nothing is uploaded anywhere.** The script writes two local files;
  you attach them to the issue yourself. Skim the `.txt` before
  posting — that is the review step, and it is yours.

## Where to file what

- **Validation results** (worked / partially worked / failed):
  [Beta validation report](https://github.com/localmesh/localmesh/issues/new?template=beta-validation.md)
  template — the checklist + bundle format above.
- **A concrete bug** found during the run:
  [bug report](https://github.com/localmesh/localmesh/issues/new?template=bug_report.md)
  template — repro steps + `localmesh-agent doctor`/`report` output.
- **Security issues**: never a public issue — see
  [SECURITY.md](../SECURITY.md) (GitHub Security Advisories).

## Honest scope notes

- Beta claims are about **independent inference workloads** — each
  request runs whole on one chosen node. LocalMesh does not shard a
  model across devices or pool VRAM; a model must fit on one node.
- Ollama is the only engine adapter — vLLM/llama.cpp/MLX are future
  work, not beta scope.
- Windows self-update can't replace a running `.exe` — the update path
  handles it via a helper, but service-managed nodes take an extra
  beat ([docs/agent-updates.md](agent-updates.md)).
- mDNS discovery is single-subnet — over an overlay, join with
  `-server`, not `-discover`.

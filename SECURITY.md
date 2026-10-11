# Security policy

LocalMesh is self-hosted infrastructure: it authenticates machines,
moves credentials, and (since v0.7) can push signed binaries to every
agent in a cluster. We take reports seriously and fix in the open.

## Reporting a vulnerability

**Do not open a public issue for a vulnerability.**

Use GitHub Security Advisories — the **"Report a vulnerability"** button
on this repository's *Security* tab. That gives us a private channel to
triage, coordinate a fix, and credit you on disclosure.

Include, when you can:

- the affected version (`localmesh-agent version` output, the
  unauthenticated `GET /api/v1/version` response from the control
  plane, or the release tag / image tag)
- the component (control plane, agent, dashboard, wire protocol,
  release/update path)
- reproduction steps or a proof of concept
- the impact you see (e.g. "unauthenticated caller can reach X",
  "forged signature accepted", "credential disclosed in log")

We work in public, but the advisory thread stays private until a fix
ships or we agree on a disclosure date together.

## Response expectations

- **Acknowledgement:** within 72 hours.
- **Triage verdict** (accepted / needs-more-info / out-of-scope):
  within 7 days.
- **Fix timeline:** we aim to ship fixes for accepted reports in the
  next point release; severe issues (remote code execution,
  authentication bypass, signature-forgery paths) get prioritized ahead
  of feature work. We'll keep you posted on the advisory thread.

This project is maintained by a small team — if we go quiet past these
windows, a polite ping on the advisory is welcome, not rude.

## Supported versions

While the project is pre-1.0, **only the latest release** receives
security fixes. We do not maintain backport branches; the answer to
"affects v0.5.x" is "upgrade to the current release." The upgrade path
itself is documented in `docs/agent-updates.md` and
`docs/updates-orchestration.md`.

## Scope

**In scope** — the things we designed to be attacked:

- **Update trust** — release signature verification, the release
  registry's admission checks, the agent's pinned-key model, the
  authenticated release-file proxy (`/api/v1/agent-releases/...`).
- **Authentication & authorization** — API keys, enrollment tokens,
  node credentials, dashboard sessions, role enforcement (admin /
  inference / readonly), TLS pinning and CA handling.
- **Enrollment flow** — token misuse, approval bypass, credential
  substitution, replay.
- **The agent↔server wire protocol** — anything reachable over
  `/api/v1/agent/connect` or the gateway that escapes its intended
  privileges.
- **Data at rest** — leaks of usable secrets from the database or data
  dir beyond what the documented threat model already accepts.

**Out of scope** — by design or by definition:

- Actions available to a legitimate **admin** key holder (the admin
  role *is* root: it registers keys, approves nodes, schedules
  updates). "An admin can do damage" is the model working, not a bug.
- Denial of service against **your own** cluster — e.g. an agent you
  enrolled consuming its own host's resources, or an admin flooding
  their own API.
- Vulnerabilities requiring **prior compromise** of the operator's
  machine or the Postgres server.
- Third-party dependencies — report upstream; we'll still take a
  pointer when it affects a shipped artifact.
- The self-signed-CA *mechanism itself* — a missing
  `-ca-fingerprint` pin is TOFU by design (documented, warned loudly),
  not a finding. Breaks of the pinning *enforcement* are in scope.

When in doubt, report it — a wrong-scope report costs us minutes; a
missed real one costs users a cluster.

## Security model

The trust model these rules protect — internal CA, credential
prefixing and hashing, the approval gate, update signature trust, and
what a compromised control plane can and cannot do — is documented in
[docs/security.md](docs/security.md). Reading it first will likely
answer whether your finding is a bug or the design working as intended.

Operators hardening a deployment should start with
[docs/production-hardening.md](docs/production-hardening.md); the
pre-release security audits are
[docs/security-audit-v09.md](docs/security-audit-v09.md) and
[docs/security-audit-v10.md](docs/security-audit-v10.md) — the v0.10
audit re-classifies every v0.9 finding against current code.

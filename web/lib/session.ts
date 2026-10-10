// Dashboard session auth — server-side only. The BFF proxy injects an
// admin API key into upstream requests, so every dashboard visitor must
// hold a session; otherwise anyone who can reach the dashboard URL would
// have full cluster-admin privileges.
//
// Password resolution order:
//   1. LOCALMESH_DASHBOARD_PASSWORD env
//   2. LOCALMESH_DASHBOARD_PASSWORD_FILE (compose: the server's
//      bootstrap-dashboard.password, shared via the data volume)
//   3. A per-boot random password, printed to the web process logs.
//
// Sessions are STATELESS SIGNED COOKIES so logins survive web-process
// restarts. The cookie value is
//
//   base64url(expiry_ms) "." base64url(nonce) "." base64url(hmac)
//
// where hmac = HMAC-SHA256 over "<expiry>.<nonce>" with the session key.
// The cookie carries no secret material: the nonce is random, the expiry
// is plaintext, and the HMAC proves the token was minted here.
//
// Session key resolution order:
//   1. LOCALMESH_SESSION_KEY env
//   2. LOCALMESH_SESSION_KEY_FILE (compose mounts /ca/session.key, which
//      the Go server generates 0600 into the shared data volume)
//   3. A per-boot random key — restarts then invalidate all sessions
//      (a warning is logged).
//
// Sessions expire after 12h. Because the cookie is self-contained,
// logout can't "delete" it server-side — instead the token's nonce is
// added to an in-memory denylist so a logged-out token stays rejected
// until it expires. The denylist is bounded by its own expiry sweep.
import { createHmac, randomBytes, timingSafeEqual } from "crypto";
import { readFileSync } from "fs";
import { upstreamFetch } from "./upstream";

export const SESSION_COOKIE = "lm_session";
const SESSION_TTL_MS = 12 * 60 * 60 * 1000;

let cachedPassword: string | undefined;

export function dashboardPassword(): string {
  if (process.env.LOCALMESH_DASHBOARD_PASSWORD) {
    return process.env.LOCALMESH_DASHBOARD_PASSWORD;
  }
  if (cachedPassword) return cachedPassword;
  const f = process.env.LOCALMESH_DASHBOARD_PASSWORD_FILE;
  if (f) {
    try {
      cachedPassword = readFileSync(f, "utf8").trim();
      if (cachedPassword) return cachedPassword;
    } catch {
      // file not written yet — fall through to ephemeral
    }
  }
  cachedPassword = "lm-" + randomBytes(9).toString("hex");
  console.warn(
    "[localmesh] no dashboard password configured — generated an ephemeral one " +
      "(set LOCALMESH_DASHBOARD_PASSWORD or LOCALMESH_DASHBOARD_PASSWORD_FILE): " +
      cachedPassword
  );
  return cachedPassword;
}

export function verifyPassword(candidate: string): boolean {
  const a = Buffer.from(candidate);
  const b = Buffer.from(dashboardPassword());
  if (a.length !== b.length) return false;
  return timingSafeEqual(a, b);
}

// ---- session signing key ----

let cachedKey: Buffer | undefined;

function sessionKey(): Buffer {
  if (cachedKey) return cachedKey;
  if (process.env.LOCALMESH_SESSION_KEY) {
    cachedKey = Buffer.from(process.env.LOCALMESH_SESSION_KEY, "utf8");
    return cachedKey;
  }
  const f = process.env.LOCALMESH_SESSION_KEY_FILE;
  if (f) {
    try {
      const raw = readFileSync(f, "utf8").trim();
      if (raw) {
        cachedKey = Buffer.from(raw, "utf8");
        return cachedKey;
      }
    } catch {
      // file not written yet — fall through to ephemeral
    }
  }
  cachedKey = randomBytes(32);
  console.warn(
    "[localmesh] no session key configured — generated an ephemeral one " +
      "(sessions will not survive restarts; set LOCALMESH_SESSION_KEY or " +
      "LOCALMESH_SESSION_KEY_FILE)"
  );
  return cachedKey;
}

// ---- token mint / verify ----

// Nonces of explicitly destroyed sessions -> their token expiry epoch ms.
// Entries are swept on write so the map can't grow past the number of
// logouts within one TTL window.
const revokedNonces = new Map<string, number>();

function sign(payload: string): string {
  return createHmac("sha256", sessionKey()).update(payload).digest("base64url");
}

export function createSession(): string {
  const expiry = Date.now() + SESSION_TTL_MS;
  const nonce = randomBytes(16).toString("base64url");
  const expiryB64 = Buffer.from(String(expiry), "utf8").toString("base64url");
  const payload = `${expiryB64}.${nonce}`;
  return `${payload}.${sign(payload)}`;
}

// Nonces verified clean by the control plane, with the check timestamp.
// The backend lookup happens once per CHECK_TTL per nonce rather than on
// every request — dashboard polling would otherwise add an upstream
// roundtrip to each call.
const CHECK_TTL_MS = 60_000;
const remoteOK = new Map<string, number>();
let warnedOnce = false;

export interface CheckOptions {
  // privileged = the request performs a mutation (POST/PATCH/PUT/DELETE
  // through the BFF, or an admin action). Privileged checks always
  // consult the durable revocation store — the 60s positive cache is
  // never consulted — and FAIL CLOSED when the control plane can't be
  // reached. Read-only checks may use the cache and degrade to local
  // validation on upstream outage (dashboard availability shouldn't die
  // with the API, but a revoked admin must never regain mutation).
  privileged?: boolean;
}

export async function checkSession(
  token: string | undefined | null,
  opts?: CheckOptions
): Promise<boolean> {
  if (!token) return false;
  const parts = token.split(".");
  if (parts.length !== 3) return false;
  const [expiryB64, nonce, sig] = parts;
  if (!expiryB64 || !nonce || !sig) return false;

  const expected = Buffer.from(sign(`${expiryB64}.${nonce}`), "base64url");
  const given = Buffer.from(sig, "base64url");
  if (expected.length !== given.length || !timingSafeEqual(expected, given)) {
    return false;
  }
  const expiry = Number(Buffer.from(expiryB64, "base64url").toString("utf8"));
  if (!Number.isFinite(expiry) || expiry <= Date.now()) return false;
  if (revokedNonces.has(nonce)) return false;

  // Durable check: the control plane stores revocations in Postgres, so
  // logouts issued before this process (re)started still count.
  // Privileged calls skip the positive cache entirely — a revocation
  // that lands after a cached "clean" verdict must still deny mutations.
  if (!opts?.privileged && (remoteOK.get(nonce) ?? 0) > Date.now()) return true;
  try {
    const res = await upstreamFetch(
      `/api/v1/dashboard/sessions/check?nonce=${encodeURIComponent(nonce)}`
    );
    if (res.ok) {
      const body = (await res.json()) as { revoked?: boolean };
      if (body.revoked) {
        revokedNonces.set(nonce, expiry); // subsequent reads reject fast
        remoteOK.delete(nonce);
        return false;
      }
      remoteOK.set(nonce, Date.now() + CHECK_TTL_MS);
      if (remoteOK.size > 8192) remoteOK.clear(); // bound memory
      return true;
    }
  } catch {
    // upstream unreachable — policy branch below
  }
  if (opts?.privileged) {
    // Fail closed: we cannot prove the session isn't revoked, so a
    // mutation must not proceed. Reads still degrade (below).
    return false;
  }
  // The durable check couldn't run: fall back to local-only validation
  // rather than logging everyone out whenever the API blips. The window
  // this misses is "revoked after web restart AND control plane
  // unreachable" — narrow, time-bounded, and read-only.
  if (!warnedOnce) {
    warnedOnce = true;
    console.warn("[localmesh] session revocation check unavailable — reads degrade to local-only, mutations denied");
  }
  return true;
}

export function destroySession(token: string | undefined | null): void {
  if (!token) return;
  const parts = token.split(".");
  if (parts.length !== 3) return;
  const expiry = Number(Buffer.from(parts[0], "base64url").toString("utf8"));
  if (!Number.isFinite(expiry)) return;
  // Opportunistic sweep: dead entries can't outlive their token's expiry.
  const now = Date.now();
  for (const [n, exp] of revokedNonces) {
    if (exp <= now) revokedNonces.delete(n);
  }
  const nonce = parts[1];
  revokedNonces.set(nonce, expiry);
  remoteOK.delete(nonce); // drop any cached "clean" verdict
  // Persist the revocation on the control plane so a web restart can't
  // resurrect the session. Fire-and-forget: the local denylist already
  // killed the cookie; durability only needs the write to land.
  void upstreamFetch("/api/v1/dashboard/sessions/revoke", {
    method: "POST",
    body: JSON.stringify({ nonce, expires_at: expiry }),
  }).catch(() => {});
}

export function sessionCookieValue(req: Request): string | undefined {
  const header = req.headers.get("cookie");
  if (!header) return undefined;
  for (const part of header.split(";")) {
    const [k, ...v] = part.trim().split("=");
    if (k === SESSION_COOKIE) return decodeURIComponent(v.join("="));
  }
  return undefined;
}

// Per-IP login rate limiting: 5 failed attempts per minute.
const attempts = new Map<string, { count: number; resetAt: number }>();

export function loginAllowed(ip: string): boolean {
  const now = Date.now();
  const rec = attempts.get(ip);
  if (!rec || rec.resetAt <= now) return true;
  return rec.count < 5;
}

export function recordLoginFailure(ip: string): void {
  const now = Date.now();
  const rec = attempts.get(ip);
  if (!rec || rec.resetAt <= now) {
    attempts.set(ip, { count: 1, resetAt: now + 60_000 });
  } else {
    rec.count++;
  }
  if (attempts.size > 4096) attempts.clear(); // bound memory
}

export function clearLoginFailures(ip: string): void {
  attempts.delete(ip);
}

// X-Forwarded-For is honored only when LOCALMESH_TRUST_PROXY is set —
// same contract as the Go control plane (config.go TrustProxy). Without
// it a client can rotate XFF per request and mint a fresh rate-limit
// bucket each time, defeating the login failure cap.
export function clientIP(req: Request): string {
  const trustProxy =
    process.env.LOCALMESH_TRUST_PROXY === "1" ||
    process.env.LOCALMESH_TRUST_PROXY === "true";
  if (trustProxy) {
    const fwd = req.headers.get("x-forwarded-for");
    if (fwd) return fwd.split(",")[0].trim();
  }
  return "local";
}

// Same-origin check for mutations (CSRF defense-in-depth on top of
// SameSite=Lax cookies).
export function sameOrigin(req: Request): boolean {
  const origin = req.headers.get("origin");
  if (!origin) return true; // non-browser clients don't send Origin
  try {
    return new URL(origin).host === req.headers.get("host");
  } catch {
    return false;
  }
}

// BFF proxy: forwards /api/lm/* to the control plane, injecting the
// admin API key from server-side env so it never reaches the browser.
//
//   /api/lm/nodes          → {LOCALMESH_API_URL}/api/v1/nodes
//   /api/lm/openai/models  → {LOCALMESH_API_URL}/v1/models  (gateway prefix)
//
// Env:
//   LOCALMESH_API_URL   e.g. http://localmesh-server:8443 or http://localhost:8080
//   LOCALMESH_API_KEY   admin (or readonly) API key
//   LOCALMESH_API_KEY_FILE  optional file to read the key from (e.g. the
//                           bootstrap-admin.key the server writes on first boot)
//   LOCALMESH_CA_FILE   optional PEM for the self-signed cluster CA (TLS)
//   LOCALMESH_TLS_INSECURE=1 skips TLS verification (dev only)
import { readFileSync } from "fs";
import { Agent, fetch as undiciFetch } from "undici";
import type { Dispatcher } from "undici";
import { checkSession, sameOrigin, sessionCookieValue } from "@/lib/session";

let dispatcher: Dispatcher | undefined;

function getDispatcher(): Dispatcher | undefined {
  if (dispatcher) return dispatcher;
  const caFile = process.env.LOCALMESH_CA_FILE;
  if (caFile) {
    try {
      // Retry on every request until the file exists — in compose the CA is
      // written by the server on first boot into a shared volume.
      dispatcher = new Agent({ connect: { ca: readFileSync(caFile) } });
      return dispatcher;
    } catch {
      // CA not available yet
    }
  }
  if (process.env.LOCALMESH_TLS_INSECURE === "1" || process.env.LOCALMESH_TLS_INSECURE === "true") {
    dispatcher = new Agent({ connect: { rejectUnauthorized: false } });
  }
  return dispatcher;
}

let cachedKey: string | undefined;

function apiKey(): string {
  if (process.env.LOCALMESH_API_KEY) return process.env.LOCALMESH_API_KEY;
  if (cachedKey) return cachedKey;
  const f = process.env.LOCALMESH_API_KEY_FILE;
  if (f) {
    try {
      cachedKey = readFileSync(f, "utf8").trim();
    } catch {
      // key file not written yet (server still bootstrapping); retry next request
    }
  }
  return cachedKey ?? "";
}

async function proxy(req: Request, ctx: { params: Promise<{ path: string[] }> }) {
  // Session gate: this proxy upgrades every call to admin-level access,
  // so an unauthenticated visitor must get nothing through.
  if (!(await checkSession(sessionCookieValue(req)))) {
    return Response.json(
      { error: { message: "authentication required", type: "authentication_error" } },
      { status: 401 }
    );
  }
  // Mutations must come from our own origin (CSRF; SameSite=Lax also helps).
  if (req.method !== "GET" && req.method !== "HEAD" && req.method !== "OPTIONS" && !sameOrigin(req)) {
    return Response.json(
      { error: { message: "cross-origin request rejected", type: "permission_error" } },
      { status: 403 }
    );
  }

  const base = (process.env.LOCALMESH_API_URL || "http://localhost:8080").replace(/\/$/, "");
  const key = apiKey();
  const { path } = await ctx.params;
  const url = new URL(req.url);
  const segs = path.join("/");
  // "/openai/<x>" maps to the OpenAI gateway at /v1/<x>.
  const target =
    path[0] === "openai"
      ? `${base}/v1/${path.slice(1).join("/")}${url.search}`
      : `${base}/api/v1/${segs}${url.search}`;

  const headers: Record<string, string> = {
    Authorization: `Bearer ${key}`,
    Accept: req.headers.get("accept") || "application/json",
  };
  const ct = req.headers.get("content-type");
  if (ct) headers["Content-Type"] = ct;

  let body: ArrayBuffer | undefined;
  if (req.method !== "GET" && req.method !== "HEAD") {
    body = await req.arrayBuffer();
  }

  const res = await undiciFetch(target, {
    method: req.method,
    headers,
    body,
    dispatcher: getDispatcher(),
  });

  const out = new Response(res.body as unknown as BodyInit, {
    status: res.status,
    statusText: res.statusText,
  });
  const respCT = res.headers.get("content-type");
  if (respCT) out.headers.set("Content-Type", respCT);
  out.headers.set("Cache-Control", "no-cache");
  out.headers.set("X-Accel-Buffering", "no");
  return out;
}

export { proxy as GET, proxy as POST, proxy as PUT, proxy as PATCH, proxy as DELETE, proxy as OPTIONS };

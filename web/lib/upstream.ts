// Shared server-side access to the control-plane API: the same env
// config + TLS dispatcher the BFF proxy uses, so session.ts can reach
// the durable revocation endpoints without duplicating CA handling.
import { readFileSync } from "fs";
import { Agent, fetch as undiciFetch } from "undici";
import type { Dispatcher } from "undici";

let dispatcher: Dispatcher | undefined;

function getDispatcher(): Dispatcher | undefined {
  if (dispatcher) return dispatcher;
  const caFile = process.env.LOCALMESH_CA_FILE;
  if (caFile) {
    try {
      dispatcher = new Agent({ connect: { ca: readFileSync(caFile) } });
      return dispatcher;
    } catch {
      // CA not available yet — retry next request
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
      // key file not written yet (server still bootstrapping)
    }
  }
  return cachedKey ?? "";
}

export function upstreamBase(): string {
  return (process.env.LOCALMESH_API_URL || "http://localhost:8080").replace(/\/$/, "");
}

// Returns undici's Response — structurally compatible for our use
// (status/json); the DOM Response types differ under newer lib typings.
export async function upstreamFetch(
  path: string,
  init: { method?: string; body?: string } = {}
): Promise<{ ok: boolean; status: number; json(): Promise<unknown> }> {
  return undiciFetch(`${upstreamBase()}${path}`, {
    method: init.method ?? "GET",
    headers: {
      Authorization: `Bearer ${apiKey()}`,
      ...(init.body ? { "Content-Type": "application/json" } : {}),
    },
    body: init.body,
    dispatcher: getDispatcher(),
  });
}

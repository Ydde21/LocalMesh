// Client-side API access. All requests go through the /api/lm proxy so
// the admin key stays server-side.
"use client";

import useSWR from "swr";
import type * as T from "./types";

// Session expiry is broadcast so the Shell can route to /login without
// a fetch-layer dependency on the router.
export const UNAUTHORIZED_EVENT = "localmesh:unauthorized";

const notifyUnauthorized = () => window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));

export const fetcher = async <T = unknown>(path: string): Promise<T> => {
  const res = await fetch("/api/lm" + path, { cache: "no-store" });
  if (res.status === 401) {
    notifyUnauthorized();
    throw new ApiError(401, "session expired");
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new ApiError(res.status, body?.error?.message || res.statusText);
  }
  return res.json();
};

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

export async function api<T = unknown>(
  method: string,
  path: string,
  body?: unknown
): Promise<T> {
  const res = await fetch("/api/lm" + path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    notifyUnauthorized();
    throw new ApiError(401, "session expired");
  }
  if (!res.ok) {
    const b = await res.json().catch(() => ({}));
    throw new ApiError(res.status, b?.error?.message || res.statusText);
  }
  if (res.status === 204) return undefined as T;
  return res.json();
}

// ---- hooks ----

export function useOverview() {
  return useSWR<T.Overview>("/overview", fetcher, { refreshInterval: 5000 });
}

export function useNodes() {
  return useSWR<T.Node[]>("/nodes", fetcher, { refreshInterval: 5000 });
}

export function useNode(id: string | null) {
  return useSWR<T.Node>(id ? `/nodes/${id}` : null, fetcher, {
    refreshInterval: 4000,
  });
}

export function useModels() {
  return useSWR<T.ClusterModel[]>("/models", fetcher, { refreshInterval: 8000 });
}

export function useWorkloads(status?: string) {
  const q = status ? `?status=${status}` : "";
  return useSWR<T.Workload[]>(`/workloads${q}`, fetcher, {
    refreshInterval: 4000,
  });
}

export function useWorkload(id: string | null) {
  return useSWR<T.Workload>(id ? `/workloads/${id}` : null, fetcher, {
    refreshInterval: 3000,
  });
}

export function usePlacement() {
  return useSWR<T.PlacementEntry[]>("/models/placement", fetcher, {
    refreshInterval: 8000,
  });
}

// Shared SWR key for the ops list — large enough to cover queued + active
// ops plus ~20 finished rows for the history section. Import this for
// mutate() calls so the key never drifts.
export const MODEL_OPS_KEY = "/model-operations?limit=60";

export function useModelOps() {
  return useSWR<T.ModelOperation[]>(MODEL_OPS_KEY, fetcher, {
    refreshInterval: 3000,
  });
}

// Control-plane version. Unauthenticated upstream, absent on pre-v0.5
// servers — no retry so a 404 doesn't loop.
export function useVersion() {
  return useSWR<T.VersionInfo>("/version", fetcher, {
    shouldRetryOnError: false,
    errorRetryCount: 0,
  });
}

// LAN discovery status (mDNS advertising). 404 = server built without the
// feature — consumers should hide the UI, so disable retries.
export function useDiscoveryStatus() {
  return useSWR<T.DiscoveryStatus>("/discovery/status", fetcher, {
    shouldRetryOnError: false,
    errorRetryCount: 0,
  });
}

export function useEnrollments(status?: string) {
  const q = status ? `?status=${status}` : "";
  return useSWR<T.EnrollmentRequest[]>(`/enrollments${q}`, fetcher, {
    refreshInterval: 4000,
  });
}

export function useEnrollTokens() {
  return useSWR<T.EnrollmentToken[]>("/enrollment-tokens", fetcher, {
    refreshInterval: 10000,
  });
}

export function useAPIKeys() {
  return useSWR<T.APIKey[]>("/api-keys", fetcher, { refreshInterval: 10000 });
}

export function useSettings() {
  return useSWR<Record<string, unknown>>("/settings", fetcher);
}

// Connection lifecycle history for one node (newest first). v0.6+ — a
// 404 (node gone or older server) shouldn't loop retries; consumers
// render nothing when there's no data.
export function useNodeEvents(id: string | null, limit = 10) {
  return useSWR<T.NodeEvent[]>(
    id ? `/nodes/${id}/events?limit=${limit}` : null,
    fetcher,
    { shouldRetryOnError: false, errorRetryCount: 0, refreshInterval: 10000 }
  );
}

// Redacted operational snapshot for support/bug reports. v0.6+ — 404s on
// older servers, so no retries; UI gates on `data` being present.
export function useDiagnostics() {
  return useSWR<T.Diagnostics>("/diagnostics", fetcher, {
    shouldRetryOnError: false,
    errorRetryCount: 0,
  });
}

// Just "has the cluster ever served a request?" — the onboarding
// checklist needs existence, not rows, so fetch a single record.
export function useHasWorkloads() {
  return useSWR<T.Workload[]>("/workloads?limit=1", fetcher, {
    refreshInterval: 10000,
  });
}

export function useAudit() {
  return useSWR<T.AuditEntry[]>("/audit?limit=50", fetcher, {
    refreshInterval: 8000,
  });
}

// ---- v0.7+ releases / rollouts / control plane -----------------------------
// All of these 404 on older servers — no retries, consumers gate UI on
// `data` being present (same contract as useNodeEvents/useDiagnostics).

// Signed releases accepted into the registry.
export function useReleases() {
  return useSWR<T.Release[]>("/releases", fetcher, {
    shouldRetryOnError: false,
    errorRetryCount: 0,
    refreshInterval: 15000,
  });
}

// Trusted release-signing public keys.
export function useReleaseKeys() {
  return useSWR<T.ReleaseKey[]>("/release-keys", fetcher, {
    shouldRetryOnError: false,
    errorRetryCount: 0,
    refreshInterval: 15000,
  });
}

// Agent-update rollout campaigns (with per-node counters).
export function useRollouts() {
  return useSWR<T.Rollout[]>("/rollouts", fetcher, {
    shouldRetryOnError: false,
    errorRetryCount: 0,
    refreshInterval: 4000,
  });
}

// Single rollout — some servers only ship the list endpoint, so callers
// should fall back to `useRollouts().find(r => r.id === id)`.
export function useRollout(id: string | null) {
  return useSWR<T.Rollout>(id ? `/rollouts/${id}` : null, fetcher, {
    shouldRetryOnError: false,
    errorRetryCount: 0,
    refreshInterval: 3000,
  });
}

// Per-node agent update rows. `filter` maps to query params (node_id,
// rollout_id, limit) — each distinct filter is its own SWR key.
export function useAgentUpdates(filter?: {
  node_id?: string;
  rollout_id?: string;
  limit?: number;
}) {
  const q = new URLSearchParams();
  if (filter?.node_id) q.set("node_id", filter.node_id);
  if (filter?.rollout_id) q.set("rollout_id", filter.rollout_id);
  if (filter?.limit) q.set("limit", String(filter.limit));
  const qs = q.toString();
  return useSWR<T.AgentUpdate[]>(`/agent-updates${qs ? `?${qs}` : ""}`, fetcher, {
    shouldRetryOnError: false,
    errorRetryCount: 0,
    refreshInterval: 4000,
  });
}

// Single-active ownership / HA status. 404 on older servers.
export function useControlPlane() {
  return useSWR<T.ControlPlaneStatus>("/control-plane", fetcher, {
    shouldRetryOnError: false,
    errorRetryCount: 0,
    refreshInterval: 8000,
  });
}

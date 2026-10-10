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

export function useModelOps() {
  return useSWR<T.ModelOperation[]>("/model-operations?limit=30", fetcher, {
    refreshInterval: 3000,
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

export function useAudit() {
  return useSWR<T.AuditEntry[]>("/audit?limit=50", fetcher, {
    refreshInterval: 8000,
  });
}

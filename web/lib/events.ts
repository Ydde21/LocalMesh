// Live cluster events via SSE through the /api/lm proxy.
"use client";

import { useEffect, useRef, useState } from "react";
import { useSWRConfig } from "swr";

export interface ClusterEvent {
  type: string;
  data: unknown;
  at: number;
}

// useClusterEvents subscribes to the SSE stream, returns recent events,
// and invalidates SWR caches on relevant event types so views refresh
// immediately rather than waiting for the next poll.
export function useClusterEvents(enabled = true) {
  const [events, setEvents] = useState<ClusterEvent[]>([]);
  const [connected, setConnected] = useState(false);
  const { mutate } = useSWRConfig();
  const bufRef = useRef<ClusterEvent[]>([]);

  useEffect(() => {
    if (!enabled) return;
    const es = new EventSource("/api/lm/events");

    es.onopen = () => setConnected(true);
    es.onerror = () => setConnected(false);

    const handler = (ev: MessageEvent) => {
      const ce: ClusterEvent = { type: ev.type, data: safeParse(ev.data), at: Date.now() };
      bufRef.current = [ce, ...bufRef.current].slice(0, 60);
      setEvents(bufRef.current);

      // Invalidate the caches each event type affects.
      const t = ev.type;
      if (t.startsWith("node.") || t.startsWith("enrollment.")) {
        mutate((key) => typeof key === "string" && (key === "/nodes" || key.startsWith("/nodes/") || key === "/overview" || key === "/enrollments" || key.startsWith("/enrollments")));
      } else if (t.startsWith("workload.")) {
        mutate((key) => typeof key === "string" && (key.startsWith("/workloads") || key === "/overview"));
      } else if (t.startsWith("model.") || t.startsWith("enrollment_token.") || t.startsWith("api_key.")) {
        mutate((key) => typeof key === "string" && (key === "/models" || key.startsWith("/model-operations") || key === "/enrollment-tokens" || key === "/api-keys" || key === "/nodes" || key.startsWith("/nodes/")));
      }
    };

    // EventSource delivers named events via addEventListener.
    for (const t of [
      "node.connected", "node.disconnected", "node.capability", "node.metrics",
      "node.approved", "node.revoked", "node.reinstated", "node.deleted", "node.updated",
      "workload.queued", "workload.started", "workload.finished",
      "model.operation", "enrollment.requested", "enrollment.denied",
      "enrollment_token.created", "api_key.created", "api_key.revoked",
      "settings.updated",
    ]) {
      es.addEventListener(t, handler as EventListener);
    }

    return () => {
      es.close();
      setConnected(false);
    };
  }, [enabled, mutate]);

  return { events, connected };
}

function safeParse(s: string) {
  try {
    return JSON.parse(s);
  } catch {
    return s;
  }
}

"use client";

import { Node } from "@/lib/types";

export function NodeBadge({ node }: { node: Node }) {
  if (node.status === "revoked") {
    return <span className="badge badge-neutral"><span className="dot" />revoked</span>;
  }
  if (node.status === "pending") {
    return <span className="badge badge-warn"><span className="dot" />pending</span>;
  }
  if (!node.online) {
    return <span className="badge badge-danger"><span className="dot" />offline</span>;
  }
  if (node.stale_heartbeat) {
    return <span className="badge badge-warn"><span className="dot" />stale</span>;
  }
  return (
    <span className="badge badge-ok"><span className="dot" />online</span>
  );
}

export function StatusBadge({ status }: { status: string }) {
  switch (status) {
    case "completed":
    case "succeeded":
    case "approved":
      return <span className="badge badge-ok"><span className="dot" />{status}</span>;
    case "running":
    case "dispatched":
      return <span className="badge badge-idle"><span className="dot pulse" />{status}</span>;
    case "queued":
    case "pending":
      return <span className="badge badge-warn"><span className="dot" />{status}</span>;
    case "failed":
    case "denied":
      return <span className="badge badge-danger"><span className="dot" />{status}</span>;
    case "interrupted":
      return <span className="badge badge-warn"><span className="dot" />{status}</span>;
    case "cancelled":
      // Deliberate abort, not a failure — keep it visually distinct.
      return <span className="badge badge-neutral"><span className="dot" />{status}</span>;
    default:
      return <span className="badge badge-neutral"><span className="dot" />{status}</span>;
  }
}

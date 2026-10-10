"use client";

import { Node } from "@/lib/types";
import { Wrench } from "lucide-react";

export function NodeBadge({ node }: { node: Node }) {
  if (node.status === "revoked") {
    return <span className="badge badge-neutral"><span className="dot" />revoked</span>;
  }
  if (node.status === "pending") {
    return <span className="badge badge-warn"><span className="dot" />pending</span>;
  }
  // v0.7+ maintenance states — shown ahead of online/offline because they
  // explain the node's scheduling state (and a node mid-update will flap
  // offline anyway).
  if (node.maintenance === "draining") {
    return (
      <span className="badge badge-warn" title={node.maintenance_reason || "draining in-flight work"}>
        <span className="dot pulse" />draining
      </span>
    );
  }
  if (node.maintenance === "maintenance") {
    return (
      <span className="badge badge-neutral" title={node.maintenance_reason || "in maintenance"}>
        <Wrench size={10} />maintenance
      </span>
    );
  }
  if (node.maintenance === "updating") {
    return (
      <span className="badge badge-accent" title={node.maintenance_reason || "agent update in progress"}>
        <span className="dot pulse" />updating
      </span>
    );
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
    // Live agent-update phases (v0.7+) — in flight like "running".
    case "draining":
    case "verifying":
    case "downloading":
    case "staging":
    case "installing":
    case "restarting":
    case "verify_health":
      return (
        <span className="badge badge-idle">
          <span className="dot pulse" />{status.replace(/_/g, " ")}
        </span>
      );
    case "queued":
    case "pending":
      return <span className="badge badge-warn"><span className="dot" />{status}</span>;
    case "awaiting_approval":
      return <span className="badge badge-warn"><span className="dot" />awaiting approval</span>;
    case "failed":
    case "denied":
      return <span className="badge badge-danger"><span className="dot" />{status}</span>;
    case "interrupted":
    case "rolled_back":
      return <span className="badge badge-warn"><span className="dot" />{status.replace(/_/g, " ")}</span>;
    case "cancelled":
      // Deliberate abort, not a failure — keep it visually distinct.
      return <span className="badge badge-neutral"><span className="dot" />{status}</span>;
    default:
      return <span className="badge badge-neutral"><span className="dot" />{status}</span>;
  }
}

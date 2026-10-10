"use client";

import { useMemo } from "react";
import {
  ReactFlow,
  Background,
  Controls,
  MiniMap,
  type Node as RFNode,
  type Edge,
  MarkerType,
  Position,
  Handle,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import type { ModelOperation, Node, Workload } from "@/lib/types";
import { Hexagon, Cpu } from "lucide-react";
import { agentVersion, fmtVersion } from "@/lib/format";

// Custom node components ------------------------------------------------

function HubNode() {
  return (
    <div className="panel flex items-center gap-2.5 px-4 py-3 shadow-lg shadow-black/30"
      style={{ borderColor: "var(--color-accent)" }}>
      <Hexagon size={16} className="text-[var(--color-accent)]" />
      <div>
        <div className="text-[12.5px] font-semibold">Control Plane</div>
        <div className="text-[10px] text-[var(--color-text-faint)]">localmesh-server</div>
      </div>
      <Handle type="source" position={Position.Bottom} className="!invisible" />
      <Handle type="target" position={Position.Bottom} className="!invisible" />
    </div>
  );
}

function DeviceNode({ data }: { data: { node: Node; busy: boolean; op?: string } }) {
  const n = data.node;
  const ver = agentVersion(n);
  const color = !n.online
    ? "var(--color-danger)"
    : data.busy
      ? "var(--color-idle)"
      : "var(--color-online)";
  return (
    <div className="panel min-w-[180px] px-3.5 py-3 shadow-lg shadow-black/30"
      style={{ borderColor: n.online ? color : "var(--color-border)" }}>
      <div className="flex items-center gap-2">
        <span className="h-2 w-2 rounded-full" style={{ background: n.online ? color : "var(--color-text-faint)", boxShadow: n.online ? `0 0 8px ${color}66` : undefined }} />
        <span className="text-[12.5px] font-semibold">{n.name}</span>
        {ver && (
          <span className="mono ml-auto text-[9.5px] text-[var(--color-text-faint)]" title={`agent ${ver}`}>
            {fmtVersion(ver)}
          </span>
        )}
      </div>
      <div className="mt-1.5 flex items-center gap-1.5 text-[10.5px] text-[var(--color-text-dim)]">
        <Cpu size={10} />
        {n.hardware.cpu_cores}c
        {(n.hardware.gpus?.length ?? 0) > 0 && ` · ${n.hardware.gpus!.length} GPU`}
        {" · "}{(n.models ?? []).length} models
      </div>
      <div className="mt-1 text-[10px] text-[var(--color-text-faint)]">
        {n.online ? `${n.in_flight}/${n.max_concurrent} slots · ${(n.latency_ms ?? 0).toFixed(0)}ms` : "offline"}
      </div>
      {/* Model pull/delete in flight on this node — pulsing marker */}
      {data.op && (
        <div className="mt-1.5 flex items-center gap-1.5 text-[10px] text-[var(--color-idle)]">
          <span className="h-1.5 w-1.5 shrink-0 rounded-full bg-[var(--color-idle)] pulse" />
          <span className="truncate" title={data.op}>{data.op}</span>
        </div>
      )}
      <Handle type="target" position={Position.Top} className="!invisible" />
      <Handle type="source" position={Position.Top} className="!invisible" />
    </div>
  );
}

const nodeTypes = { hub: HubNode, device: DeviceNode };

// -----------------------------------------------------------------------

export function TopologyGraph({
  nodes,
  workloads,
  ops = [],
}: {
  nodes: Node[];
  workloads: Workload[];
  ops?: ModelOperation[];
}) {
  // Map each node to the models it is actively executing (live routing).
  const activeByNode = useMemo(() => {
    const m = new Map<string, string[]>();
    workloads.forEach((w) => {
      if (w.status === "running" && w.node_id) {
        m.set(w.node_id, [...(m.get(w.node_id) ?? []), w.model]);
      }
    });
    return m;
  }, [workloads]);
  const busyNodes = useMemo(() => new Set(activeByNode.keys()), [activeByNode]);

  // Map each node to its in-flight model ops ("pull llama3.2:1b"). Queued
  // ops count too — the node is reserved for the work.
  const opByNode = useMemo(() => {
    const m = new Map<string, string>();
    for (const o of ops) {
      if (
        o.status === "queued" || o.status === "pending" ||
        o.status === "dispatched" || o.status === "running"
      ) {
        m.set(o.node_id, `${o.op} ${o.model}`);
      }
    }
    return m;
  }, [ops]);

  const { rfNodes, rfEdges } = useMemo(() => {
    const rfNodes: RFNode[] = [
      { id: "hub", type: "hub", position: { x: 0, y: 0 }, data: {}, draggable: false },
    ];
    const rfEdges: Edge[] = [];

    const radius = Math.max(240, nodes.length * 90);
    nodes.forEach((n, i) => {
      const angle = (2 * Math.PI * i) / Math.max(1, nodes.length) - Math.PI / 2;
      const x = radius * Math.cos(angle);
      const y = radius * Math.sin(angle) + 60;
      const busy = busyNodes.has(n.id);
      rfNodes.push({
        id: n.id,
        type: "device",
        position: { x, y },
        data: { node: n, busy, op: opByNode.get(n.id) },
      });
      const active = activeByNode.get(n.id);
      rfEdges.push({
        id: `e-${n.id}`,
        source: "hub",
        target: n.id,
        animated: n.online,
        label: active?.length ? active.join(", ") : undefined,
        labelStyle: { fill: "#a78bfa", fontSize: 10, fontFamily: "var(--font-mono, monospace)" },
        labelBgStyle: { fill: "#0c0c10", fillOpacity: 0.85 },
        style: {
          stroke: !n.online
            ? "#3a3a44"
            : busy
              ? "#a78bfa"
              : "#34d39966",
          strokeWidth: busy ? 2 : n.online ? 1.5 : 1,
          strokeDasharray: n.online ? undefined : "4 4",
        },
        markerEnd: { type: MarkerType.ArrowClosed, color: n.online ? "#34d39966" : "#3a3a44" },
      });
    });
    return { rfNodes, rfEdges };
  }, [nodes, busyNodes, activeByNode, opByNode]);

  return (
    <div className="panel h-[calc(100vh-190px)] min-h-[420px] overflow-hidden">
      <ReactFlow
        nodes={rfNodes}
        edges={rfEdges}
        nodeTypes={nodeTypes}
        fitView
        fitViewOptions={{ padding: 0.25 }}
        minZoom={0.3}
        maxZoom={1.5}
        proOptions={{ hideAttribution: true }}
        colorMode="dark"
      >
        <Background gap={24} color="#ffffff08" />
        <Controls showInteractive={false} />
        <MiniMap pannable zoomable className="!bg-[var(--color-panel)]" maskColor="#09090bdd" nodeColor="#232326" />
      </ReactFlow>
    </div>
  );
}

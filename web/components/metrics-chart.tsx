"use client";

import { useMemo } from "react";
import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { useEvents } from "@/components/shell";
import { useNodes } from "@/lib/api";
import type { Metrics } from "@/lib/types";

interface Point {
  t: number;
  cpu: number;
  mem: number;
  req: number;
}

// MetricsChart renders a rolling window of cluster-wide utilization built
// from live node.metrics SSE events (one heartbeat per node per ~10s).
export function MetricsChart() {
  const { events } = useEvents();
  const { data: nodes } = useNodes();

  const points = useMemo<Point[]>(() => {
    const latest = new Map<string, Metrics>();
    for (const n of nodes ?? []) {
      if (n.online && n.metrics) latest.set(n.id, n.metrics);
    }
    const agg = (): Omit<Point, "t"> => {
      let cpu = 0, memUsed = 0, memTotal = 0, req = 0, n = 0;
      for (const m of latest.values()) {
        cpu += m.cpu_util_percent;
        memUsed += m.memory_used_bytes;
        memTotal += m.memory_total_bytes;
        req += m.active_requests;
        n++;
      }
      return {
        cpu: n ? cpu / n : 0,
        mem: memTotal ? (memUsed / memTotal) * 100 : 0,
        req,
      };
    };
    const pts: Point[] = [];
    // events arrive newest-first; walk oldest→newest for the chart.
    const metricEvents = events.filter((e) => e.type === "node.metrics").reverse();
    for (const e of metricEvents) {
      const d = e.data as { id?: string; metrics?: Metrics };
      if (d?.id && d.metrics) latest.set(d.id, d.metrics);
      pts.push({ t: e.at, ...agg() });
    }
    return pts.slice(-90);
  }, [events, nodes]);

  return (
    <div className="h-[190px] w-full">
      {points.length < 2 ? (
        <div className="flex h-full items-center justify-center text-[11.5px] text-[var(--color-text-faint)]">
          Collecting live metrics — points appear with each node heartbeat (~10s)
        </div>
      ) : (
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={points} margin={{ top: 6, right: 4, bottom: 0, left: -18 }}>
            <defs>
              <linearGradient id="cpuFill" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor="#4f7cff" stopOpacity={0.28} />
                <stop offset="100%" stopColor="#4f7cff" stopOpacity={0} />
              </linearGradient>
              <linearGradient id="memFill" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor="#34d399" stopOpacity={0.22} />
                <stop offset="100%" stopColor="#34d399" stopOpacity={0} />
              </linearGradient>
            </defs>
            <CartesianGrid stroke="#232326" strokeDasharray="3 4" vertical={false} />
            <XAxis
              dataKey="t"
              tickFormatter={(t: number) =>
                new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })
              }
              tick={{ fill: "#5c5c68", fontSize: 10 }}
              axisLine={false}
              tickLine={false}
              minTickGap={60}
            />
            <YAxis
              domain={[0, 100]}
              tickFormatter={(v: number) => `${v}%`}
              tick={{ fill: "#5c5c68", fontSize: 10 }}
              axisLine={false}
              tickLine={false}
            />
            <Tooltip
              contentStyle={{
                background: "#131316",
                border: "1px solid #30303a",
                borderRadius: 8,
                fontSize: 11,
              }}
              labelStyle={{ color: "#9d9daa" }}
              labelFormatter={(t) => new Date(Number(t)).toLocaleTimeString()}
              formatter={(v, name) => [
                name === "req" ? `${v}` : `${Number(v).toFixed(1)}%`,
                name === "cpu" ? "CPU" : name === "mem" ? "Memory" : "Active req",
              ]}
            />
            <Area type="monotone" dataKey="cpu" stroke="#4f7cff" strokeWidth={1.6} fill="url(#cpuFill)" isAnimationActive={false} />
            <Area type="monotone" dataKey="mem" stroke="#34d399" strokeWidth={1.6} fill="url(#memFill)" isAnimationActive={false} />
            <Area type="stepAfter" dataKey="req" stroke="#a78bfa" strokeWidth={1.2} fill="none" isAnimationActive={false} />
          </AreaChart>
        </ResponsiveContainer>
      )}
    </div>
  );
}

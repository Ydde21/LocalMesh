"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { createContext, useContext, useEffect } from "react";
import {
  LayoutDashboard,
  Server,
  Network,
  Boxes,
  Rocket,
  Activity,
  Settings,
  Hexagon,
  LogOut,
} from "lucide-react";
import { useClusterEvents, type ClusterEvent } from "@/lib/events";
import { UNAUTHORIZED_EVENT, useVersion } from "@/lib/api";
import { fmtVersion } from "@/lib/format";
import { OperationalAlerts } from "@/components/alerts";

const NAV = [
  { href: "/", label: "Overview", icon: LayoutDashboard },
  { href: "/devices", label: "Devices", icon: Server },
  { href: "/topology", label: "Topology", icon: Network },
  { href: "/models", label: "Models", icon: Boxes },
  { href: "/releases", label: "Releases", icon: Rocket },
  { href: "/workloads", label: "Workloads", icon: Activity },
  { href: "/settings", label: "Settings", icon: Settings },
];

interface EventsCtx {
  events: ClusterEvent[];
  connected: boolean;
}

const Ctx = createContext<EventsCtx>({ events: [], connected: false });

export function useEvents() {
  return useContext(Ctx);
}

export function Shell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const router = useRouter();
  const { events, connected } = useClusterEvents();
  // Absent on pre-v0.5 servers — the footer line simply stays hidden.
  const { data: ver } = useVersion();

  // Any API call that comes back 401 means the session is gone.
  useEffect(() => {
    const onUnauthorized = () => router.push("/login");
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
  }, [router]);

  return (
    <Ctx.Provider value={{ events, connected }}>
      <div className="flex h-screen overflow-hidden">
        {/* Sidebar — icon-only rail below md so content stays usable on
            narrow screens; labels and footnote reappear at md+. */}
        <aside
          className="flex w-[54px] shrink-0 flex-col border-r md:w-[212px]"
          style={{ borderColor: "var(--color-border)", background: "var(--color-panel)" }}
        >
          <div className="flex items-center justify-center gap-2.5 px-2 py-4 md:justify-start md:px-4">
            <Hexagon size={18} strokeWidth={1.8} className="shrink-0 text-[var(--color-accent)]" />
            <div className="hidden md:block">
              <div className="text-[13px] font-semibold tracking-tight">LocalMesh</div>
              <div className="text-[10px] text-[var(--color-text-faint)]">
                private AI cluster
              </div>
            </div>
          </div>

          <nav className="mt-2 flex flex-col gap-0.5 px-1.5 md:px-2">
            {NAV.map((item) => {
              const active =
                item.href === "/" ? pathname === "/" : pathname.startsWith(item.href);
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  aria-label={item.label}
                  title={item.label}
                  className={`flex items-center justify-center gap-2.5 rounded-md px-2.5 py-[7px] text-[13px] font-medium transition-colors md:justify-start ${
                    active
                      ? "bg-[var(--color-accent-soft)] text-[var(--color-accent)]"
                      : "text-[var(--color-text-dim)] hover:bg-white/[0.04] hover:text-[var(--color-text)]"
                  }`}
                >
                  <item.icon size={15} strokeWidth={1.8} className="shrink-0" />
                  <span className="hidden md:inline">{item.label}</span>
                </Link>
              );
            })}
          </nav>

          <div className="mt-auto px-2 py-3 md:px-4">
            <div className="flex items-center justify-center gap-2 text-[11px] text-[var(--color-text-faint)] md:justify-start">
              <span
                className={`h-1.5 w-1.5 shrink-0 rounded-full ${
                  connected ? "bg-[var(--color-online)] pulse" : "bg-[var(--color-text-faint)]"
                }`}
              />
              <span className="hidden md:inline">
                {connected ? "Live" : "Reconnecting…"}
              </span>
              <button
                title="Sign out"
                aria-label="Sign out"
                className="rounded p-1 text-[var(--color-text-faint)] transition-colors hover:bg-white/[0.06] hover:text-[var(--color-text)] md:ml-auto"
                onClick={async () => {
                  await fetch("/api/lm-auth/logout", { method: "POST" });
                  router.push("/login");
                }}
              >
                <LogOut size={12} />
              </button>
            </div>
            {ver && (
              <div
                className="mono mt-1.5 hidden text-[10px] text-[var(--color-text-faint)] md:block"
                title={`server ${ver.server_version} · protocol ${ver.protocol} (agents ≥ ${ver.min_agent_protocol})`}
              >
                server {fmtVersion(ver.server_version)} · proto {ver.protocol}
              </div>
            )}
          </div>
        </aside>

        {/* Main */}
        <div className="flex min-w-0 flex-1 flex-col">
          <main className="min-w-0 flex-1 overflow-y-auto">
            <OperationalAlerts connected={connected} />
            {children}
          </main>
        </div>
      </div>
    </Ctx.Provider>
  );
}

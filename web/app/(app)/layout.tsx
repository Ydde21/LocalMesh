import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { Shell } from "@/components/shell";
import { checkSession, SESSION_COOKIE } from "@/lib/session";

// Everything under (app) requires a dashboard session. The BFF proxy
// enforces auth on API calls too — this guard covers page loads so an
// unauthenticated visitor gets redirected before any cluster data ships.
//
// `instant = false`: the cookie check makes this segment intentionally
// blocking per-request (Cache Components route-segment config).
export const instant = false;

export default async function AppLayout({ children }: { children: React.ReactNode }) {
  const c = await cookies();
  if (!(await checkSession(c.get(SESSION_COOKIE)?.value))) {
    redirect("/login");
  }
  return <Shell>{children}</Shell>;
}

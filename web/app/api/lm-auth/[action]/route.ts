// Dashboard auth endpoints (session cookies, not API keys).
//   POST /api/lm-auth/login    {password}  -> Set-Cookie session
//   POST /api/lm-auth/logout               -> clears session
//   GET  /api/lm-auth/session              -> {authenticated}
import {
  checkSession,
  clearLoginFailures,
  clientIP,
  createSession,
  destroySession,
  loginAllowed,
  recordLoginFailure,
  sessionCookieValue,
  SESSION_COOKIE,
  verifyPassword,
} from "@/lib/session";

function json(status: number, body: unknown) {
  return Response.json(body, { status });
}

function cookieHeader(req: Request, token: string, maxAge: number) {
  const secure = new URL(req.url).protocol === "https:";
  return `${SESSION_COOKIE}=${encodeURIComponent(token)}; Path=/; HttpOnly; SameSite=Lax; Max-Age=${maxAge}${secure ? "; Secure" : ""}`;
}

async function handle(req: Request, ctx: { params: Promise<{ action: string }> }) {
  const { action } = await ctx.params;

  if (action === "session" && req.method === "GET") {
    return json(200, { authenticated: await checkSession(sessionCookieValue(req)) });
  }

  if (action === "login" && req.method === "POST") {
    const ip = clientIP(req);
    if (!loginAllowed(ip)) {
      return json(429, { error: "too many attempts — try again in a minute" });
    }
    let password = "";
    try {
      const body = (await req.json()) as { password?: string };
      password = body.password ?? "";
    } catch {
      return json(400, { error: "invalid body" });
    }
    if (!verifyPassword(password)) {
      recordLoginFailure(ip);
      return json(401, { error: "incorrect password" });
    }
    clearLoginFailures(ip);
    const token = createSession();
    const res = json(200, { ok: true });
    res.headers.set("Set-Cookie", cookieHeader(req, token, 43200));
    return res;
  }

  if (action === "logout" && req.method === "POST") {
    destroySession(sessionCookieValue(req));
    const res = json(200, { ok: true });
    res.headers.set("Set-Cookie", cookieHeader(req, "", 0));
    return res;
  }

  return json(404, { error: "not found" });
}

export { handle as GET, handle as POST };

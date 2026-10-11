// Actionable error guidance: maps the error strings the control plane,
// scheduler, node hub, and agent engines actually emit into a next step
// the operator can take. The raw message is always shown too — this adds
// context, it never replaces the error.
//
// Matching is case-insensitive substring matching, ordered most-specific
// first. Patterns mirror the real strings:
//   gateway:    "no eligible node has capacity for this request (no
//                eligible node: <filtered reasons>)", "could not dispatch
//                request: …", "control plane is at its concurrent request
//                limit", "model not found", "engine unreachable", "ollama: …"
//   scheduler:  "model not installed on this node", "node is offline",
//               "at capacity (N/M)", "in maintenance", "insufficient free
//               memory for model", "estimated model memory exceeds node
//               memory", "model fits neither in free VRAM nor free memory",
//               "node heartbeat is stale", "remote (WAN) nodes disabled"
//   nodehub:    "node is not connected", "node not found", "node became
//               unavailable", "node has no free execution slots"
//   adminapi:   "invalid, expired, or exhausted enrollment token",
//               "awaiting administrator approval", "no online eligible
//               nodes for this operation", "model operation queue is full"
//   engine:     "engine unreachable: …", "model not found: …", "ollama:
//               <status>: <msg>" (HTTPError), "ollama pull: <msg>"
//   agent-side: "certificate"/"x509" TLS verification failures and
//               "connection refused"/"no route to host" reachability
//               errors surface via node events and op last_error.

export interface ErrorGuidance {
  // Short headline naming the failure class.
  title: string;
  // What to do about it — one or two sentences, always actionable.
  detail: string;
  // Where the fix lives, when the dashboard has a page for it.
  action?: { href: string; label: string };
}

interface Rule {
  // Any of these substrings (lowercased) matching selects the rule.
  any: string[];
  // Optional disqualifiers — checked before `any` wins.
  none?: string[];
  guidance: ErrorGuidance;
}

const RULES: Rule[] = [
  // --- enrollment / device lifecycle --------------------------------------
  {
    any: ["awaiting administrator approval", "enrollment pending"],
    guidance: {
      title: "Enrollment is waiting for approval",
      detail:
        "The agent enrolled but an admin hasn't approved it yet — the node stays unschedulable until then.",
      action: { href: "/settings", label: "Approve in Settings" },
    },
  },
  {
    any: ["invalid, expired, or exhausted enrollment token"],
    guidance: {
      title: "Enrollment token rejected",
      detail:
        "Mint a fresh enrollment token and re-run `localmesh-agent join` — used-up and expired tokens can't be revived.",
      action: { href: "/settings", label: "Create token in Settings" },
    },
  },
  {
    any: ["device is revoked", "node is revoked", "credential revoked"],
    guidance: {
      title: "Device access revoked",
      detail:
        "This node's credential was revoked. Reinstate it from the device page, or re-enroll the machine.",
      action: { href: "/devices", label: "Open Devices" },
    },
  },

  // --- TLS / connectivity (agent → control plane) -------------------------
  {
    any: ["certificate", "x509", "tls:"],
    // "control plane unreachable: …" is the BFF proxy's own 502 — the
    // failing hop is dashboard→control-plane, not agent→control-plane,
    // so agent-side guidance would misattribute it.
    none: ["certificate has not", "control plane unreachable"],
    guidance: {
      title: "TLS verification failed",
      detail:
        "The agent can't verify the control plane's certificate. Check server cert expiry under Settings → Diagnostics, and confirm the node trusts this cluster's CA (re-join to pick up a renewed CA).",
      action: { href: "/settings", label: "Check Diagnostics" },
    },
  },
  {
    any: [
      "no route to host",
      "connection refused",
      "network is unreachable",
      "i/o timeout",
      "tls handshake timeout",
    ],
    none: ["engine unreachable", "ollama", "control plane unreachable"],
    guidance: {
      title: "Can't reach the control plane",
      detail:
        "The agent can't open a connection to the control plane — check the address/port it's dialing, firewall rules, and that the server is listening.",
    },
  },

  // --- scheduling / capacity ----------------------------------------------
  {
    any: ["model not installed on this node", "model not found"],
    guidance: {
      title: "Model not installed",
      detail:
        "No online node has this model. Install it first — pulls dispatch to the nodes you pick.",
      action: { href: "/models", label: "Install from Models" },
    },
  },
  {
    any: [
      "insufficient free memory for model",
      "estimated model memory exceeds node memory",
      "model fits neither in free vram nor free memory",
    ],
    guidance: {
      title: "Model won't fit in memory",
      detail:
        "Every node that hosts this model is too low on free memory/VRAM to load it safely. Free memory on a node, install it somewhere bigger, or pick a smaller quantization.",
      action: { href: "/models", label: "Check placement" },
    },
  },
  {
    any: ["in maintenance", "is draining", "mid-update", "is updating"],
    guidance: {
      title: "Node is out of service",
      detail:
        "The target node is draining, in maintenance, or mid-update — it takes no new work until resumed.",
      action: { href: "/devices", label: "Open Devices" },
    },
  },
  {
    any: [
      "no eligible node",
      "no online eligible nodes",
      "at capacity",
      "no free execution slots",
      "node became unavailable",
      "heartbeat is stale",
      "node is offline",
      "node is not connected",
    ],
    guidance: {
      title: "No node can take this",
      detail:
        "Every replica is busy, offline, stale, or out of service — retry shortly, raise a node's slot limit, or install the model on more nodes.",
      action: { href: "/models", label: "Check placement" },
    },
  },
  {
    any: ["remote (wan) nodes disabled", "missing required label"],
    guidance: {
      title: "Scheduling policy filtered this out",
      detail:
        "Cluster privacy/label rules excluded the candidate nodes. Adjust the policy under Settings → Scheduling if intended.",
      action: { href: "/settings", label: "Open Settings" },
    },
  },
  {
    any: ["concurrent request limit", "queue is full", "too many"],
    guidance: {
      title: "Control plane is saturated",
      detail:
        "The request/operation queue is at its limit — retry in a few seconds once in-flight work drains.",
    },
  },

  // --- engine-side ---------------------------------------------------------
  {
    any: ["engine unreachable", "hosting inference engine is unhealthy"],
    guidance: {
      title: "Inference engine is down",
      detail:
        "The node's engine (e.g. Ollama) isn't responding — start it on that machine (`ollama serve`), or install it if missing. The node's engine health shows on its device card.",
      action: { href: "/devices", label: "Check the node" },
    },
  },
  {
    any: ["ollama pull:", "ollama:", "pull stream ended"],
    guidance: {
      title: "Engine reported an error",
      detail:
        "Ollama returned this failure — check the engine on the target node (`ollama list`, `ollama ps`) and confirm the model reference and disk space.",
      action: { href: "/devices", label: "Check the node" },
    },
  },
  {
    any: ["insufficient disk", "no space left", "disk space"],
    guidance: {
      title: "Out of disk space",
      detail:
        "The target node doesn't have enough free disk — free space (the pull needs roughly the model size) and retry.",
    },
  },

  // --- transport between dashboard and control plane -----------------------
  {
    any: ["in standby"],
    guidance: {
      title: "Control plane is in standby",
      detail:
        "This instance is waiting for ownership — the active control plane serves requests. If the active just died, promotion happens automatically within seconds; retry shortly.",
    },
  },
  // Late catch-all: specific rules above must win when the message
  // carries both a status phrase and a real reason.
  {
    any: [
      "control plane unreachable", // the BFF proxy's own 502 prefix
      "fetch failed",
      "failed to fetch",
      "networkerror",
      "load failed",
      "econnrefused",
      "enotfound",
      "socket hang up",
      "bad gateway",
      "service unavailable",
      "internal server error",
      "gateway timeout",
    ],
    guidance: {
      title: "Control plane unreachable",
      detail:
        "The dashboard couldn't get a response from the control plane — check LOCALMESH_API_URL on the web server, that the control plane is running, and TLS trust (LOCALMESH_CA_FILE).",
    },
  },
];

// guidanceFor maps a raw API/workload/op error string to actionable
// guidance. Returns a generic fallback rather than null so callers never
// render a bare "something went wrong" — the raw message still shows.
// `context` selects the fallback: "op" for a dispatched request/op that
// failed, "load" for a dashboard data fetch (where the suspicion is the
// BFF→control-plane hop, not a node).
export function guidanceFor(
  raw: string | undefined | null,
  context: "op" | "load" = "op"
): ErrorGuidance {
  const msg = (raw ?? "").toLowerCase();
  if (msg) {
    for (const r of RULES) {
      if (r.none?.some((n) => msg.includes(n))) continue;
      if (r.any.some((a) => msg.includes(a))) return r.guidance;
    }
  }
  if (context === "load") {
    return {
      title: "Couldn't load this",
      detail:
        "The dashboard couldn't fetch from the control plane — confirm it's running and that LOCALMESH_API_URL/LOCALMESH_API_KEY are set on the web server, then retry.",
    };
  }
  return {
    title: "Request failed",
    detail:
      "The gateway rejected or the node failed this request — check the node's engine health and retry. The full error is shown above.",
    action: { href: "/devices", label: "Open Devices" },
  };
}

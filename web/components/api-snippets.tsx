"use client";

// Copyable client examples for the OpenAI-compatible gateway. The API
// key is always a placeholder — real keys are created above in Settings
// and shown exactly once. The base URL comes from /overview
// (server_url, v0.5+) and degrades to a placeholder on older servers.
import { useState } from "react";
import { Copy, Check } from "lucide-react";

type Lang = "curl" | "python" | "node";

function snippets(base: string): Record<Lang, string> {
  return {
    curl: `curl ${base}/v1/chat/completions \\
  -H "Authorization: Bearer <api-key>" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "<model>",
    "stream": true,
    "messages": [{"role": "user", "content": "Hello"}]
  }'`,
    python: `from openai import OpenAI

client = OpenAI(
    base_url="${base}/v1",
    api_key="<api-key>",
)

r = client.chat.completions.create(
    model="<model>",
    messages=[{"role": "user", "content": "Hello"}],
)
print(r.choices[0].message.content)`,
    node: `import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "${base}/v1",
  apiKey: "<api-key>",
});

const r = await client.chat.completions.create({
  model: "<model>",
  messages: [{ role: "user", content: "Hello" }],
});
console.log(r.choices[0].message.content);`,
  };
}

const LABELS: Record<Lang, string> = { curl: "curl", python: "Python", node: "Node.js" };

export function ApiSnippets({ serverUrl }: { serverUrl?: string }) {
  const [lang, setLang] = useState<Lang>("curl");
  const [copied, setCopied] = useState(false);
  const base = serverUrl || "https://<control-plane>:<port>";
  const code = snippets(base)[lang];

  const copy = () => {
    navigator.clipboard.writeText(code);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  return (
    <div>
      <div className="mb-2 flex gap-1">
        {(Object.keys(LABELS) as Lang[]).map((l) => (
          <button
            key={l}
            className={`btn btn-sm ${lang === l ? "btn-primary" : ""}`}
            onClick={() => setLang(l)}
          >
            {LABELS[l]}
          </button>
        ))}
        <button className="btn btn-sm ml-auto" onClick={copy}>
          {copied ? <Check size={12} /> : <Copy size={12} />} {copied ? "Copied" : "Copy"}
        </button>
      </div>
      <div className="codeblock">{code}</div>
      <p className="mt-2 text-[11px] leading-relaxed text-[var(--color-text-faint)]">
        Any OpenAI-compatible client works — point it at{" "}
        <code className="mono">{base}/v1</code> with an <span className="mono">inference</span>-role
        key. Discover servable models at <code className="mono">GET /v1/models</code>. On a
        self-signed deployment, trust the cluster CA (served at{" "}
        <code className="mono">/api/v1/ca.pem</code>) or the client will refuse the certificate.
      </p>
    </div>
  );
}

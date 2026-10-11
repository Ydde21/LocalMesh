package agent

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/shirou/gopsutil/v4/disk"

	"github.com/localmesh/localmesh/internal/agent/hardware"
)

// Doctor runs connectivity and environment diagnostics against the
// configured control plane and local inference engine. It prints one
// PASS/WARN/FAIL line per check and returns a process exit code: 0 when
// nothing failed, 1 if any check failed (warnings do not fail). Secrets
// are never printed. Works before join: a missing config reports
// "not enrolled" and control-plane checks are skipped.
func Doctor(ctx context.Context, cfgPath, caFile string, out io.Writer) int {
	fails := 0
	emit := func(status, name, format string, a ...any) {
		if status == "FAIL" {
			fails++
		}
		fmt.Fprintf(out, "%s %s: %s\n", status, name, fmt.Sprintf(format, a...))
	}

	cfg, err := LoadConfig(cfgPath)
	enrolled := true
	switch {
	case os.IsNotExist(err):
		emit("WARN", "config", "not enrolled — no config at %s; skipping control-plane checks (run 'localmesh-agent join')", cfgPath)
		enrolled = false
		cfg = &Config{}
	case err != nil:
		emit("FAIL", "config", "cannot parse %s: %v — fix the JSON or re-run join", cfgPath, err)
		enrolled = false
		cfg = &Config{}
	default:
		emit("PASS", "config", "%s present and parseable", cfgPath)
	}

	if enrolled && cfg.ServerURL != "" {
		doctorIdentity(cfg, emit)
	} else if enrolled {
		emit("FAIL", "config", "%s is missing server_url — re-run 'localmesh-agent join --server ...'", cfgPath)
		enrolled = false
	}

	caPEM := cfg.CAPEM
	if caFile != "" {
		if b, err := os.ReadFile(caFile); err == nil {
			caPEM = string(b)
		} else {
			emit("FAIL", "tls", "cannot read --ca-file %s: %v", caFile, err)
		}
	}
	var pinnedFP string
	if caPEM != "" {
		pinnedFP = "sha256:" + fingerprintOf([]byte(caPEM))
	}
	tlsCfg := tlsConfigForPEM(caPEM)

	base, perr := url.Parse(strings.TrimRight(cfg.ServerURL, "/"))
	if enrolled && (perr != nil || base.Host == "") {
		emit("FAIL", "control-plane", "unusable server_url %q — re-run join with a valid --server", cfg.ServerURL)
		enrolled = false
	}
	if enrolled {
		doctorDNS(ctx, base, emit)
		doctorNet(ctx, base, emit)
		doctorTLS(ctx, base, caPEM, pinnedFP, base.Scheme == "https", emit)
		doctorCA(ctx, base, caPEM, pinnedFP, emit)
		doctorHealthz(ctx, base, tlsCfg, emit)
		doctorWS(ctx, cfg, tlsCfg, emit)
	}

	doctorEngine(ctx, cfg.OllamaURL, emit)
	doctorDisk(cfgPath, emit)
	doctorPendingUpdate(cfgPath, emit)
	doctorUpdateHistory(cfgPath, emit)
	doctorHardware(ctx, cfgPath, emit)

	if fails > 0 {
		return 1
	}
	return 0
}

// doctorIdentity verifies the enrolled node identity. The credential is
// masked to its last 4 characters.
func doctorIdentity(cfg *Config, emit func(string, string, string, ...any)) {
	var missing []string
	if cfg.NodeID == "" {
		missing = append(missing, "node_id")
	}
	if cfg.NodeToken == "" {
		missing = append(missing, "node_token")
	}
	if len(missing) > 0 {
		emit("FAIL", "identity", "missing %s — re-run 'localmesh-agent join' to re-enroll", strings.Join(missing, ", "))
		return
	}
	emit("PASS", "identity", "node_id %s, node_token %s", cfg.NodeID, maskToken(cfg.NodeToken))
}

func doctorDNS(ctx context.Context, base *url.URL, emit func(string, string, string, ...any)) {
	host := base.Hostname()
	if net.ParseIP(host) != nil {
		emit("PASS", "dns", "%s is a literal IP — no resolution needed", host)
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	addrs, err := net.DefaultResolver.LookupIPAddr(dctx, host)
	if err != nil {
		emit("FAIL", "dns", "%s does not resolve: %v — use the control plane's LAN IP, an /etc/hosts entry, or its Tailscale name", host, err)
		return
	}
	ips := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP.String())
	}
	emit("PASS", "dns", "%s → %s in %dms", host, strings.Join(ips, ", "), time.Since(start).Milliseconds())
}

func doctorNet(ctx context.Context, base *url.URL, emit func(string, string, string, ...any)) {
	host := base.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		port := "443"
		if base.Scheme == "http" {
			port = "80"
		}
		host = net.JoinHostPort(host, port)
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", host)
	if err != nil {
		emit("FAIL", "tcp", "%s unreachable: %v — check the port, the compose mapping (LOCALMESH_PORT), and firewalls", host, err)
		return
	}
	conn.Close()
	emit("PASS", "tcp", "%s reachable in %dms", host, time.Since(start).Milliseconds())
}

// doctorTLS handshakes (insecurely, so the cert can be inspected even
// when invalid), reports leaf identity/SANs/expiry, verifies hostname
// coverage explicitly — a configured IP not present in SANs is called
// out — then verifies the chain against the pinned CA (or system roots).
func doctorTLS(ctx context.Context, base *url.URL, caPEM, pinnedFP string, https bool, emit func(string, string, string, ...any)) {
	if !https {
		emit("WARN", "tls", "server_url is plaintext %s — no TLS", base.Scheme)
		return
	}
	host := base.Hostname()
	port := base.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(host, port)
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := (&tls.Dialer{NetDialer: &net.Dialer{}, Config: &tls.Config{InsecureSkipVerify: true}}).
		DialContext(dctx, "tcp", addr)
	if err != nil {
		emit("FAIL", "tls", "handshake failed: %v — is a TLS server listening on %s?", err, addr)
		return
	}
	defer conn.Close()
	st := conn.(*tls.Conn).ConnectionState()
	if len(st.PeerCertificates) == 0 {
		emit("FAIL", "tls", "server presented no certificate")
		return
	}
	leaf := st.PeerCertificates[0]
	var sans []string
	sans = append(sans, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		sans = append(sans, ip.String())
	}
	days := int(time.Until(leaf.NotAfter).Hours() / 24)
	switch {
	case days < 0:
		emit("FAIL", "cert", "cn=%s expired %d days ago — reissue the server certificate", leaf.Subject.CommonName, -days)
	case days < 14:
		emit("WARN", "cert", "cn=%s san=[%s] issuer=%s expires in %dd — reissue soon", leaf.Subject.CommonName, strings.Join(sans, ","), leaf.Issuer.CommonName, days)
	default:
		emit("PASS", "cert", "cn=%s san=[%s] issuer=%s expires in %dd", leaf.Subject.CommonName, strings.Join(sans, ","), leaf.Issuer.CommonName, days)
	}

	// Hostname coverage is reported separately from chain validity —
	// on a LAN this is the most common enrollment breakage.
	if err := leaf.VerifyHostname(host); err != nil {
		if net.ParseIP(host) != nil {
			emit("FAIL", "tls", "cert SANs do not cover IP %s — set server_url to one of [%s] instead of the IP", host, strings.Join(leaf.DNSNames, ","))
		} else {
			emit("FAIL", "tls", "cert SANs do not cover '%s' — add it to LOCALMESH_HOSTNAMES and reissue the server cert", host)
		}
		return
	}

	roots := (*x509.CertPool)(nil)
	if caPEM != "" {
		roots = x509.NewCertPool()
		roots.AppendCertsFromPEM([]byte(caPEM))
	}
	inter := x509.NewCertPool()
	for _, c := range st.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	chains, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter})
	if err != nil {
		against := "system roots"
		if caPEM != "" {
			against = "the pinned CA — check ca_pem belongs to this cluster"
		}
		emit("FAIL", "tls", "chain does not verify against %s: %v", against, err)
		return
	}
	if caPEM == "" {
		emit("PASS", "tls", "handshake ok in %dms, verified against system roots (no pinned CA in config)", time.Since(start).Milliseconds())
		return
	}
	gotFP := ""
	if len(chains) > 0 {
		chain := chains[0]
		gotFP = "sha256:" + fingerprintOfDER(chain[len(chain)-1].Raw)
	}
	if gotFP != "" && gotFP != pinnedFP {
		emit("WARN", "tls", "chain verified but root %s differs from config CA %s", shortFP(gotFP), shortFP(pinnedFP))
		return
	}
	emit("PASS", "tls", "handshake ok in %dms, verified against pinned CA %s", time.Since(start).Milliseconds(), shortFP(pinnedFP))
}

// doctorCA fetches the live cluster CA and compares its fingerprint
// with the CA stored in config. The fetch is intentionally unverified —
// the CA is public trust material; the fingerprint comparison is what
// authenticates it.
func doctorCA(ctx context.Context, base *url.URL, caPEM, pinnedFP string, emit func(string, string, string, ...any)) {
	if base.Scheme != "https" {
		return
	}
	hc := &http.Client{Timeout: 6 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+"/api/v1/ca.pem", nil)
	resp, err := hc.Do(req)
	if err != nil {
		emit("WARN", "ca", "cannot fetch /api/v1/ca.pem: %v", err)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !isPEMCert(body) {
		emit("WARN", "ca", "GET /api/v1/ca.pem → %s — no cluster CA served", resp.Status)
		return
	}
	liveFP := "sha256:" + fingerprintOf(body)
	if pinnedFP == "" {
		emit("WARN", "ca", "no CA pinned in config; live cluster CA is %s", liveFP)
		return
	}
	if liveFP != pinnedFP {
		emit("FAIL", "ca", "live CA %s != pinned %s — wrong control plane, or the cluster CA rotated; re-join or update ca_pem", liveFP, pinnedFP)
		return
	}
	emit("PASS", "ca", "pinned CA matches live /api/v1/ca.pem (%s)", liveFP)
}

func doctorHealthz(ctx context.Context, base *url.URL, tlsCfg *tls.Config, emit func(string, string, string, ...any)) {
	hc := &http.Client{Timeout: 8 * time.Second}
	if tlsCfg != nil {
		hc.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+"/healthz", nil)
	resp, err := hc.Do(req)
	if err != nil {
		emit("FAIL", "healthz", "GET /healthz: %v — is the control plane up on %s?", err, base.Host)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		emit("FAIL", "healthz", "GET /healthz → %s — service unhealthy or wrong port", resp.Status)
		return
	}
	emit("PASS", "healthz", "GET /healthz → 200")
}

// doctorWS performs the websocket upgrade with the node credential and
// reports handshake latency. On 101 the socket is closed immediately —
// the connection lives well under 2s — before any hello, so the hub
// never registers the session and a live agent is never kicked.
func doctorWS(ctx context.Context, cfg *Config, tlsCfg *tls.Config, emit func(string, string, string, ...any)) {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+cfg.NodeToken)
	opts := &websocket.DialOptions{HTTPHeader: hdr}
	if tlsCfg != nil {
		opts.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	}
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	start := time.Now()
	ws, resp, err := websocket.Dial(dctx, cfg.WSURL(), opts)
	if err == nil {
		ms := time.Since(start).Milliseconds()
		ws.Close(websocket.StatusNormalClosure, "doctor")
		emit("PASS", "websocket", "upgrade 101 in %dms — node credential accepted", ms)
		return
	}
	if resp == nil {
		emit("FAIL", "websocket", "%s", err)
		return
	}
	reason := strings.TrimSpace(readLimit(resp.Body, 256))
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		emit("FAIL", "websocket", "401 unauthorized: %s — node credential rejected (bad/revoked token); re-run join to re-enroll", reason)
	case http.StatusForbidden:
		emit("FAIL", "websocket", "403 forbidden: %s — node not approved or revoked by an administrator", reason)
	default:
		emit("WARN", "websocket", "unexpected HTTP %s during upgrade", resp.Status)
	}
}

func doctorEngine(ctx context.Context, ollamaURL string, emit func(string, string, string, ...any)) {
	if ollamaURL == "" {
		ollamaURL = "http://127.0.0.1:11434"
	}
	ollamaURL = strings.TrimRight(ollamaURL, "/")
	hc := &http.Client{Timeout: 5 * time.Second}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ollamaURL+"/api/version", nil)
	resp, err := hc.Do(req)
	if err != nil {
		emit("FAIL", "engine", "ollama at %s unreachable: %v — install Ollama, or set ollama_url in the config", ollamaURL, err)
		emit("WARN", "models", "skipped — engine unreachable")
		return
	}
	var v struct {
		Version string `json:"version"`
	}
	decodeJSON(resp, &v)
	if resp.StatusCode != http.StatusOK {
		emit("FAIL", "engine", "ollama /api/version → %s", resp.Status)
		emit("WARN", "models", "skipped — engine not healthy")
		return
	}
	emit("PASS", "engine", "ollama %s at %s", v.Version, ollamaURL)

	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, ollamaURL+"/api/tags", nil)
	resp2, err := hc.Do(req2)
	if err != nil {
		emit("WARN", "models", "GET /api/tags: %v", err)
		return
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	decodeJSON(resp2, &tags)
	if resp2.StatusCode != http.StatusOK {
		emit("WARN", "models", "GET /api/tags → %s", resp2.Status)
		return
	}
	emit("PASS", "models", "%d model(s) installed", len(tags.Models))
}

// doctorDisk reports free space on the filesystem holding the config
// dir — where models would be pulled. Pre-join the dir may not exist
// yet, so the nearest existing ancestor is measured.
func doctorDisk(cfgPath string, emit func(string, string, string, ...any)) {
	dir := filepath.Dir(cfgPath)
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			emit("WARN", "disk", "cannot locate filesystem for %s", cfgPath)
			return
		}
		dir = parent
	}
	u, err := disk.Usage(dir)
	if err != nil {
		emit("WARN", "disk", "cannot measure free space on %s: %v", dir, err)
		return
	}
	freeGiB := float64(u.Free) / (1 << 30)
	if u.Free < 10<<30 {
		emit("WARN", "disk", "%.1f GiB free on %s — models need several GiB each", freeGiB, dir)
		return
	}
	emit("PASS", "disk", "%.1f GiB free on %s (%.0f%% used)", freeGiB, dir, u.UsedPercent)
}

// doctorPendingUpdate surfaces an unfinished centralized update: the
// marker means a swapped-in binary still owes the server a health
// confirmation (or a rollback is in progress).
func doctorPendingUpdate(cfgPath string, emit func(string, string, string, ...any)) {
	if s := PendingUpdateSummary(cfgPath); s != "" {
		emit("WARN", "update", "%s", s)
	}
}

// doctorUpdateHistory surfaces the last recorded update outcome: a
// failed or rolled-back install warns (it also arms the redispatch
// tripwire, so the operator should know), a confirmed one is a PASS
// note. Nothing is emitted when no update has ever run.
func doctorUpdateHistory(cfgPath string, emit func(string, string, string, ...any)) {
	e, ok := latestUpdateEntry(cfgPath)
	if !ok {
		return
	}
	if e.Outcome == upOutcomeSucceeded {
		emit("PASS", "update-history", "%s", updateHistoryLine(e))
		return
	}
	emit("WARN", "update-history", "%s", updateHistoryLine(e))
}

// doctorHardware reports the static snapshot plus which live metrics
// this platform can actually measure — the same flags the heartbeat
// emits — with memory total/available called out explicitly.
func doctorHardware(ctx context.Context, cfgPath string, emit func(string, string, string, ...any)) {
	hw := hardware.Detect(ctx)
	m := hardware.Metrics(ctx, 0, nil, filepath.Dir(cfgPath))
	gpuDesc := "none"
	if len(hw.GPUs) > 0 {
		gpuDesc = fmt.Sprintf("%d (%s)", len(hw.GPUs), hw.GPUs[0].Name)
	}
	emit("PASS", "hardware", "%s/%s %s, %d cores, %.1f GiB RAM, GPU %s",
		hw.OS, hw.Arch, hw.CPUModel, hw.CPUCores,
		float64(hw.MemoryBytes)/(1<<30), gpuDesc)
	if m.MemAvailMeasured {
		emit("PASS", "memory", "%.1f GiB total, %.1f GiB available (measured)",
			float64(m.MemoryTotalBytes)/(1<<30), float64(m.MemoryAvailableBytes)/(1<<30))
	} else {
		emit("WARN", "memory", "%.1f GiB total — available memory not measurable on this platform",
			float64(m.MemoryTotalBytes)/(1<<30))
	}
	emit("PASS", "metrics", "measured now: cpu=%v mem_avail=%v gpu_util=%v vram_used=%v",
		m.CPUUtilMeasured, m.MemAvailMeasured, m.GPUUtilMeasured, m.VRAMUsedMeasured)
}

// maskToken renders a credential safe for display: only the last 4
// characters are shown ("…x1yz").
func maskToken(t string) string {
	if len(t) <= 4 {
		return "…"
	}
	return "…" + t[len(t)-4:]
}

func shortFP(fp string) string {
	if len(fp) > 23 {
		return fp[:23] + "…"
	}
	return fp
}

func tlsConfigForPEM(pemStr string) *tls.Config {
	if pemStr == "" {
		return nil
	}
	return tlsConfigForCA([]byte(pemStr))
}

// fingerprintOfDER hashes a raw DER certificate (peer of fingerprintOf,
// which takes PEM).
func fingerprintOfDER(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func decodeJSON(resp *http.Response, v any) {
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(v)
}

// readLimit reads a small response body for a one-line diagnostic.
func readLimit(r io.Reader, n int64) string {
	b, _ := io.ReadAll(io.LimitReader(r, n))
	return string(b)
}

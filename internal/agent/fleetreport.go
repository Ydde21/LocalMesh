// fleetreport.go implements the agent-side fleet diagnostic report: a
// single REDACTED snapshot of this machine's agent state that a
// developer can paste into an issue or attach to the physical-fleet
// runbook (docs/fleet-validation.md). Unlike `doctor` — an interactive
// troubleshooting session — the report is a bundle: machine-readable
// JSON or a compact human-readable summary, safe to share by design.
//
// REDACTION CONTRACT: the report never contains API keys, enrollment
// tokens, node credentials (lmn_*), poll secrets, prompt text, or
// completion text. The config is never serialized — fields are copied
// out individually. Error strings are scrubbed of credential-shaped
// substrings. Paths under the user's home directory collapse to ~ so
// filesystem usernames don't leak. The pinned CA cert is reported as
// its sha256 fingerprint — public trust material, that's the point of
// a fingerprint. The OS hostname IS included: the report exists to
// identify one physical machine in a fleet, and the hostname is
// already what the agent reports to the control plane.
package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/shirou/gopsutil/v4/disk"

	"github.com/localmesh/localmesh/internal/agent/engine"
	"github.com/localmesh/localmesh/internal/agent/hardware"
	"github.com/localmesh/localmesh/internal/contracts"
)

// reportSchemaVersion is the JSON schema version of FleetReport —
// bump when fields change meaning so pasted reports stay comparable
// across releases.
const reportSchemaVersion = 1

// fleetReportDefaultTimeout bounds the whole probe pass when the
// caller doesn't set one. Every individual probe also carries its own
// deadline, so a hung engine or control plane can't stall the report.
const fleetReportDefaultTimeout = 60 * time.Second

// fleetReportSelfTestPrompt is the fixed, content-free inference
// self-test prompt. It is a constant — not user data — and is never
// written into the report itself; only the outcome (status, latency,
// token count) is recorded.
const fleetReportSelfTestPrompt = "Reply with the single word OK."

// FleetReportOpts controls report collection.
type FleetReportOpts struct {
	// ConfigPath locates agent.json. Default: DefaultPath(). Used for
	// existence/parsability checks, update-state files, and the
	// free-disk measurement — never for secret material.
	ConfigPath string
	// CAFile optionally overrides the config's embedded CA (same role
	// as doctor --ca-file). Only its fingerprint is reported.
	CAFile string
	// JSON selects machine-readable output; false renders the
	// human-readable summary.
	JSON bool
	// SelfTest runs one bounded real inference through the configured
	// engine — the only check that proves the engine can actually
	// generate. Off by default: it touches the engine for real.
	SelfTest bool
	// SelfTestModel pins the model used; empty picks the first
	// installed model the engine reports.
	SelfTestModel string
	// Timeout bounds the whole collection. Default 60s.
	Timeout time.Duration
}

// FleetReport is the collected, redacted snapshot. Every field is safe
// to share — see the package comment for the redaction contract.
type FleetReport struct {
	ReportVersion int                `json:"report_version"`
	GeneratedAt   time.Time          `json:"generated_at"`
	Agent         ReportAgent        `json:"agent"`
	System        ReportSystem       `json:"system"`
	Config        ReportConfig       `json:"config"`
	TLS           ReportTLS          `json:"tls"`
	Engine        ReportEngine       `json:"engine"`
	ControlPlane  ReportControlPlane `json:"control_plane"`
	SelfTest      ReportSelfTest     `json:"self_test"`
	Update        ReportUpdate       `json:"update"`
	Notes         []string           `json:"notes,omitempty"`
}

// ReportAgent identifies the binary that produced the report.
type ReportAgent struct {
	Version   string `json:"version"`    // build stamp ("dev" when unstamped)
	Protocol  int    `json:"protocol"`   // wire protocol version
	GoVersion string `json:"go_version"` // toolchain the binary was built with
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// ReportSystem is the hardware/OS view — what hardware detection
// actually provides on this platform, no more.
type ReportSystem struct {
	Hostname             string `json:"hostname"`
	CPUModel             string `json:"cpu_model"`
	CPUCores             int    `json:"cpu_cores"`
	MemoryTotalBytes     uint64 `json:"memory_total_bytes"`
	MemoryAvailableBytes uint64 `json:"memory_available_bytes,omitempty"`
	// MemAvailMeasured distinguishes a real availability figure from a
	// platform that can't measure one — consumers must not read an
	// unmeasured zero as "no memory free".
	MemAvailMeasured bool            `json:"mem_avail_measured"`
	GPUs             []contracts.GPU `json:"gpus,omitempty"`
	// GPUMetricsMeasured reports whether live GPU telemetry
	// (util/VRAM) is measurable here — false without nvidia-smi.
	GPUMetricsMeasured bool `json:"gpu_metrics_measured"`
	// DiskFreeBytes is free space on the filesystem holding the agent
	// state dir (where update staging lands). Zero + DiskMeasured=false
	// when unmeasurable.
	DiskFreeBytes uint64 `json:"disk_free_bytes,omitempty"`
	DiskMeasured  bool   `json:"disk_measured"`
	DiskDir       string `json:"disk_dir,omitempty"` // ~-redacted
}

// ReportConfig is the enrollment/config view — the fields an operator
// needs, minus every secret in the underlying Config.
type ReportConfig struct {
	Path              string `json:"path"` // ~-redacted
	Exists            bool   `json:"exists"`
	Parseable         bool   `json:"parseable"`
	Enrolled          bool   `json:"enrolled"`             // node_id + credential present
	ServerURL         string `json:"server_url,omitempty"` // userinfo/path/query stripped
	NodeID            string `json:"node_id,omitempty"`
	Name              string `json:"name,omitempty"`
	OllamaURL         string `json:"ollama_url,omitempty"` // userinfo stripped
	Discovery         bool   `json:"discovery_enabled,omitempty"`
	DiscoveredCluster string `json:"discovered_cluster,omitempty"`
	// UpdatePubkeyID identifies the pinned release-signing key — a
	// public identity, safe to report (the key material itself is not).
	UpdatePubkeyID string `json:"update_pubkey_id,omitempty"`
}

// ReportTLS describes the pinned cluster CA — fingerprint only.
type ReportTLS struct {
	CAConfigured        bool   `json:"ca_configured"`
	CAFingerprintSHA256 string `json:"ca_fingerprint_sha256,omitempty"` // "sha256:<hex>" of the DER
	CASource            string `json:"ca_source,omitempty"`             // "config" | "ca_file"
	// LiveCAFingerprint is the CA the control plane currently serves at
	// /api/v1/ca.pem, when fetched (https only).
	LiveCAFingerprint string `json:"live_ca_fingerprint_sha256,omitempty"`
	// LiveCAMatches compares pinned vs live CA — nil when no live
	// fetch happened. False means wrong control plane or a CA rotation.
	LiveCAMatches *bool `json:"live_ca_matches,omitempty"`
}

// ReportEngine is the local inference engine (Ollama today).
type ReportEngine struct {
	Type         string   `json:"type"`
	Address      string   `json:"address"` // userinfo stripped
	Reachable    bool     `json:"reachable"`
	Healthy      bool     `json:"healthy"`
	Version      string   `json:"version,omitempty"`
	LatencyMS    int64    `json:"latency_ms,omitempty"`
	ModelCount   int      `json:"model_count"`
	Models       []string `json:"models,omitempty"`
	LoadedModels []string `json:"loaded_models,omitempty"`
	Error        string   `json:"error,omitempty"` // scrubbed
}

// ReportControlPlane is the connectivity view toward the configured
// control plane. Checked=false when the node isn't enrolled or the
// server URL is unusable — all probe fields are then their zero
// values, not failures.
type ReportControlPlane struct {
	Checked      bool     `json:"checked"`
	ServerURL    string   `json:"server_url,omitempty"` // redacted
	Scheme       string   `json:"scheme,omitempty"`     // http | https
	DNSChecked   bool     `json:"dns_checked"`          // false for literal-IP servers
	DNSResolved  bool     `json:"dns_resolved,omitempty"`
	Addrs        []string `json:"addrs,omitempty"`
	TCPReachable bool     `json:"tcp_reachable"`
	TCPLatencyMS int64    `json:"tcp_latency_ms,omitempty"`
	// Healthz / WSUpgrade are outcome strings, never error dumps:
	//   healthz:    "ok" | "http_<code>" | "unreachable" | "skipped"
	//   ws_upgrade: "ok_101" | "rejected_<code>" | "unreachable" | "skipped"
	Healthz     string `json:"healthz"`
	HealthzMS   int64  `json:"healthz_latency_ms,omitempty"`
	WSUpgrade   string `json:"ws_upgrade"`
	WSLatencyMS int64  `json:"ws_latency_ms,omitempty"`
	Error       string `json:"error,omitempty"` // scrubbed
}

// ReportSelfTest records the optional inference self-test. Status:
// "passed" | "failed" | "skipped" — skipped always names its reason.
// Prompt and completion content are never recorded.
type ReportSelfTest struct {
	Status           string `json:"status"`
	Reason           string `json:"reason,omitempty"`
	Model            string `json:"model,omitempty"`
	LatencyMS        int64  `json:"latency_ms,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
}

// ReportUpdate surfaces the durable update state beside agent.json.
type ReportUpdate struct {
	Pending string `json:"pending,omitempty"` // PendingUpdateSummary
	Last    string `json:"last,omitempty"`    // UpdateHistorySummary
}

// credPattern matches LocalMesh credential-shaped strings wherever they
// appear — lmk_ API keys, lme_ enrollment tokens, lmn_ node creds, lmp_
// poll secrets. Belt-and-braces on top of never serializing them: any
// error string echoing one is scrubbed before it can reach the report.
var credPattern = regexp.MustCompile(`lm[kepn]_[A-Za-z0-9_\-]+`)

// scrubSecrets removes credential material from a string destined for
// the report: anything matching the LocalMesh credential prefixes, plus
// every explicitly-provided secret.
func scrubSecrets(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	return credPattern.ReplaceAllString(s, "[redacted-cred]")
}

// redactHomePath collapses the user's home directory and the agent
// state dir to placeholders so paths can't leak filesystem usernames.
func redactHomePath(p string) string {
	if p == "" {
		return ""
	}
	clean := filepath.Clean(p)
	// Longest-prefix-first: the agent dir usually lives under home.
	for _, sub := range []struct{ path, repl string }{
		{os.Getenv("LOCALMESH_AGENT_DIR"), "$LOCALMESH_AGENT_DIR"},
		{mustHomeDir(), "~"},
	} {
		if sub.path == "" {
			continue
		}
		base := filepath.Clean(sub.path)
		if clean == base {
			return sub.repl
		}
		if strings.HasPrefix(clean, base+string(os.PathSeparator)) {
			return sub.repl + clean[len(base):]
		}
	}
	return p
}

func mustHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// redactURL strips credentials, path, query, and fragment from a URL
// for display — the report needs the endpoint, never userinfo or
// embedded secrets. Returns "" for unparseable input (the raw string
// is dropped entirely rather than risk leaking a token inside it).
func redactURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	u.User = nil
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// BuildFleetReport collects the report. cfg may be nil — the report
// then records an unenrolled/unparseable state and control-plane
// checks are marked skipped, exactly like doctor's pre-join behavior.
// Every probe degrades to a recorded outcome; nothing here panics or
// hard-fails on a missing engine, dead control plane, or absent
// config.
func BuildFleetReport(ctx context.Context, cfg *Config, opts FleetReportOpts) *FleetReport {
	if opts.Timeout <= 0 {
		opts.Timeout = fleetReportDefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	rep := &FleetReport{
		ReportVersion: reportSchemaVersion,
		GeneratedAt:   time.Now().UTC(),
	}
	info := Current()
	rep.Agent = ReportAgent{
		Version:   info.Version,
		Protocol:  info.Protocol,
		GoVersion: info.GoVersion,
		OS:        info.OS,
		Arch:      info.Arch,
	}

	// Known secrets to scrub from every emitted error string. The
	// node credential is the only live one on a configured agent.
	var secrets []string
	if cfg != nil {
		secrets = append(secrets, cfg.NodeToken)
	}

	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		if p, err := DefaultPath(); err == nil {
			cfgPath = p
		}
	}

	buildConfigSection(rep, cfgPath, cfg, opts, secrets)
	buildSystemSection(ctx, rep, cfgPath)
	buildTLSSection(rep, cfg, opts)
	buildEngineSection(ctx, rep, cfg, secrets)
	buildControlPlaneSection(ctx, rep, cfg, opts, secrets)
	buildSelfTestSection(ctx, rep, cfg, opts, secrets)
	buildUpdateSection(rep, cfgPath)

	rep.Notes = append([]string{
		"report is redacted by construction: no credentials, tokens, prompts, or completions are included",
		"hostname is the OS hostname — review before pasting into a public issue",
	}, rep.Notes...)
	return rep
}

// buildConfigSection records config existence/parsability and copies
// the non-secret fields out of cfg. When the caller passes nil cfg the
// file is loaded here so the report is identical whether the caller
// pre-loaded or not.
func buildConfigSection(rep *FleetReport, cfgPath string, cfg *Config, _ FleetReportOpts, secrets []string) {
	rep.Config.Path = redactHomePath(cfgPath)
	if cfgPath == "" {
		rep.Notes = append(rep.Notes, "config path could not be determined (no home dir)")
		return
	}
	_, statErr := os.Stat(cfgPath)
	rep.Config.Exists = statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		rep.Notes = append(rep.Notes, "config stat failed: "+scrubSecrets(statErr.Error(), secrets...))
	}

	effective := cfg
	if effective == nil && rep.Config.Exists {
		if loaded, err := LoadConfig(cfgPath); err == nil {
			effective = loaded
			secrets = append(secrets, loaded.NodeToken)
		} else {
			rep.Notes = append(rep.Notes, "config present but not parseable: "+scrubSecrets(err.Error(), secrets...))
		}
	}
	rep.Config.Parseable = effective != nil
	if effective == nil {
		return
	}
	rep.Config.Enrolled = effective.NodeID != "" && effective.NodeToken != ""
	rep.Config.ServerURL = redactURL(effective.ServerURL)
	rep.Config.NodeID = effective.NodeID
	rep.Config.Name = effective.Name
	rep.Config.OllamaURL = redactURL(effective.OllamaURL)
	rep.Config.Discovery = effective.DiscoveryEnabled
	rep.Config.DiscoveredCluster = effective.DiscoveredCluster
	rep.Config.UpdatePubkeyID = effective.UpdatePubkeyID
}

// buildSystemSection records the static hardware snapshot plus the
// measurable-memory and GPU-telemetry flags — the same data the
// heartbeat reports — and free space on the agent state filesystem.
func buildSystemSection(ctx context.Context, rep *FleetReport, cfgPath string) {
	hw := hardware.Detect(ctx)
	m := hardware.Metrics(ctx, 0, nil, filepath.Dir(cfgPath))
	rep.System = ReportSystem{
		Hostname:           hw.Hostname,
		CPUModel:           hw.CPUModel,
		CPUCores:           hw.CPUCores,
		MemoryTotalBytes:   hw.MemoryBytes,
		MemAvailMeasured:   m.MemAvailMeasured,
		GPUs:               hw.GPUs,
		GPUMetricsMeasured: m.GPUUtilMeasured || m.VRAMUsedMeasured,
	}
	if m.MemAvailMeasured {
		rep.System.MemoryAvailableBytes = m.MemoryAvailableBytes
		if m.MemoryTotalBytes > 0 {
			rep.System.MemoryTotalBytes = m.MemoryTotalBytes
		}
	}

	// Free space on the filesystem holding the agent state dir —
	// nearest existing ancestor, same approach as doctorDisk (a
	// pre-join dir may not exist yet).
	dir := filepath.Dir(cfgPath)
	if cfgPath == "" {
		dir = "."
	}
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
	if u, err := disk.Usage(dir); err == nil {
		rep.System.DiskFreeBytes = u.Free
		rep.System.DiskMeasured = true
		rep.System.DiskDir = redactHomePath(dir)
	}
}

// buildTLSSection fingerprints the pinned CA (config-embedded or the
// --ca-file override). Only the sha256 fingerprint is reported.
func buildTLSSection(rep *FleetReport, cfg *Config, opts FleetReportOpts) {
	caPEM := ""
	if cfg != nil {
		caPEM = cfg.CAPEM
	}
	if caPEM != "" {
		rep.TLS.CASource = "config"
	}
	if opts.CAFile != "" {
		if b, err := os.ReadFile(opts.CAFile); err == nil && isPEMCert(b) {
			caPEM = string(b)
			rep.TLS.CASource = "ca_file"
		} else if err != nil {
			rep.Notes = append(rep.Notes, "ca-file "+redactHomePath(opts.CAFile)+" unreadable: "+err.Error())
		} else {
			rep.Notes = append(rep.Notes, "ca-file "+redactHomePath(opts.CAFile)+" is not a PEM certificate — ignored")
		}
	}
	if caPEM == "" {
		return
	}
	rep.TLS.CAConfigured = true
	if fp := fingerprintOf([]byte(caPEM)); fp != "" {
		rep.TLS.CAFingerprintSHA256 = "sha256:" + fp
	} else {
		rep.Notes = append(rep.Notes, "configured CA is not a parseable PEM certificate")
	}
}

// buildEngineSection probes the local engine for reachability,
// version, and model inventory — model names are engine metadata, not
// user content, and safe to report.
func buildEngineSection(ctx context.Context, rep *FleetReport, cfg *Config, secrets []string) {
	ollamaURL := "http://127.0.0.1:11434"
	if cfg != nil && cfg.OllamaURL != "" {
		ollamaURL = cfg.OllamaURL
	}
	eng := engine.NewOllama(ollamaURL)
	rep.Engine.Type = eng.Type()
	rep.Engine.Address = redactURL(ollamaURL)
	if rep.Engine.Address == "" {
		rep.Engine.Address = "[unparseable]"
	}

	ctx2, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	start := time.Now()
	einfo, err := eng.Detect(ctx2)
	if err != nil || einfo == nil {
		rep.Engine.Error = scrubSecrets(errString(err), secrets...)
		return
	}
	rep.Engine.Reachable = true
	rep.Engine.Healthy = einfo.Healthy
	rep.Engine.Version = einfo.Version
	rep.Engine.LatencyMS = time.Since(start).Milliseconds()
	if einfo.Address != "" {
		rep.Engine.Address = redactURL(einfo.Address)
	}

	ctx3, cancel3 := context.WithTimeout(ctx, 15*time.Second)
	defer cancel3()
	models, err := eng.Models(ctx3)
	if err != nil {
		rep.Engine.Error = scrubSecrets(errString(err), secrets...)
		return
	}
	for _, m := range models {
		rep.Engine.Models = append(rep.Engine.Models, m.Name)
		if m.Loaded {
			rep.Engine.LoadedModels = append(rep.Engine.LoadedModels, m.Name)
		}
	}
	rep.Engine.ModelCount = len(rep.Engine.Models)
}

// buildControlPlaneSection runs the connectivity probes: DNS → TCP →
// /healthz → websocket upgrade → live CA compare. All are skipped
// (recorded, not failed) when the node has no usable server_url.
func buildControlPlaneSection(ctx context.Context, rep *FleetReport, cfg *Config, _ FleetReportOpts, secrets []string) {
	cp := &rep.ControlPlane
	cp.Healthz = "skipped"
	cp.WSUpgrade = "skipped"
	if cfg == nil || cfg.ServerURL == "" {
		return
	}
	base, err := url.Parse(strings.TrimRight(cfg.ServerURL, "/"))
	if err != nil || base.Host == "" {
		cp.Error = "unusable server_url in config"
		return
	}
	cp.Checked = true
	cp.ServerURL = redactURL(cfg.ServerURL)
	cp.Scheme = base.Scheme

	// DNS — skipped for literal IPs.
	host := base.Hostname()
	if net.ParseIP(host) != nil {
		cp.DNSChecked = false
		cp.Addrs = []string{host}
	} else {
		cp.DNSChecked = true
		dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, err := net.DefaultResolver.LookupIPAddr(dctx, host)
		dcancel()
		if err != nil {
			cp.Error = scrubSecrets("dns: "+err.Error(), secrets...)
		} else {
			cp.DNSResolved = true
			for _, a := range addrs {
				cp.Addrs = append(cp.Addrs, a.IP.String())
			}
		}
	}

	// TCP.
	hostport := base.Host
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		port := "443"
		if base.Scheme == "http" {
			port = "80"
		}
		hostport = net.JoinHostPort(hostport, port)
	}
	tctx, tcancel := context.WithTimeout(ctx, 5*time.Second)
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(tctx, "tcp", hostport)
	tcancel()
	if err != nil {
		cp.Error = scrubSecrets("tcp "+hostport+": "+err.Error(), secrets...)
		return
	}
	conn.Close()
	cp.TCPReachable = true
	cp.TCPLatencyMS = time.Since(start).Milliseconds()

	tlsCfg := tlsConfigForPEM(caPEMForReport(cfg))

	// /healthz.
	hc := &http.Client{Timeout: 8 * time.Second}
	if tlsCfg != nil {
		hc.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+"/healthz", nil)
	start = time.Now()
	resp, err := hc.Do(hreq)
	if err != nil {
		cp.Healthz = "unreachable"
		cp.Error = scrubSecrets("healthz: "+err.Error(), secrets...)
	} else {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		cp.HealthzMS = time.Since(start).Milliseconds()
		if resp.StatusCode == http.StatusOK {
			cp.Healthz = "ok"
		} else {
			cp.Healthz = fmt.Sprintf("http_%d", resp.StatusCode)
		}
	}

	// Websocket upgrade with the node credential — proves auth works
	// end to end. On 101 the socket closes immediately, before hello,
	// so a live agent's session is never disturbed (same guarantee as
	// doctor). The credential rides the Authorization header only; it
	// is never recorded.
	if cfg.NodeToken != "" {
		hdr := http.Header{}
		hdr.Set("Authorization", "Bearer "+cfg.NodeToken)
		wopts := &websocket.DialOptions{HTTPHeader: hdr}
		if tlsCfg != nil {
			wopts.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
		}
		wctx, wcancel := context.WithTimeout(ctx, 8*time.Second)
		start = time.Now()
		ws, wresp, err := websocket.Dial(wctx, cfg.WSURL(), wopts)
		wcancel()
		if err == nil {
			cp.WSLatencyMS = time.Since(start).Milliseconds()
			cp.WSUpgrade = "ok_101"
			ws.Close(websocket.StatusNormalClosure, "fleet-report")
		} else if wresp != nil {
			cp.WSUpgrade = fmt.Sprintf("rejected_%d", wresp.StatusCode)
			if wresp.StatusCode == http.StatusUnauthorized || wresp.StatusCode == http.StatusForbidden {
				cp.Error = "node credential rejected — re-run join to re-enroll"
			}
		} else {
			cp.WSUpgrade = "unreachable"
			cp.Error = scrubSecrets("websocket: "+err.Error(), secrets...)
		}
	}

	// Live CA compare (https only): the fetch is intentionally
	// unverified — the fingerprint comparison is what authenticates.
	if base.Scheme == "https" && rep.TLS.CAFingerprintSHA256 != "" {
		live := fetchLiveCAFingerprint(ctx, base.String())
		if live != "" {
			rep.TLS.LiveCAFingerprint = live
			match := live == rep.TLS.CAFingerprintSHA256
			rep.TLS.LiveCAMatches = &match
		}
	}
}

// fetchLiveCAFingerprint GETs the cluster CA and returns its
// "sha256:<hex>" fingerprint; "" when unavailable. Mirrors doctorCA.
func fetchLiveCAFingerprint(ctx context.Context, base string) string {
	hc := &http.Client{Timeout: 6 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/ca.pem", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !isPEMCert(body) {
		return ""
	}
	return "sha256:" + fingerprintOf(body)
}

// buildSelfTestSection runs (or records the skip of) the real
// inference self-test. Content is never recorded — only status,
// latency, and token counts.
func buildSelfTestSection(ctx context.Context, rep *FleetReport, cfg *Config, opts FleetReportOpts, secrets []string) {
	st := &rep.SelfTest
	if !opts.SelfTest {
		st.Status = "skipped"
		st.Reason = "not requested (pass --self-test to run one bounded inference)"
		return
	}
	if !rep.Engine.Reachable {
		st.Status = "skipped"
		st.Reason = "engine unreachable"
		return
	}
	model := opts.SelfTestModel
	if model == "" && len(rep.Engine.Models) > 0 {
		model = rep.Engine.Models[0]
	}
	if model == "" {
		st.Status = "skipped"
		st.Reason = "no models installed on the engine"
		return
	}
	ollamaURL := "http://127.0.0.1:11434"
	if cfg != nil && cfg.OllamaURL != "" {
		ollamaURL = cfg.OllamaURL
	}
	eng := engine.NewOllama(ollamaURL)
	maxTok := 8
	st.Model = model

	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	var lastUsage *contracts.Usage
	sawDone := false
	err := eng.Chat(ctx2, contracts.InferenceRequest{
		RequestID: "fleet-report-self-test",
		Model:     model,
		Messages: []contracts.Message{
			{Role: "user", Content: fleetReportSelfTestPrompt},
		},
		Stream:    true,
		MaxTokens: &maxTok,
	}, func(c contracts.ChatChunk) {
		if c.Usage != nil {
			lastUsage = c.Usage
		}
		if c.Done {
			sawDone = true
		}
	})
	st.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		st.Status = "failed"
		st.Reason = scrubSecrets(errString(err), secrets...)
		return
	}
	if !sawDone {
		st.Status = "failed"
		st.Reason = "stream ended without a done frame"
		return
	}
	st.Status = "passed"
	if lastUsage != nil {
		st.CompletionTokens = lastUsage.CompletionTokens
	}
}

// buildUpdateSection surfaces pending/last update outcomes — already
// one-line operator strings with no secrets.
func buildUpdateSection(rep *FleetReport, cfgPath string) {
	if cfgPath == "" {
		return
	}
	rep.Update.Pending = PendingUpdateSummary(cfgPath)
	rep.Update.Last = UpdateHistorySummary(cfgPath)
}

// caPEMForReport returns the CA PEM the report's probes should trust —
// the config's embedded CA (the --ca-file override only feeds the
// fingerprint section, matching doctor's split: file overrides the
// pinned comparison, config CA still drives the TLS client).
func caPEMForReport(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.CAPEM
}

func errString(err error) string {
	if err == nil {
		return "probe failed"
	}
	return err.Error()
}

// WriteFleetReport renders the report: indented JSON when opts.JSON,
// otherwise the human-readable summary. Collection errors are recorded
// inside the report — a returned error means the write itself failed.
func WriteFleetReport(ctx context.Context, w io.Writer, cfg *Config, opts FleetReportOpts) error {
	rep := BuildFleetReport(ctx, cfg, opts)
	if opts.JSON {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		_, err = w.Write(append(b, '\n'))
		return err
	}
	_, err := io.WriteString(w, renderFleetReportText(rep))
	return err
}

// renderFleetReportText produces the human-readable form: one
// section per line, keyed like doctor output.
func renderFleetReportText(r *FleetReport) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "localmesh-agent fleet report — %s (redacted: no credentials)\n",
		r.GeneratedAt.Format(time.RFC3339))

	fmt.Fprintf(&sb, "agent:    %s proto=%d %s/%s %s\n",
		r.Agent.Version, r.Agent.Protocol, r.Agent.OS, r.Agent.Arch, r.Agent.GoVersion)

	mem := fmt.Sprintf("%.1f GiB RAM", float64(r.System.MemoryTotalBytes)/(1<<30))
	if r.System.MemAvailMeasured {
		mem += fmt.Sprintf(" (%.1f GiB available)", float64(r.System.MemoryAvailableBytes)/(1<<30))
	} else {
		mem += " (available not measurable on this platform)"
	}
	gpuDesc := "none detected"
	if len(r.System.GPUs) > 0 {
		parts := make([]string, 0, len(r.System.GPUs))
		for _, g := range r.System.GPUs {
			d := g.Name
			if g.Vendor != "" {
				d += " (" + g.Vendor
				if g.SharedVRAM {
					d += ", shared mem"
				}
				d += ")"
			}
			parts = append(parts, d)
		}
		gpuDesc = strings.Join(parts, "; ")
	}
	fmt.Fprintf(&sb, "system:   host %q — %s, %d cores, %s; gpu %s\n",
		r.System.Hostname, r.System.CPUModel, r.System.CPUCores, mem, gpuDesc)
	if r.System.DiskMeasured {
		fmt.Fprintf(&sb, "disk:     %.1f GiB free on %s\n",
			float64(r.System.DiskFreeBytes)/(1<<30), r.System.DiskDir)
	}

	switch {
	case !r.Config.Exists:
		fmt.Fprintf(&sb, "config:   %s — absent (not enrolled)\n", r.Config.Path)
	case !r.Config.Parseable:
		fmt.Fprintf(&sb, "config:   %s — present but NOT parseable\n", r.Config.Path)
	case r.Config.Enrolled:
		fmt.Fprintf(&sb, "config:   %s — enrolled node %q name %q server %s\n",
			r.Config.Path, r.Config.NodeID, r.Config.Name, r.Config.ServerURL)
	default:
		fmt.Fprintf(&sb, "config:   %s — present, not fully enrolled\n", r.Config.Path)
	}

	if r.TLS.CAConfigured {
		line := fmt.Sprintf("tls:      cluster CA %s (from %s)", r.TLS.CAFingerprintSHA256, r.TLS.CASource)
		if r.TLS.LiveCAMatches != nil {
			if *r.TLS.LiveCAMatches {
				line += " — matches live cluster CA"
			} else {
				line += " — MISMATCH vs live cluster CA " + r.TLS.LiveCAFingerprint
			}
		}
		sb.WriteString(line + "\n")
	} else {
		sb.WriteString("tls:      no cluster CA pinned\n")
	}

	cp := r.ControlPlane
	if !cp.Checked {
		sb.WriteString("control:  skipped — not enrolled / no server_url\n")
	} else {
		var parts []string
		if cp.DNSChecked {
			if cp.DNSResolved {
				parts = append(parts, "dns ok → "+strings.Join(cp.Addrs, ","))
			} else {
				parts = append(parts, "dns FAILED")
			}
		}
		if cp.TCPReachable {
			parts = append(parts, fmt.Sprintf("tcp %dms", cp.TCPLatencyMS))
		} else {
			parts = append(parts, "tcp unreachable")
		}
		parts = append(parts, "healthz "+cp.Healthz)
		parts = append(parts, "ws "+cp.WSUpgrade)
		fmt.Fprintf(&sb, "control:  %s — %s\n", cp.ServerURL, strings.Join(parts, ", "))
		if cp.Error != "" {
			fmt.Fprintf(&sb, "          error: %s\n", cp.Error)
		}
	}

	eng := r.Engine
	if eng.Reachable {
		models := fmt.Sprintf("%d models", eng.ModelCount)
		if len(eng.Models) > 0 {
			models = fmt.Sprintf("%d [%s]", eng.ModelCount, strings.Join(eng.Models, ", "))
		}
		fmt.Fprintf(&sb, "engine:   %s %s — reachable in %dms, v%s, %s",
			eng.Type, eng.Address, eng.LatencyMS, eng.Version, models)
		if len(eng.LoadedModels) > 0 {
			fmt.Fprintf(&sb, ", loaded [%s]", strings.Join(eng.LoadedModels, ", "))
		}
		sb.WriteString("\n")
	} else {
		fmt.Fprintf(&sb, "engine:   %s %s — unreachable: %s\n", eng.Type, eng.Address, eng.Error)
	}

	st := r.SelfTest
	switch st.Status {
	case "passed":
		fmt.Fprintf(&sb, "self-test: PASSED — %s answered in %dms (%d completion tokens; content not recorded)\n",
			st.Model, st.LatencyMS, st.CompletionTokens)
	case "failed":
		fmt.Fprintf(&sb, "self-test: FAILED — %s: %s\n", st.Model, st.Reason)
	default:
		fmt.Fprintf(&sb, "self-test: skipped — %s\n", st.Reason)
	}

	if r.Update.Pending != "" {
		fmt.Fprintf(&sb, "update:   %s\n", r.Update.Pending)
	}
	if r.Update.Last != "" {
		fmt.Fprintf(&sb, "update:   %s\n", r.Update.Last)
	}

	for _, n := range r.Notes {
		fmt.Fprintf(&sb, "note:     %s\n", n)
	}
	return sb.String()
}

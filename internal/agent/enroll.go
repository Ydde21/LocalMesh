package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/localmesh/localmesh/internal/agent/hardware"
	"github.com/localmesh/localmesh/internal/creds"
)

// EnrollOptions controls the join flow.
type EnrollOptions struct {
	Server    string        // http(s)://host:port
	Token     string        // lme_ enrollment token
	Name      string        // display name; defaults to hostname
	CAFinger  string        // optional sha256:... fingerprint pin
	Wait      time.Duration // max time to wait for approval (0 = 30m)
	PollEvery time.Duration // approval poll interval
	Stderr    io.Writer     // progress output
}

// EnrollResult is the persisted outcome of a successful join.
type EnrollResult struct {
	Config *Config
}

// approvalReminderEvery is how often the approval wait re-prints a
// "still waiting" line so a long join does not look hung. Declared as a
// var so tests can shrink it — production value never changes.
var approvalReminderEvery = time.Minute

// Enroll performs the join handshake: exchange the enrollment token for a
// pending request, wait for admin approval, and return the node config.
// The node credential is generated locally — its plaintext never leaves
// the agent except over the (pinned) TLS enrollment channel.
func Enroll(ctx context.Context, opts EnrollOptions) (*EnrollResult, error) {
	if opts.Server == "" {
		return nil, errors.New("--server is required")
	}
	if opts.Token == "" {
		return nil, errors.New("--token is required")
	}
	if !strings.HasPrefix(opts.Token, creds.PrefixEnrollment+"_") {
		return nil, fmt.Errorf("--token is not an enrollment token (they start with %s_) — check for truncation, or mint a fresh one in the dashboard (Settings → Device enrollment)", creds.PrefixEnrollment)
	}
	if !strings.HasPrefix(opts.Server, "http") {
		opts.Server = "https://" + opts.Server
	}
	base := strings.TrimRight(opts.Server, "/")
	if opts.Wait <= 0 {
		opts.Wait = 30 * time.Minute
	}
	if opts.PollEvery <= 0 {
		opts.PollEvery = 3 * time.Second
	}
	logf := func(format string, a ...any) {
		if opts.Stderr != nil {
			fmt.Fprintf(opts.Stderr, format+"\n", a...)
		}
	}

	hw := hardware.Detect(ctx)
	if opts.Name == "" {
		opts.Name = hw.Hostname
	}
	if opts.Name == "" {
		opts.Name = "node"
	}

	// Bootstrap trust: fetch the cluster CA cert. When the server uses a
	// publicly trusted cert this is plain HTTPS; for the built-in CA we
	// fetch it once (optionally verified by fingerprint).
	caPEM, tlsCfg, err := fetchCA(ctx, base, opts.CAFinger, logf)
	if err != nil {
		return nil, err
	}

	hc := &http.Client{Timeout: 30 * time.Second}
	if tlsCfg != nil {
		hc.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}

	nodeToken := creds.Generate(creds.PrefixNode, 32)
	body, _ := json.Marshal(map[string]any{
		"token":      opts.Token,
		"name":       opts.Name,
		"hardware":   hw,
		"node_token": nodeToken,
	})
	resp, err := hc.Post(base+"/api/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s — check the server address/port and that the control plane is running (%w)", base, err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("enrollment token was rejected (invalid, expired, or exhausted)")
	}
	if resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("enroll failed: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var er struct {
		RequestID  string `json:"request_id"`
		PollSecret string `json:"poll_secret"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(respBody, &er); err != nil || er.RequestID == "" {
		return nil, errors.New("malformed enroll response")
	}
	logf("enrollment submitted — waiting for administrator approval (request %s)", er.RequestID)
	logf("  approve it in the dashboard (Settings → Device enrollment, or Devices → Add device)")
	logf("  or via the admin API: curl -sk -X POST \"%s/api/v1/enrollments/%s/approve\" -H \"Authorization: Bearer <admin-key>\"", base, er.RequestID)

	started := time.Now()
	deadline := started.Add(opts.Wait)
	nextReminder := started.Add(approvalReminderEvery)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for approval after %s — request %s stays pending server-side; re-run join (mint a fresh enrollment token if the old one is exhausted) and approve the new request promptly", opts.Wait, er.RequestID)
		}
		if time.Now().After(nextReminder) {
			logf("still waiting for administrator approval (request %s, %s elapsed)", er.RequestID, time.Since(started).Round(time.Second))
			nextReminder = time.Now().Add(approvalReminderEvery)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("join interrupted — request %s stays pending server-side; approve or deny it in the dashboard, then re-run join: %w", er.RequestID, ctx.Err())
		case <-time.After(opts.PollEvery):
		}
		// Poll secret travels in a header, not the URL — query strings
		// end up in intermediary access logs. The server accepts the
		// header (handlers.go) and keeps the ?secret= fallback for
		// older agents.
		u := fmt.Sprintf("%s/api/v1/enroll/%s", base, url.PathEscape(er.RequestID))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Poll-Secret", er.PollSecret)
		resp, err := hc.Do(req)
		if err != nil {
			logf("poll error: %v (retrying)", err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("enrollment request %s no longer exists on the server — re-run join to submit a new one", er.RequestID)
		}
		if resp.StatusCode != http.StatusOK {
			continue
		}
		var p struct {
			Status string `json:"status"`
			NodeID string `json:"node_id"`
			CAPEM  string `json:"ca_pem"`
		}
		if json.Unmarshal(b, &p) != nil {
			continue
		}
		switch p.Status {
		case "approved":
			if p.CAPEM != "" {
				caPEM = p.CAPEM // authoritative CA from server
			}
			return &EnrollResult{Config: &Config{
				ServerURL: base,
				NodeID:    p.NodeID,
				NodeToken: nodeToken,
				CAPEM:     caPEM,
				Name:      opts.Name,
			}}, nil
		case "denied":
			return nil, errors.New("enrollment denied by administrator")
		}
	}
}

// fetchCA obtains the cluster CA cert and a TLS config that trusts it.
// For https:// servers; http:// returns nil.
func fetchCA(ctx context.Context, base, fingerprint string, logf func(string, ...any)) (string, *tls.Config, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", nil, err
	}
	if u.Scheme != "https" {
		return "", nil, nil // plaintext dev mode
	}

	caURL := base + "/api/v1/ca.pem"
	// First try strict TLS — covers servers fronted by a public CA.
	strict := &http.Client{Timeout: 15 * time.Second}
	if resp, err := strict.Get(caURL); err == nil && resp.StatusCode == http.StatusOK {
		pemBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if isPEMCert(pemBytes) {
			return string(pemBytes), tlsConfigForCA(pemBytes), nil
		}
	}

	// Self-signed cluster CA: fetch once over insecure TLS and verify the
	// fingerprint when provided (TOFU otherwise, with a loud warning).
	insecure := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := insecure.Get(caURL)
	if err != nil {
		return "", nil, fmt.Errorf("cannot reach %s — check the server address/port and that the control plane is running (%w)", base, err)
	}
	pemBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if !isPEMCert(pemBytes) {
		return "", nil, errors.New("server did not return a CA certificate")
	}
	fp := "sha256:" + fingerprintOf(pemBytes)
	if fingerprint != "" {
		if !strings.EqualFold(fingerprint, fp) {
			return "", nil, fmt.Errorf("CA fingerprint mismatch: got %s, expected %s", fp, fingerprint)
		}
		logf("verified cluster CA fingerprint %s", fp)
	} else {
		logf("warning: trusting cluster CA %s (TOFU). Re-run with --ca-fingerprint=%s to pin explicitly.", fp, fp)
	}
	return string(pemBytes), tlsConfigForCA(pemBytes), nil
}

func isPEMCert(b []byte) bool {
	blk, _ := pem.Decode(b)
	return blk != nil && blk.Type == "CERTIFICATE"
}

func fingerprintOf(pemBytes []byte) string {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return ""
	}
	sum := sha256.Sum256(blk.Bytes)
	return hex.EncodeToString(sum[:])
}

func tlsConfigForCA(pemBytes []byte) *tls.Config {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pemBytes)
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
}

// TLSDialer returns a tls.Config from a persisted config for reconnects.
func (c *Config) TLSConfig() *tls.Config {
	if c.CAPEM == "" {
		return nil
	}
	return tlsConfigForCA([]byte(c.CAPEM))
}

// WSURL derives the websocket endpoint from the base server URL.
func (c *Config) WSURL() string {
	u := strings.Replace(c.ServerURL, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	return strings.TrimRight(u, "/") + "/api/v1/agent/connect"
}

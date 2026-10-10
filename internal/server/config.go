// Package server wires together the LocalMesh control plane: storage,
// node hub, scheduler, OpenAI gateway, and admin API.
package server

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen          string // TLS listen address, e.g. ":8443"
	DatabaseURL     string // postgres DSN
	DataDir         string // CA + cert persistence
	DevInsecureHTTP bool   // plain HTTP for local development
	// TrustProxy makes the rate limiter honor X-Forwarded-For. Default
	// false: only enable behind a reverse proxy that overwrites the
	// header, or clients can spoof their rate-limit identity.
	TrustProxy  bool
	AdminKey    string   // optional preset admin API key
	Hostnames   []string // extra SANs for the server cert
	CORSOrigins []string // allowed dashboard origins
	LogLevel    string
	// Discovery controls mDNS (_localmesh._tcp) advertisement on the
	// LAN. Default on — it announces only the service port, cluster
	// name, and CA fingerprint (all public info) and never authorizes
	// anything by itself. Disable on networks where multicast is
	// unwanted or unavailable.
	Discovery   bool
	ClusterName string // advertised instance name (default "localmesh")
	// Standby makes this instance an active-standby replica: it serves a
	// minimal status surface and waits on the advisory lock instead of
	// refusing startup when another control plane owns the cluster. On
	// takeover it promotes to full active duty in-process.
	Standby bool
}

func ConfigFromEnv() Config {
	c := Config{
		Listen:      envStr("LOCALMESH_LISTEN", ":8443"),
		DatabaseURL: envStr("LOCALMESH_DATABASE_URL", ""),
		DataDir:     envStr("LOCALMESH_DATA_DIR", "./data"),
		AdminKey:    os.Getenv("LOCALMESH_ADMIN_KEY"),
		LogLevel:    envStr("LOCALMESH_LOG_LEVEL", "info"),
	}
	if v := os.Getenv("LOCALMESH_DEV_INSECURE_HTTP"); v == "1" || v == "true" {
		c.DevInsecureHTTP = true
		c.Listen = envStr("LOCALMESH_LISTEN", ":8080")
	}
	for _, h := range strings.Split(os.Getenv("LOCALMESH_HOSTNAMES"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.Hostnames = append(c.Hostnames, h)
		}
	}
	for _, o := range strings.Split(os.Getenv("LOCALMESH_CORS_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.CORSOrigins = append(c.CORSOrigins, o)
		}
	}
	if os.Getenv("LOCALMESH_TRUST_PROXY") == "1" || os.Getenv("LOCALMESH_TRUST_PROXY") == "true" {
		c.TrustProxy = true
	}
	c.Discovery = true
	if v := os.Getenv("LOCALMESH_DISCOVERY"); v == "0" || v == "false" || v == "off" {
		c.Discovery = false
	}
	c.ClusterName = envStr("LOCALMESH_CLUSTER_NAME", "localmesh")
	if v := os.Getenv("LOCALMESH_ROLE"); v == "standby" || v == "STANDBY" {
		c.Standby = true
	}
	if v := os.Getenv("LOCALMESH_STANDBY"); v == "1" || v == "true" {
		c.Standby = true
	}
	if c.DevInsecureHTTP && len(c.CORSOrigins) == 0 {
		c.CORSOrigins = []string{"http://localhost:3000", "http://127.0.0.1:3000"}
	}
	return c
}

func (c Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("LOCALMESH_DATABASE_URL is required (postgres DSN)")
	}
	if c.AdminKey != "" && len(c.AdminKey) < 16 {
		return fmt.Errorf("LOCALMESH_ADMIN_KEY must be at least 16 characters")
	}
	return nil
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

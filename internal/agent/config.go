// Package agent implements the LocalMesh node agent: hardware discovery,
// secure enrollment, heartbeat reporting, and local inference execution
// via pluggable engine adapters.
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config is the persisted agent state.
type Config struct {
	ServerURL string `json:"server_url"` // http(s)://host:port of the control plane
	NodeID    string `json:"node_id"`
	NodeToken string `json:"node_token"` // lmn_ credential, agent-generated
	CAPEM     string `json:"ca_pem,omitempty"`
	Name      string `json:"name"`
	OllamaURL string `json:"ollama_url,omitempty"`
	// DiscoveryEnabled records that this node joined via mDNS
	// discovery. Informational only — server_url and ca_pem remain
	// authoritative; reconnects do not re-discover.
	DiscoveryEnabled bool `json:"discovery_enabled,omitempty"`
	// DiscoveredCluster is the advertised instance name of the cluster
	// joined through discovery.
	DiscoveredCluster string `json:"discovered_cluster,omitempty"`
	// DiscoveredAt is when the discovery-assisted join happened.
	DiscoveredAt time.Time `json:"discovered_at,omitempty"`
}

// DefaultDir returns the agent config directory.
func DefaultDir() (string, error) {
	if v := os.Getenv("LOCALMESH_AGENT_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".localmesh"), nil
}

// DefaultPath returns the default config file path.
func DefaultPath() (string, error) {
	d, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "agent.json"), nil
}

// LoadConfig reads the persisted config.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("corrupt config %s: %w", path, err)
	}
	return &c, nil
}

// Save writes config with owner-only permissions.
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

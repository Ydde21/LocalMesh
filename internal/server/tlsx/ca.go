// Package tlsx provides the LocalMesh internal certificate authority.
// The control plane generates a self-signed CA on first boot, persists it
// in the configured data directory, and issues itself a server
// certificate. Agents receive the CA certificate during enrollment and
// use it to verify the control plane — no public CA or DNS required.
package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	caCertFile = "ca.pem"
	caKeyFile  = "ca-key.pem"
	srvCert    = "server.pem"
	srvKey     = "server-key.pem"
)

// CA wraps a generated certificate authority on disk.
type CA struct {
	Dir      string
	Cert     *x509.Certificate
	Key      *ecdsa.PrivateKey
	Server   tls.Certificate
	Pool     *x509.CertPool
	PEMBytes []byte
}

// Fingerprint returns the SHA-256 fingerprint of the CA certificate —
// the value admins pin when enrolling agents with --ca-fingerprint.
func (c *CA) Fingerprint() string {
	sum := sha256.Sum256(c.Cert.Raw)
	return "sha256:" + hexEncode(sum[:])
}

func hexEncode(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0xf]
	}
	return string(out)
}

// LoadOrCreate opens (or initializes) a CA in dir and issues/loads a
// server certificate covering the given DNS names and IPs.
func LoadOrCreate(dir string, dnsNames []string, ips []net.IP) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Hostnames that are IP literals belong in the IP SAN list — a
	// DNS-name SAN "192.168.1.10" never matches dialing that address.
	dnsNames, ips = splitNamesIPs(dnsNames, ips)
	ca := &CA{Dir: dir}
	if fileExists(filepath.Join(dir, caCertFile)) && fileExists(filepath.Join(dir, caKeyFile)) {
		if err := ca.loadCA(); err != nil {
			return nil, err
		}
	} else {
		if err := ca.generateCA(); err != nil {
			return nil, err
		}
	}
	if fileExists(filepath.Join(dir, srvCert)) && fileExists(filepath.Join(dir, srvKey)) {
		cert, err := tls.LoadX509KeyPair(filepath.Join(dir, srvCert), filepath.Join(dir, srvKey))
		if err != nil {
			return nil, fmt.Errorf("load server cert: %w", err)
		}
		// Config changed (new hostname/IP)? Reissue — the CA persists,
		// so CA-pinned agents are unaffected by a new leaf.
		if leaf := leafOf(cert); leaf != nil && covers(leaf, dnsNames, ips) {
			ca.Server = cert
		} else {
			if err := ca.generateServerCert(dnsNames, ips); err != nil {
				return nil, err
			}
		}
	} else {
		if err := ca.generateServerCert(dnsNames, ips); err != nil {
			return nil, err
		}
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	ca.Pool = pool
	return ca, nil
}

// splitNamesIPs moves IP literals out of the DNS-name list into the IP
// SAN list; defaults apply inside generateServerCert.
func splitNamesIPs(dnsNames []string, ips []net.IP) ([]string, []net.IP) {
	var names []string
	for _, h := range dnsNames {
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
			continue
		}
		names = append(names, h)
	}
	return names, ips
}

// leafOf parses the leaf cert from a keypair for SAN inspection.
func leafOf(cert tls.Certificate) *x509.Certificate {
	if cert.Leaf != nil {
		return cert.Leaf
	}
	if len(cert.Certificate) == 0 {
		return nil
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	return leaf
}

// covers reports whether the existing leaf already covers every
// requested DNS name and IP — the only inputs to SAN correctness.
func covers(leaf *x509.Certificate, dnsNames []string, ips []net.IP) bool {
	haveDNS := map[string]bool{}
	for _, n := range leaf.DNSNames {
		haveDNS[n] = true
	}
	haveIP := map[string]bool{}
	for _, ip := range leaf.IPAddresses {
		haveIP[ip.String()] = true
	}
	for _, n := range dnsNames {
		if !haveDNS[n] {
			return false
		}
	}
	for _, ip := range ips {
		if !haveIP[ip.String()] {
			return false
		}
	}
	return true
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (c *CA) loadCA() error {
	certPEM, err := os.ReadFile(filepath.Join(c.Dir, caCertFile))
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(filepath.Join(c.Dir, caKeyFile))
	if err != nil {
		return err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return errors.New("bad CA cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return errors.New("bad CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return err
	}
	c.Cert, c.Key, c.PEMBytes = cert, key, certPEM
	return nil
}

func (c *CA) generateCA() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"LocalMesh"}, CommonName: "LocalMesh Root CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(filepath.Join(c.Dir, caCertFile), certPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.Dir, caKeyFile), keyPEM, 0o600); err != nil {
		return err
	}
	c.Cert, c.Key, c.PEMBytes = cert, key, certPEM
	return nil
}

func (c *CA) generateServerCert(dnsNames []string, ips []net.IP) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	// Loopback coverage is unconditional — local agents/dashboards dial
	// 127.0.0.1 regardless of the configured LAN names.
	dnsNames = append(dnsNames, "localhost", "localmesh-server")
	ips = append(ips, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"LocalMesh"}, CommonName: "localmesh-server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.Cert, &key.PublicKey, c.Key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(filepath.Join(c.Dir, srvCert), certPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.Dir, srvKey), keyPEM, 0o600); err != nil {
		return err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	c.Server = cert
	return nil
}

// ServerTLSConfig returns a TLS config for the control plane listener.
func (c *CA) ServerTLSConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{c.Server},
		MinVersion:   tls.VersionTLS13,
	}
}

// ClientPool returns the CA pool agents should pin when dialing.
func (c *CA) ClientPool() *x509.CertPool { return c.Pool }

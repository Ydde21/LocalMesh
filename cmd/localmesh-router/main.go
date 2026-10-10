// Command localmesh-router is the stable-endpoint TCP router for a
// LocalMesh active-standby control-plane pair. It health-checks each
// backend's /readyz and splices client connections to the instance that
// currently owns the cluster — after standby promotion, connections to
// the same router address reach the promoted control plane with no
// client or agent configuration change.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/localmesh/localmesh/internal/router"
)

func env(name, def string) string {
	if v := os.Getenv("LOCALMESH_ROUTER_" + name); v != "" {
		return v
	}
	return def
}

func main() {
	listen := flag.String("listen", env("LISTEN", ":8443"), "address clients/agents connect to")
	backends := flag.String("backends", env("BACKENDS", ""), "comma-separated control-plane URLs, primary first (https://a:8443,https://b:8443)")
	ca := flag.String("ca", env("CA", ""), "PEM CA bundle verifying backend TLS (cluster ca.pem)")
	insecure := flag.Bool("insecure", env("INSECURE", "") == "1", "skip backend TLS verification (dev only)")
	interval := flag.Duration("interval", mustDuration(env("INTERVAL", "1s")), "backend health-check interval")
	wait := flag.Duration("wait", mustDuration(env("WAIT", "10s")), "how long an accepted connection waits for a ready backend")
	drain := flag.Duration("drain", mustDuration(env("DRAIN", "30s")), "grace window for the surviving direction of a half-closed splice")
	rise := flag.Int("rise", mustInt(env("RISE", "2")), "consecutive healthy probes before a backend counts ready")
	fall := flag.Int("fall", mustInt(env("FALL", "3")), "consecutive failed probes before a ready backend is demoted")
	maxConns := flag.Int("max-conns", mustInt(env("MAX_CONNS", "4096")), "maximum simultaneous client connections")
	status := flag.String("status", env("STATUS", ""), "optional diagnostics listener (e.g. 127.0.0.1:9100 serves /status, /statusz, /metrics, /healthz)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *backends == "" {
		fmt.Fprintln(os.Stderr, "localmesh-router: -backends (or LOCALMESH_ROUTER_BACKENDS) is required")
		os.Exit(2)
	}
	r, err := router.New(log, router.Options{
		Listen:     *listen,
		Backends:   strings.Split(*backends, ","),
		CAFile:     *ca,
		Insecure:   *insecure,
		Interval:   *interval,
		Wait:       *wait,
		Drain:      *drain,
		Rise:       *rise,
		Fall:       *fall,
		MaxConns:   *maxConns,
		StatusAddr: *status,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "localmesh-router:", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := r.Serve(ctx, router.Options{
		Listen: *listen, StatusAddr: *status,
	}); err != nil {
		log.Error("router exited", "err", err)
		os.Exit(1)
	}
}

func mustDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Second
	}
	return d
}

func mustInt(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

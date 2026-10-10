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
	status := flag.String("status", env("STATUS", ""), "optional status listener (e.g. 127.0.0.1:9100 serves /status + /healthz)")
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

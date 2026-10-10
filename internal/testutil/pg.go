// Package testutil provides shared test infrastructure: a throwaway
// Postgres container (via docker) or the LOCALMESH_TEST_DSN env var.
package testutil

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// PostgresDSN returns a usable Postgres DSN for tests, starting a
// disposable container when necessary. Skips the test if neither docker
// nor LOCALMESH_TEST_DSN is available.
func PostgresDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("LOCALMESH_TEST_DSN"); dsn != "" {
		return dsn
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker and no LOCALMESH_TEST_DSN; skipping DB-backed test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	name := "localmesh-test-pg-" + randSuffix()
	// Random host port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot allocate port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	// Registered BEFORE `docker run` so every exit path — Skip on run
	// failure, readiness timeout, t.Fatal, panic — still sweeps a
	// container that may have been created daemon-side before the
	// client returned an error (e.g. context deadline mid-run).
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rmCancel()
		exec.CommandContext(rmCtx, "docker", "rm", "-f", name).Run()
	})

	// --tmpfs over PGDATA stops postgres:16-alpine's declared VOLUME
	// from leaking an anonymous volume per run (--rm only removes the
	// container) and is faster than disk. Ownership labels let
	// scripts/clean-test-resources.sh sweep only test-owned resources.
	cmd := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-l", "localmesh.test=1",
		"-l", "localmesh.test.role=postgres",
		"--tmpfs", "/var/lib/postgresql/data:rw,size=512m",
		"-e", "POSTGRES_PASSWORD=localmeshtest",
		"-e", "POSTGRES_DB=localmesh",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
		"postgres:16-alpine")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("docker postgres unavailable: %v: %s", err, strings.TrimSpace(string(out)))
	}

	dsn := fmt.Sprintf("postgres://postgres:localmeshtest@127.0.0.1:%d/localmesh?sslmode=disable", port)
	// Wait for readiness.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			c.Close()
			// Give postgres a beat to finish init after TCP comes up.
			time.Sleep(500 * time.Millisecond)
			if pingPG(ctx, dsn) {
				return dsn
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Skip("postgres container did not become ready")
	return ""
}

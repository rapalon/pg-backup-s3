//go:build integration

// Package test holds integration tests that need the compose stand from
// test/compose.yml: a PostgreSQL server, a Garage S3 server and the PostgreSQL
// client tools on PATH.
package test

import (
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestStand proves the harness itself works: the client tools are installed
// and both services are reachable. Later phases add real tests next to it.
func TestStand(t *testing.T) {
	if os.Getenv("PGBACKUP_INTEGRATION") != "1" {
		t.Skip("PGBACKUP_INTEGRATION != 1; run via test/compose.yml")
	}

	for _, bin := range []string{"pg_dump", "pg_dumpall", "pg_restore", "psql", "vacuumdb"} {
		path, err := exec.LookPath(bin)
		if err != nil {
			t.Fatalf("%s not on PATH: %v", bin, err)
		}
		out, err := exec.Command(path, "--version").CombinedOutput()
		if err != nil {
			t.Fatalf("%s --version: %v: %s", bin, err, out)
		}
		t.Logf("%s", strings.TrimSpace(string(out)))
	}

	s3, err := url.Parse(os.Getenv("S3_ENDPOINT"))
	if err != nil {
		t.Fatalf("S3_ENDPOINT: %v", err)
	}
	for _, addr := range []string{
		net.JoinHostPort(os.Getenv("PGHOST"), os.Getenv("PGPORT")),
		s3.Host,
	} {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		conn.Close()
	}

	out, err := exec.Command("psql", "-At", "-c", "select version()").CombinedOutput()
	if err != nil {
		t.Fatalf("psql: %v: %s", err, out)
	}
	t.Logf("server: %s", strings.TrimSpace(string(out)))
}

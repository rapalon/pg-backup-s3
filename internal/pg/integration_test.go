//go:build integration

package pg

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// standConn builds a Conn from the compose stand's environment.
func standConn(t *testing.T) Conn {
	t.Helper()
	if os.Getenv("PGBACKUP_INTEGRATION") != "1" {
		t.Skip("PGBACKUP_INTEGRATION != 1; run via test/compose.yml")
	}
	port, _ := strconv.Atoi(os.Getenv("PGPORT"))
	return Conn{
		Host:     os.Getenv("PGHOST"),
		Port:     port,
		User:     os.Getenv("PGUSER"),
		Password: os.Getenv("PGPASSWORD"),
		SSLMode:  os.Getenv("PGSSLMODE"),
		Database: os.Getenv("PGDATABASE"),
	}
}

func standRunner(t *testing.T) *Runner {
	t.Helper()
	return NewRunner(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func TestIntegrationVersions(t *testing.T) {
	conn := standConn(t)
	r := standRunner(t)
	ctx := context.Background()

	client, err := r.ClientVersion(ctx, "pg_dump")
	if err != nil {
		t.Fatalf("ClientVersion: %v", err)
	}
	server, err := r.ServerVersion(ctx, conn)
	if err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	t.Logf("client %s, server %s", client, server)
	if client.Major < 14 || server.Major < 14 {
		t.Errorf("unexpectedly old versions: client %s server %s", client, server)
	}
	if client.Major != server.Major {
		t.Errorf("stand mismatch: client %s vs server %s (image tag must equal PG_MAJOR)", client, server)
	}
}

func TestIntegrationListDatabases(t *testing.T) {
	conn := standConn(t)
	r := standRunner(t)
	dbs, err := r.ListDatabases(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListDatabases: %v", err)
	}
	t.Logf("databases: %v", dbs)
	if !contains(dbs, "postgres") || !contains(dbs, conn.MaintenanceDB()) {
		t.Errorf("expected postgres and %s in %v", conn.MaintenanceDB(), dbs)
	}
	for _, d := range dbs {
		if strings.HasPrefix(d, "template") {
			t.Errorf("template database %s listed", d)
		}
	}
}

func TestIntegrationDumpIsReadableArchive(t *testing.T) {
	conn := standConn(t)
	r := standRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Give the dump something to contain.
	_, err := r.QueryDB(ctx, conn, conn.MaintenanceDB(),
		"create table if not exists phase3 as select g as id, md5(g::text) as v from generate_series(1, 1000) g")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	dump, err := r.Dump(ctx, conn, conn.MaintenanceDB(), DumpOptions{Compression: "zstd:3", LockWaitTimeout: time.Minute})
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}

	// Stream the archive straight into pg_restore --list: no disk, no buffering.
	var toc strings.Builder
	listErr := r.Run(ctx, Spec{Binary: "pg_restore", Args: []string{"--list"}, Stdin: dump.Stdout, Stdout: &toc})
	if err := dump.Wait(); err != nil {
		t.Fatalf("pg_dump: %v", err)
	}
	if listErr != nil {
		t.Fatalf("pg_restore --list: %v", listErr)
	}
	if !strings.Contains(toc.String(), "Archive created at") {
		t.Errorf("TOC header missing:\n%s", toc.String())
	}
	if !strings.Contains(toc.String(), "TABLE public phase3") {
		t.Errorf("table phase3 missing from TOC:\n%s", toc.String())
	}
	if !strings.Contains(toc.String(), "Compression: ") {
		t.Logf("TOC does not state compression (older pg_restore?):\n%s", toc.String())
	}
}

func TestIntegrationDumpUnknownDatabaseFails(t *testing.T) {
	conn := standConn(t)
	r := standRunner(t)
	dump, err := r.Dump(context.Background(), conn, "does_not_exist_xyz", DumpOptions{Compression: "none"})
	if err != nil {
		t.Fatalf("Dump start: %v", err)
	}
	io.Copy(io.Discard, dump.Stdout)
	err = dump.Wait()
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("Wait = %v, want *ExitError", err)
	}
	if !strings.Contains(ee.StderrTail, "does_not_exist_xyz") {
		t.Errorf("stderr tail lacks the database name: %q", ee.StderrTail)
	}
}

func TestIntegrationDumpGlobals(t *testing.T) {
	conn := standConn(t)
	r := standRunner(t)
	p, err := r.DumpGlobals(context.Background(), conn)
	if err != nil {
		t.Fatalf("DumpGlobals: %v", err)
	}
	sql, _ := io.ReadAll(p.Stdout)
	if err := p.Wait(); err != nil {
		t.Fatalf("pg_dumpall: %v", err)
	}
	if !strings.Contains(string(sql), "CREATE ROLE") && !strings.Contains(string(sql), "ALTER ROLE") {
		t.Errorf("globals dump has no role statements:\n%s", sql)
	}
}

func TestIntegrationDumpCancel(t *testing.T) {
	conn := standConn(t)
	r := standRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// pg_sleep inside the dump session is not possible, so cancel a dump whose
	// output we never read: pg_dump blocks on the full pipe until SIGTERM.
	_, err := r.QueryDB(ctx, conn, conn.MaintenanceDB(),
		"create table if not exists phase3_big as select g as id, repeat(md5(g::text), 20) as v from generate_series(1, 200000) g")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	dump, err := r.Dump(ctx, conn, conn.MaintenanceDB(), DumpOptions{Compression: "none"})
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	buf := make([]byte, 4096)
	if _, err := io.ReadFull(dump.Stdout, buf); err != nil {
		t.Fatalf("first read: %v", err)
	}
	cancel()
	start := time.Now()
	err = dump.Wait()
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("Wait = %v, want context canceled", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("pg_dump took %s to stop", d)
	}
}

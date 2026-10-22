//go:build integration

package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"

	"github.com/rapalon/pg-backup-s3/internal/crypt"
	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pg"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

// raceEnabled is set by race_test.go when the race detector is on.
var raceEnabled = false

func standDeps(t *testing.T) (Deps, *store.S3Store, *age.X25519Identity, string) {
	t.Helper()
	if os.Getenv("PGBACKUP_INTEGRATION") != "1" {
		t.Skip("PGBACKUP_INTEGRATION != 1; run via test/compose.yml")
	}
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.NewS3(store.S3Config{
		Endpoint:  os.Getenv("S3_ENDPOINT"),
		Region:    os.Getenv("S3_REGION"),
		Bucket:    os.Getenv("S3_BUCKET"),
		AccessKey: os.Getenv("S3_ACCESS_KEY"),
		SecretKey: os.Getenv("S3_SECRET_KEY"),
		PathStyle: true,
		PartSize:  8 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(os.Getenv("PGPORT"))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prefix := fmt.Sprintf("it-pipeline/%d", time.Now().UnixNano())
	deps := Deps{
		Logger: logger,
		DB:     pg.NewRunner(logger),
		Store:  st,
		Conn: pg.Conn{
			Host: os.Getenv("PGHOST"), Port: port, User: os.Getenv("PGUSER"),
			Password: os.Getenv("PGPASSWORD"), SSLMode: os.Getenv("PGSSLMODE"), Database: os.Getenv("PGDATABASE"),
		},
		Recipients:      []age.Recipient{key.Recipient()},
		Encryption:      manifest.Encryption{Type: "age", Recipients: []string{key.Recipient().String()}},
		Prefix:          prefix,
		Cluster:         "stand",
		Compression:     "zstd:3",
		LockWaitTimeout: time.Minute,
		PartSize:        8 << 20,
		ToolVersion:     "integration",
	}
	t.Cleanup(func() {
		ctx := context.Background()
		infos, _ := st.List(ctx, prefix)
		keys := make([]string, len(infos))
		for i, o := range infos {
			keys[i] = o.Key
		}
		st.Delete(ctx, keys)
	})
	return deps, st, key, prefix
}

func TestIntegrationBackupRun(t *testing.T) {
	deps, st, key, _ := standDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runner := deps.DB.(*pg.Runner)

	// ~50 MB source database.
	const db = "p6src"
	runner.Query(ctx, deps.Conn, "drop database if exists "+db)
	if _, err := runner.Query(ctx, deps.Conn, "create database "+db); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { runner.Query(context.Background(), deps.Conn, "drop database if exists "+db) })
	if _, err := runner.QueryDB(ctx, deps.Conn, db,
		"create table big as select g as id, repeat(md5(g::text), 3) as v, now() as at from generate_series(1, 450000) g"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := runner.QueryDB(ctx, deps.Conn, db, "create index big_id on big(id)"); err != nil {
		t.Fatalf("index: %v", err)
	}
	rows, _ := runner.QueryDB(ctx, deps.Conn, db, "select pg_size_pretty(pg_database_size(current_database()))")
	t.Logf("source database size: %v", rows)

	report, err := Run(ctx, deps, RunOptions{Databases: []string{db}, Retry: DefaultRetryPolicy(2), StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.OK() {
		t.Fatalf("run failed: %+v", report.Failed())
	}
	m := report.Databases[0].Manifest
	t.Logf("dump %s: %d bytes in %.1fs", m.Key, m.SizeBytes, m.DurationSeconds)

	// The stored manifest matches what Run returned.
	rc, _, err := st.Get(ctx, manifest.ManifestKeyFor(m.Key))
	if err != nil {
		t.Fatalf("Get manifest: %v", err)
	}
	mb, _ := io.ReadAll(rc)
	rc.Close()
	stored, err := manifest.Unmarshal(mb)
	if err != nil || stored.SHA256 != m.SHA256 || stored.SizeBytes != m.SizeBytes || stored.GlobalsKey != report.Globals.Manifest.Key {
		t.Fatalf("stored manifest mismatch: %+v (%v)", stored, err)
	}

	// A fresh download hashes to the manifest's SHA-256 ...
	rc, info, err := st.Get(ctx, m.Key)
	if err != nil {
		t.Fatalf("Get dump: %v", err)
	}
	hr := NewHashReader(rc)
	// ... and decrypts into an archive pg_restore can read, without touching disk.
	dec, err := crypt.NewDecryptor(hr, []age.Identity{key})
	if err != nil {
		t.Fatalf("NewDecryptor: %v", err)
	}
	var toc bytes.Buffer
	if err := runner.Run(ctx, pg.Spec{Binary: "pg_restore", Args: []string{"--list"}, Stdin: dec, Stdout: &toc}); err != nil {
		t.Fatalf("pg_restore --list: %v", err)
	}
	// pg_restore --list stops reading after the TOC; drain the rest so the
	// whole stream is authenticated by age and hashed.
	if _, err := io.Copy(io.Discard, dec); err != nil {
		t.Fatalf("draining the decrypted stream: %v", err)
	}
	rc.Close()
	if hr.Sum() != m.SHA256 || hr.Count() != m.SizeBytes || info.Size != m.SizeBytes {
		t.Errorf("downloaded object: sha %s size %d, manifest sha %s size %d", hr.Sum(), hr.Count(), m.SHA256, m.SizeBytes)
	}
	if !strings.Contains(toc.String(), "TABLE public big") || !strings.Contains(toc.String(), "INDEX public big_id") {
		t.Errorf("TOC lacks the table or index:\n%s", toc.String())
	}

	// Globals decrypt and decompress to role statements.
	rc, _, err = st.Get(ctx, report.Globals.Manifest.Key)
	if err != nil {
		t.Fatalf("Get globals: %v", err)
	}
	gdec, err := crypt.NewDecryptor(rc, []age.Identity{key})
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zstd.NewReader(gdec)
	if err != nil {
		t.Fatal(err)
	}
	sql, err := io.ReadAll(zr)
	zr.Close()
	rc.Close()
	if err != nil || !strings.Contains(string(sql), "CREATE ROLE") {
		t.Errorf("globals: err=%v sql=%q", err, truncate(string(sql), 300))
	}

	// Memory: the whole dump must not have been buffered.
	if hwm := peakRSSMiB(t); hwm > 0 {
		t.Logf("peak RSS: %d MiB (race detector: %v)", hwm, raceEnabled)
		if !raceEnabled && hwm > 150 {
			t.Errorf("peak RSS %d MiB exceeds 150 MiB", hwm)
		}
	}
}

func TestIntegrationBackupMissingDatabase(t *testing.T) {
	deps, st, _, prefix := standDeps(t)
	ctx := context.Background()
	report, err := Run(ctx, deps, RunOptions{Databases: []string{"no_such_database_p6"}, Retry: DefaultRetryPolicy(3)})
	if err != nil {
		t.Fatal(err)
	}
	res := report.Databases[0]
	if res.Err == nil || res.Attempts != 1 || !strings.Contains(res.Err.Error(), "no_such_database_p6") {
		t.Errorf("result = %+v", res)
	}
	infos, _ := st.List(ctx, prefix)
	for _, o := range infos {
		if strings.Contains(o.Key, "no_such_database_p6") {
			t.Errorf("object left behind: %s", o.Key)
		}
	}
	if report.Globals.Err != nil {
		t.Errorf("globals failed: %v", report.Globals.Err)
	}
}

func peakRSSMiB(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.Atoi(fields[1])
				return kb / 1024
			}
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

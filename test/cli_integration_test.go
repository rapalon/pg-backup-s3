//go:build integration

package test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/rapalon/pg-backup-s3/internal/store"
)

// buildBinary compiles pgbackup once per test binary.
func buildBinary(t *testing.T) string {
	t.Helper()
	if os.Getenv("PGBACKUP_INTEGRATION") != "1" {
		t.Skip("PGBACKUP_INTEGRATION != 1; run via test/compose.yml")
	}
	bin := filepath.Join(t.TempDir(), "pgbackup")
	// -buildvcs=false: in CI the checkout is mounted into the container with a
	// foreign owner and git refuses to report VCS status, which would fail the build.
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "./cmd/pgbackup")
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func runCLI(t *testing.T, bin string, env []string, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	t.Logf("$ pgbackup %s → exit %d\n--- stdout ---\n%s--- stderr ---\n%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

func TestCLIBackup(t *testing.T) {
	bin := buildBinary(t)
	key, _ := age.GenerateX25519Identity()
	prefix := fmt.Sprintf("it-cli/%d", time.Now().UnixNano())
	stateDir := t.TempDir()
	env := []string{
		"AGE_RECIPIENTS=" + key.Recipient().String(),
		"S3_PREFIX=" + prefix,
		"STATE_DIR=" + stateDir,
		"CLUSTER_NAME=cli",
		"LOG_FORMAT=text",
	}

	st, err := store.NewS3(store.S3Config{
		Endpoint: os.Getenv("S3_ENDPOINT"), Region: os.Getenv("S3_REGION"), Bucket: os.Getenv("S3_BUCKET"),
		AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"), PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		infos, _ := st.List(context.Background(), prefix)
		var keys []string
		for _, o := range infos {
			keys = append(keys, o.Key)
		}
		st.Delete(context.Background(), keys)
	})

	// Give the stand's database something to back up, so restore targets are non-empty.
	if out, err := exec.Command("psql", "--no-password", "-c", "create table if not exists cli_rows as select g as id, md5(g::text) as v from generate_series(1, 1000) g").CombinedOutput(); err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}

	// Success: the database from the stand, explicit --db.
	code, stdout, _ := runCLI(t, bin, env, "backup", "--db", os.Getenv("PGDATABASE"))
	if code != 0 {
		t.Fatalf("backup exit %d", code)
	}
	if !strings.Contains(stdout, "globals") || !strings.Contains(stdout, "ok") || !strings.Contains(stdout, "0 failed") {
		t.Errorf("summary: %s", stdout)
	}
	infos, _ := st.List(context.Background(), prefix+"/cli/")
	var manifests int
	for _, o := range infos {
		if strings.HasSuffix(o.Key, ".manifest.json") {
			manifests++
		}
	}
	if manifests != 2 {
		t.Errorf("expected 2 manifests (globals + db), found %d in %v", manifests, infos)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "last_success")); err != nil {
		t.Errorf("heartbeat not written: %v", err)
	}

	// list shows both manifests; --json parses.
	code, stdout, _ = runCLI(t, bin, env, "list")
	if code != 0 || !strings.Contains(stdout, "globals") || !strings.Contains(stdout, os.Getenv("PGDATABASE")) || !strings.Contains(stdout, "2 manifest(s)") {
		t.Errorf("list: exit %d\n%s", code, stdout)
	}
	code, stdout, _ = runCLI(t, bin, env, "list", "--json", "--db", os.Getenv("PGDATABASE"))
	if code != 0 || !strings.Contains(stdout, `"kind": "dump"`) || strings.Contains(stdout, `"kind": "globals"`) {
		t.Errorf("list --json --db: exit %d\n%s", code, stdout)
	}

	// prune: nothing to delete with one run; after a second run and KEEP_LAST=1
	// the older dump and its globals go, newest stays.
	code, stdout, _ = runCLI(t, bin, append(env, "RETENTION_KEEP_LAST=1"), "prune", "--dry-run")
	if code != 0 || !strings.Contains(stdout, "would delete 0 object(s)") {
		t.Errorf("prune --dry-run (nothing): exit %d\n%s", code, stdout)
	}
	time.Sleep(1100 * time.Millisecond) // distinct run timestamp (second precision)
	if code, _, _ = runCLI(t, bin, env, "backup", "--db", os.Getenv("PGDATABASE")); code != 0 {
		t.Fatalf("second backup exit %d", code)
	}
	// The older dump and its manifest go; its globals are younger than the
	// orphan age (24 h) and stay until a later prune.
	code, stdout, _ = runCLI(t, bin, append(env, "RETENTION_KEEP_LAST=1"), "prune")
	if code != 0 || !strings.Contains(stdout, "deleted 2 object(s)") || !strings.Contains(stdout, "1 dump(s) kept") {
		t.Errorf("prune: exit %d\n%s", code, stdout)
	}
	code, stdout, _ = runCLI(t, bin, env, "list")
	if code != 0 || !strings.Contains(stdout, "3 manifest(s), 0 orphan(s)") {
		t.Errorf("list after prune: exit %d\n%s", code, stdout)
	}

	// restore / verify through the binary.
	var stderr string
	restoreEnv := append(env, "AGE_IDENTITY="+key.String())
	code, stdout, _ = runCLI(t, bin, restoreEnv, "restore", "--db", os.Getenv("PGDATABASE"), "--target-db", "test_cli_restored", "--create", "--dry-run")
	if code != 0 || !strings.Contains(stdout, "Dry run") || !strings.Contains(stdout, "create database") {
		t.Errorf("restore --dry-run: exit %d\n%s", code, stdout)
	}
	code, stdout, _ = runCLI(t, bin, restoreEnv, "restore", "--db", os.Getenv("PGDATABASE"), "--target-db", "test_cli_restored", "--create")
	if code != 0 || !strings.Contains(stdout, `Restored `) {
		t.Errorf("restore: exit %d\n%s", code, stdout)
	}
	code, _, stderr = runCLI(t, bin, restoreEnv, "restore", "--db", os.Getenv("PGDATABASE"), "--target-db", "test_cli_restored")
	if code != 3 || !strings.Contains(stderr, "add --clean") {
		t.Errorf("restore into non-empty: exit %d, want 3\n%s", code, stderr)
	}
	code, _, stderr = runCLI(t, bin, restoreEnv, "restore", "--db", os.Getenv("PGDATABASE"), "--target-db", "test_cli_restored", "--drop-existing")
	if code != 3 || !strings.Contains(stderr, "--yes") {
		t.Errorf("drop without yes: exit %d, want 3\n%s", code, stderr)
	}
	code, _, stderr = runCLI(t, bin, env, "restore", "--db", os.Getenv("PGDATABASE"), "--target-db", "x", "--create")
	if code != 2 || !strings.Contains(stderr, "AGE_IDENTITY or AGE_PASSPHRASE") {
		t.Errorf("restore without identity: exit %d, want 2\n%s", code, stderr)
	}
	code, stdout, _ = runCLI(t, bin, restoreEnv, "verify", "--db", os.Getenv("PGDATABASE"), "--full")
	if code != 0 || !strings.HasPrefix(stdout, "OK ") || !strings.Contains(stdout, "dropped") {
		t.Errorf("verify --full: exit %d\n%s", code, stdout)
	}
	runCLI(t, bin, restoreEnv, "restore", "--db", os.Getenv("PGDATABASE"), "--target-db", "test_cli_restored", "--drop-existing", "--yes", "--schema-only")

	// Failure: unknown database → exit 1, nothing uploaded for it, globals still ok.
	code, stdout, stderr = runCLI(t, bin, env, "backup", "--db", "no_such_db_cli")
	if code != 1 {
		t.Errorf("unknown database: exit %d, want 1", code)
	}
	if !strings.Contains(stdout, "FAILED") || !strings.Contains(stderr, "no_such_db_cli") {
		t.Errorf("failure not reported: stdout=%s stderr=%s", stdout, stderr)
	}

	// Configuration error: no key material → exit 2 before touching anything.
	code, _, stderr = runCLI(t, bin, []string{"AGE_RECIPIENTS=", "S3_PREFIX=" + prefix}, "backup")
	if code != 2 || !strings.Contains(stderr, "AGE_RECIPIENTS or AGE_PASSPHRASE") {
		t.Errorf("missing keys: exit %d stderr %s", code, stderr)
	}

	// Configuration error: wrong bucket → exit 2.
	code, _, stderr = runCLI(t, bin, append(env, "S3_BUCKET=nope-"+fmt.Sprint(time.Now().UnixNano())), "backup")
	if code != 2 || !strings.Contains(stderr, "bucket not found") {
		t.Errorf("wrong bucket: exit %d stderr %s", code, stderr)
	}
}

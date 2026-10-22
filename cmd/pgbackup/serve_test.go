package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/pipeline"
	"github.com/rapalon/pg-backup-s3/internal/sched"
)

// withEnv runs fn with osGetenv answering from m. Tests using it must not
// run in parallel with each other.
func withEnv(t *testing.T, m map[string]string, fn func()) {
	t.Helper()
	old := osGetenv
	osGetenv = func(k string) string { return m[k] }
	defer func() { osGetenv = old }()
	fn()
}

func TestHealthCommand(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{
		"S3_ENDPOINT": "http://s3:9000", "S3_BUCKET": "b", "S3_ACCESS_KEY": "a", "S3_SECRET_KEY": "s",
		"STATE_DIR": dir, "BACKUP_SCHEDULE": "0 3 * * *",
	}
	run := func() (int, string) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"health"}, &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}
	withEnv(t, env, func() {
		if code, out := run(); code != exitFailure || !strings.Contains(out, "no backup has ever succeeded") {
			t.Errorf("empty state dir: exit %d %q", code, out)
		}
		sched.WriteStarted(dir, time.Now())
		if code, out := run(); code != exitOK || !strings.Contains(out, "process start") {
			t.Errorf("fresh start: exit %d %q", code, out)
		}
		pipeline.WriteHeartbeat(dir, time.Now().Add(-48*time.Hour))
		if code, out := run(); code != exitFailure || !strings.Contains(out, "limit 26h0m0s") {
			t.Errorf("stale heartbeat: exit %d %q", code, out)
		}
		pipeline.WriteHeartbeat(dir, time.Now().Add(-time.Hour))
		if code, out := run(); code != exitOK || !strings.Contains(out, "last successful backup was") {
			t.Errorf("fresh heartbeat: exit %d %q", code, out)
		}
	})
	env["HEALTH_MAX_AGE"] = "30m"
	withEnv(t, env, func() {
		if code, _ := run(); code != exitFailure {
			t.Errorf("explicit max age 30m with a 1h-old heartbeat: exit %d", code)
		}
	})
	// A probe needs no S3 configuration at all.
	withEnv(t, map[string]string{"STATE_DIR": dir}, func() {
		if code, out := run(); code != exitOK {
			t.Errorf("health without S3 variables: exit %d %q", code, out)
		}
	})
	if _, err := os.Stat(filepath.Join(dir, sched.StartedFile)); err != nil {
		t.Error("started_at missing")
	}
}

func TestServeNeedsSchedule(t *testing.T) {
	env := map[string]string{
		"S3_ENDPOINT": "http://s3:9000", "S3_BUCKET": "b", "S3_ACCESS_KEY": "a", "S3_SECRET_KEY": "s",
		"AGE_RECIPIENTS": "age1x",
	}
	withEnv(t, env, func() {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"serve"}, &stdout, &stderr); code != exitUsage || !strings.Contains(stderr.String(), "BACKUP_SCHEDULE is required") {
			t.Errorf("serve without schedule: exit %d %s", code, stderr.String())
		}
		if code := run(context.Background(), []string{"serve", "extra"}, &stdout, &stderr); code != exitUsage {
			t.Errorf("serve with argument: exit %d", code)
		}
	})
}

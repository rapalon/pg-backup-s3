//go:build integration

package test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
)

// TestServe starts the real binary in serve mode with RUN_ON_START, waits
// for /healthz to turn green, checks /metrics and the health command, then
// stops it with SIGTERM and expects a clean exit.
func TestServe(t *testing.T) {
	bin := buildBinary(t)
	key, _ := age.GenerateX25519Identity()
	stateDir := t.TempDir()
	prefix := fmt.Sprintf("it-serve/%d", time.Now().UnixNano())
	const addr = "127.0.0.1:19097"
	env := append(os.Environ(),
		"AGE_RECIPIENTS="+key.Recipient().String(),
		"S3_PREFIX="+prefix,
		"STATE_DIR="+stateDir,
		"CLUSTER_NAME=serve",
		"BACKUP_SCHEDULE=* * * * *",
		"RUN_ON_START=true",
		"METRICS_ADDR="+addr,
		"SHUTDOWN_TIMEOUT=30s",
		"LOG_FORMAT=text",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "serve")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
		}
		t.Logf("--- serve stdout ---\n%s--- serve stderr (tail) ---\n%s", stdout.String(), tail(stderr.String(), 2500))
	}()

	// Wait for the first run to succeed.
	var healthy bool
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-exited:
			t.Fatalf("serve exited early: %v", err)
		default:
		}
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && strings.Contains(string(body), "last successful backup") {
				healthy = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !healthy {
		t.Fatal("/healthz never reported a successful backup")
	}

	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`pgbackup_runs_total{result="success"} 1`, "pgbackup_healthy 1", "pgbackup_last_run_bytes "} {
		if !strings.Contains(string(metrics), want) {
			t.Errorf("metrics lack %q:\n%s", want, metrics)
		}
	}
	if _, err := os.Stat(stateDir + "/last_success"); err != nil {
		t.Errorf("heartbeat missing: %v", err)
	}

	// The health command agrees, using only the state directory.
	hc := exec.Command(bin, "health")
	hc.Env = env
	if out, err := hc.CombinedOutput(); err != nil || !strings.Contains(string(out), "last successful backup") {
		t.Errorf("health: %v %s", err, out)
	}

	// Graceful stop.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("serve exit after SIGTERM: %v (want 0)", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("serve did not exit after SIGTERM")
	}
	if !strings.Contains(stderr.String(), "scheduler stopped") {
		t.Error("clean shutdown not logged")
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

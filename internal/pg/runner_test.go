package pg

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRunSuccess(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	var out strings.Builder
	if err := r.Run(context.Background(), Spec{Binary: "fake", Args: []string{"ok"}, Stdout: &out}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.String() != "hello\n" {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestRunExitError(t *testing.T) {
	t.Parallel()
	r, logs := fakeRunner(t)
	err := r.Run(context.Background(), Spec{Binary: "fake", Args: []string{"exit", "3"}})

	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("error %T %v, want *ExitError", err, err)
	}
	if ee.Binary != "fake" || ee.Code != 3 {
		t.Errorf("ExitError = %+v", ee)
	}
	if ee.StderrTail != "first warning\nsecond warning\n" {
		t.Errorf("StderrTail = %q", ee.StderrTail)
	}
	if want := "fake exited with code 3: second warning"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}

	// Every stderr line is logged at warn with the binary attached.
	var warnings []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if rec["level"] == "WARN" {
			warnings = append(warnings, rec)
		}
	}
	if len(warnings) != 2 {
		t.Fatalf("got %d warn records, want 2: %s", len(warnings), logs.String())
	}
	if warnings[0]["msg"] != "first warning" || warnings[0]["binary"] != "fake" {
		t.Errorf("first warn record = %v", warnings[0])
	}
}

func TestRunStderrPartialLineIsFlushed(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	err := r.Run(context.Background(), Spec{Binary: "fake", Args: []string{"stderr-partial"}})
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("error %v, want *ExitError", err)
	}
	if ee.Code != 2 || ee.StderrTail != "no newline at end\n" {
		t.Errorf("ExitError = %+v", ee)
	}
}

func TestRunStderrTailIsBounded(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	err := r.Run(context.Background(), Spec{Binary: "fake", Args: []string{"stderr-many"}})
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("error %v, want *ExitError", err)
	}
	if len(ee.StderrTail) > stderrTailLimit {
		t.Errorf("tail is %d bytes, limit %d", len(ee.StderrTail), stderrTailLimit)
	}
	if !strings.HasSuffix(ee.StderrTail, "099"+strings.Repeat("x", 1020)+"\n") {
		t.Error("tail does not end with the last line")
	}
	if strings.HasPrefix(ee.StderrTail, "000") {
		t.Error("tail still starts with the first line")
	}
}

func TestRunStdinAndStdout(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	in := strings.Repeat("payload ", 100000) // ~800 KiB through both pipes
	var out strings.Builder
	if err := r.Run(context.Background(), Spec{Binary: "fake", Args: []string{"echo"}, Stdin: strings.NewReader(in), Stdout: &out}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.String() != in {
		t.Errorf("stdout length %d, want %d", out.Len(), len(in))
	}
}

func TestRunEnvironmentIsExplicit(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	var out strings.Builder
	err := r.Run(context.Background(), Spec{Binary: "fake", Args: []string{"env"}, Env: []string{"PGPASSWORD=secret"}, Stdout: &out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	env := out.String()
	if !strings.Contains(env, "PGPASSWORD=secret\n") {
		t.Error("spec Env was not passed to the child")
	}
	if !strings.Contains(env, "PG_FAKE_HELPER=1\n") {
		t.Error("base env was not passed to the child")
	}
}

func TestRunMissingBinary(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	r.lookPath = func(name string) (string, error) { return "", errors.New("executable file not found in $PATH") }
	err := r.Run(context.Background(), Spec{Binary: "pg_dump"})
	if err == nil || !strings.Contains(err.Error(), "pg_dump: executable file not found") {
		t.Errorf("err = %v", err)
	}
}

func TestRunCancelTerminatesGracefully(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p, err := r.Start(ctx, Spec{Binary: "fake", Args: []string{"hang-term"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReady(t, p.Stdout)
	cancel()

	start := time.Now()
	err = p.Wait()
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Wait = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("child took %s to stop after SIGTERM", d)
	}
}

func TestRunCancelKillsAfterGrace(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	r.KillGrace = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p, err := r.Start(ctx, Spec{Binary: "fake", Args: []string{"hang-ignore"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReady(t, p.Stdout)
	cancel()

	start := time.Now()
	err = p.Wait()
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Wait = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 3*time.Second {
		t.Errorf("child stopped after %s, want roughly the %s grace period", d, r.KillGrace)
	}
	if err := p.Wait(); !errors.Is(err, context.Canceled) {
		t.Errorf("second Wait = %v, want the same error", err)
	}
}

func TestRunDeadline(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var out strings.Builder
	err := r.Run(ctx, Spec{Binary: "fake", Args: []string{"hang-term"}, Stdout: &out})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if !strings.HasPrefix(err.Error(), "fake: ") {
		t.Errorf("err = %q, want binary prefix", err)
	}
}

func TestProcessStdoutClosedEarly(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	p, err := r.Start(context.Background(), Spec{Binary: "fake", Args: []string{"echo"}, Stdin: strings.NewReader(strings.Repeat("x", 1<<20))})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	buf := make([]byte, 10)
	if _, err := io.ReadFull(p.Stdout, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	p.Stdout.Close()
	// The child may finish cleanly or die of EPIPE; either way Wait returns.
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait hung after stdout was closed early")
	}
}

// waitReady reads the child's "ready" line so a signal is not sent before
// the helper installed its handler.
func waitReady(t *testing.T, r io.Reader) {
	t.Helper()
	buf := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != "ready\n" {
		t.Fatalf("waiting for ready: %q %v", buf, err)
	}
}

func TestLastLine(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":              "",
		"one\n":         "one",
		"one\ntwo\n":    "two",
		"one\ntwo":      "two",
		"  padded  \n":  "padded",
		"a\n\n":         "a",
		"a\nb\n\n\n":    "b",
		"single":        "single",
		"x\r\ny\r\n":    "y",
		"last\nline\n ": "line",
	}
	for in, want := range cases {
		if got := lastLine(in); got != want {
			t.Errorf("lastLine(%q) = %q, want %q", in, got, want)
		}
	}
}

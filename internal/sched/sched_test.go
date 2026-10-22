package sched

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pipeline"
)

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func okReport(bytes int64) pipeline.Report {
	return pipeline.Report{
		Time:      t0,
		Duration:  3 * time.Second,
		Globals:   pipeline.Result{Name: "globals", Manifest: manifest.Manifest{SizeBytes: 10}},
		Databases: []pipeline.Result{{Name: "app", Manifest: manifest.Manifest{SizeBytes: bytes}}},
	}
}

func TestScheduleIntervalAndMaxAge(t *testing.T) {
	t.Parallel()
	cases := map[string]time.Duration{
		"0 3 * * *":    24 * time.Hour,
		"*/15 * * * *": 15 * time.Minute,
		"@hourly":      time.Hour,
		"0 0 * * 0":    7 * 24 * time.Hour,
		"":             0,
		"garbage":      0,
	}
	for spec, want := range cases {
		if got := ScheduleInterval(spec, time.UTC, t0); got != want {
			t.Errorf("ScheduleInterval(%q) = %s, want %s", spec, got, want)
		}
	}
	if MaxAge(0, 24*time.Hour) != 26*time.Hour {
		t.Error("daily: want 26h cap")
	}
	if MaxAge(0, 15*time.Minute) != 30*time.Minute {
		t.Error("15m: want 30m")
	}
	if MaxAge(0, 0) != MaxHealthAge {
		t.Error("unknown interval: want cap")
	}
	if MaxAge(2*time.Hour, 24*time.Hour) != 2*time.Hour {
		t.Error("explicit wins")
	}
}

func TestStatusHealthAndMetrics(t *testing.T) {
	t.Parallel()
	st := newStatus(t0)
	maxAge := time.Hour

	ok, msg := st.Healthy(t0.Add(30*time.Minute), maxAge)
	if !ok || !strings.Contains(msg, "process start") {
		t.Errorf("fresh process: %v %q", ok, msg)
	}
	ok, msg = st.Healthy(t0.Add(2*time.Hour), maxAge)
	if ok || !strings.Contains(msg, "limit 1h0m0s") {
		t.Errorf("stale process: %v %q", ok, msg)
	}

	st.recordBackup(okReport(500), nil, t0.Add(time.Hour))
	ok, msg = st.Healthy(t0.Add(90*time.Minute), maxAge)
	if !ok || !strings.Contains(msg, "last successful backup was 30m0s ago") {
		t.Errorf("after success: %v %q", ok, msg)
	}

	failed := okReport(1)
	failed.Databases[0].Err = errors.New("boom")
	st.recordBackup(failed, nil, t0.Add(2*time.Hour))
	st.recordBackup(pipeline.Report{Duration: 2 * time.Second}, errors.New("version mismatch"), t0.Add(3*time.Hour))
	ok, msg = st.Healthy(t0.Add(3*time.Hour), maxAge)
	if ok || !strings.Contains(msg, "last error: version mismatch") {
		t.Errorf("after failures: %v %q", ok, msg)
	}
	st.recordVerify(nil, t0.Add(4*time.Hour))
	st.recordVerify(errors.New("bad"), t0.Add(5*time.Hour))

	var buf bytes.Buffer
	st.setRunning("backup", true)
	st.Metrics(&buf, t0.Add(3*time.Hour), maxAge)
	out := buf.String()
	for _, want := range []string{
		"# TYPE pgbackup_last_success_timestamp_seconds gauge",
		fmt.Sprintf("pgbackup_last_success_timestamp_seconds %g", float64(t0.Add(time.Hour).Unix())),
		"pgbackup_last_run_duration_seconds 2",
		"pgbackup_last_run_bytes 510",
		"pgbackup_healthy 0",
		"pgbackup_running 1",
		`pgbackup_runs_total{result="success"} 1`,
		`pgbackup_runs_total{result="failure"} 2`,
		`pgbackup_runs_total{result="verify_success"} 1`,
		`pgbackup_runs_total{result="verify_failure"} 1`,
		"# TYPE pgbackup_runs_total counter",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}

	// HTTP handler.
	srv := httptest.NewServer(st.Handler(func() time.Time { return t0.Add(3 * time.Hour) }, maxAge))
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/healthz")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "last error") {
		t.Errorf("/healthz = %d %s", resp.StatusCode, body)
	}
	st.recordBackup(okReport(1), nil, t0.Add(3*time.Hour))
	resp, _ = http.Get(srv.URL + "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz after success = %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/metrics")
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "pgbackup_healthy 1") || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Errorf("/metrics = %d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	resp, _ = http.Get(srv.URL + "/other")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/other = %d", resp.StatusCode)
	}
}

func TestStateFilesAndCheckHealth(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if ok, msg := CheckHealth(dir, t0, time.Hour); ok || !strings.Contains(msg, "no backup has ever succeeded") {
		t.Errorf("empty dir: %v %q", ok, msg)
	}
	if err := WriteStarted(dir, t0); err != nil {
		t.Fatal(err)
	}
	if got := ReadStarted(dir); !got.Equal(t0) {
		t.Errorf("ReadStarted = %v", got)
	}
	if ok, _ := CheckHealth(dir, t0.Add(30*time.Minute), time.Hour); !ok {
		t.Error("fresh start should be healthy")
	}
	pipeline.WriteHeartbeat(dir, t0.Add(10*time.Hour))
	if ok, msg := CheckHealth(dir, t0.Add(10*time.Hour+30*time.Minute), time.Hour); !ok || !strings.Contains(msg, "last successful backup") {
		t.Errorf("heartbeat: %v %q", ok, msg)
	}
	if ok, _ := CheckHealth(dir, t0.Add(20*time.Hour), time.Hour); ok {
		t.Error("stale heartbeat should be unhealthy")
	}
	if !ReadStarted(t.TempDir()).IsZero() {
		t.Error("ReadStarted on a missing file")
	}
}

func TestNotifier(t *testing.T) {
	t.Parallel()
	type hit struct {
		method, path string
		body         Summary
	}
	var mu sync.Mutex
	var hits []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var s Summary
		if r.Method == http.MethodPost {
			json.NewDecoder(r.Body).Decode(&s)
		}
		mu.Lock()
		hits = append(hits, hit{r.Method, r.URL.Path, s})
		mu.Unlock()
		if r.URL.Path == "/reject" {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	n := Notifier{SuccessURL: srv.URL + "/ok", FailureURL: srv.URL + "/fail", Method: "POST", Logger: logger}
	n.Notify(context.Background(), summarize("prod", "1.0", okReport(42), nil))
	failed := okReport(1)
	failed.Databases[0].Err = errors.New("boom")
	n.Notify(context.Background(), summarize("prod", "1.0", failed, nil))
	n.Notify(context.Background(), summarize("prod", "1.0", pipeline.Report{}, errors.New("fatal")))

	snapshot := func() []hit {
		mu.Lock()
		defer mu.Unlock()
		return append([]hit(nil), hits...)
	}
	got := snapshot()
	if len(got) != 3 || got[0].path != "/ok" || got[1].path != "/fail" || got[2].path != "/fail" {
		t.Fatalf("hits = %+v", got)
	}
	if h := got[0]; h.method != "POST" || h.body.Result != "success" || h.body.Bytes != 42 || h.body.Cluster != "prod" || h.body.Databases != 1 {
		t.Errorf("success body = %+v", h.body)
	}
	if h := got[1]; h.body.Result != "failure" || len(h.body.Failed) != 1 || h.body.Failed[0] != "app" {
		t.Errorf("failure body = %+v", h.body)
	}
	if h := got[2]; h.body.Error != "fatal" {
		t.Errorf("fatal body = %+v", h.body)
	}

	// GET sends no body; rejections and connection errors are only logged.
	g := Notifier{SuccessURL: srv.URL + "/reject", Method: "GET", Logger: logger}
	g.Notify(context.Background(), Summary{Result: "success"})
	bad := Notifier{FailureURL: "http://127.0.0.1:1/nope", Method: "POST", Logger: logger, Client: &http.Client{Timeout: time.Second}}
	bad.Notify(context.Background(), Summary{Result: "failure"})
	if got := snapshot(); len(got) != 4 || got[3].method != "GET" {
		t.Errorf("GET hit = %+v", got)
	}
	if !strings.Contains(logs.String(), "notification rejected") || !strings.Contains(logs.String(), "notification failed") {
		t.Errorf("logs = %s", logs.String())
	}
	if (Notifier{}).enabled() || !(Notifier{FailureURL: "x"}).enabled() {
		t.Error("enabled")
	}
	(Notifier{}).Notify(context.Background(), Summary{Result: "success"}) // no URL: nothing happens
}

func newScheduler(t *testing.T, opts Options, jobs Jobs) *Scheduler {
	t.Helper()
	if opts.BackupSpec == "" {
		opts.BackupSpec = "0 3 * * *"
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	s, err := New(opts, jobs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	ok := func(context.Context) (pipeline.Report, error) { return pipeline.Report{}, nil }
	if _, err := New(Options{}, Jobs{Backup: ok}); err == nil {
		t.Error("no schedule accepted")
	}
	if _, err := New(Options{BackupSpec: "0 3 * * *"}, Jobs{}); err == nil {
		t.Error("no job accepted")
	}
	if _, err := New(Options{BackupSpec: "bad"}, Jobs{Backup: ok}); err == nil {
		t.Error("bad spec accepted")
	}
	if _, err := New(Options{BackupSpec: "@daily", VerifySpec: "@weekly"}, Jobs{Backup: ok}); err == nil {
		t.Error("verify spec without job accepted")
	}
	if _, err := New(Options{BackupSpec: "@daily", VerifySpec: "nope"}, Jobs{Backup: ok, Verify: func(context.Context) error { return nil }}); err == nil {
		t.Error("bad verify spec accepted")
	}
	s, err := New(Options{BackupSpec: "*/10 * * * *"}, Jobs{Backup: ok})
	if err != nil || s.MaxAge() != 20*time.Minute || s.opts.ShutdownTimeout != 30*time.Second || s.opts.Location != time.UTC {
		t.Errorf("defaults: %+v %v", s.opts, err)
	}
}

func TestRunIdleShutdown(t *testing.T) {
	t.Parallel()
	s := newScheduler(t, Options{ShutdownTimeout: 5 * time.Second}, Jobs{Backup: func(context.Context) (pipeline.Report, error) { return okReport(1), nil }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-s.started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("idle shutdown = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestRunOnStartThenShutdown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var runs atomic.Int32
	started := make(chan struct{})
	s := newScheduler(t, Options{RunOnStart: true, ShutdownTimeout: 5 * time.Second, StateDir: dir, PruneAfterBackup: true},
		Jobs{
			Backup: func(ctx context.Context) (pipeline.Report, error) {
				runs.Add(1)
				close(started)
				return okReport(7), nil
			},
			Prune: func(context.Context) error { runs.Add(10); return nil },
		})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-started
	// Give the job time to finish recording before shutting down.
	deadline := time.Now().Add(5 * time.Second)
	for s.anyRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
	if runs.Load() != 11 {
		t.Errorf("backup+prune runs = %d, want 11", runs.Load())
	}
	if ok, _ := s.Status().Healthy(time.Now(), time.Hour); !ok {
		t.Error("status not healthy after a successful run")
	}
	if ReadStarted(dir).IsZero() {
		t.Error("started_at not written")
	}
}

func TestRunAbortsRunningJob(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	var sawCancel atomic.Bool
	s := newScheduler(t, Options{RunOnStart: true, ShutdownTimeout: 5 * time.Second},
		Jobs{Backup: func(ctx context.Context) (pipeline.Report, error) {
			close(entered)
			<-ctx.Done()
			sawCancel.Store(true)
			return pipeline.Report{}, ctx.Err()
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-entered
	cancel()
	err := <-done
	if !errors.Is(err, ErrAborted) {
		t.Errorf("Run = %v, want ErrAborted", err)
	}
	if !sawCancel.Load() {
		t.Error("job context was not cancelled")
	}
	if n := s.Status().runs["failure"]; n != 1 {
		t.Errorf("failure count = %d", n)
	}
}

func TestRunShutdownTimeout(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	s := newScheduler(t, Options{RunOnStart: true, ShutdownTimeout: 200 * time.Millisecond},
		Jobs{Backup: func(ctx context.Context) (pipeline.Report, error) {
			close(entered)
			<-release // ignores cancellation
			return pipeline.Report{}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrAborted) {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not give up after the shutdown timeout")
	}
	close(release)
}

func TestRunJobSkipsOverlap(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := newScheduler(t, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))},
		Jobs{Backup: func(context.Context) (pipeline.Report, error) { return okReport(1), nil }})
	s.runCtx, s.cancel = context.WithCancel(context.Background())
	defer s.cancel()

	block := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runJob("backup", func(context.Context) { <-block })
	}()
	for !s.anyRunning() {
		time.Sleep(time.Millisecond)
	}
	ran := false
	s.runJob("backup", func(context.Context) { ran = true })
	if ran {
		t.Error("overlapping run was not skipped")
	}
	if !strings.Contains(logs.String(), "previous one still running") {
		t.Error("skip not logged")
	}
	// A different job is independent.
	other := false
	s.runJob("verify", func(context.Context) { other = true })
	if !other {
		t.Error("independent job was blocked")
	}
	close(block)
	wg.Wait()
	s.runJob("backup", func(context.Context) { ran = true })
	if !ran {
		t.Error("run after release was skipped")
	}
}

func TestVerifyJobAndNotification(t *testing.T) {
	t.Parallel()
	var got []Summary
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var s Summary
		json.NewDecoder(r.Body).Decode(&s)
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	}))
	defer srv.Close()
	s := newScheduler(t, Options{VerifySpec: "@weekly", Notifier: Notifier{SuccessURL: srv.URL, FailureURL: srv.URL, Method: "POST"}},
		Jobs{
			Backup: func(context.Context) (pipeline.Report, error) { return okReport(1), nil },
			Verify: func(context.Context) error { return errors.New("verify broke") },
		})
	s.runCtx, s.cancel = context.WithCancel(context.Background())
	defer s.cancel()
	s.runJob("backup", s.backup)
	s.runJob("verify", s.verify)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].Event != "backup" || got[0].Result != "success" || got[1].Event != "verify" || got[1].Error != "verify broke" {
		t.Errorf("notifications = %+v", got)
	}
	if s.Status().runs["verify_failure"] != 1 {
		t.Error("verify failure not recorded")
	}
}

func TestMetricsServerStartsAndStops(t *testing.T) {
	t.Parallel()
	s := newScheduler(t, Options{MetricsAddr: "127.0.0.1:0", ShutdownTimeout: time.Second},
		Jobs{Backup: func(context.Context) (pipeline.Report, error) { return okReport(1), nil }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-s.started
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
	// A port that cannot be bound fails Run immediately.
	bad := newScheduler(t, Options{MetricsAddr: "256.256.256.256:1"},
		Jobs{Backup: func(context.Context) (pipeline.Report, error) { return okReport(1), nil }})
	if err := bad.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "metrics listener") {
		t.Errorf("bad listener: %v", err)
	}
}

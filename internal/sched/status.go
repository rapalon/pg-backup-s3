// Package sched runs backups on a cron schedule inside the container and
// exposes health and metrics for the outside world.
package sched

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/rapalon/pg-backup-s3/internal/pipeline"
)

// StartedFile records when serve started; health treats it like a success
// so a fresh deployment is not "unhealthy" until its first scheduled run.
const StartedFile = "started_at"

// MaxHealthAge caps the automatic health window.
const MaxHealthAge = 26 * time.Hour

// Status is the in-memory view the HTTP endpoints serve. It is updated by
// the job wrapper and seeded from the state directory at startup.
type Status struct {
	mu          sync.Mutex
	startedAt   time.Time
	lastSuccess time.Time
	lastRun     time.Time
	lastDur     time.Duration
	lastBytes   int64
	lastError   string
	runs        map[string]int // result -> count
	running     map[string]bool
	verifyOK    time.Time
	verifyError string
}

func newStatus(startedAt time.Time) *Status {
	return &Status{startedAt: startedAt, runs: map[string]int{}, running: map[string]bool{}}
}

func (s *Status) setRunning(job string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running[job] = on
}

func (s *Status) recordBackup(report pipeline.Report, err error, finished time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRun = finished
	s.lastDur = report.Duration
	if err == nil && report.OK() {
		s.runs["success"]++
		s.lastSuccess = finished
		s.lastError = ""
		var bytes int64
		for _, d := range report.Databases {
			bytes += d.Manifest.SizeBytes
		}
		bytes += report.Globals.Manifest.SizeBytes
		s.lastBytes = bytes
		return
	}
	s.runs["failure"]++
	switch {
	case err != nil:
		s.lastError = err.Error()
	default:
		var names []string
		for _, f := range report.Failed() {
			names = append(names, f.Name)
		}
		s.lastError = "failed: " + strings.Join(names, ", ")
	}
}

func (s *Status) recordVerify(err error, finished time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.verifyOK = finished
		s.verifyError = ""
		s.runs["verify_success"]++
		return
	}
	s.verifyError = err.Error()
	s.runs["verify_failure"]++
}

// seedLastSuccess loads the heartbeat written by a previous process.
func (s *Status) seedLastSuccess(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.After(s.lastSuccess) {
		s.lastSuccess = t
	}
}

// Healthy reports whether the last success (or the start, if nothing
// succeeded yet) is within maxAge of now.
func (s *Status) Healthy(now time.Time, maxAge time.Duration) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return healthy(now, maxAge, s.lastSuccess, s.startedAt, s.lastError)
}

func healthy(now time.Time, maxAge time.Duration, lastSuccess, startedAt time.Time, lastError string) (bool, string) {
	ref, what := lastSuccess, "last successful backup"
	if ref.IsZero() {
		ref, what = startedAt, "process start (no backup yet)"
	}
	if ref.IsZero() {
		return false, "no backup has ever succeeded and the start time is unknown"
	}
	age := now.Sub(ref)
	if age > maxAge {
		msg := fmt.Sprintf("%s was %s ago, limit %s", what, age.Round(time.Second), maxAge)
		if lastError != "" {
			msg += "; last error: " + lastError
		}
		return false, msg
	}
	return true, fmt.Sprintf("%s was %s ago", what, age.Round(time.Second))
}

// Metrics renders the Prometheus text exposition format by hand; no client
// library is needed for a handful of series.
func (s *Status) Metrics(w io.Writer, now time.Time, maxAge time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok, _ := healthy(now, maxAge, s.lastSuccess, s.startedAt, s.lastError)
	gauge := func(name, help string, v float64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
	}
	ts := func(t time.Time) float64 {
		if t.IsZero() {
			return 0
		}
		return float64(t.Unix())
	}
	gauge("pgbackup_last_success_timestamp_seconds", "Unix time of the last fully successful backup run.", ts(s.lastSuccess))
	gauge("pgbackup_last_run_timestamp_seconds", "Unix time of the last backup run, successful or not.", ts(s.lastRun))
	gauge("pgbackup_last_run_duration_seconds", "Duration of the last backup run.", s.lastDur.Seconds())
	gauge("pgbackup_last_run_bytes", "Bytes uploaded by the last successful backup run.", float64(s.lastBytes))
	gauge("pgbackup_last_verify_success_timestamp_seconds", "Unix time of the last successful scheduled verify.", ts(s.verifyOK))
	gauge("pgbackup_healthy", "1 if the last success is within the health window.", b2f(ok))
	running := 0.0
	for _, on := range s.running {
		if on {
			running = 1
		}
	}
	gauge("pgbackup_running", "1 while a scheduled job is running.", running)
	gauge("pgbackup_started_timestamp_seconds", "Unix time the scheduler process started.", ts(s.startedAt))

	fmt.Fprintf(w, "# HELP pgbackup_runs_total Scheduled runs by result.\n# TYPE pgbackup_runs_total counter\n")
	for _, result := range []string{"success", "failure", "verify_success", "verify_failure"} {
		fmt.Fprintf(w, "pgbackup_runs_total{result=%q} %d\n", result, s.runs[result])
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Handler serves /healthz and /metrics.
func (s *Status) Handler(now func() time.Time, maxAge time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ok, msg := s.Healthy(now(), maxAge)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		fmt.Fprintf(w, "%s\n", msg)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.Metrics(w, now(), maxAge)
	})
	return mux
}

// ScheduleInterval returns the time between two consecutive firings of a
// cron spec after now, or 0 if the spec is empty or invalid.
func ScheduleInterval(spec string, loc *time.Location, now time.Time) time.Duration {
	if spec == "" {
		return 0
	}
	s, err := cron.ParseStandard(spec)
	if err != nil {
		return 0
	}
	if loc == nil {
		loc = time.UTC
	}
	first := s.Next(now.In(loc))
	second := s.Next(first)
	return second.Sub(first)
}

// MaxAge returns the health window: the configured value, or twice the
// schedule interval capped at MaxHealthAge when the configured value is 0.
func MaxAge(configured, interval time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	if interval <= 0 {
		return MaxHealthAge
	}
	return min(2*interval, MaxHealthAge)
}

// WriteStarted records the process start time in dir.
func WriteStarted(dir string, t time.Time) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, StartedFile), []byte(t.UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// ReadStarted returns the recorded start time, or zero if there is none.
func ReadStarted(dir string) time.Time {
	b, err := os.ReadFile(filepath.Join(dir, StartedFile))
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}
	}
	return t
}

// CheckHealth is the `health` command: it reads the state directory and
// decides without touching the network.
func CheckHealth(dir string, now time.Time, maxAge time.Duration) (bool, string) {
	last, _ := pipeline.ReadHeartbeat(dir)
	return healthy(now, maxAge, last, ReadStarted(dir), "")
}

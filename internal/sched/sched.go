package sched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/rapalon/pg-backup-s3/internal/pipeline"
)

// ErrAborted is returned by Run when shutdown interrupted a running job.
var ErrAborted = errors.New("shutdown aborted a running job")

// Jobs are the units of work the scheduler runs; the command layer builds
// them from the pipeline so this package stays free of S3 and PostgreSQL.
type Jobs struct {
	Backup func(ctx context.Context) (pipeline.Report, error)
	Prune  func(ctx context.Context) error // optional, after a successful backup
	Verify func(ctx context.Context) error // optional, on VerifySpec
}

// Options configure the scheduler.
type Options struct {
	BackupSpec       string
	VerifySpec       string
	Location         *time.Location
	RunOnStart       bool
	PruneAfterBackup bool
	ShutdownTimeout  time.Duration
	HealthMaxAge     time.Duration // 0 = automatic
	MetricsAddr      string        // empty disables the HTTP server
	StateDir         string
	Cluster          string
	ToolVersion      string
	Notifier         Notifier
	Logger           *slog.Logger
	Now              func() time.Time
}

// Scheduler owns the cron, the HTTP server and the status.
type Scheduler struct {
	opts   Options
	jobs   Jobs
	log    *slog.Logger
	now    func() time.Time
	status *Status
	maxAge time.Duration

	mu      sync.Mutex
	locks   map[string]bool // job name -> running
	wg      sync.WaitGroup  // every job execution
	runCtx  context.Context
	cancel  context.CancelFunc
	started chan struct{}
}

// New validates the options and prepares the scheduler without starting it.
func New(opts Options, jobs Jobs) (*Scheduler, error) {
	if opts.BackupSpec == "" {
		return nil, errors.New("sched: BACKUP_SCHEDULE is required")
	}
	if jobs.Backup == nil {
		return nil, errors.New("sched: no backup job")
	}
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	if _, err := cron.ParseStandard(opts.BackupSpec); err != nil {
		return nil, fmt.Errorf("sched: BACKUP_SCHEDULE: %w", err)
	}
	if opts.VerifySpec != "" {
		if _, err := cron.ParseStandard(opts.VerifySpec); err != nil {
			return nil, fmt.Errorf("sched: VERIFY_SCHEDULE: %w", err)
		}
		if jobs.Verify == nil {
			return nil, errors.New("sched: VERIFY_SCHEDULE set but no verify job")
		}
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 30 * time.Second
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Scheduler{
		opts:    opts,
		jobs:    jobs,
		log:     log,
		now:     now,
		status:  newStatus(now()),
		locks:   map[string]bool{},
		started: make(chan struct{}),
	}
	s.maxAge = MaxAge(opts.HealthMaxAge, ScheduleInterval(opts.BackupSpec, opts.Location, now()))
	if opts.StateDir != "" {
		if last, err := pipeline.ReadHeartbeat(opts.StateDir); err == nil {
			s.status.seedLastSuccess(last)
		}
	}
	return s, nil
}

// Status exposes the live status (for tests and the HTTP handler).
func (s *Scheduler) Status() *Status { return s.status }

// MaxAge is the health window in effect.
func (s *Scheduler) MaxAge() time.Duration { return s.maxAge }

// Handler serves /healthz and /metrics for this scheduler.
func (s *Scheduler) Handler() http.Handler { return s.status.Handler(s.now, s.maxAge) }

// Run blocks until ctx is cancelled. On cancellation it stops scheduling,
// cancels any running job, waits up to ShutdownTimeout for it to unwind and
// returns ErrAborted if a job was interrupted, nil if the scheduler was idle.
func (s *Scheduler) Run(ctx context.Context) error {
	s.runCtx, s.cancel = context.WithCancel(context.Background())
	defer s.cancel()

	if s.opts.StateDir != "" {
		if err := WriteStarted(s.opts.StateDir, s.now()); err != nil {
			s.log.Warn("could not write start marker", "dir", s.opts.StateDir, "err", err)
		}
	}

	c := cron.New(cron.WithLocation(s.opts.Location), cron.WithLogger(cronLogger{s.log}), cron.WithChain(cron.Recover(cronLogger{s.log})))
	if _, err := c.AddFunc(s.opts.BackupSpec, func() { s.runJob("backup", s.backup) }); err != nil {
		return err
	}
	if s.opts.VerifySpec != "" {
		if _, err := c.AddFunc(s.opts.VerifySpec, func() { s.runJob("verify", s.verify) }); err != nil {
			return err
		}
	}

	var srv *http.Server
	if s.opts.MetricsAddr != "" {
		ln, err := net.Listen("tcp", s.opts.MetricsAddr)
		if err != nil {
			return fmt.Errorf("metrics listener: %w", err)
		}
		srv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Error("metrics server stopped", "err", err)
			}
		}()
		s.log.Info("metrics server listening", "addr", ln.Addr().String())
	}

	c.Start()
	next := c.Entries()
	for _, e := range next {
		s.log.Info("scheduled", "next", e.Next.Format(time.RFC3339))
	}
	s.log.Info("scheduler started", "backup", s.opts.BackupSpec, "verify", s.opts.VerifySpec, "tz", s.opts.Location.String(),
		"health_max_age", s.maxAge, "run_on_start", s.opts.RunOnStart)
	if s.opts.RunOnStart {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.runJob("backup", s.backup)
		}()
	}
	close(s.started)

	<-ctx.Done()
	s.log.Info("shutdown requested", "timeout", s.opts.ShutdownTimeout)
	wasRunning := s.anyRunning()
	cronDone := c.Stop() // no new jobs; waits for cron-launched ones
	s.cancel()           // interrupt whatever is running

	done := make(chan struct{})
	go func() {
		<-cronDone.Done()
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(s.opts.ShutdownTimeout):
		s.log.Error("jobs did not stop within the shutdown timeout")
		if srv != nil {
			srv.Close()
		}
		return ErrAborted
	}
	if srv != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}
	if wasRunning {
		return ErrAborted
	}
	s.log.Info("scheduler stopped")
	return nil
}

// runJob runs fn under the job's lock; an overlapping firing is skipped.
func (s *Scheduler) runJob(name string, fn func(ctx context.Context)) {
	s.mu.Lock()
	if s.locks[name] {
		s.mu.Unlock()
		s.log.Warn("skipping scheduled run: previous one still running", "job", name)
		return
	}
	s.locks[name] = true
	s.mu.Unlock()
	s.status.setRunning(name, true)
	defer func() {
		s.status.setRunning(name, false)
		s.mu.Lock()
		s.locks[name] = false
		s.mu.Unlock()
	}()
	fn(s.runCtx)
}

func (s *Scheduler) anyRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, on := range s.locks {
		if on {
			return true
		}
	}
	return false
}

func (s *Scheduler) backup(ctx context.Context) {
	report, err := s.jobs.Backup(ctx)
	finished := s.now()
	s.status.recordBackup(report, err, finished)
	if err != nil {
		s.log.Error("scheduled backup failed", "err", err)
	}
	if err == nil && report.OK() && s.opts.PruneAfterBackup && s.jobs.Prune != nil {
		if perr := s.jobs.Prune(ctx); perr != nil {
			s.log.Error("prune after backup failed", "err", perr)
		}
	}
	if s.opts.Notifier.enabled() {
		s.opts.Notifier.Notify(ctx, summarize(s.opts.Cluster, s.opts.ToolVersion, report, err))
	}
}

func (s *Scheduler) verify(ctx context.Context) {
	err := s.jobs.Verify(ctx)
	s.status.recordVerify(err, s.now())
	if err != nil {
		s.log.Error("scheduled verify failed", "err", err)
		if s.opts.Notifier.enabled() {
			s.opts.Notifier.Notify(ctx, Summary{Event: "verify", Result: "failure", Cluster: s.opts.Cluster, StartedAt: s.now(), Error: err.Error(), ToolVersion: s.opts.ToolVersion})
		}
	}
}

// cronLogger adapts slog to robfig/cron's logger.
type cronLogger struct{ l *slog.Logger }

func (c cronLogger) Info(msg string, kv ...any) { c.l.Debug("cron: "+msg, kv...) }
func (c cronLogger) Error(err error, msg string, kv ...any) {
	c.l.Error("cron: "+msg, append(kv, "err", err)...)
}

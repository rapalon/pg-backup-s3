package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/config"
	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pipeline"
	"github.com/rapalon/pg-backup-s3/internal/restore"
	"github.com/rapalon/pg-backup-s3/internal/sched"
	"github.com/rapalon/pg-backup-s3/internal/version"
)

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("serve", stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}

	a, err := loadApp(ctx, stderr, config.Config.ValidateForServe)
	if err != nil {
		return err
	}
	cfg := a.cfg
	if cfg.Schedule.Verify != "" {
		if err := cfg.ValidateForRestore(); err != nil {
			return fmt.Errorf("%w (VERIFY_SCHEDULE needs to decrypt)", err)
		}
	}
	deps, err := a.backupDeps()
	if err != nil {
		return err
	}

	jobs := sched.Jobs{
		Backup: func(ctx context.Context) (pipeline.Report, error) {
			return pipeline.Run(ctx, deps, a.runOptions(nil))
		},
		Prune: func(ctx context.Context) error {
			_, err := pipeline.Prune(ctx, a.logger, a.store, cfg.S3.Prefix, cfg.Backup.ClusterName, pipeline.PruneOptions{Policy: policyFrom(cfg)})
			return err
		},
	}
	if cfg.Schedule.Verify != "" {
		rdeps, err := a.restoreDeps()
		if err != nil {
			return err
		}
		jobs.Verify = func(ctx context.Context) error { return verifyAll(ctx, rdeps) }
	}

	s, err := sched.New(sched.Options{
		BackupSpec:       cfg.Schedule.Backup,
		VerifySpec:       cfg.Schedule.Verify,
		Location:         cfg.Schedule.Location,
		RunOnStart:       cfg.Schedule.RunOnStart,
		PruneAfterBackup: cfg.Schedule.PruneAfterBackup,
		ShutdownTimeout:  cfg.Runtime.ShutdownTimeout,
		HealthMaxAge:     cfg.Runtime.HealthMaxAge,
		MetricsAddr:      cfg.Runtime.MetricsAddr,
		StateDir:         cfg.Runtime.StateDir,
		Cluster:          cfg.Backup.ClusterName,
		ToolVersion:      version.Version,
		Notifier: sched.Notifier{
			SuccessURL: cfg.Runtime.NotifyURLSuccess,
			FailureURL: cfg.Runtime.NotifyURLFailure,
			Method:     cfg.Runtime.NotifyMethod,
			Logger:     a.logger,
		},
		Logger: a.logger,
	}, jobs)
	if err != nil {
		return fmt.Errorf("%w: %v", config.ErrInvalid, err)
	}
	fmt.Fprintf(stdout, "pgbackup %s serving: backup %q, verify %q, tz %s, health window %s\n",
		version.Version, cfg.Schedule.Backup, cfg.Schedule.Verify, cfg.Schedule.Location, s.MaxAge())

	if err := s.Run(ctx); err != nil {
		if errors.Is(err, sched.ErrAborted) {
			return &exitError{code: exitFailure, err: err}
		}
		return err
	}
	return nil
}

// verifyAll runs verify --full on the latest backup of every database that
// has one. Every failure is reported; one bad database does not hide another.
func verifyAll(ctx context.Context, deps restore.Deps) error {
	cat, err := manifest.Load(ctx, deps.Store, deps.Prefix, deps.Cluster, "")
	if err != nil {
		return err
	}
	var errs []error
	for _, db := range cat.Databases() {
		e, ok := cat.Latest(db)
		if !ok || !e.ObjectFound {
			errs = append(errs, fmt.Errorf("%s: no restorable backup", db))
			continue
		}
		if _, err := restore.Verify(ctx, deps, e, true); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", db, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func runHealth(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("health", stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	// Only the health variables are read: a probe must not need S3 credentials.
	h, err := config.LoadHealth(osGetenv)
	if err != nil {
		return err
	}
	now := time.Now()
	maxAge := sched.MaxAge(h.MaxAge, sched.ScheduleInterval(h.Schedule, h.Location, now))
	ok, msg := sched.CheckHealth(h.StateDir, now, maxAge)
	fmt.Fprintln(stdout, msg)
	if !ok {
		return &exitError{code: exitFailure, err: errors.New("unhealthy: " + msg)}
	}
	return nil
}

package pipeline

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/manifest"
)

// RunOptions control one backup run.
type RunOptions struct {
	AllDatabases bool
	Databases    []string // explicit list when AllDatabases is false
	Exclude      []string // applied when AllDatabases is true
	Parallel     int      // databases dumped concurrently; 1 when zero
	Retry        RetryPolicy
	Timeout      time.Duration // deadline for the whole run; 0 = none
	StateDir     string        // heartbeat directory; empty disables
}

// Result is the outcome for one object (globals or a database).
type Result struct {
	Name     string // database name or "globals"
	Manifest manifest.Manifest
	Err      error
	Attempts int
	Duration time.Duration
}

// Report is the outcome of a run.
type Report struct {
	Time      time.Time
	Globals   Result
	Databases []Result
	Duration  time.Duration
}

// Failed returns every result that ended in an error, globals first.
func (r Report) Failed() []Result {
	var out []Result
	if r.Globals.Err != nil {
		out = append(out, r.Globals)
	}
	for _, d := range r.Databases {
		if d.Err != nil {
			out = append(out, d)
		}
	}
	return out
}

// OK reports whether everything succeeded.
func (r Report) OK() bool { return len(r.Failed()) == 0 }

// Run performs one backup run: globals first, then every database through a
// worker pool, each with retries. Per-database failures do not stop the
// others. A heartbeat is written only when the whole run succeeded. The
// returned error is non-nil only for run-level failures (versions, database
// discovery); per-object failures are in the Report.
func Run(ctx context.Context, deps Deps, opts RunOptions) (Report, error) {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	log := deps.logger()
	start := deps.now()

	client, err := deps.DB.ClientVersion(ctx, "pg_dump")
	if err != nil {
		return Report{}, err
	}
	server, err := deps.DB.ServerVersion(ctx, deps.Conn)
	if err != nil {
		return Report{}, fmt.Errorf("server version: %w", err)
	}
	if client.Major < server.Major {
		return Report{}, fmt.Errorf("pg_dump %s is older than the server %s; use the image tagged for PostgreSQL %d", client, server, server.Major)
	}
	dbs, err := resolveDatabases(ctx, deps, opts)
	if err != nil {
		return Report{}, err
	}

	info := RunInfo{Time: start.UTC().Truncate(time.Second), ServerVersion: server, ClientVersion: client}
	log.Info("backup run starting", "cluster", deps.Cluster, "databases", dbs, "server", server.String(), "pg_dump", client.String(), "timestamp", manifest.FormatTime(info.Time))

	report := Report{Time: info.Time}
	report.Globals = runOne(ctx, deps, opts.Retry, "globals", func(ctx context.Context) (manifest.Manifest, error) {
		return BackupGlobals(ctx, deps, info)
	})
	if report.Globals.Err == nil {
		info.GlobalsKey = report.Globals.Manifest.Key
	}

	report.Databases = make([]Result, len(dbs))
	parallel := max(opts.Parallel, 1)
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, db := range dbs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			report.Databases[i] = runOne(ctx, deps, opts.Retry, db, func(ctx context.Context) (manifest.Manifest, error) {
				return BackupDatabase(ctx, deps, db, info)
			})
		}()
	}
	wg.Wait()
	report.Duration = deps.now().Sub(start)

	failed := report.Failed()
	log.Info("backup run finished", "ok", len(dbs)+1-len(failed), "failed", len(failed), "duration", report.Duration)
	if len(failed) == 0 && opts.StateDir != "" {
		if err := WriteHeartbeat(opts.StateDir, deps.now()); err != nil {
			log.Warn("could not write heartbeat", "dir", opts.StateDir, "err", err)
		}
	}
	return report, nil
}

func runOne(ctx context.Context, deps Deps, policy RetryPolicy, name string, fn func(context.Context) (manifest.Manifest, error)) Result {
	log := deps.logger()
	start := deps.now()
	var m manifest.Manifest
	attempts, err := Retry(ctx, log, policy, name, func(ctx context.Context) error {
		var ferr error
		m, ferr = fn(ctx)
		return ferr
	})
	res := Result{Name: name, Manifest: m, Err: err, Attempts: attempts, Duration: deps.now().Sub(start)}
	if err != nil {
		log.Error("backup failed", "name", name, "attempts", attempts, "duration", res.Duration, "err", err)
	} else {
		log.Info("backup complete", "name", name, "key", m.Key, "bytes", m.SizeBytes, "sha256", m.SHA256, "attempts", attempts, "duration", res.Duration)
	}
	return res
}

func resolveDatabases(ctx context.Context, deps Deps, opts RunOptions) ([]string, error) {
	if !opts.AllDatabases {
		if len(opts.Databases) == 0 {
			return nil, fmt.Errorf("no databases selected")
		}
		return slices.Clone(opts.Databases), nil
	}
	all, err := deps.DB.ListDatabases(ctx, deps.Conn)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	var out []string
	for _, db := range all {
		if !slices.Contains(opts.Exclude, db) {
			out = append(out, db)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no databases left after exclusions (server has %v)", all)
	}
	return out, nil
}

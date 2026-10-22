package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/rapalon/pg-backup-s3/internal/config"
	"github.com/rapalon/pg-backup-s3/internal/pipeline"
)

func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("backup", stderr)
	var dbs multiFlag
	fs.Var(&dbs, "db", "database to back up (repeatable; overrides BACKUP_DATABASES)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}

	a, err := loadApp(ctx, stderr, config.Config.ValidateForBackup)
	if err != nil {
		return err
	}
	deps, err := a.backupDeps()
	if err != nil {
		return err
	}

	report, err := pipeline.Run(ctx, deps, a.runOptions(dbs))
	if err != nil {
		return err
	}
	printReport(stdout, report)

	failed := report.Failed()
	if len(failed) == 0 {
		return nil
	}
	code := exitFailure
	for _, f := range failed {
		if errors.Is(f.Err, pipeline.ErrIntegrity) {
			code = exitIntegrity
		}
	}
	return &exitError{code: code, err: fmt.Errorf("%d of %d backups failed", len(failed), len(report.Databases)+1)}
}

func printReport(w io.Writer, r pipeline.Report) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tBYTES\tDURATION\tATTEMPTS\tKEY")
	all := append([]pipeline.Result{r.Globals}, r.Databases...)
	for _, res := range all {
		status, key := "ok", res.Manifest.Key
		if res.Err != nil {
			status, key = "FAILED", res.Err.Error()
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%d\t%s\n", res.Name, status, res.Manifest.SizeBytes, res.Duration.Round(1e6), res.Attempts, key)
	}
	tw.Flush()
	fmt.Fprintf(w, "run %s: %d ok, %d failed, %s\n", r.Time.Format("2006-01-02T15:04:05Z"), len(all)-len(r.Failed()), len(r.Failed()), r.Duration.Round(1e6))
}

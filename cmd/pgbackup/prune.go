package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/config"
	"github.com/rapalon/pg-backup-s3/internal/pipeline"
	"github.com/rapalon/pg-backup-s3/internal/retention"
)

func runPrune(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("prune", stderr)
	dryRun := fs.Bool("dry-run", false, "print what would be deleted and change nothing")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}

	a, err := loadApp(ctx, stderr, nil)
	if err != nil {
		return err
	}
	plan, err := pipeline.Prune(ctx, a.logger, a.store, a.cfg.S3.Prefix, a.cfg.Backup.ClusterName, pipeline.PruneOptions{
		Policy: policyFrom(a.cfg),
		DryRun: *dryRun,
	})
	if err != nil {
		return err
	}
	printPrunePlan(stdout, plan, *dryRun)
	return nil
}

func policyFrom(cfg config.Config) retention.Policy {
	return retention.Policy{
		KeepLast:    cfg.Retention.KeepLast,
		KeepDaily:   cfg.Retention.KeepDaily,
		KeepWeekly:  cfg.Retention.KeepWeekly,
		KeepMonthly: cfg.Retention.KeepMonthly,
	}
}

func printPrunePlan(w io.Writer, plan pipeline.PrunePlan, dryRun bool) {
	verb := "deleted"
	if dryRun {
		verb = "would delete"
	}
	if len(plan.Delete) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ACTION\tKIND\tDATABASE\tCREATED\tSIZE\tREASON\tKEY")
		for _, a := range plan.Delete {
			created := "-"
			if !a.Time.IsZero() {
				created = a.Time.UTC().Format(time.RFC3339)
			}
			db := a.Database
			if db == "" {
				db = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", strings.ToUpper(verb[:1])+verb[1:], a.Kind, db, created, humanSize(a.Size), a.Reason, a.Key)
		}
		tw.Flush()
	}
	fmt.Fprintf(w, "%s %d object(s), %s; %d dump(s) kept", verb, len(plan.Delete), humanSize(plan.Bytes()), len(plan.Kept))
	if n := len(plan.Catalog.Broken); n > 0 {
		fmt.Fprintf(w, "; %d broken manifest(s) left untouched", n)
	}
	fmt.Fprintln(w)
}

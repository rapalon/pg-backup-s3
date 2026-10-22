package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/config"
	"github.com/rapalon/pg-backup-s3/internal/crypt"
	"github.com/rapalon/pg-backup-s3/internal/restore"
)

// selectorFlags adds --key / --db / --latest / --at to fs.
func selectorFlags(fs *flag.FlagSet) func() (restore.Selector, error) {
	key := fs.String("key", "", "object key of the backup to use")
	db := fs.String("db", "", "database whose backup to use")
	latest := fs.Bool("latest", false, "use the newest backup of --db (default)")
	at := fs.String("at", "", "use the newest backup of --db created at or before this RFC 3339 time")
	return func() (restore.Selector, error) {
		sel := restore.Selector{Key: *key, Database: *db}
		if *at != "" {
			if *latest {
				return sel, fmt.Errorf("%w: --latest and --at are mutually exclusive", errUsage)
			}
			t, err := time.Parse(time.RFC3339, *at)
			if err != nil {
				return sel, fmt.Errorf("%w: --at %q is not an RFC 3339 time (e.g. 2026-10-22T03:00:00Z)", errUsage, *at)
			}
			sel.At = t
		}
		return sel, sel.Validate()
	}
}

func (a *app) restoreDeps() (restore.Deps, error) {
	ids, err := crypt.IdentitiesFor(a.cfg.Encryption.Identity, a.cfg.Encryption.Passphrase)
	if err != nil {
		return restore.Deps{}, fmt.Errorf("%w: %v", config.ErrInvalid, err)
	}
	return restore.Deps{
		Logger:     a.logger,
		DB:         a.runner,
		Store:      a.store,
		Conn:       a.conn,
		Identities: ids,
		Prefix:     a.cfg.S3.Prefix,
		Cluster:    a.cfg.Backup.ClusterName,
	}, nil
}

func runRestore(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("restore", stderr)
	selector := selectorFlags(fs)
	var opts restore.Options
	var tables multiFlag
	fs.StringVar(&opts.TargetDB, "target-db", "", "restore into this database instead of the original name")
	fs.BoolVar(&opts.Create, "create", false, "create the target database if it does not exist")
	fs.BoolVar(&opts.Clean, "clean", false, "drop objects before recreating them (pg_restore --clean --if-exists)")
	fs.BoolVar(&opts.DropExisting, "drop-existing", false, "drop and recreate the target database (needs --yes)")
	fs.BoolVar(&opts.Yes, "yes", false, "confirm destructive actions (also PGBACKUP_YES=1)")
	fs.StringVar(&opts.Owner, "owner", "", "owner of a database created by --create or --drop-existing")
	fs.IntVar(&opts.Jobs, "jobs", 1, "parallel pg_restore jobs; >1 needs --spool-dir")
	fs.StringVar(&opts.SpoolDir, "spool-dir", "", "directory for the decrypted archive when --jobs > 1")
	fs.BoolVar(&opts.KeepSpool, "keep-spool", false, "keep the spooled archive after the restore")
	fs.BoolVar(&opts.NoOwner, "no-owner", false, "pg_restore --no-owner")
	fs.BoolVar(&opts.NoPrivileges, "no-privileges", false, "pg_restore --no-privileges")
	fs.BoolVar(&opts.SchemaOnly, "schema-only", false, "pg_restore --schema-only")
	fs.BoolVar(&opts.DataOnly, "data-only", false, "pg_restore --data-only")
	fs.Var(&tables, "table", "restore only this table (repeatable)")
	fs.BoolVar(&opts.SingleTransaction, "single-transaction", false, "pg_restore --single-transaction")
	fs.BoolVar(&opts.IgnoreErrors, "ignore-errors", false, "continue after pg_restore errors instead of stopping at the first")
	fs.BoolVar(&opts.Globals, "globals", false, "replay roles and tablespaces from the backup's globals object first (needs superuser)")
	fs.BoolVar(&opts.Analyze, "analyze", false, "run vacuumdb --analyze-in-stages afterwards")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "print the plan after all checks and change nothing")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}
	opts.Tables = tables
	sel, err := selector()
	if err != nil {
		return err
	}

	a, err := loadApp(ctx, stderr, config.Config.ValidateForRestore)
	if err != nil {
		return err
	}
	opts.Yes = opts.Yes || a.cfg.Runtime.Yes
	deps, err := a.restoreDeps()
	if err != nil {
		return err
	}
	entry, err := restore.Resolve(ctx, a.store, deps.Prefix, deps.Cluster, sel)
	if err != nil {
		return err
	}
	plan, err := restore.Preflight(ctx, deps, entry, opts)
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, plan.Describe())
	if opts.DryRun {
		fmt.Fprintln(stdout, "Dry run: nothing was changed.")
		return nil
	}

	rep, err := restore.Execute(ctx, deps, plan)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Restored %s into %q: %d bytes, sha256 %s, %s\n", rep.Key, rep.TargetDB, rep.Bytes, rep.SHA256, rep.Duration.Round(time.Millisecond))
	if rep.IgnoredErrors > 0 {
		fmt.Fprintf(stdout, "Warning: pg_restore ignored %d error(s); see the log\n", rep.IgnoredErrors)
	}
	if plan.Globals {
		fmt.Fprintf(stdout, "Globals replayed with %d error(s) (\"already exists\" is expected on a non-empty cluster)\n", rep.GlobalsErrors)
	}
	if rep.SpoolPath != "" && plan.KeepSpool {
		fmt.Fprintf(stdout, "Spooled archive kept at %s\n", rep.SpoolPath)
	}
	if rep.Analyzed {
		fmt.Fprintln(stdout, "Statistics collected with vacuumdb --analyze-in-stages")
	}
	return nil
}

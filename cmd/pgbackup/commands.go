package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/rapalon/pg-backup-s3/internal/version"
)

// commands returns the subcommand table. Every subcommand from plan.md §1.1
// exists here; the ones not built yet return errNotImplemented.
func commands() map[string]command {
	return map[string]command{
		"serve":   {name: "serve", usage: "run the scheduler loop", run: runServe},
		"backup":  {name: "backup", usage: "run one backup now and exit", run: runBackup},
		"list":    {name: "list", usage: "list backups in the bucket", run: runList},
		"verify":  {name: "verify", usage: "check that a backup is readable", run: runVerify},
		"restore": {name: "restore", usage: "restore a backup into a database", run: runRestore},
		"prune":   {name: "prune", usage: "delete backups outside the retention policy", run: runPrune},
		"health":  {name: "health", usage: "exit 0 if the last backup succeeded recently", run: runHealth},
		"version": {name: "version", usage: "print build information", run: runVersion},
	}
}

// newFlagSet creates a FlagSet for a subcommand that reports usage errors
// through the returned error instead of exiting.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("pgbackup "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseFlags parses args and converts flag errors into usage errors.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return &exitError{code: exitOK, err: err}
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	return nil
}

func runVersion(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("version", stderr)
	short := fs.Bool("short", false, "print only the version number")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *short {
		fmt.Fprintln(stdout, version.Version)
		return nil
	}
	fmt.Fprintln(stdout, version.String())
	return nil
}

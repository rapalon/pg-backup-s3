package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/config"
	"github.com/rapalon/pg-backup-s3/internal/restore"
)

func runVerify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("verify", stderr)
	selector := selectorFlags(fs)
	full := fs.Bool("full", false, "also restore into a scratch database and drop it again (needs CREATEDB)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}
	sel, err := selector()
	if err != nil {
		return err
	}

	a, err := loadApp(ctx, stderr, config.Config.ValidateForRestore)
	if err != nil {
		return err
	}
	deps, err := a.restoreDeps()
	if err != nil {
		return err
	}
	entry, err := restore.Resolve(ctx, a.store, deps.Prefix, deps.Cluster, sel)
	if err != nil {
		return err
	}
	rep, err := restore.Verify(ctx, deps, entry, *full)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "OK %s: %d bytes, sha256 %s", rep.Key, rep.Bytes, rep.SHA256)
	if rep.TOCEntries > 0 {
		fmt.Fprintf(stdout, ", %d archive entries", rep.TOCEntries)
	}
	if *full {
		fmt.Fprintf(stdout, ", restored into %s (%d relations) and dropped", rep.ScratchDB, rep.Relations)
	}
	fmt.Fprintf(stdout, ", %s\n", rep.Duration.Round(time.Millisecond))
	return nil
}

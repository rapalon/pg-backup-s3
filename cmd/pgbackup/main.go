// Command pgbackup takes encrypted, streaming PostgreSQL backups to
// S3-compatible storage and restores them.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/rapalon/pg-backup-s3/internal/config"
	"github.com/rapalon/pg-backup-s3/internal/pipeline"
	"github.com/rapalon/pg-backup-s3/internal/restore"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

// Exit codes, see plan.md §1.1.
const (
	exitOK        = 0
	exitFailure   = 1
	exitUsage     = 2
	exitRefused   = 3
	exitIntegrity = 4
)

// errUsage marks a usage or configuration error (exit code 2).
var errUsage = errors.New("usage error")

// exitError carries an explicit process exit code through the error chain.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// command is one CLI subcommand.
type command struct {
	name  string
	usage string
	run   func(ctx context.Context, args []string, stdout, stderr io.Writer) error
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run dispatches args to a subcommand and maps its error to an exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmds := commands()

	if len(args) == 0 {
		printUsage(stderr, cmds)
		return exitUsage
	}

	name := args[0]
	switch name {
	case "help", "-h", "--help":
		printUsage(stdout, cmds)
		return exitOK
	}

	cmd, ok := cmds[name]
	if !ok {
		fmt.Fprintf(stderr, "pgbackup: unknown command %q\n\n", name)
		printUsage(stderr, cmds)
		return exitUsage
	}

	err := cmd.run(ctx, args[1:], stdout, stderr)
	code := exitCodeFor(err)
	if code != exitOK {
		fmt.Fprintf(stderr, "pgbackup %s: %v\n", name, err)
	}
	return code
}

// exitCodeFor maps an error to the process exit code.
func exitCodeFor(err error) int {
	var ee *exitError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ee):
		return ee.code
	case errors.Is(err, errUsage), errors.Is(err, config.ErrInvalid), errors.Is(err, store.ErrBucketNotFound):
		return exitUsage
	case errors.Is(err, restore.ErrRefused):
		return exitRefused
	case errors.Is(err, pipeline.ErrIntegrity):
		return exitIntegrity
	default:
		return exitFailure
	}
}

func printUsage(w io.Writer, cmds map[string]command) {
	names := make([]string, 0, len(cmds))
	for n := range cmds {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("Usage: pgbackup <command> [flags]\n\nCommands:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  %-10s %s\n", n, cmds[n].usage)
	}
	b.WriteString("\nRun \"pgbackup <command> -h\" for command flags.\n")
	io.WriteString(w, b.String())
}

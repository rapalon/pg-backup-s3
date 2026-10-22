package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/rapalon/pg-backup-s3/internal/config"
	"github.com/rapalon/pg-backup-s3/internal/pipeline"
	"github.com/rapalon/pg-backup-s3/internal/restore"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

func init() {
	// Unit tests must not see the host's (or the test stand's) environment.
	osGetenv = func(string) string { return "" }
	osReadFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no args", args: nil, wantCode: exitUsage, wantStderr: "Usage: pgbackup"},
		{name: "help", args: []string{"help"}, wantCode: exitOK, wantStdout: "Commands:"},
		{name: "dash h", args: []string{"-h"}, wantCode: exitOK, wantStdout: "Commands:"},
		{name: "unknown", args: []string{"frobnicate"}, wantCode: exitUsage, wantStderr: `unknown command "frobnicate"`},
		{name: "version", args: []string{"version"}, wantCode: exitOK, wantStdout: "pgbackup dev"},
		{name: "version short", args: []string{"version", "--short"}, wantCode: exitOK, wantStdout: "dev\n"},
		{name: "version help", args: []string{"version", "-h"}, wantCode: exitOK, wantStderr: "-short"},
		{name: "help prints no error line", args: []string{"backup", "-h"}, wantCode: exitOK, wantStderr: "-db value"},
		{name: "version bad flag", args: []string{"version", "--bogus"}, wantCode: exitUsage, wantStderr: "flag provided but not defined"},
		{name: "backup without config", args: []string{"backup"}, wantCode: exitUsage, wantStderr: "S3_ENDPOINT: is required"},
		{name: "backup positional arg", args: []string{"backup", "extra"}, wantCode: exitUsage, wantStderr: "unexpected argument"},
		{name: "backup bad flag", args: []string{"backup", "--nope"}, wantCode: exitUsage, wantStderr: "flag provided but not defined"},
		{name: "restore without selector", args: []string{"restore"}, wantCode: exitUsage, wantStderr: "--key or --db is required"},
		{name: "restore both selectors", args: []string{"restore", "--key", "k", "--db", "d"}, wantCode: exitUsage, wantStderr: "not both"},
		{name: "restore bad at", args: []string{"restore", "--db", "d", "--at", "yesterday"}, wantCode: exitUsage, wantStderr: "RFC 3339"},
		{name: "restore latest and at", args: []string{"restore", "--db", "d", "--latest", "--at", "2026-10-22T00:00:00Z"}, wantCode: exitUsage, wantStderr: "mutually exclusive"},
		{name: "restore without config", args: []string{"restore", "--db", "d"}, wantCode: exitUsage, wantStderr: "S3_ENDPOINT: is required"},
		{name: "verify without config", args: []string{"verify", "--key", "k"}, wantCode: exitUsage, wantStderr: "S3_ENDPOINT: is required"},
		{name: "verify positional", args: []string{"verify", "--key", "k", "x"}, wantCode: exitUsage, wantStderr: "unexpected argument"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, tc.wantCode, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tc.wantStdout)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
			if tc.wantCode == exitOK && strings.Contains(stderr.String(), "help requested") {
				t.Errorf("stderr leaks the flag.ErrHelp sentinel: %q", stderr.String())
			}
		})
	}
}

func TestEveryPlannedCommandExists(t *testing.T) {
	t.Parallel()
	want := []string{"serve", "backup", "list", "verify", "restore", "prune", "health", "version"}
	cmds := commands()
	for _, name := range want {
		if _, ok := cmds[name]; !ok {
			t.Errorf("command %q missing from the table", name)
		}
	}
	if len(cmds) != len(want) {
		t.Errorf("command table has %d entries, want %d", len(cmds), len(want))
	}
}

func TestExitCodeFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want int
	}{
		{nil, exitOK},
		{errors.New("boom"), exitFailure},
		{errUsage, exitUsage},
		{errors.Join(errors.New("ctx"), errUsage), exitUsage},
		{fmt.Errorf("load: %w", config.ErrInvalid), exitUsage},
		{fmt.Errorf("ping: %w", store.ErrBucketNotFound), exitUsage},
		{fmt.Errorf("backup: %w", pipeline.ErrIntegrity), exitIntegrity},
		{fmt.Errorf("plan: %w", restore.ErrRefused), exitRefused},
		{&exitError{code: exitRefused, err: errors.New("refused")}, exitRefused},
		{&exitError{code: exitIntegrity, err: errors.New("hash")}, exitIntegrity},
	}
	for _, tc := range tests {
		if got := exitCodeFor(tc.err); got != tc.want {
			t.Errorf("exitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

package pg

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// RestoreOptions control pg_restore.
type RestoreOptions struct {
	Database          string
	Clean             bool // --clean --if-exists
	NoOwner           bool
	NoPrivileges      bool
	SchemaOnly        bool
	DataOnly          bool
	Tables            []string
	ExitOnError       bool // --exit-on-error: stop at the first error
	SingleTransaction bool
}

// RestoreResult reports what pg_restore ignored when ExitOnError is off.
type RestoreResult struct {
	IgnoredErrors int
}

var ignoredErrorsRE = regexp.MustCompile(`errors ignored on restore: (\d+)`)

func (o RestoreOptions) args() []string {
	args := []string{"--dbname=" + o.Database, "--no-password"}
	if o.Clean {
		args = append(args, "--clean", "--if-exists")
	}
	if o.NoOwner {
		args = append(args, "--no-owner")
	}
	if o.NoPrivileges {
		args = append(args, "--no-privileges")
	}
	if o.SchemaOnly {
		args = append(args, "--schema-only")
	}
	if o.DataOnly {
		args = append(args, "--data-only")
	}
	for _, t := range o.Tables {
		args = append(args, "--table="+t)
	}
	if o.ExitOnError {
		args = append(args, "--exit-on-error")
	}
	if o.SingleTransaction {
		args = append(args, "--single-transaction")
	}
	return args
}

// Restore runs pg_restore reading a custom-format archive from stdin.
//
// With ExitOnError off, pg_restore exits 1 after reporting "errors ignored
// on restore: N"; that case is returned as a successful RestoreResult with
// the count, so the caller can decide how loud to be.
func (r *Runner) Restore(ctx context.Context, conn Conn, opts RestoreOptions, stdin io.Reader) (RestoreResult, error) {
	return r.restore(ctx, conn, opts, stdin, "", 0)
}

// RestoreFile runs pg_restore on a spooled archive file, optionally with
// parallel jobs (which pg_restore only supports for files).
func (r *Runner) RestoreFile(ctx context.Context, conn Conn, opts RestoreOptions, path string, jobs int) (RestoreResult, error) {
	return r.restore(ctx, conn, opts, nil, path, jobs)
}

func (r *Runner) restore(ctx context.Context, conn Conn, opts RestoreOptions, stdin io.Reader, path string, jobs int) (RestoreResult, error) {
	args := opts.args()
	if jobs > 1 {
		args = append(args, "--jobs="+strconv.Itoa(jobs))
	}
	if path != "" {
		args = append(args, path)
	}
	var res RestoreResult
	err := r.Run(ctx, Spec{
		Binary: "pg_restore",
		Args:   args,
		Env:    conn.Env(),
		Stdin:  stdin,
		OnStderr: func(line string) {
			if m := ignoredErrorsRE.FindStringSubmatch(line); m != nil {
				res.IgnoredErrors, _ = strconv.Atoi(m[1])
			}
		},
	})
	var exitErr *ExitError
	if err != nil && !opts.ExitOnError && errors.As(err, &exitErr) && exitErr.Code == 1 && res.IgnoredErrors > 0 {
		return res, nil
	}
	return res, err
}

// ListArchive runs `pg_restore --list` on an archive read from stdin and
// returns the table of contents. pg_restore stops reading after the TOC;
// the caller must drain stdin itself if it needs the whole stream consumed.
func (r *Runner) ListArchive(ctx context.Context, stdin io.Reader) (string, error) {
	var toc strings.Builder
	err := r.Run(ctx, Spec{Binary: "pg_restore", Args: []string{"--list"}, Stdin: stdin, Stdout: &toc})
	if err != nil {
		return "", err
	}
	return toc.String(), nil
}

// RunSQLScript feeds a plain SQL script to psql against db with
// ON_ERROR_STOP off, so every statement is attempted. It returns the number
// of ERROR lines psql reported; the exit status is 0 in that mode.
func (r *Runner) RunSQLScript(ctx context.Context, conn Conn, db string, script io.Reader) (int, error) {
	errorsSeen := 0
	err := r.Run(ctx, Spec{
		Binary: "psql",
		Args: []string{
			"--no-psqlrc", "--no-password", "--quiet",
			"--set=ON_ERROR_STOP=0",
			"--dbname=" + db,
			"--file=-",
		},
		Env:   conn.Env(),
		Stdin: script,
		OnStderr: func(line string) {
			if strings.Contains(line, "ERROR:") {
				errorsSeen++
			}
		},
	})
	return errorsSeen, err
}

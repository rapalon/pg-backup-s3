package pg

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrCompressionUnsupported is returned when the pg_dump client is too old
// for the requested compression algorithm.
var ErrCompressionUnsupported = errors.New("compression not supported by this pg_dump")

// dumpSessionEnv disables the server-side statement timeout for the dump
// session so a long dump is not killed by a cluster-wide default.
const dumpSessionEnv = "PGOPTIONS=-c statement_timeout=0"

// DumpOptions control pg_dump.
type DumpOptions struct {
	// Compression is the BACKUP_COMPRESSION value: zstd[:N], gzip[:N], lz4[:N] or none.
	Compression string
	// LockWaitTimeout is passed as --lock-wait-timeout; zero waits forever.
	LockWaitTimeout time.Duration
}

// Dump starts `pg_dump -Fc` of one database. The archive streams from
// Process.Stdout; the caller reads it to EOF and then calls Wait, which
// returns an *ExitError if pg_dump failed. Nothing touches the disk.
func (r *Runner) Dump(ctx context.Context, conn Conn, db string, opts DumpOptions) (*Process, error) {
	v, err := r.ClientVersion(ctx, "pg_dump")
	if err != nil {
		return nil, err
	}
	compress, err := compressArg(v, opts.Compression)
	if err != nil {
		return nil, err
	}
	args := []string{"--format=custom", "--no-password"}
	if compress != "" {
		args = append(args, "--compress="+compress)
	}
	args = append(args,
		"--lock-wait-timeout="+strconv.FormatInt(opts.LockWaitTimeout.Milliseconds(), 10),
		"--dbname="+db,
	)
	return r.Start(ctx, Spec{
		Binary: "pg_dump",
		Args:   args,
		Env:    append(conn.Env(), dumpSessionEnv),
	})
}

// DumpGlobals starts `pg_dumpall --globals-only`: roles, role memberships,
// tablespaces and cluster settings as plain SQL on Process.Stdout.
func (r *Runner) DumpGlobals(ctx context.Context, conn Conn) (*Process, error) {
	return r.Start(ctx, Spec{
		Binary: "pg_dumpall",
		Args:   []string{"--globals-only", "--no-password", "--database=" + conn.MaintenanceDB()},
		Env:    append(conn.Env(), dumpSessionEnv),
	})
}

// compressArg maps a BACKUP_COMPRESSION value to pg_dump's --compress
// argument for the given client version. An empty result omits the flag.
//
// PostgreSQL 16 introduced algorithm names (gzip, lz4, zstd, none); older
// clients only accept a gzip level 0-9.
func compressArg(v Version, spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", nil
	}
	algo, level, hasLevel := strings.Cut(spec, ":")
	algo = strings.ToLower(algo)
	if v.Major >= 16 {
		switch algo {
		case "zstd", "gzip", "lz4", "none":
			return spec, nil
		}
		return "", fmt.Errorf("unknown compression %q", spec)
	}
	switch algo {
	case "none":
		return "0", nil
	case "gzip":
		if hasLevel {
			return level, nil
		}
		return "", nil // pg_dump's default gzip level
	case "zstd", "lz4":
		return "", fmt.Errorf("%w: %s needs a PostgreSQL 16+ client, found %s", ErrCompressionUnsupported, algo, v)
	}
	return "", fmt.Errorf("unknown compression %q", spec)
}

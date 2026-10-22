package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/rapalon/pg-backup-s3/internal/crypt"
	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pipeline"
)

// Report is what a restore did.
type Report struct {
	TargetDB      string
	Key           string
	Bytes         int64
	SHA256        string
	Duration      time.Duration
	IgnoredErrors int // pg_restore errors ignored (only with --ignore-errors)
	GlobalsErrors int // psql errors while replaying globals (expected on a non-empty cluster)
	SpoolPath     string
	Analyzed      bool
}

// Execute carries out a plan. Any failure after the database was created or
// dropped leaves the target in an undefined state and returns a non-nil
// error; the documented safe procedure is to restore under a new name.
func Execute(ctx context.Context, deps Deps, plan Plan) (Report, error) {
	log := deps.logger()
	start := deps.now()
	m := plan.Entry.Manifest
	rep := Report{TargetDB: plan.TargetDB, Key: m.Key}

	if plan.Globals {
		n, err := restoreGlobals(ctx, deps, plan.GlobalsKey)
		if err != nil {
			return rep, fmt.Errorf("globals: %w", err)
		}
		rep.GlobalsErrors = n
		log.Info("globals restored", "key", plan.GlobalsKey, "errors", n)
	}

	if plan.DropDB {
		log.Warn("dropping database", "database", plan.TargetDB)
		if err := deps.DB.TerminateConnections(ctx, deps.Conn, plan.TargetDB); err != nil {
			return rep, fmt.Errorf("terminating sessions on %q: %w", plan.TargetDB, err)
		}
		if err := deps.DB.DropDatabase(ctx, deps.Conn, plan.TargetDB, plan.DropForce); err != nil {
			return rep, fmt.Errorf("dropping %q: %w", plan.TargetDB, err)
		}
	}
	if plan.CreateDB {
		if err := deps.DB.CreateDatabase(ctx, deps.Conn, plan.TargetDB, plan.Owner); err != nil {
			return rep, fmt.Errorf("creating %q: %w", plan.TargetDB, err)
		}
		log.Info("database created", "database", plan.TargetDB, "owner", plan.Owner)
	}

	var err error
	if plan.Jobs > 1 {
		err = restoreSpooled(ctx, deps, plan, &rep)
	} else {
		err = restoreStreamed(ctx, deps, plan, &rep)
	}
	if err != nil {
		return rep, err
	}

	if plan.Analyze {
		if err := deps.DB.AnalyzeDatabase(ctx, deps.Conn, plan.TargetDB); err != nil {
			return rep, fmt.Errorf("analyze: %w", err)
		}
		rep.Analyzed = true
	}
	rep.Duration = deps.now().Sub(start)
	log.Info("restore complete", "database", plan.TargetDB, "key", m.Key, "bytes", rep.Bytes,
		"ignored_errors", rep.IgnoredErrors, "duration", rep.Duration)
	return rep, nil
}

// openDecrypted opens the object and returns the decrypted stream together
// with the hashing reader (for the stored bytes) and the error recorder
// (for age authentication failures).
func openDecrypted(ctx context.Context, deps Deps, key string) (io.ReadCloser, *pipeline.HashReader, *errRecorder, io.Reader, error) {
	rc, _, err := deps.Store.Get(ctx, key)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	hr := pipeline.NewHashReader(rc)
	dec, err := crypt.NewDecryptor(hr, deps.Identities)
	if err != nil {
		rc.Close()
		return nil, nil, nil, nil, fmt.Errorf("%w: %v", pipeline.ErrIntegrity, err)
	}
	rec := &errRecorder{r: dec}
	return rc, hr, rec, rec, nil
}

func checkIntegrity(m manifest.Manifest, hr *pipeline.HashReader, rec *errRecorder) error {
	if rec.err != nil {
		return fmt.Errorf("%w: decrypting %s: %v", pipeline.ErrIntegrity, m.Key, rec.err)
	}
	if hr.Count() != m.SizeBytes || hr.Sum() != m.SHA256 {
		return fmt.Errorf("%w: %s is %d bytes with sha256 %s, manifest says %d bytes and %s",
			pipeline.ErrIntegrity, m.Key, hr.Count(), hr.Sum(), m.SizeBytes, m.SHA256)
	}
	return nil
}

// restoreStreamed pipes store → sha256 → age → pg_restore. The hash is
// verified after pg_restore; age authenticates each chunk before it is
// handed over, so corrupted bytes never reach the server.
func restoreStreamed(ctx context.Context, deps Deps, plan Plan, rep *Report) error {
	m := plan.Entry.Manifest
	rc, hr, rec, dec, err := openDecrypted(ctx, deps, m.Key)
	if err != nil {
		return err
	}
	defer rc.Close()

	res, restoreErr := deps.DB.Restore(ctx, deps.Conn, plan.Restore, dec)
	rep.IgnoredErrors = res.IgnoredErrors
	if restoreErr != nil {
		if rec.err != nil {
			// pg_restore saw a truncated stream because decryption failed.
			return fmt.Errorf("%w: decrypting %s: %v (pg_restore: %v)", pipeline.ErrIntegrity, m.Key, rec.err, restoreErr)
		}
		return fmt.Errorf("pg_restore: %w", restoreErr)
	}
	// pg_restore may stop reading before EOF (e.g. --schema-only); drain so
	// the hash and the age authentication cover the whole object.
	if _, err := io.Copy(io.Discard, dec); err != nil && rec.err == nil {
		rec.err = err
	}
	rep.Bytes, rep.SHA256 = hr.Count(), hr.Sum()
	return checkIntegrity(m, hr, rec)
}

// restoreSpooled downloads and decrypts into a file, verifies the hash, then
// runs a parallel pg_restore on it.
func restoreSpooled(ctx context.Context, deps Deps, plan Plan, rep *Report) error {
	log := deps.logger()
	m := plan.Entry.Manifest
	rc, hr, rec, dec, err := openDecrypted(ctx, deps, m.Key)
	if err != nil {
		return err
	}
	defer rc.Close()

	f, err := os.CreateTemp(plan.SpoolDir, "pgbackup-*.dump")
	if err != nil {
		return fmt.Errorf("spool: %w", err)
	}
	path := f.Name()
	rep.SpoolPath = path
	defer func() {
		if !plan.KeepSpool {
			os.Remove(path)
		}
	}()

	_, copyErr := io.Copy(f, dec)
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		if rec.err != nil {
			return fmt.Errorf("%w: decrypting %s: %v", pipeline.ErrIntegrity, m.Key, rec.err)
		}
		return fmt.Errorf("spool: %w", errors.Join(copyErr, syncErr, closeErr))
	}
	rep.Bytes, rep.SHA256 = hr.Count(), hr.Sum()
	if err := checkIntegrity(m, hr, rec); err != nil {
		return err
	}
	log.Info("archive spooled and verified", "path", path, "bytes", rep.Bytes, "jobs", plan.Jobs)

	res, err := deps.DB.RestoreFile(ctx, deps.Conn, plan.Restore, path, plan.Jobs)
	rep.IgnoredErrors = res.IgnoredErrors
	if err != nil {
		return fmt.Errorf("pg_restore: %w", err)
	}
	return nil
}

// restoreGlobals replays the globals SQL (roles, memberships, tablespaces)
// through psql with ON_ERROR_STOP off. "already exists" errors are expected
// on a cluster that is not empty; the count is returned, not an error.
func restoreGlobals(ctx context.Context, deps Deps, key string) (int, error) {
	rc, _, err := deps.Store.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	dec, err := crypt.NewDecryptor(rc, deps.Identities)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", pipeline.ErrIntegrity, err)
	}
	zr, err := zstd.NewReader(dec)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", pipeline.ErrIntegrity, err)
	}
	defer zr.Close()
	rec := &errRecorder{r: zr}
	n, err := deps.DB.RunSQLScript(ctx, deps.Conn, deps.Conn.MaintenanceDB(), rec)
	if rec.err != nil {
		return n, fmt.Errorf("%w: %v", pipeline.ErrIntegrity, rec.err)
	}
	return n, err
}

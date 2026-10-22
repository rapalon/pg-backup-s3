package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"

	"github.com/rapalon/pg-backup-s3/internal/crypt"
	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pg"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

// ErrIntegrity marks a mismatch between what was uploaded and what the store
// reports. Commands map it to exit code 4.
var ErrIntegrity = errors.New("integrity check failed")

// Database is the part of *pg.Runner the pipeline uses; tests fake it.
type Database interface {
	ClientVersion(ctx context.Context, binary string) (pg.Version, error)
	ServerVersion(ctx context.Context, conn pg.Conn) (pg.Version, error)
	ListDatabases(ctx context.Context, conn pg.Conn) ([]string, error)
	Dump(ctx context.Context, conn pg.Conn, db string, opts pg.DumpOptions) (*pg.Process, error)
	DumpGlobals(ctx context.Context, conn pg.Conn) (*pg.Process, error)
}

// Deps is everything a backup needs. The command layer builds it from the
// configuration once per process.
type Deps struct {
	Logger     *slog.Logger
	DB         Database
	Store      store.ObjectStore
	Conn       pg.Conn
	Recipients []age.Recipient
	// Encryption describes Recipients for the manifest (public data only).
	Encryption manifest.Encryption

	Prefix          string
	Cluster         string
	Compression     string // BACKUP_COMPRESSION value
	LockWaitTimeout time.Duration
	PartSize        int64
	ToolVersion     string
	Now             func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// RunInfo is shared by every backup of one run.
type RunInfo struct {
	Time          time.Time // one timestamp for all objects of the run
	ServerVersion pg.Version
	ClientVersion pg.Version
	GlobalsKey    string // set once the globals object of the run exists
}

// BackupDatabase dumps one database through pg_dump → age → store and writes
// its manifest. The object becomes visible only if pg_dump exited 0 and
// every byte reached the store; any failure aborts the upload.
func BackupDatabase(ctx context.Context, deps Deps, db string, info RunInfo) (manifest.Manifest, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	start := deps.now()
	key := manifest.DumpKey(deps.Prefix, deps.Cluster, db, info.Time)

	proc, err := deps.DB.Dump(ctx, deps.Conn, db, pg.DumpOptions{
		Compression:     deps.Compression,
		LockWaitTimeout: deps.LockWaitTimeout,
	})
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("pg_dump %s: %w", db, err)
	}

	size, sum, err := encryptAndUpload(ctx, cancel, deps, key, proc, false, info)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("backup %s: %w", db, err)
	}

	m := manifest.Manifest{
		FormatVersion:   manifest.FormatVersion,
		ToolVersion:     deps.ToolVersion,
		Cluster:         deps.Cluster,
		Database:        db,
		Kind:            manifest.KindDump,
		Key:             key,
		CreatedAt:       info.Time,
		DurationSeconds: deps.now().Sub(start).Seconds(),
		ServerVersion:   info.ServerVersion.String(),
		PgDumpVersion:   info.ClientVersion.String(),
		DumpFormat:      "custom",
		Compression:     deps.Compression,
		SizeBytes:       size,
		SHA256:          sum,
		Encryption:      deps.Encryption,
		GlobalsKey:      info.GlobalsKey,
	}
	if err := writeManifest(ctx, deps, m); err != nil {
		return manifest.Manifest{}, fmt.Errorf("backup %s: %w", db, err)
	}
	return m, nil
}

// BackupGlobals dumps roles, memberships and tablespaces through
// pg_dumpall --globals-only → zstd → age → store and writes its manifest.
func BackupGlobals(ctx context.Context, deps Deps, info RunInfo) (manifest.Manifest, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	start := deps.now()
	key := manifest.GlobalsKey(deps.Prefix, deps.Cluster, info.Time)

	proc, err := deps.DB.DumpGlobals(ctx, deps.Conn)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("pg_dumpall: %w", err)
	}
	size, sum, err := encryptAndUpload(ctx, cancel, deps, key, proc, true, info)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("backup globals: %w", err)
	}

	m := manifest.Manifest{
		FormatVersion:   manifest.FormatVersion,
		ToolVersion:     deps.ToolVersion,
		Cluster:         deps.Cluster,
		Kind:            manifest.KindGlobals,
		Key:             key,
		CreatedAt:       info.Time,
		DurationSeconds: deps.now().Sub(start).Seconds(),
		ServerVersion:   info.ServerVersion.String(),
		PgDumpVersion:   info.ClientVersion.String(),
		DumpFormat:      "plain",
		Compression:     "zstd",
		SizeBytes:       size,
		SHA256:          sum,
		Encryption:      deps.Encryption,
	}
	if err := writeManifest(ctx, deps, m); err != nil {
		return manifest.Manifest{}, fmt.Errorf("backup globals: %w", err)
	}
	return m, nil
}

// encryptAndUpload streams proc.Stdout → [zstd] → age → sha256 → store.Put.
//
// The producer goroutine finalises the age stream only after the child
// exited 0; if the child fails, the pipe is closed with its error, Put sees
// a reader error and the upload is aborted. A Put failure cancels ctx, which
// terminates the child, and unblocks the producer by closing the pipe.
func encryptAndUpload(ctx context.Context, cancel context.CancelFunc, deps Deps, key string, proc *pg.Process, compress bool, info RunInfo) (int64, string, error) {
	pr, pw := io.Pipe()
	produced := make(chan error, 1)
	go func() {
		err := produce(deps, pw, proc, compress)
		pw.CloseWithError(err) // nil closes cleanly → EOF for the uploader
		produced <- err
	}()

	hr := NewHashReader(pr)
	res, putErr := deps.Store.Put(ctx, key, hr, store.PutOptions{
		ContentType: "application/octet-stream",
		PartSize:    deps.PartSize,
		Metadata: map[string]string{
			"pgbackup-tool-version": deps.ToolVersion,
			"pgbackup-created-at":   manifest.FormatTime(info.Time),
		},
	})
	if putErr != nil {
		cancel()                  // SIGTERM the child so the producer can finish
		pr.CloseWithError(putErr) // wake a producer blocked in Write
	}
	prodErr := <-produced

	var exitErr *pg.ExitError
	switch {
	case prodErr != nil && errors.As(prodErr, &exitErr):
		// The child's own failure is the root cause even when Put also
		// failed because the pipe was closed on it.
		return 0, "", prodErr
	case putErr != nil:
		// Put failed first; the producer's error is only the consequence of
		// the cancellation and pipe close above.
		return 0, "", putErr
	case prodErr != nil:
		return 0, "", prodErr
	}

	if res.Size != hr.Count() {
		return 0, "", fmt.Errorf("%w: uploader reports %d bytes, stream had %d", ErrIntegrity, res.Size, hr.Count())
	}
	stat, err := deps.Store.Stat(ctx, key)
	if err != nil {
		return 0, "", fmt.Errorf("stat after upload: %w", err)
	}
	if stat.Size != hr.Count() {
		if derr := deps.Store.Delete(ctx, []string{key}); derr != nil {
			deps.logger().Warn("could not delete object with wrong size", "key", key, "err", derr)
		}
		return 0, "", fmt.Errorf("%w: store reports %d bytes for %s, uploaded %d", ErrIntegrity, stat.Size, key, hr.Count())
	}
	return hr.Count(), hr.Sum(), nil
}

// produce copies the child's stdout through the optional zstd compressor
// and the age encryptor into pw. It returns the child's exit error first.
func produce(deps Deps, pw *io.PipeWriter, proc *pg.Process, compress bool) error {
	enc, err := crypt.NewEncryptor(pw, deps.Recipients)
	if err != nil {
		proc.Stdout.Close()
		proc.Wait()
		return err
	}
	var w io.Writer = enc
	var zw *zstd.Encoder
	if compress {
		zw, err = zstd.NewWriter(enc, zstd.WithEncoderConcurrency(1))
		if err != nil {
			proc.Stdout.Close()
			proc.Wait()
			return err
		}
		w = zw
	}

	_, copyErr := io.Copy(w, proc.Stdout)
	if copyErr != nil {
		proc.Stdout.Close() // the child gets EPIPE / SIGTERM via ctx
	}
	waitErr := proc.Wait()
	switch {
	case waitErr != nil:
		return waitErr
	case copyErr != nil:
		return copyErr
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			return err
		}
	}
	return enc.Close()
}

func writeManifest(ctx context.Context, deps Deps, m manifest.Manifest) error {
	b, err := m.Marshal()
	if err != nil {
		return err
	}
	_, err = deps.Store.Put(ctx, manifest.ManifestKeyFor(m.Key), bytes.NewReader(b), store.PutOptions{ContentType: "application/json"})
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	return nil
}

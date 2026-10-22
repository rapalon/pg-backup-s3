// Package restore brings a backup back into a PostgreSQL server and proves
// that a backup is readable (verify). A backup tool is judged by this.
//
// Every safety check runs before the first byte is downloaded (Preflight);
// a refused plan is exit code 3. Integrity failures (hash or age
// authentication) are exit code 4 and are never retried.
package restore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"filippo.io/age"

	"github.com/rapalon/pg-backup-s3/internal/pg"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

// ErrRefused marks a restore that a safety check turned down. Exit code 3.
var ErrRefused = errors.New("restore refused")

// Database is the part of *pg.Runner restore uses; tests fake it.
type Database interface {
	ClientVersion(ctx context.Context, binary string) (pg.Version, error)
	ServerVersion(ctx context.Context, conn pg.Conn) (pg.Version, error)
	DatabaseExists(ctx context.Context, conn pg.Conn, db string) (bool, error)
	UserObjectCount(ctx context.Context, conn pg.Conn, db string) (int, error)
	TerminateConnections(ctx context.Context, conn pg.Conn, db string) error
	CreateDatabase(ctx context.Context, conn pg.Conn, db, owner string) error
	DropDatabase(ctx context.Context, conn pg.Conn, db string, force bool) error
	Restore(ctx context.Context, conn pg.Conn, opts pg.RestoreOptions, stdin io.Reader) (pg.RestoreResult, error)
	RestoreFile(ctx context.Context, conn pg.Conn, opts pg.RestoreOptions, path string, jobs int) (pg.RestoreResult, error)
	ListArchive(ctx context.Context, stdin io.Reader) (string, error)
	RunSQLScript(ctx context.Context, conn pg.Conn, db string, script io.Reader) (int, error)
	AnalyzeDatabase(ctx context.Context, conn pg.Conn, db string) error
}

// Deps is what restore and verify need.
type Deps struct {
	Logger     *slog.Logger
	DB         Database
	Store      store.ObjectStore
	Conn       pg.Conn
	Identities []age.Identity
	Prefix     string
	Cluster    string
	Now        func() time.Time
	// DiskFree returns the free bytes on the filesystem holding path.
	// Nil means the platform default (statfs).
	DiskFree func(path string) (uint64, error)
}

func (d Deps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) diskFree(path string) (uint64, error) {
	if d.DiskFree != nil {
		return d.DiskFree(path)
	}
	return diskFree(path)
}

// Options are the restore flags.
type Options struct {
	TargetDB     string // defaults to the manifest's database
	Create       bool
	Clean        bool
	DropExisting bool
	Yes          bool
	Owner        string

	Jobs      int
	SpoolDir  string
	KeepSpool bool

	NoOwner           bool
	NoPrivileges      bool
	SchemaOnly        bool
	DataOnly          bool
	Tables            []string
	SingleTransaction bool
	IgnoreErrors      bool

	Globals bool
	Analyze bool
	DryRun  bool
}

// errRecorder remembers the first non-EOF error a reader returned, so a
// decryption/authentication failure is not lost behind the consumer's own
// "unexpected end of file".
type errRecorder struct {
	r   io.Reader
	err error
}

func (e *errRecorder) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}

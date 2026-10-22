package restore

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pg"
	"github.com/rapalon/pg-backup-s3/internal/pipeline"
)

// VerifyReport is what verify established.
type VerifyReport struct {
	Key        string
	Kind       manifest.Kind
	Bytes      int64
	SHA256     string
	TOCEntries int    // dump: entries in pg_restore --list
	ScratchDB  string // full: the temporary database used
	Relations  int    // full: user relations found after the restore
	Duration   time.Duration

	tocRelations int // TABLE/SEQUENCE/VIEW entries in the TOC
}

// Verify downloads, decrypts and hashes the object. For a dump it also runs
// `pg_restore --list` on the stream; with full it restores into a scratch
// database that is dropped afterwards, whatever happened. Any mismatch is
// pipeline.ErrIntegrity (exit code 4).
func Verify(ctx context.Context, deps Deps, entry manifest.Entry, full bool) (VerifyReport, error) {
	start := deps.now()
	m := entry.Manifest
	rep := VerifyReport{Key: m.Key, Kind: m.Kind}
	if len(deps.Identities) == 0 {
		return rep, fmt.Errorf("AGE_IDENTITY or AGE_PASSPHRASE is required to decrypt")
	}

	rc, hr, rec, dec, err := openDecrypted(ctx, deps, m.Key)
	if err != nil {
		return rep, err
	}
	defer rc.Close()

	switch m.Kind {
	case manifest.KindGlobals:
		zr, err := zstd.NewReader(dec)
		if err != nil {
			return rep, fmt.Errorf("%w: %v", pipeline.ErrIntegrity, err)
		}
		n, err := io.Copy(io.Discard, zr)
		zr.Close()
		if err != nil && rec.err == nil {
			return rep, fmt.Errorf("%w: zstd: %v", pipeline.ErrIntegrity, err)
		}
		if n == 0 {
			return rep, fmt.Errorf("%w: globals object is empty", pipeline.ErrIntegrity)
		}
	case manifest.KindDump:
		toc, err := deps.DB.ListArchive(ctx, dec)
		if err != nil {
			if rec.err != nil {
				return rep, fmt.Errorf("%w: decrypting %s: %v", pipeline.ErrIntegrity, m.Key, rec.err)
			}
			return rep, fmt.Errorf("pg_restore --list: %w", err)
		}
		rep.TOCEntries = countTOCEntries(toc)
		if rep.TOCEntries == 0 {
			return rep, fmt.Errorf("%w: archive table of contents is empty", pipeline.ErrIntegrity)
		}
		rep.tocRelations = countTOCRelations(toc)
		// pg_restore --list stops after the TOC: drain the rest.
		if _, err := io.Copy(io.Discard, dec); err != nil && rec.err == nil {
			rec.err = err
		}
	default:
		return rep, fmt.Errorf("unknown manifest kind %q", m.Kind)
	}
	rep.Bytes, rep.SHA256 = hr.Count(), hr.Sum()
	if err := checkIntegrity(m, hr, rec); err != nil {
		return rep, err
	}

	if full && m.Kind == manifest.KindDump {
		if err := verifyFull(ctx, deps, entry, &rep); err != nil {
			return rep, err
		}
	}
	rep.Duration = deps.now().Sub(start)
	deps.logger().Info("verify ok", "key", m.Key, "bytes", rep.Bytes, "toc_entries", rep.TOCEntries, "full", full, "duration", rep.Duration)
	return rep, nil
}

// verifyFull restores into a scratch database and drops it again, even if
// the restore failed, using a context that survives the caller's.
func verifyFull(ctx context.Context, deps Deps, entry manifest.Entry, rep *VerifyReport) error {
	log := deps.logger()
	m := entry.Manifest
	scratch := scratchName(m.Database, deps.now())
	rep.ScratchDB = scratch

	if err := deps.DB.CreateDatabase(ctx, deps.Conn, scratch, ""); err != nil {
		return fmt.Errorf("creating scratch database %q: %w", scratch, err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		server, _ := deps.DB.ServerVersion(dctx, deps.Conn)
		if err := deps.DB.DropDatabase(dctx, deps.Conn, scratch, server.Major >= 13); err != nil {
			log.Error("could not drop scratch database; drop it by hand", "database", scratch, "err", err)
		} else {
			log.Info("scratch database dropped", "database", scratch)
		}
	}()

	plan := Plan{
		Entry:    entry,
		TargetDB: scratch,
		Jobs:     1,
		Restore:  pg.RestoreOptions{Database: scratch, NoOwner: true, NoPrivileges: true, ExitOnError: true},
	}
	var r Report
	if err := restoreStreamed(ctx, deps, plan, &r); err != nil {
		return fmt.Errorf("full verify: %w", err)
	}
	n, err := deps.DB.UserObjectCount(ctx, deps.Conn, scratch)
	if err != nil {
		return fmt.Errorf("full verify: %w", err)
	}
	// A dump of an empty database legitimately restores to nothing; only an
	// archive that lists relations must produce some.
	if n == 0 && rep.tocRelations > 0 {
		return fmt.Errorf("%w: archive lists %d relations but the restore into %q produced none", pipeline.ErrIntegrity, rep.tocRelations, scratch)
	}
	rep.Relations = n
	return nil
}

// countTOCRelations counts TABLE, SEQUENCE and VIEW entries in a TOC.
func countTOCRelations(toc string) int {
	n := 0
	for _, line := range strings.Split(toc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ";") {
			continue
		}
		for _, kind := range []string{" TABLE ", " SEQUENCE ", " VIEW ", " MATERIALIZED VIEW "} {
			if strings.Contains(line, kind) && !strings.Contains(line, " TABLE DATA ") {
				n++
				break
			}
		}
	}
	return n
}

// scratchName builds verify_<db>_<ts>, kept under PostgreSQL's 63-byte limit.
func scratchName(db string, now time.Time) string {
	const maxDB = 63 - len("verify__20060102T150405Z")
	if len(db) > maxDB {
		db = db[:maxDB]
	}
	return "verify_" + db + "_" + manifest.FormatTime(now)
}

// countTOCEntries counts non-comment lines of a pg_restore --list output.
func countTOCEntries(toc string) int {
	n := 0
	for _, line := range strings.Split(toc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		n++
	}
	return n
}

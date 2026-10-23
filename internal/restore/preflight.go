package restore

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/rapalon/pg-backup-s3/internal/config"
	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pg"
)

// spoolHeadroom is how much free space a spool needs relative to the object.
const spoolHeadroom = 1.1

// Plan is a fully checked restore, ready to execute or to print (dry run).
type Plan struct {
	Entry    manifest.Entry
	TargetDB string

	DropDB    bool // terminate sessions, drop, recreate
	DropForce bool // server supports DROP DATABASE ... WITH (FORCE)
	CreateDB  bool
	Owner     string
	// ClusterDB is the database the cluster-level statements (terminate
	// sessions, DROP/CREATE DATABASE, globals) connect to when PGDATABASE
	// itself is the database being dropped or created; empty otherwise.
	ClusterDB string

	Restore   pg.RestoreOptions
	Jobs      int
	SpoolDir  string
	KeepSpool bool

	Globals    bool
	GlobalsKey string
	Analyze    bool

	ClientVersion pg.Version
	ServerVersion pg.Version
	Warnings      []string
	Actions       []string // human-readable, in execution order
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// clusterDBCandidates are tried, in order, when PGDATABASE names the database
// that the restore drops or creates: PostgreSQL refuses to drop the database
// a session is connected to, so the statements must run from another one.
var clusterDBCandidates = []string{"postgres", "template1"}

// clusterConn is the connection for cluster-level statements: deps.Conn,
// pointed at ClusterDB when Preflight had to choose another database.
func (p Plan) clusterConn(deps Deps) pg.Conn {
	conn := deps.Conn
	if p.ClusterDB != "" {
		conn.Database = p.ClusterDB
	}
	return conn
}

// alternativeClusterDB finds a database other than target that accepts a
// connection, proving it the same way Preflight proves the server: by asking
// for its version. No candidate connectable is a refusal naming PGDATABASE.
func alternativeClusterDB(ctx context.Context, deps Deps, target string) (string, error) {
	var failures []string
	for _, db := range clusterDBCandidates {
		if db == target {
			continue
		}
		conn := deps.Conn
		conn.Database = db
		if _, err := deps.DB.ServerVersion(ctx, conn); err == nil {
			return db, nil
		} else if ctx.Err() != nil {
			return "", err
		} else {
			failures = append(failures, fmt.Sprintf("%q: %v", db, err))
		}
	}
	return "", refuse("PGDATABASE=%q is the database to drop or create, and the cluster-level statements cannot run from %s; set PGDATABASE to another database on this server",
		target, strings.Join(failures, ", "))
}

// Preflight validates everything that can be validated before downloading
// a single byte. Refusals wrap ErrRefused; usage problems wrap config.ErrInvalid.
func Preflight(ctx context.Context, deps Deps, entry manifest.Entry, opts Options) (Plan, error) {
	m := entry.Manifest
	log := deps.logger()
	plan := Plan{Entry: entry, TargetDB: opts.TargetDB, Owner: opts.Owner, Jobs: max(opts.Jobs, 1), SpoolDir: opts.SpoolDir, KeepSpool: opts.KeepSpool, Analyze: opts.Analyze}

	// 1. The manifest itself.
	if m.Kind != manifest.KindDump {
		return Plan{}, fmt.Errorf("%w: %s is a %s object, not a database dump; use verify for it", config.ErrInvalid, m.Key, m.Kind)
	}
	if m.Encryption.Type != "age" {
		return Plan{}, refuse("unsupported encryption %q", m.Encryption.Type)
	}
	if plan.TargetDB == "" {
		plan.TargetDB = m.Database
	}

	// 2. Key material and option sanity.
	if len(deps.Identities) == 0 {
		return Plan{}, fmt.Errorf("%w: AGE_IDENTITY or AGE_PASSPHRASE is required to decrypt", config.ErrInvalid)
	}
	switch {
	case opts.SchemaOnly && opts.DataOnly:
		return Plan{}, fmt.Errorf("%w: --schema-only and --data-only are mutually exclusive", config.ErrInvalid)
	case opts.Clean && opts.DropExisting:
		return Plan{}, fmt.Errorf("%w: --clean and --drop-existing are mutually exclusive", config.ErrInvalid)
	case opts.Jobs > 1 && opts.SpoolDir == "":
		return Plan{}, refuse("--jobs %d needs --spool-dir: pg_restore can only parallelise from a file", opts.Jobs)
	case opts.Jobs > 1 && opts.SingleTransaction:
		return Plan{}, fmt.Errorf("%w: --single-transaction cannot be combined with --jobs", config.ErrInvalid)
	case opts.DropExisting && !opts.Yes:
		return Plan{}, refuse("--drop-existing destroys database %q; add --yes (or PGBACKUP_YES=1) to confirm", plan.TargetDB)
	}

	// 3. Client and server versions.
	client, err := deps.DB.ClientVersion(ctx, "pg_restore")
	if err != nil {
		return Plan{}, err
	}
	dumpVersion, err := pg.ParseVersion(m.PgDumpVersion)
	if err != nil {
		return Plan{}, fmt.Errorf("manifest pg_dump_version: %w", err)
	}
	if client.Major < dumpVersion.Major {
		return Plan{}, refuse("pg_restore %s cannot read an archive written by pg_dump %s; use the image tagged for PostgreSQL %d", client, dumpVersion, dumpVersion.Major)
	}
	// The maintenance database cannot be the one that is dropped or created:
	// choose another one for the cluster-level statements before the first
	// connection, since PGDATABASE may not even exist yet (--create).
	conn := deps.Conn
	if conn.MaintenanceDB() == plan.TargetDB && (opts.DropExisting || opts.Create) {
		alt, err := alternativeClusterDB(ctx, deps, plan.TargetDB)
		if err != nil {
			return Plan{}, err
		}
		conn.Database, plan.ClusterDB = alt, alt
	}
	server, err := deps.DB.ServerVersion(ctx, conn)
	if err != nil {
		return Plan{}, fmt.Errorf("target server: %w", err)
	}
	plan.ClientVersion, plan.ServerVersion = client, server
	if sv, err := pg.ParseVersion(m.ServerVersion); err == nil && server.Major < sv.Major {
		w := fmt.Sprintf("target server %s is older than the source server %s; the restore may fail on newer syntax", server, sv)
		plan.Warnings = append(plan.Warnings, w)
		log.Warn(w)
	}
	plan.DropForce = server.Major >= 13

	// 4. Target database state.
	exists, err := deps.DB.DatabaseExists(ctx, conn, plan.TargetDB)
	if err != nil {
		return Plan{}, fmt.Errorf("checking database %q: %w", plan.TargetDB, err)
	}
	switch {
	case !exists:
		if !opts.Create {
			return Plan{}, refuse("database %q does not exist; add --create", plan.TargetDB)
		}
		plan.CreateDB = true
		plan.Actions = append(plan.Actions, fmt.Sprintf("create database %q", plan.TargetDB))
	case opts.DropExisting:
		plan.DropDB, plan.CreateDB = true, true
		plan.Actions = append(plan.Actions,
			fmt.Sprintf("terminate sessions and drop database %q", plan.TargetDB),
			fmt.Sprintf("create database %q", plan.TargetDB))
	default:
		n, err := deps.DB.UserObjectCount(ctx, conn, plan.TargetDB)
		if err != nil {
			return Plan{}, fmt.Errorf("inspecting database %q: %w", plan.TargetDB, err)
		}
		if n > 0 && !opts.Clean {
			return Plan{}, refuse("database %q is not empty (%d relations); add --clean to drop objects as they are restored, or --drop-existing --yes to recreate it", plan.TargetDB, n)
		}
		if n == 0 && opts.Clean {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("database %q is empty; --clean has nothing to drop", plan.TargetDB))
		}
	}
	switch {
	case plan.ClusterDB != "" && !plan.DropDB && !plan.CreateDB:
		// --create on a database that exists: nothing is dropped or created,
		// so the maintenance database stays as configured.
		plan.ClusterDB = ""
	case plan.ClusterDB != "":
		log.Info("PGDATABASE is the target database; cluster-level statements run from another one",
			"target", plan.TargetDB, "cluster_db", plan.ClusterDB)
	}

	// 5. Spool directory and free space.
	if plan.Jobs > 1 {
		info, err := os.Stat(opts.SpoolDir)
		if err != nil || !info.IsDir() {
			return Plan{}, refuse("spool dir %q is not a directory", opts.SpoolDir)
		}
		// Prove it is writable now, so --dry-run does not promise what a
		// read-only mount would refuse later.
		probe, err := os.CreateTemp(opts.SpoolDir, "pgbackup-probe-*")
		if err != nil {
			return Plan{}, refuse("spool dir %q is not writable: %v", opts.SpoolDir, err)
		}
		probe.Close()
		os.Remove(probe.Name())
		free, err := deps.diskFree(opts.SpoolDir)
		if err != nil {
			return Plan{}, refuse("cannot determine free space in %q: %v", opts.SpoolDir, err)
		}
		need := uint64(float64(m.SizeBytes) * spoolHeadroom)
		if free < need {
			return Plan{}, refuse("spool dir %q has %d MiB free, the archive needs %d MiB", opts.SpoolDir, free>>20, need>>20)
		}
	}

	// 6. Globals.
	if opts.Globals {
		if m.GlobalsKey == "" {
			return Plan{}, refuse("backup %s has no globals object to restore", m.Key)
		}
		plan.Globals, plan.GlobalsKey = true, m.GlobalsKey
		plan.Actions = append([]string{fmt.Sprintf("restore roles and tablespaces from %s into %q", m.GlobalsKey, plan.clusterConn(deps).MaintenanceDB())}, plan.Actions...)
	}

	plan.Restore = pg.RestoreOptions{
		Database:          plan.TargetDB,
		Clean:             opts.Clean,
		NoOwner:           opts.NoOwner,
		NoPrivileges:      opts.NoPrivileges,
		SchemaOnly:        opts.SchemaOnly,
		DataOnly:          opts.DataOnly,
		Tables:            opts.Tables,
		ExitOnError:       !opts.IgnoreErrors,
		SingleTransaction: opts.SingleTransaction,
	}
	mode := "stream"
	if plan.Jobs > 1 {
		mode = fmt.Sprintf("spool to %s, %d jobs", opts.SpoolDir, plan.Jobs)
	}
	plan.Actions = append(plan.Actions, fmt.Sprintf("pg_restore %s into %q (%s)", strings.Join(restoreFlags(plan.Restore), " "), plan.TargetDB, mode))
	if plan.Analyze {
		plan.Actions = append(plan.Actions, fmt.Sprintf("vacuumdb --analyze-in-stages %q", plan.TargetDB))
	}
	return plan, nil
}

func restoreFlags(o pg.RestoreOptions) []string {
	var f []string
	if o.Clean {
		f = append(f, "--clean --if-exists")
	}
	if o.NoOwner {
		f = append(f, "--no-owner")
	}
	if o.NoPrivileges {
		f = append(f, "--no-privileges")
	}
	if o.SchemaOnly {
		f = append(f, "--schema-only")
	}
	if o.DataOnly {
		f = append(f, "--data-only")
	}
	for _, t := range o.Tables {
		f = append(f, "--table="+t)
	}
	if o.ExitOnError {
		f = append(f, "--exit-on-error")
	}
	if o.SingleTransaction {
		f = append(f, "--single-transaction")
	}
	if len(f) == 0 {
		return []string{"(default flags)"}
	}
	return f
}

// Describe renders the plan for --dry-run. It contains no secrets.
func (p Plan) Describe() string {
	m := p.Entry.Manifest
	var b strings.Builder
	fmt.Fprintf(&b, "Backup:   %s\n", m.Key)
	fmt.Fprintf(&b, "Created:  %s  (%s, pg_dump %s, server %s, %d bytes, %s)\n",
		m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), m.Database, m.PgDumpVersion, m.ServerVersion, m.SizeBytes, m.Compression)
	fmt.Fprintf(&b, "Target:   database %q on the configured server (server %s, pg_restore %s)\n", p.TargetDB, p.ServerVersion, p.ClientVersion)
	if p.ClusterDB != "" {
		fmt.Fprintf(&b, "Via:      database %q for DROP/CREATE DATABASE, because PGDATABASE names the target\n", p.ClusterDB)
	}
	b.WriteString("Actions:\n")
	for i, a := range p.Actions {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, a)
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(&b, "Warning:  %s\n", w)
	}
	return b.String()
}

package pg

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// QuoteIdent quotes a SQL identifier such as a database name.
func QuoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// QuoteLiteral quotes a SQL string literal.
func QuoteLiteral(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

// DatabaseExists reports whether db exists on the server.
func (r *Runner) DatabaseExists(ctx context.Context, conn Conn, db string) (bool, error) {
	rows, err := r.Query(ctx, conn, "select 1 from pg_database where datname = "+QuoteLiteral(db))
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

// userObjectsSQL counts relations outside the system schemas: the question
// "is this database empty?" for restore pre-flight and verify --full.
const userObjectsSQL = `select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace
where n.nspname not in ('pg_catalog', 'information_schema') and n.nspname not like 'pg_toast%'`

// UserObjectCount returns the number of user relations in db.
func (r *Runner) UserObjectCount(ctx context.Context, conn Conn, db string) (int, error) {
	rows, err := r.QueryDB(ctx, conn, db, userObjectsSQL)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return 0, fmt.Errorf("user object count returned nothing")
	}
	n, err := strconv.Atoi(strings.TrimSpace(rows[0][0]))
	if err != nil {
		return 0, fmt.Errorf("user object count: %w", err)
	}
	return n, nil
}

// TerminateConnections kills every other session connected to db.
func (r *Runner) TerminateConnections(ctx context.Context, conn Conn, db string) error {
	_, err := r.Query(ctx, conn,
		"select pg_terminate_backend(pid) from pg_stat_activity where datname = "+QuoteLiteral(db)+" and pid <> pg_backend_pid()")
	return err
}

// CreateDatabase creates db from template0 (so it is truly empty and any
// encoding/locale from the dump can be applied), optionally with an owner.
func (r *Runner) CreateDatabase(ctx context.Context, conn Conn, db, owner string) error {
	sql := "create database " + QuoteIdent(db) + " template template0"
	if owner != "" {
		sql += " owner " + QuoteIdent(owner)
	}
	_, err := r.Query(ctx, conn, sql)
	return err
}

// DropDatabase drops db; force (PostgreSQL 13+) terminates sessions first.
func (r *Runner) DropDatabase(ctx context.Context, conn Conn, db string, force bool) error {
	sql := "drop database if exists " + QuoteIdent(db)
	if force {
		sql += " with (force)"
	}
	_, err := r.Query(ctx, conn, sql)
	return err
}

// AnalyzeDatabase runs vacuumdb --analyze-in-stages so planner statistics
// exist right after a restore.
func (r *Runner) AnalyzeDatabase(ctx context.Context, conn Conn, db string) error {
	return r.Run(ctx, Spec{
		Binary: "vacuumdb",
		Args:   []string{"--analyze-in-stages", "--no-password", "--dbname=" + db},
		Env:    conn.Env(),
	})
}

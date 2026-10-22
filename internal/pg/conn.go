package pg

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// Conn describes how to reach a server. It is handed to the client tools as
// libpq environment variables, so the password never appears on a command line.
type Conn struct {
	Host     string
	Port     int
	User     string
	Password string
	SSLMode  string
	// Database is the maintenance database used for queries; "postgres" when empty.
	Database string
	// ConnectTimeout bounds connection establishment; 10s when zero.
	ConnectTimeout time.Duration
}

// Env returns the libpq variables for this connection.
func (c Conn) Env() []string {
	var env []string
	add := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	add("PGHOST", c.Host)
	if c.Port > 0 {
		add("PGPORT", strconv.Itoa(c.Port))
	}
	add("PGUSER", c.User)
	add("PGPASSWORD", c.Password)
	add("PGSSLMODE", c.SSLMode)
	timeout := c.ConnectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	add("PGCONNECT_TIMEOUT", strconv.Itoa(int(timeout.Seconds())))
	return env
}

// MaintenanceDB is the database psql connects to for cluster-level queries.
func (c Conn) MaintenanceDB() string {
	if c.Database == "" {
		return "postgres"
	}
	return c.Database
}

// Query runs sql through psql against the maintenance database and returns
// the result rows, tab-separated fields split into columns.
func (r *Runner) Query(ctx context.Context, conn Conn, sql string) ([][]string, error) {
	return r.QueryDB(ctx, conn, conn.MaintenanceDB(), sql)
}

// QueryDB is Query against an explicit database.
func (r *Runner) QueryDB(ctx context.Context, conn Conn, db, sql string) ([][]string, error) {
	var out strings.Builder
	err := r.Run(ctx, Spec{
		Binary: "psql",
		Args: []string{
			"--no-psqlrc", "--no-password", "--quiet",
			"--no-align", "--tuples-only", "--field-separator=\t",
			"--set=ON_ERROR_STOP=1",
			"--dbname=" + db,
			"--command=" + sql,
		},
		Env:    conn.Env(),
		Stdout: &out,
	})
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, line := range strings.Split(out.String(), "\n") {
		if line == "" {
			continue
		}
		rows = append(rows, strings.Split(line, "\t"))
	}
	return rows, nil
}

// ListDatabases returns every database that is not a template and allows
// connections, sorted by name.
func (r *Runner) ListDatabases(ctx context.Context, conn Conn) ([]string, error) {
	rows, err := r.Query(ctx, conn,
		"select datname from pg_database where not datistemplate and datallowconn order by 1")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		if len(row) > 0 && row[0] != "" {
			names = append(names, row[0])
		}
	}
	return names, nil
}

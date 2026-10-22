package pg

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestConnEnv(t *testing.T) {
	t.Parallel()
	c := Conn{Host: "db", Port: 6432, User: "backup", Password: "pw", SSLMode: "require", ConnectTimeout: 5 * time.Second}
	got := strings.Join(c.Env(), " ")
	want := "PGHOST=db PGPORT=6432 PGUSER=backup PGPASSWORD=pw PGSSLMODE=require PGCONNECT_TIMEOUT=5"
	if got != want {
		t.Errorf("Env = %q, want %q", got, want)
	}

	minimal := Conn{}
	if got := strings.Join(minimal.Env(), " "); got != "PGCONNECT_TIMEOUT=10" {
		t.Errorf("empty Conn Env = %q", got)
	}
	if minimal.MaintenanceDB() != "postgres" {
		t.Errorf("MaintenanceDB = %q", minimal.MaintenanceDB())
	}
	if (Conn{Database: "admin"}).MaintenanceDB() != "admin" {
		t.Error("explicit Database ignored")
	}
}

func TestParseVersion(t *testing.T) {
	t.Parallel()
	ok := map[string]Version{
		"pg_dump (PostgreSQL) 18.6":               {18, 6},
		"psql (PostgreSQL) 16.15\n":               {16, 15},
		"16.15 (Debian 16.15-1.pgdg120+1)":        {16, 15},
		"17devel":                                 {17, 0},
		"18beta1":                                 {18, 0},
		"pg_restore (PostgreSQL) 17.4 (Homebrew)": {17, 4},
		"9.6.24": {9, 6},
	}
	for in, want := range ok {
		got, err := ParseVersion(in)
		if err != nil {
			t.Errorf("ParseVersion(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseVersion(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"", "no digits here", "0.1"} {
		if _, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) succeeded", in)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b Version
		want int
	}{
		{Version{16, 2}, Version{16, 2}, 0},
		{Version{16, 2}, Version{16, 3}, -1},
		{Version{16, 9}, Version{17, 0}, -1},
		{Version{18, 0}, Version{17, 9}, 1},
		{Version{17, 5}, Version{17, 4}, 1},
	}
	for _, c := range cases {
		if got := c.a.Compare(c.b); got != c.want {
			t.Errorf("%v.Compare(%v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if (Version{18, 6}).String() != "18.6" {
		t.Error("String")
	}
}

func TestClientVersionIsCached(t *testing.T) {
	t.Parallel()
	r, logs := fakeRunner(t)
	for i := 0; i < 3; i++ {
		v, err := r.ClientVersion(context.Background(), "pg_dump")
		if err != nil {
			t.Fatalf("ClientVersion: %v", err)
		}
		if v != (Version{18, 6}) {
			t.Errorf("version = %v", v)
		}
	}
	if n := strings.Count(logs.String(), `"msg":"exec"`); n != 1 {
		t.Errorf("pg_dump --version was executed %d times, want 1", n)
	}
}

func TestClientVersionErrors(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	if _, err := r.ClientVersion(context.Background(), "fake"); err == nil {
		t.Error("want error for a binary that prints no version")
	}
}

func TestServerVersion(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	v, err := r.ServerVersion(context.Background(), Conn{})
	if err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	if v != (Version{16, 15}) {
		t.Errorf("version = %v", v)
	}
}

func TestListDatabases(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	dbs, err := r.ListDatabases(context.Background(), Conn{})
	if err != nil {
		t.Fatalf("ListDatabases: %v", err)
	}
	if got := strings.Join(dbs, ","); got != "app,postgres,zeta" {
		t.Errorf("databases = %q", got)
	}
}

func TestQuery(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)

	rows, err := r.Query(context.Background(), Conn{}, "select multi")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 2 || rows[0][0] != "a" || rows[0][1] != "b" || rows[1][1] != "d" {
		t.Errorf("rows = %v", rows)
	}

	rows, err = r.Query(context.Background(), Conn{}, "select nothing")
	if err != nil || len(rows) != 0 {
		t.Errorf("empty query: rows=%v err=%v", rows, err)
	}

	rows, err = r.QueryDB(context.Background(), Conn{}, "other", "select dbname")
	if err != nil || len(rows) != 1 || rows[0][0] != "other" {
		t.Errorf("QueryDB: rows=%v err=%v", rows, err)
	}

	_, err = r.Query(context.Background(), Conn{}, "select fail")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Binary != "psql" || !strings.Contains(ee.StderrTail, "does not exist") {
		t.Errorf("failed query: %v", err)
	}
}

func TestDumpArgs(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	conn := Conn{Host: "db", Password: "pw"}
	p, err := r.Dump(context.Background(), conn, "app", DumpOptions{Compression: "zstd:3", LockWaitTimeout: time.Minute})
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	out, _ := io.ReadAll(p.Stdout)
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	s := string(out)
	wantArgs := "ARGS --format=custom --no-password --compress=zstd:3 --lock-wait-timeout=60000 --dbname=app\n"
	if !strings.Contains(s, wantArgs) {
		t.Errorf("args line missing in %q, want %q", s, wantArgs)
	}
	if !strings.Contains(s, "PGOPTIONS=-c statement_timeout=0\n") {
		t.Error("statement_timeout=0 not set for the dump session")
	}
	if !strings.Contains(s, "PGPASSWORD=pw\n") {
		t.Error("password not passed through the environment")
	}
	if !strings.Contains(s, "S3_SECRET_KEY=must-not-leak\n") {
		// the fake base env deliberately contains it; the real hostEnv does not
		t.Error("fake base env not applied")
	}
}

func TestDumpRejectsZstdOnOldClient(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t, "FAKE_PG_DUMP_VERSION=15.8")
	_, err := r.Dump(context.Background(), Conn{}, "app", DumpOptions{Compression: "zstd:3"})
	if !errors.Is(err, ErrCompressionUnsupported) {
		t.Errorf("err = %v, want ErrCompressionUnsupported", err)
	}
}

func TestDumpGzipOnOldClient(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t, "FAKE_PG_DUMP_VERSION=15.8")
	p, err := r.Dump(context.Background(), Conn{}, "app", DumpOptions{Compression: "gzip:6"})
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	out, _ := io.ReadAll(p.Stdout)
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "--compress=6 ") {
		t.Errorf("old client should get a numeric level: %q", out)
	}
}

func TestCompressArg(t *testing.T) {
	t.Parallel()
	v16, v15 := Version{16, 0}, Version{15, 8}
	cases := []struct {
		v       Version
		spec    string
		want    string
		wantErr bool
	}{
		{v16, "zstd:3", "zstd:3", false},
		{v16, "gzip", "gzip", false},
		{v16, "lz4:1", "lz4:1", false},
		{v16, "none", "none", false},
		{v16, "", "", false},
		{v16, "brotli", "", true},
		{v15, "gzip:9", "9", false},
		{v15, "gzip", "", false},
		{v15, "none", "0", false},
		{v15, "zstd", "", true},
		{v15, "lz4:3", "", true},
		{v15, "brotli", "", true},
	}
	for _, c := range cases {
		got, err := compressArg(c.v, c.spec)
		if (err != nil) != c.wantErr {
			t.Errorf("compressArg(%v, %q) err = %v, wantErr %v", c.v, c.spec, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("compressArg(%v, %q) = %q, want %q", c.v, c.spec, got, c.want)
		}
	}
}

func TestDumpGlobalsArgs(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	p, err := r.DumpGlobals(context.Background(), Conn{Database: "maint"})
	if err != nil {
		t.Fatalf("DumpGlobals: %v", err)
	}
	out, _ := io.ReadAll(p.Stdout)
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "ARGS --globals-only --no-password --database=maint\n") {
		t.Errorf("args: %q", out)
	}
	if !strings.Contains(string(out), "PGOPTIONS=-c statement_timeout=0\n") {
		t.Error("statement_timeout=0 not set")
	}
}

func TestHostEnvIsAllowlisted(t *testing.T) {
	t.Setenv("S3_SECRET_KEY", "leak?")
	t.Setenv("PGPASSWORD", "leak?")
	t.Setenv("TZ", "UTC")
	for _, kv := range hostEnv() {
		if strings.HasPrefix(kv, "S3_SECRET_KEY=") || strings.HasPrefix(kv, "PGPASSWORD=") {
			t.Errorf("hostEnv leaks %s", kv)
		}
	}
	if !contains(hostEnv(), "TZ=UTC") {
		t.Error("hostEnv drops TZ")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestNewRunnerNilLogger(t *testing.T) {
	t.Parallel()
	r := NewRunner(nil)
	if r.Logger == nil || r.KillGrace != DefaultKillGrace {
		t.Error("NewRunner(nil) defaults")
	}
}

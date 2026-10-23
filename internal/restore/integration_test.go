//go:build integration

package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pg"
	"github.com/rapalon/pg-backup-s3/internal/pipeline"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

type stand struct {
	runner  *pg.Runner
	store   *store.S3Store
	conn    pg.Conn
	key     *age.X25519Identity
	prefix  string
	backup  pipeline.Deps
	restore Deps
}

func newStand(t *testing.T) *stand {
	t.Helper()
	if os.Getenv("PGBACKUP_INTEGRATION") != "1" {
		t.Skip("PGBACKUP_INTEGRATION != 1; run via test/compose.yml")
	}
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.NewS3(store.S3Config{
		Endpoint: os.Getenv("S3_ENDPOINT"), Region: os.Getenv("S3_REGION"), Bucket: os.Getenv("S3_BUCKET"),
		AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"), PathStyle: true, PartSize: 8 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(os.Getenv("PGPORT"))
	conn := pg.Conn{Host: os.Getenv("PGHOST"), Port: port, User: os.Getenv("PGUSER"), Password: os.Getenv("PGPASSWORD"),
		SSLMode: os.Getenv("PGSSLMODE"), Database: os.Getenv("PGDATABASE")}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	runner := pg.NewRunner(logger)
	prefix := fmt.Sprintf("it-restore/%d", time.Now().UnixNano())
	s := &stand{runner: runner, store: st, conn: conn, key: key, prefix: prefix}
	s.backup = pipeline.Deps{
		Logger: logger, DB: runner, Store: st, Conn: conn,
		Recipients: []age.Recipient{key.Recipient()},
		Encryption: manifest.Encryption{Type: "age", Recipients: []string{key.Recipient().String()}},
		Prefix:     prefix, Cluster: "stand", Compression: "zstd:3", LockWaitTimeout: time.Minute, PartSize: 8 << 20, ToolVersion: "it",
	}
	s.restore = Deps{Logger: logger, DB: runner, Store: st, Conn: conn, Identities: []age.Identity{key}, Prefix: prefix, Cluster: "stand"}
	t.Cleanup(func() {
		ctx := context.Background()
		infos, _ := st.List(ctx, prefix)
		keys := make([]string, len(infos))
		for i, o := range infos {
			keys[i] = o.Key
		}
		st.Delete(ctx, keys)
	})
	return s
}

func (s *stand) sql(t *testing.T, db, q string) [][]string {
	t.Helper()
	rows, err := s.runner.QueryDB(context.Background(), s.conn, db, q)
	if err != nil {
		t.Fatalf("sql %q on %s: %v", q, db, err)
	}
	return rows
}

func (s *stand) dropDB(t *testing.T, db string) {
	t.Helper()
	s.runner.DropDatabase(context.Background(), s.conn, db, true)
}

func (s *stand) createSource(t *testing.T, db string) {
	t.Helper()
	ctx := context.Background()
	s.dropDB(t, db)
	if err := s.runner.CreateDatabase(ctx, s.conn, db, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.dropDB(t, db) })
	for _, q := range []string{
		"create sequence order_seq start 1000",
		"create table customers (id int primary key, name text not null, note text)",
		"create table orders (id bigint primary key default nextval('order_seq'), customer_id int references customers(id), amount numeric(12,2), payload text)",
		"create index orders_customer on orders(customer_id)",
		"create view big_orders as select * from orders where amount > 500",
		"create function order_total(cid int) returns numeric language sql as $$ select coalesce(sum(amount), 0) from orders where customer_id = cid $$",
		"insert into customers select g, 'customer ' || g, repeat(md5(g::text), 2) from generate_series(1, 2000) g",
		"insert into orders (customer_id, amount, payload) select (g % 2000) + 1, (g % 1000) + 0.5, repeat(md5(g::text), 4) from generate_series(1, 40000) g",
		"select lo_from_bytea(0, decode(repeat('deadbeef', 4096), 'hex'))",
		"select lo_from_bytea(0, decode(repeat('cafebabe', 8192), 'hex'))",
		"select setval('order_seq', 777777)",
	} {
		s.sql(t, db, q)
	}
}

// fingerprint captures everything the restore must reproduce.
func (s *stand) fingerprint(t *testing.T, db string) map[string]string {
	t.Helper()
	fp := map[string]string{}
	for _, q := range []string{
		"select count(*) from customers",
		"select count(*) from orders",
		"select md5(string_agg(id || ':' || customer_id || ':' || amount || ':' || md5(payload), ',' order by id)) from orders",
		"select md5(string_agg(id || ':' || name || ':' || note, ',' order by id)) from customers",
		"select last_value from order_seq",
		"select count(*) from big_orders",
		"select order_total(7)",
		"select string_agg(md5(lo_get(oid)), ',' order by oid) from pg_largeobject_metadata",
		"select count(*) from pg_indexes where tablename = 'orders'",
		"select count(*) from pg_constraint where conname = 'orders_customer_id_fkey'",
	} {
		rows := s.sql(t, db, q)
		fp[q] = fmt.Sprint(rows)
	}
	return fp
}

func (s *stand) schema(t *testing.T, db string) string {
	t.Helper()
	var out bytes.Buffer
	err := s.runner.Run(context.Background(), pg.Spec{
		Binary: "pg_dump", Args: []string{"--schema-only", "--no-owner", "--no-privileges", "--dbname=" + db},
		Env: s.conn.Env(), Stdout: &out,
	})
	if err != nil {
		t.Fatalf("schema dump of %s: %v", db, err)
	}
	// pg_dump ≥ 17.6 wraps plain dumps in \restrict <random token> ...
	// \unrestrict <token>; the token differs per invocation.
	var lines []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, `\restrict `) || strings.HasPrefix(l, `\unrestrict `) {
			continue
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

func (s *stand) backupOf(t *testing.T, db string) manifest.Entry {
	t.Helper()
	report, err := pipeline.Run(context.Background(), s.backup, pipeline.RunOptions{Databases: []string{db}, Retry: pipeline.DefaultRetryPolicy(2)})
	if err != nil || !report.OK() {
		t.Fatalf("backup: %v %+v", err, report.Failed())
	}
	e, err := Resolve(context.Background(), s.store, s.prefix, "stand", Selector{Database: db})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestIntegrationRestoreCycle(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()
	const src = "p8src"
	s.createSource(t, src)
	want := s.fingerprint(t, src)
	wantSchema := s.schema(t, src)
	entry := s.backupOf(t, src)
	t.Logf("backup %s: %d bytes", entry.Manifest.Key, entry.Manifest.SizeBytes)

	check := func(t *testing.T, db string) {
		t.Helper()
		if got := s.fingerprint(t, db); fmt.Sprint(got) != fmt.Sprint(want) {
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s: %q\n  src: %s\n  dst: %s", db, k, v, got[k])
				}
			}
		}
		if got := s.schema(t, db); got != wantSchema {
			t.Errorf("%s: schema differs:\n%s", db, diffLines(wantSchema, got))
		}
	}

	t.Run("stream with create", func(t *testing.T) {
		const dst = "p8dst_stream"
		s.dropDB(t, dst)
		t.Cleanup(func() { s.dropDB(t, dst) })
		plan, err := Preflight(ctx, s.restore, entry, Options{TargetDB: dst, Create: true, Analyze: true})
		if err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		rep, err := Execute(ctx, s.restore, plan)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if rep.Bytes != entry.Manifest.SizeBytes || rep.SHA256 != entry.Manifest.SHA256 || !rep.Analyzed {
			t.Errorf("report = %+v", rep)
		}
		check(t, dst)

		// Non-empty target without --clean is refused and left intact.
		_, err = Preflight(ctx, s.restore, entry, Options{TargetDB: dst})
		if !errors.Is(err, ErrRefused) {
			t.Errorf("non-empty: %v", err)
		}
		// --drop-existing without --yes is refused.
		_, err = Preflight(ctx, s.restore, entry, Options{TargetDB: dst, DropExisting: true})
		if !errors.Is(err, ErrRefused) {
			t.Errorf("drop without yes: %v", err)
		}
		check(t, dst)

		// --clean into the already restored database.
		plan, err = Preflight(ctx, s.restore, entry, Options{TargetDB: dst, Clean: true})
		if err != nil {
			t.Fatalf("Preflight clean: %v", err)
		}
		if _, err := Execute(ctx, s.restore, plan); err != nil {
			t.Fatalf("Execute clean: %v", err)
		}
		check(t, dst)

		// --drop-existing --yes recreates it.
		plan, err = Preflight(ctx, s.restore, entry, Options{TargetDB: dst, DropExisting: true, Yes: true})
		if err != nil {
			t.Fatalf("Preflight drop: %v", err)
		}
		if _, err := Execute(ctx, s.restore, plan); err != nil {
			t.Fatalf("Execute drop: %v", err)
		}
		check(t, dst)
	})

	// PGDATABASE equal to the target (a compose stack whose image only creates
	// the application database): DROP/CREATE DATABASE must run from another
	// database, since PostgreSQL refuses to drop the one the session is in.
	t.Run("maintenance database is the target", func(t *testing.T) {
		const dst = "p8dst_maint"
		s.dropDB(t, dst)
		t.Cleanup(func() { s.dropDB(t, dst) })
		deps := s.restore
		deps.Conn.Database = dst

		// --create while PGDATABASE does not exist yet.
		plan, err := Preflight(ctx, deps, entry, Options{TargetDB: dst, Create: true})
		if err != nil {
			t.Fatalf("Preflight create: %v", err)
		}
		if plan.ClusterDB != "postgres" {
			t.Errorf("ClusterDB = %q, want postgres", plan.ClusterDB)
		}
		if _, err := Execute(ctx, deps, plan); err != nil {
			t.Fatalf("Execute create: %v", err)
		}
		check(t, dst)

		// --drop-existing --yes while PGDATABASE is the database being dropped.
		plan, err = Preflight(ctx, deps, entry, Options{TargetDB: dst, DropExisting: true, Yes: true, Globals: true})
		if err != nil {
			t.Fatalf("Preflight drop: %v", err)
		}
		if plan.ClusterDB != "postgres" || !strings.Contains(plan.Describe(), `Via:      database "postgres"`) {
			t.Errorf("plan = %+v\n%s", plan, plan.Describe())
		}
		if _, err := Execute(ctx, deps, plan); err != nil {
			t.Fatalf("Execute drop: %v", err)
		}
		check(t, dst)

		// Nothing to drop or create: the configured database stays in use.
		plan, err = Preflight(ctx, deps, entry, Options{TargetDB: dst, Clean: true})
		if err != nil || plan.ClusterDB != "" {
			t.Errorf("clean: %+v %v", plan, err)
		}
	})

	t.Run("spool with jobs", func(t *testing.T) {
		const dst = "p8dst_spool"
		s.dropDB(t, dst)
		t.Cleanup(func() { s.dropDB(t, dst) })
		dir := t.TempDir()
		plan, err := Preflight(ctx, s.restore, entry, Options{TargetDB: dst, Create: true, Jobs: 4, SpoolDir: dir})
		if err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		rep, err := Execute(ctx, s.restore, plan)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if _, err := os.Stat(rep.SpoolPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("spool file left: %s", rep.SpoolPath)
		}
		check(t, dst)
	})

	t.Run("verify", func(t *testing.T) {
		rep, err := Verify(ctx, s.restore, entry, false)
		if err != nil || rep.TOCEntries == 0 {
			t.Fatalf("verify: %+v %v", rep, err)
		}
		rep, err = Verify(ctx, s.restore, entry, true)
		if err != nil || rep.Relations == 0 {
			t.Fatalf("verify --full: %+v %v", rep, err)
		}
		if exists, _ := s.runner.DatabaseExists(ctx, s.conn, rep.ScratchDB); exists {
			t.Errorf("scratch database %s still exists", rep.ScratchDB)
		}
		g, err := Resolve(ctx, s.store, s.prefix, "stand", Selector{Key: entry.Manifest.GlobalsKey})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(ctx, s.restore, g, false); err != nil {
			t.Errorf("verify globals: %v", err)
		}
	})

	t.Run("globals", func(t *testing.T) {
		const dst = "p8dst_globals"
		s.dropDB(t, dst)
		t.Cleanup(func() { s.dropDB(t, dst) })
		plan, err := Preflight(ctx, s.restore, entry, Options{TargetDB: dst, Create: true, Globals: true})
		if err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		rep, err := Execute(ctx, s.restore, plan)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		// The stand's role already exists, so replaying globals reports errors but succeeds.
		if rep.GlobalsErrors == 0 {
			t.Error("expected 'already exists' errors while replaying globals on a populated cluster")
		}
		check(t, dst)
	})

	t.Run("corrupted object", func(t *testing.T) {
		// Flip one byte in the middle of the stored object through the S3 API.
		rc, _, err := s.store.Get(ctx, entry.Manifest.Key)
		if err != nil {
			t.Fatal(err)
		}
		original, _ := io.ReadAll(rc)
		rc.Close()
		t.Cleanup(func() { s.store.Put(ctx, entry.Manifest.Key, bytes.NewReader(original), store.PutOptions{}) })
		tampered := append([]byte(nil), original...)
		tampered[len(tampered)/2] ^= 0x01
		if _, err := s.store.Put(ctx, entry.Manifest.Key, bytes.NewReader(tampered), store.PutOptions{}); err != nil {
			t.Fatal(err)
		}

		if _, err := Verify(ctx, s.restore, entry, false); !errors.Is(err, pipeline.ErrIntegrity) {
			t.Errorf("verify of tampered object: %v", err)
		}
		const dst = "p8dst_tampered"
		s.dropDB(t, dst)
		t.Cleanup(func() { s.dropDB(t, dst) })
		plan, err := Preflight(ctx, s.restore, entry, Options{TargetDB: dst, Create: true})
		if err != nil {
			t.Fatal(err)
		}
		_, err = Execute(ctx, s.restore, plan)
		if !errors.Is(err, pipeline.ErrIntegrity) {
			t.Errorf("restore of tampered object: %v", err)
		}
		if n, _ := s.runner.UserObjectCount(ctx, s.conn, dst); n >= 6 {
			t.Errorf("tampered restore completed with %d relations", n)
		}
	})
}

func diffLines(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			out = append(out, fmt.Sprintf("line %d:\n  src: %s\n  dst: %s", i+1, x, y))
			if len(out) > 10 {
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

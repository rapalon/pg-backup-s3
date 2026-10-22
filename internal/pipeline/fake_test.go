package pipeline

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pg"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

// fakeDB stands in for *pg.Runner. Dumps are in-memory byte slices; a
// per-database wait error simulates pg_dump failing after partial output.
type fakeDB struct {
	client, server pg.Version
	dbs            []string
	listErr        error
	dumps          map[string][]byte
	globals        []byte
	waitErr        map[string]error // returned by Process.Wait for that db
	startErr       map[string]error // returned by Dump itself

	mu      sync.Mutex
	started []string
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		client:   pg.Version{Major: 18, Minor: 6},
		server:   pg.Version{Major: 18, Minor: 6},
		dbs:      []string{"alpha", "beta", "gamma"},
		dumps:    map[string][]byte{"alpha": testBytes(1, 3<<20), "beta": testBytes(2, 1<<20), "gamma": testBytes(3, 512)},
		globals:  []byte("--\n-- PostgreSQL database cluster dump\n--\nCREATE ROLE test;\nALTER ROLE test WITH SUPERUSER;\n"),
		waitErr:  map[string]error{},
		startErr: map[string]error{},
	}
}

func (f *fakeDB) ClientVersion(context.Context, string) (pg.Version, error) { return f.client, nil }
func (f *fakeDB) ServerVersion(context.Context, pg.Conn) (pg.Version, error) {
	return f.server, nil
}
func (f *fakeDB) ListDatabases(context.Context, pg.Conn) ([]string, error) {
	return append([]string(nil), f.dbs...), f.listErr
}

func (f *fakeDB) Dump(ctx context.Context, _ pg.Conn, db string, _ pg.DumpOptions) (*pg.Process, error) {
	f.mu.Lock()
	f.started = append(f.started, db)
	f.mu.Unlock()
	if err := f.startErr[db]; err != nil {
		return nil, err
	}
	data := f.dumps[db]
	werr := f.waitErr[db]
	if werr != nil {
		data = data[:len(data)/2] // a failing pg_dump stops mid-stream
	}
	return f.process(ctx, data, werr), nil
}

func (f *fakeDB) DumpGlobals(ctx context.Context, _ pg.Conn) (*pg.Process, error) {
	return f.process(ctx, f.globals, f.waitErr["globals"]), nil
}

func (f *fakeDB) process(ctx context.Context, data []byte, waitErr error) *pg.Process {
	return pg.NewProcess(io.NopCloser(bytes.NewReader(data)), func() error {
		if err := ctx.Err(); err != nil {
			return err // a real child terminated by cancellation reports ctx.Err
		}
		return waitErr
	})
}

func (f *fakeDB) startedDumps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

func testBytes(seed uint64, n int) []byte {
	rng := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

var fixedTime = time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)

type harness struct {
	deps    Deps
	db      *fakeDB
	store   *store.MemStore
	key     *age.X25519Identity
	logs    *syncBuilder
	clockMu sync.Mutex
}

// syncBuilder is a strings.Builder safe for the logger's concurrent writes.
type syncBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{db: newFakeDB(), store: store.NewMem(), key: key, logs: &syncBuilder{}}
	clock := fixedTime
	h.deps = Deps{
		Logger:          slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		DB:              h.db,
		Store:           h.store,
		Conn:            pg.Conn{Host: "db", User: "test"},
		Recipients:      []age.Recipient{key.Recipient()},
		Encryption:      manifest.Encryption{Type: "age", Recipients: []string{key.Recipient().String()}},
		Prefix:          "backups",
		Cluster:         "unit",
		Compression:     "zstd:3",
		LockWaitTimeout: time.Minute,
		ToolVersion:     "test",
		Now: func() time.Time {
			h.clockMu.Lock()
			defer h.clockMu.Unlock()
			clock = clock.Add(time.Second)
			return clock
		},
	}
	return h
}

func (h *harness) info() RunInfo {
	return RunInfo{Time: fixedTime, ServerVersion: h.db.server, ClientVersion: h.db.client}
}

func instantRetry(attempts int) RetryPolicy {
	p := DefaultRetryPolicy(attempts)
	p.Sleep = func(context.Context, time.Duration) error { return nil }
	return p
}

// flakyStore fails the first n Puts with a transient error, then delegates.
type flakyStore struct {
	store.ObjectStore
	mu   sync.Mutex
	left int
	seen int
}

func (f *flakyStore) Put(ctx context.Context, key string, r io.Reader, opts store.PutOptions) (store.PutResult, error) {
	f.mu.Lock()
	f.seen++
	fail := f.left > 0
	if fail {
		f.left--
	}
	f.mu.Unlock()
	if fail {
		io.Copy(io.Discard, io.LimitReader(r, 4096))
		return store.PutResult{}, &transientErr{}
	}
	return f.ObjectStore.Put(ctx, key, r, opts)
}

type transientErr struct{}

func (*transientErr) Error() string { return "503 Slow Down" }

package restore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"

	"github.com/rapalon/pg-backup-s3/internal/crypt"
	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pg"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

var fixedTime = time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)

// fakeDB records every administrative call and captures what pg_restore
// would have read.
type fakeDB struct {
	mu            sync.Mutex
	client        pg.Version
	server        pg.Version
	exists        map[string]bool
	objects       map[string]int
	calls         []string
	restored      []byte
	restoredFile  []byte
	restoreErr    error
	restoreResult pg.RestoreResult
	createErr     error
	script        []byte
	scriptErrors  int
	listErr       error
	tocOverride   string
	userObjects   int // reported for databases not in objects
	// unreachable databases refuse every connection (ServerVersion fails for
	// a Conn whose maintenance database is listed).
	unreachable map[string]bool
	// adminDBs records the maintenance database each cluster-level call
	// (terminate, drop, create, script) connected to, in order.
	adminDBs []string
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		client:  pg.Version{Major: 18, Minor: 6},
		server:  pg.Version{Major: 18, Minor: 6},
		exists:  map[string]bool{"postgres": true, "full": true, "empty": true},
		objects: map[string]int{"full": 12, "empty": 0},
	}
}

func (f *fakeDB) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeDB) ClientVersion(context.Context, string) (pg.Version, error) { return f.client, nil }
func (f *fakeDB) ServerVersion(_ context.Context, conn pg.Conn) (pg.Version, error) {
	if f.unreachable[conn.MaintenanceDB()] {
		return pg.Version{}, &pg.ExitError{Binary: "psql", Code: 2,
			StderrTail: fmt.Sprintf("psql: error: connection to server failed: FATAL:  database %q does not exist\n", conn.MaintenanceDB())}
	}
	return f.server, nil
}

func (f *fakeDB) admin(conn pg.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adminDBs = append(f.adminDBs, conn.MaintenanceDB())
}
func (f *fakeDB) DatabaseExists(_ context.Context, _ pg.Conn, db string) (bool, error) {
	return f.exists[db], nil
}
func (f *fakeDB) UserObjectCount(_ context.Context, _ pg.Conn, db string) (int, error) {
	f.record("count %s", db)
	if n, ok := f.objects[db]; ok {
		return n, nil
	}
	return f.userObjects, nil
}
func (f *fakeDB) TerminateConnections(_ context.Context, conn pg.Conn, db string) error {
	f.admin(conn)
	f.record("terminate %s", db)
	return nil
}
func (f *fakeDB) CreateDatabase(_ context.Context, conn pg.Conn, db, owner string) error {
	f.admin(conn)
	f.record("create %s owner=%q", db, owner)
	if f.createErr != nil {
		return f.createErr
	}
	f.exists[db] = true
	return nil
}
func (f *fakeDB) DropDatabase(_ context.Context, conn pg.Conn, db string, force bool) error {
	f.admin(conn)
	f.record("drop %s force=%v", db, force)
	delete(f.exists, db)
	return nil
}
func (f *fakeDB) Restore(_ context.Context, _ pg.Conn, opts pg.RestoreOptions, stdin io.Reader) (pg.RestoreResult, error) {
	f.record("restore %s %s", opts.Database, strings.Join(restoreFlags(opts), " "))
	b, err := io.ReadAll(stdin)
	f.restored = b
	if err != nil {
		return pg.RestoreResult{}, &pg.ExitError{Binary: "pg_restore", Code: 1, StderrTail: "pg_restore: error: unexpected end of file\n"}
	}
	return f.restoreResult, f.restoreErr
}
func (f *fakeDB) RestoreFile(_ context.Context, _ pg.Conn, opts pg.RestoreOptions, path string, jobs int) (pg.RestoreResult, error) {
	f.record("restorefile %s jobs=%d", opts.Database, jobs)
	b, err := readFile(path)
	if err != nil {
		return pg.RestoreResult{}, err
	}
	f.restoredFile = b
	return f.restoreResult, f.restoreErr
}
func (f *fakeDB) ListArchive(_ context.Context, stdin io.Reader) (string, error) {
	f.record("list")
	io.Copy(io.Discard, io.LimitReader(stdin, 100)) // like pg_restore --list: reads only the head
	if f.listErr != nil {
		return "", f.listErr
	}
	if f.tocOverride != "" {
		return f.tocOverride, nil
	}
	return ";\n; Archive created at 2026-10-22\n;\n1; 1259 TABLE public a owner\n2; 1259 TABLE public b owner\n", nil
}
func (f *fakeDB) RunSQLScript(_ context.Context, conn pg.Conn, db string, script io.Reader) (int, error) {
	f.admin(conn)
	f.record("script %s", db)
	b, err := io.ReadAll(script)
	f.script = b
	if err != nil {
		return 0, err
	}
	return f.scriptErrors, nil
}
func (f *fakeDB) AnalyzeDatabase(_ context.Context, _ pg.Conn, db string) error {
	f.record("analyze %s", db)
	return nil
}

func (f *fakeDB) callList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "; ")
}

type harness struct {
	deps  Deps
	db    *fakeDB
	store *store.MemStore
	key   *age.X25519Identity
	logs  strings.Builder
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{db: newFakeDB(), store: store.NewMem(), key: key}
	h.deps = Deps{
		Logger:     slog.New(slog.NewTextHandler(&h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		DB:         h.db,
		Store:      h.store,
		Conn:       pg.Conn{Host: "db", User: "test"},
		Identities: []age.Identity{key},
		Prefix:     "backups",
		Cluster:    "unit",
		Now:        func() time.Time { return fixedTime },
		DiskFree:   func(string) (uint64, error) { return 1 << 40, nil },
	}
	return h
}

func testBytes(seed uint64, n int) []byte {
	rng := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

// encryptTo encrypts plain (zstd-compressed first when compress is set).
func (h *harness) encryptTo(t *testing.T, plain []byte, compress bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := crypt.NewEncryptor(&buf, []age.Recipient{h.key.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	var w io.Writer = enc
	var zw *zstd.Encoder
	if compress {
		zw, _ = zstd.NewWriter(enc)
		w = zw
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if zw != nil {
		zw.Close()
	}
	enc.Close()
	return buf.Bytes()
}

// seedDump stores an encrypted dump plus manifest and returns the entry.
func (h *harness) seedDump(t *testing.T, db string, plain []byte, at time.Time, globalsKey string) manifest.Entry {
	t.Helper()
	ct := h.encryptTo(t, plain, false)
	sum := sha256.Sum256(ct)
	m := manifest.Manifest{
		FormatVersion: 1, ToolVersion: "t", Cluster: "unit", Database: db, Kind: manifest.KindDump,
		Key: manifest.DumpKey("backups", "unit", db, at), CreatedAt: at, ServerVersion: "18.6", PgDumpVersion: "18.6",
		DumpFormat: "custom", Compression: "zstd:3", SizeBytes: int64(len(ct)), SHA256: hex.EncodeToString(sum[:]),
		Encryption: manifest.Encryption{Type: "age", Recipients: []string{h.key.Recipient().String()}}, GlobalsKey: globalsKey,
	}
	return h.storeEntry(t, m, ct)
}

func (h *harness) seedGlobals(t *testing.T, sql string, at time.Time) manifest.Entry {
	t.Helper()
	ct := h.encryptTo(t, []byte(sql), true)
	sum := sha256.Sum256(ct)
	m := manifest.Manifest{
		FormatVersion: 1, ToolVersion: "t", Cluster: "unit", Kind: manifest.KindGlobals,
		Key: manifest.GlobalsKey("backups", "unit", at), CreatedAt: at, ServerVersion: "18.6", PgDumpVersion: "18.6",
		DumpFormat: "plain", Compression: "zstd", SizeBytes: int64(len(ct)), SHA256: hex.EncodeToString(sum[:]),
		Encryption: manifest.Encryption{Type: "age", Recipients: []string{h.key.Recipient().String()}},
	}
	return h.storeEntry(t, m, ct)
}

func (h *harness) storeEntry(t *testing.T, m manifest.Manifest, ct []byte) manifest.Entry {
	t.Helper()
	ctx := context.Background()
	if _, err := h.store.Put(ctx, m.Key, bytes.NewReader(ct), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Put(ctx, manifest.ManifestKeyFor(m.Key), bytes.NewReader(b), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	info, _ := h.store.Stat(ctx, m.Key)
	return manifest.Entry{Manifest: m, ManifestKey: manifest.ManifestKeyFor(m.Key), Object: info, ObjectFound: true}
}

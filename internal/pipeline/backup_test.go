package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"

	"github.com/rapalon/pg-backup-s3/internal/crypt"
	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pg"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

func decryptStored(t *testing.T, h *harness, key string) []byte {
	t.Helper()
	r, err := crypt.NewDecryptor(bytes.NewReader(h.store.Bytes(key)), []age.Identity{h.key})
	if err != nil {
		t.Fatalf("NewDecryptor(%s): %v", key, err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decrypt %s: %v", key, err)
	}
	return b
}

func storedManifest(t *testing.T, h *harness, objectKey string) manifest.Manifest {
	t.Helper()
	b := h.store.Bytes(manifest.ManifestKeyFor(objectKey))
	if b == nil {
		t.Fatalf("manifest for %s not stored", objectKey)
	}
	m, err := manifest.Unmarshal(b)
	if err != nil {
		t.Fatalf("stored manifest: %v", err)
	}
	return m
}

func TestBackupDatabaseSuccess(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	info := h.info()
	info.GlobalsKey = "backups/unit/globals/2026/10/globals_20261022T030000Z.sql.zst.age"

	m, err := BackupDatabase(context.Background(), h.deps, "alpha", info)
	if err != nil {
		t.Fatalf("BackupDatabase: %v", err)
	}
	wantKey := "backups/unit/alpha/2026/10/alpha_20261022T030000Z.dump.age"
	if m.Key != wantKey {
		t.Errorf("key = %s", m.Key)
	}
	if got := h.store.Keys(); len(got) != 2 || got[0] != wantKey || got[1] != manifest.ManifestKeyFor(wantKey) {
		t.Fatalf("stored keys = %v", got)
	}

	stored := h.store.Bytes(wantKey)
	sum := sha256.Sum256(stored)
	if m.SHA256 != hex.EncodeToString(sum[:]) || m.SizeBytes != int64(len(stored)) {
		t.Errorf("manifest hash/size do not describe the stored object")
	}
	if plain := decryptStored(t, h, wantKey); !bytes.Equal(plain, h.db.dumps["alpha"]) {
		t.Error("decrypted object differs from pg_dump output")
	}

	sm := storedManifest(t, h, wantKey)
	if sm.Database != "alpha" || sm.Kind != manifest.KindDump || sm.Cluster != "unit" || sm.GlobalsKey != info.GlobalsKey ||
		sm.Compression != "zstd:3" || sm.DumpFormat != "custom" || sm.ServerVersion != "18.6" || sm.ToolVersion != "test" ||
		!sm.CreatedAt.Equal(fixedTime) || sm.DurationSeconds <= 0 || sm.Encryption.Recipients[0] != h.key.Recipient().String() {
		t.Errorf("manifest = %+v", sm)
	}
	stat, _ := h.store.Stat(context.Background(), wantKey)
	if stat.Metadata["pgbackup-created-at"] != "20261022T030000Z" || stat.Metadata["pgbackup-tool-version"] != "test" {
		t.Errorf("object metadata = %v", stat.Metadata)
	}
}

func TestBackupDatabaseDumpFailsLeavesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.db.waitErr["alpha"] = &pg.ExitError{Binary: "pg_dump", Code: 1, StderrTail: "pg_dump: error: connection lost\n"}

	_, err := BackupDatabase(context.Background(), h.deps, "alpha", h.info())
	var exitErr *pg.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("err = %v, want *pg.ExitError", err)
	}
	if IsTransient(err) {
		t.Error("pg_dump failure classified as transient")
	}
	if keys := h.store.Keys(); len(keys) != 0 {
		t.Errorf("objects left behind: %v", keys)
	}
}

func TestBackupDatabaseDumpStartFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.db.startErr["alpha"] = pg.ErrCompressionUnsupported
	_, err := BackupDatabase(context.Background(), h.deps, "alpha", h.info())
	if !errors.Is(err, pg.ErrCompressionUnsupported) || IsTransient(err) {
		t.Errorf("err = %v", err)
	}
	if len(h.store.Keys()) != 0 {
		t.Error("objects left behind")
	}
}

func TestBackupDatabasePutFailsIsTransientAndClean(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.store.FailPutAfterBytes = 100 << 10

	_, err := BackupDatabase(context.Background(), h.deps, "alpha", h.info())
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("err = %v, want the store's failure", err)
	}
	var exitErr *pg.ExitError
	if errors.As(err, &exitErr) {
		t.Error("store failure misreported as a pg_dump failure")
	}
	if !IsTransient(err) {
		t.Error("store failure should be transient")
	}
	if len(h.store.Keys()) != 0 {
		t.Error("objects left behind")
	}
}

func TestBackupDatabaseStatMismatchIsIntegrityError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.store.StatSizeOffset = 1

	_, err := BackupDatabase(context.Background(), h.deps, "gamma", h.info())
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
	if IsTransient(err) {
		t.Error("integrity failure must not be retried")
	}
	if keys := h.store.Keys(); len(keys) != 0 {
		t.Errorf("object with wrong size not removed, manifest must be absent: %v", keys)
	}
}

func TestBackupDatabaseCancelled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := BackupDatabase(ctx, h.deps, "alpha", h.info())
	if err == nil {
		t.Fatal("cancelled backup succeeded")
	}
	if len(h.store.Keys()) != 0 {
		t.Error("objects left behind")
	}
}

func TestBackupGlobals(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	m, err := BackupGlobals(context.Background(), h.deps, h.info())
	if err != nil {
		t.Fatalf("BackupGlobals: %v", err)
	}
	wantKey := "backups/unit/globals/2026/10/globals_20261022T030000Z.sql.zst.age"
	if m.Key != wantKey || m.Kind != manifest.KindGlobals || m.Database != "" || m.DumpFormat != "plain" || m.Compression != "zstd" {
		t.Errorf("manifest = %+v", m)
	}
	compressed := decryptStored(t, h, wantKey)
	zr, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	defer zr.Close()
	plain, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(plain, h.db.globals) {
		t.Errorf("globals round trip failed: %v", err)
	}
	if sm := storedManifest(t, h, wantKey); sm.SHA256 != m.SHA256 {
		t.Error("stored manifest differs")
	}
}

func TestHashReader(t *testing.T) {
	t.Parallel()
	data := testBytes(9, 100_000)
	hr := NewHashReader(bytes.NewReader(data))
	n, err := io.Copy(io.Discard, hr)
	if err != nil || n != int64(len(data)) || hr.Count() != n {
		t.Fatalf("copy: n=%d count=%d err=%v", n, hr.Count(), err)
	}
	sum := sha256.Sum256(data)
	if hr.Sum() != hex.EncodeToString(sum[:]) {
		t.Error("hash mismatch")
	}
}

func TestStoreInterfaceSatisfied(t *testing.T) {
	t.Parallel()
	var _ store.ObjectStore = (*flakyStore)(nil)
	var _ Database = (*fakeDB)(nil)
	var _ Database = (*pg.Runner)(nil)
}

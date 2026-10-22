package restore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/config"
	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pg"
	"github.com/rapalon/pg-backup-s3/internal/pipeline"
)

var ctx = context.Background()

func TestResolve(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	g := h.seedGlobals(t, "CREATE ROLE a;\n", fixedTime)
	old := h.seedDump(t, "app", testBytes(1, 1000), fixedTime.Add(-48*time.Hour), g.Manifest.Key)
	mid := h.seedDump(t, "app", testBytes(2, 1000), fixedTime.Add(-24*time.Hour), g.Manifest.Key)
	newest := h.seedDump(t, "app", testBytes(3, 1000), fixedTime, g.Manifest.Key)
	h.seedDump(t, "other", testBytes(4, 10), fixedTime, "")

	cases := []struct {
		name    string
		sel     Selector
		wantKey string
		wantErr string
		invalid bool
	}{
		{name: "by key", sel: Selector{Key: mid.Manifest.Key}, wantKey: mid.Manifest.Key},
		{name: "by manifest key", sel: Selector{Key: mid.ManifestKey}, wantKey: mid.Manifest.Key},
		{name: "latest", sel: Selector{Database: "app"}, wantKey: newest.Manifest.Key},
		{name: "at exact", sel: Selector{Database: "app", At: mid.Manifest.CreatedAt}, wantKey: mid.Manifest.Key},
		{name: "at between", sel: Selector{Database: "app", At: fixedTime.Add(-30 * time.Hour)}, wantKey: old.Manifest.Key},
		{name: "at before all", sel: Selector{Database: "app", At: fixedTime.Add(-100 * time.Hour)}, wantErr: "at or before"},
		{name: "unknown db", sel: Selector{Database: "nope"}, wantErr: "no backup of database"},
		{name: "key without manifest", sel: Selector{Key: "backups/unit/app/2026/10/app_20261001T000000Z.dump.age"}, wantErr: "no manifest"},
		{name: "globals by key", sel: Selector{Key: g.Manifest.Key}, wantKey: g.Manifest.Key},
		{name: "both", sel: Selector{Key: "k", Database: "d"}, invalid: true},
		{name: "neither", sel: Selector{}, invalid: true},
		{name: "key with at", sel: Selector{Key: "k", At: fixedTime}, invalid: true},
	}
	for _, c := range cases {
		e, err := Resolve(ctx, h.store, "backups", "unit", c.sel)
		switch {
		case c.invalid:
			if !errors.Is(err, config.ErrInvalid) {
				t.Errorf("%s: err = %v, want ErrInvalid", c.name, err)
			}
		case c.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.wantErr)
			}
		default:
			if err != nil || e.Manifest.Key != c.wantKey || !e.ObjectFound {
				t.Errorf("%s: got %s (%v), want %s", c.name, e.Manifest.Key, err, c.wantKey)
			}
		}
	}

	// A manifest whose object vanished is not restorable.
	h.store.Delete(ctx, []string{newest.Manifest.Key})
	if _, err := Resolve(ctx, h.store, "backups", "unit", Selector{Database: "app"}); err == nil || !strings.Contains(err.Error(), "object is missing") {
		t.Errorf("dangling latest: %v", err)
	}
	if _, err := Resolve(ctx, h.store, "backups", "unit", Selector{Key: newest.Manifest.Key}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("dangling by key: %v", err)
	}
}

func TestPreflightRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		opts     Options
		setup    func(h *harness)
		refused  bool // else config.ErrInvalid
		contains string
	}{
		{name: "missing without create", opts: Options{TargetDB: "missing"}, refused: true, contains: "add --create"},
		{name: "non-empty without clean", opts: Options{TargetDB: "full"}, refused: true, contains: "not empty (12 relations)"},
		{name: "drop without yes", opts: Options{TargetDB: "full", DropExisting: true}, refused: true, contains: "add --yes"},
		{name: "drop without yes even dry run", opts: Options{TargetDB: "full", DropExisting: true, DryRun: true}, refused: true, contains: "add --yes"},
		{name: "jobs without spool", opts: Options{TargetDB: "empty", Jobs: 4}, refused: true, contains: "needs --spool-dir"},
		{name: "spool not a dir", opts: Options{TargetDB: "empty", Jobs: 4, SpoolDir: "/definitely/not/here"}, refused: true, contains: "not a directory"},
		{name: "spool too small", opts: Options{TargetDB: "empty", Jobs: 2, SpoolDir: os.TempDir()},
			setup: func(h *harness) { h.deps.DiskFree = func(string) (uint64, error) { return 10, nil } }, refused: true, contains: "free"},
		{name: "old pg_restore", opts: Options{TargetDB: "empty"},
			setup: func(h *harness) { h.db.client = pg.Version{Major: 16, Minor: 9} }, refused: true, contains: "cannot read an archive written by pg_dump 18.6"},
		{name: "no globals object", opts: Options{TargetDB: "empty", Globals: true}, refused: true, contains: "no globals object"},
		{name: "schema and data only", opts: Options{TargetDB: "empty", SchemaOnly: true, DataOnly: true}, contains: "mutually exclusive"},
		{name: "clean and drop", opts: Options{TargetDB: "empty", Clean: true, DropExisting: true, Yes: true}, contains: "mutually exclusive"},
		{name: "jobs and single transaction", opts: Options{TargetDB: "empty", Jobs: 2, SpoolDir: os.TempDir(), SingleTransaction: true}, contains: "--single-transaction"},
		{name: "no identities", opts: Options{TargetDB: "empty"}, setup: func(h *harness) { h.deps.Identities = nil }, contains: "AGE_IDENTITY"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if c.setup != nil {
				c.setup(h)
			}
			entry := h.seedDump(t, "app", testBytes(1, 100), fixedTime, "")
			_, err := Preflight(ctx, h.deps, entry, c.opts)
			if err == nil {
				t.Fatal("Preflight succeeded")
			}
			if c.refused && !errors.Is(err, ErrRefused) {
				t.Errorf("err = %v, want ErrRefused", err)
			}
			if !c.refused && !errors.Is(err, config.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), c.contains) {
				t.Errorf("err %q lacks %q", err, c.contains)
			}
			if len(h.db.calls) > 0 && strings.Contains(h.db.callList(), "restore") {
				t.Error("preflight must not restore anything")
			}
		})
	}

	// A globals manifest cannot be restored as a database.
	h := newHarness(t)
	g := h.seedGlobals(t, "x", fixedTime)
	if _, err := Preflight(ctx, h.deps, g, Options{}); !errors.Is(err, config.ErrInvalid) {
		t.Errorf("globals entry: %v", err)
	}
}

func TestPreflightPlans(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	g := h.seedGlobals(t, "CREATE ROLE r;\n", fixedTime)
	entry := h.seedDump(t, "app", testBytes(1, 100), fixedTime, g.Manifest.Key)

	plan, err := Preflight(ctx, h.deps, entry, Options{Create: true, Analyze: true, NoOwner: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if plan.TargetDB != "app" || !plan.CreateDB || plan.DropDB || plan.Jobs != 1 || !plan.Analyze || !plan.Restore.NoOwner || !plan.Restore.ExitOnError {
		t.Errorf("plan = %+v", plan)
	}
	if len(plan.Actions) != 3 || !strings.HasPrefix(plan.Actions[0], "create database") || !strings.Contains(plan.Actions[1], "pg_restore") || !strings.Contains(plan.Actions[2], "vacuumdb") {
		t.Errorf("actions = %v", plan.Actions)
	}
	desc := plan.Describe()
	for _, want := range []string{entry.Manifest.Key, "pg_dump 18.6", `database "app"`, "1. create database", "2. pg_restore --no-owner --exit-on-error"} {
		if !strings.Contains(desc, want) {
			t.Errorf("Describe lacks %q:\n%s", want, desc)
		}
	}

	plan, err = Preflight(ctx, h.deps, entry, Options{TargetDB: "full", Clean: true, IgnoreErrors: true})
	if err != nil || plan.CreateDB || !plan.Restore.Clean || plan.Restore.ExitOnError {
		t.Errorf("clean: %+v %v", plan, err)
	}

	plan, err = Preflight(ctx, h.deps, entry, Options{TargetDB: "full", DropExisting: true, Yes: true, Owner: "app_owner", Globals: true})
	if err != nil || !plan.DropDB || !plan.DropForce || !plan.CreateDB || plan.Owner != "app_owner" || !plan.Globals || plan.GlobalsKey != g.Manifest.Key {
		t.Errorf("drop: %+v %v", plan, err)
	}
	if !strings.HasPrefix(plan.Actions[0], "restore roles") || !strings.Contains(plan.Actions[1], "drop database") {
		t.Errorf("drop actions = %v", plan.Actions)
	}

	h.db.server = pg.Version{Major: 12, Minor: 20}
	plan, err = Preflight(ctx, h.deps, entry, Options{TargetDB: "full", DropExisting: true, Yes: true})
	if err != nil || plan.DropForce || len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "older than the source server") {
		t.Errorf("old server: %+v %v", plan, err)
	}
	if !strings.Contains(plan.Describe(), "Warning:") {
		t.Error("Describe omits warnings")
	}

	dir := t.TempDir()
	plan, err = Preflight(ctx, h.deps, entry, Options{TargetDB: "empty", Jobs: 4, SpoolDir: dir, KeepSpool: true})
	if err != nil || plan.Jobs != 4 || plan.SpoolDir != dir || !plan.KeepSpool || !strings.Contains(plan.Actions[0], "4 jobs") {
		t.Errorf("spool: %+v %v", plan, err)
	}
	if !strings.Contains(h.logs.String(), "older than the source server") {
		t.Error("warning not logged")
	}
}

func TestExecuteStreamed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	plain := testBytes(7, 300<<10) // several age chunks
	entry := h.seedDump(t, "app", plain, fixedTime, "")
	plan, err := Preflight(ctx, h.deps, entry, Options{TargetDB: "dst", Create: true, Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Execute(ctx, h.deps, plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !bytes.Equal(h.db.restored, plain) {
		t.Error("pg_restore did not receive the decrypted archive")
	}
	if rep.Bytes != entry.Manifest.SizeBytes || rep.SHA256 != entry.Manifest.SHA256 || rep.TargetDB != "dst" || !rep.Analyzed {
		t.Errorf("report = %+v", rep)
	}
	if got := h.db.callList(); got != `create dst owner=""; restore dst --exit-on-error; analyze dst` {
		t.Errorf("calls = %s", got)
	}
}

func TestExecuteDropExisting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	entry := h.seedDump(t, "app", testBytes(1, 1000), fixedTime, "")
	plan, err := Preflight(ctx, h.deps, entry, Options{TargetDB: "full", DropExisting: true, Yes: true, Owner: "o"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(ctx, h.deps, plan); err != nil {
		t.Fatal(err)
	}
	if got := h.db.callList(); got != `terminate full; drop full force=true; create full owner="o"; restore full --exit-on-error` {
		t.Errorf("calls = %s", got)
	}
}

func TestExecuteCorruptObjectIsIntegrityError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	entry := h.seedDump(t, "app", testBytes(1, 200<<10), fixedTime, "")
	h.store.Corrupt(entry.Manifest.Key, int(entry.Manifest.SizeBytes)-50)
	plan, _ := Preflight(ctx, h.deps, entry, Options{TargetDB: "empty"})
	_, err := Execute(ctx, h.deps, plan)
	if !errors.Is(err, pipeline.ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
	if !strings.Contains(err.Error(), "decrypting") {
		t.Errorf("err = %v, want a decryption failure", err)
	}
	if len(h.db.restored) >= 200<<10 {
		t.Error("full plaintext reached pg_restore despite corruption")
	}
}

func TestExecuteHashMismatchAfterRestore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	entry := h.seedDump(t, "app", testBytes(1, 1000), fixedTime, "")
	entry.Manifest.SHA256 = strings.Repeat("f", 64) // manifest lies; the object itself is intact
	plan, _ := Preflight(ctx, h.deps, entry, Options{TargetDB: "empty"})
	_, err := Execute(ctx, h.deps, plan)
	if !errors.Is(err, pipeline.ErrIntegrity) || !strings.Contains(err.Error(), "manifest says") {
		t.Fatalf("err = %v", err)
	}
	if len(h.db.restored) != 1000 {
		t.Error("restore should have completed before the mismatch was reported")
	}
}

func TestExecuteRestoreFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	entry := h.seedDump(t, "app", testBytes(1, 1000), fixedTime, "")
	h.db.restoreErr = &pg.ExitError{Binary: "pg_restore", Code: 1, StderrTail: "pg_restore: error: relation exists\n"}
	plan, _ := Preflight(ctx, h.deps, entry, Options{TargetDB: "empty"})
	_, err := Execute(ctx, h.deps, plan)
	var ee *pg.ExitError
	if !errors.As(err, &ee) || errors.Is(err, pipeline.ErrIntegrity) {
		t.Errorf("err = %v, want the pg_restore exit error", err)
	}

	h.db.restoreErr = nil
	h.db.restoreResult = pg.RestoreResult{IgnoredErrors: 4}
	plan, _ = Preflight(ctx, h.deps, entry, Options{TargetDB: "empty", IgnoreErrors: true})
	rep, err := Execute(ctx, h.deps, plan)
	if err != nil || rep.IgnoredErrors != 4 {
		t.Errorf("ignore-errors: %+v %v", rep, err)
	}
}

func TestExecuteSpooled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	plain := testBytes(9, 150<<10)
	entry := h.seedDump(t, "app", plain, fixedTime, "")
	dir := t.TempDir()

	plan, err := Preflight(ctx, h.deps, entry, Options{TargetDB: "empty", Jobs: 4, SpoolDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Execute(ctx, h.deps, plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !bytes.Equal(h.db.restoredFile, plain) {
		t.Error("spooled file content differs")
	}
	if !strings.Contains(h.db.callList(), "restorefile empty jobs=4") || len(h.db.restored) != 0 {
		t.Errorf("calls = %s", h.db.callList())
	}
	if _, err := os.Stat(rep.SpoolPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("spool file not removed: %v", err)
	}

	// --keep-spool keeps it.
	plan, _ = Preflight(ctx, h.deps, entry, Options{TargetDB: "empty", Jobs: 2, SpoolDir: dir, KeepSpool: true})
	rep, err = Execute(ctx, h.deps, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rep.SpoolPath); err != nil || filepath.Dir(rep.SpoolPath) != dir {
		t.Errorf("kept spool: %s %v", rep.SpoolPath, err)
	}

	// Corruption is caught before pg_restore ever runs, and the spool is removed.
	h2 := newHarness(t)
	entry2 := h2.seedDump(t, "app", plain, fixedTime, "")
	h2.store.Corrupt(entry2.Manifest.Key, 5000)
	plan, _ = Preflight(ctx, h2.deps, entry2, Options{TargetDB: "empty", Jobs: 2, SpoolDir: dir})
	rep, err = Execute(ctx, h2.deps, plan)
	if !errors.Is(err, pipeline.ErrIntegrity) {
		t.Errorf("corrupt spool: %v", err)
	}
	if strings.Contains(h2.db.callList(), "restorefile") {
		t.Error("pg_restore ran on a corrupt spool")
	}
	if _, err := os.Stat(rep.SpoolPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("corrupt spool file left behind")
	}
}

func TestExecuteGlobals(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sql := "CREATE ROLE app;\nALTER ROLE app WITH LOGIN;\n"
	g := h.seedGlobals(t, sql, fixedTime)
	entry := h.seedDump(t, "app", testBytes(1, 500), fixedTime, g.Manifest.Key)
	h.db.scriptErrors = 2
	plan, err := Preflight(ctx, h.deps, entry, Options{TargetDB: "empty", Globals: true})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Execute(ctx, h.deps, plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(h.db.script) != sql || rep.GlobalsErrors != 2 {
		t.Errorf("script = %q, errors = %d", h.db.script, rep.GlobalsErrors)
	}
	if !strings.Contains(h.db.callList(), "script postgres; restore empty") {
		t.Errorf("globals must run first: %s", h.db.callList())
	}

	// A corrupt globals object is an integrity failure before the dump is touched.
	h.store.Corrupt(g.Manifest.Key, 40)
	_, err = Execute(ctx, h.deps, plan)
	if !errors.Is(err, pipeline.ErrIntegrity) {
		t.Errorf("corrupt globals: %v", err)
	}
}

func TestExecuteCreateFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	entry := h.seedDump(t, "app", testBytes(1, 500), fixedTime, "")
	h.db.createErr = errors.New("permission denied to create database")
	plan, _ := Preflight(ctx, h.deps, entry, Options{TargetDB: "newdb", Create: true})
	if _, err := Execute(ctx, h.deps, plan); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(h.db.callList(), "restore") {
		t.Error("restore attempted after create failed")
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	entry := h.seedDump(t, "app", testBytes(3, 200<<10), fixedTime, "")

	rep, err := Verify(ctx, h.deps, entry, false)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.Bytes != entry.Manifest.SizeBytes || rep.SHA256 != entry.Manifest.SHA256 || rep.TOCEntries != 2 || rep.Kind != manifest.KindDump {
		t.Errorf("report = %+v", rep)
	}
	if strings.Contains(h.db.callList(), "create") {
		t.Error("plain verify touched the server")
	}

	// Corruption anywhere in the stream, including after the TOC, is caught.
	h.store.Corrupt(entry.Manifest.Key, int(entry.Manifest.SizeBytes)-10)
	if _, err := Verify(ctx, h.deps, entry, false); !errors.Is(err, pipeline.ErrIntegrity) {
		t.Errorf("corrupt tail: %v", err)
	}

	// Globals objects are decrypted and decompressed.
	g := h.seedGlobals(t, "CREATE ROLE x;\n", fixedTime)
	rep, err = Verify(ctx, h.deps, g, false)
	if err != nil || rep.Kind != manifest.KindGlobals || rep.TOCEntries != 0 || rep.Bytes != g.Manifest.SizeBytes {
		t.Errorf("globals verify: %+v %v", rep, err)
	}
	h.store.Corrupt(g.Manifest.Key, 30)
	if _, err := Verify(ctx, h.deps, g, false); !errors.Is(err, pipeline.ErrIntegrity) {
		t.Errorf("corrupt globals: %v", err)
	}

	// Lying manifest.
	entry2 := h.seedDump(t, "app", testBytes(4, 100), fixedTime.Add(time.Hour), "")
	entry2.Manifest.SizeBytes++
	if _, err := Verify(ctx, h.deps, entry2, false); !errors.Is(err, pipeline.ErrIntegrity) {
		t.Errorf("size mismatch: %v", err)
	}
	h.deps.Identities = nil
	if _, err := Verify(ctx, h.deps, entry2, false); err == nil {
		t.Error("verify without identities succeeded")
	}
}

func TestVerifyFull(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.db.userObjects = 5
	entry := h.seedDump(t, "app", testBytes(3, 50<<10), fixedTime, "")
	rep, err := Verify(ctx, h.deps, entry, true)
	if err != nil {
		t.Fatalf("Verify --full: %v", err)
	}
	want := "verify_app_20261022T030000Z"
	if rep.ScratchDB != want || rep.Relations != 5 {
		t.Errorf("report = %+v", rep)
	}
	if got := h.db.callList(); got != `list; create `+want+` owner=""; restore `+want+` --no-owner --no-privileges --exit-on-error; count `+want+`; drop `+want+` force=true` {
		t.Errorf("calls = %s", got)
	}

	// The scratch database is dropped even when the restore fails.
	h2 := newHarness(t)
	h2.db.restoreErr = &pg.ExitError{Binary: "pg_restore", Code: 1}
	entry2 := h2.seedDump(t, "app", testBytes(3, 1000), fixedTime, "")
	if _, err := Verify(ctx, h2.deps, entry2, true); err == nil {
		t.Error("full verify succeeded despite restore failure")
	}
	if !strings.Contains(h2.db.callList(), "drop verify_app_") {
		t.Errorf("scratch database not dropped: %s", h2.db.callList())
	}

	// No relations after the restore is an integrity failure.
	h3 := newHarness(t)
	h3.db.userObjects = 0
	entry3 := h3.seedDump(t, "app", testBytes(3, 1000), fixedTime, "")
	if _, err := Verify(ctx, h3.deps, entry3, true); !errors.Is(err, pipeline.ErrIntegrity) {
		t.Errorf("empty scratch: %v", err)
	}
}

func TestScratchName(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("d", 100)
	name := scratchName(long, fixedTime)
	if len(name) > 63 || !strings.HasPrefix(name, "verify_dddd") || !strings.HasSuffix(name, "_20261022T030000Z") {
		t.Errorf("scratchName = %q (%d)", name, len(name))
	}
	if scratchName("app", fixedTime) != "verify_app_20261022T030000Z" {
		t.Error("short name")
	}
}

func TestCountTOCEntries(t *testing.T) {
	t.Parallel()
	if n := countTOCEntries(";\n; Archive\n;\n1; a\n\n2; b\n"); n != 2 {
		t.Errorf("n = %d", n)
	}
	if countTOCEntries("") != 0 {
		t.Error("empty")
	}
	toc := "; TABLE comment\n1; 1259 TABLE public a owner\n2; 0 TABLE DATA public a owner\n3; 1259 SEQUENCE public s owner\n4; 1259 VIEW public v owner\n5; 1259 INDEX public i owner\n"
	if n := countTOCRelations(toc); n != 3 {
		t.Errorf("relations = %d, want 3", n)
	}
}

func TestVerifyFullEmptyDatabaseIsFine(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.db.userObjects = 0
	h.db.listErr = nil
	// Make the fake TOC list no relations at all.
	entry := h.seedDump(t, "app", testBytes(3, 1000), fixedTime, "")
	h.db.tocOverride = ";\n; Archive created at 2026-10-22\n;\n1; 0 ENCODING - ENCODING\n2; 0 STDSTRINGS - STDSTRINGS\n"
	rep, err := Verify(ctx, h.deps, entry, true)
	if err != nil || rep.Relations != 0 {
		t.Errorf("empty-database full verify: %+v %v", rep, err)
	}
}

func TestErrRecorder(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	rec := &errRecorder{r: failAfter{n: 3, err: boom}}
	buf := make([]byte, 10)
	rec.Read(buf)
	rec.Read(buf)
	if rec.err != boom {
		t.Errorf("recorded %v", rec.err)
	}
}

type failAfter struct {
	n   int
	err error
}

func (f failAfter) Read(p []byte) (int, error) {
	if f.n > 0 {
		return 0, f.err
	}
	return 0, nil
}

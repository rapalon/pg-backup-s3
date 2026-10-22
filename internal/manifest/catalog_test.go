package manifest

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/store"
)

// seed writes a dump object and its manifest for db at t.
func seed(t *testing.T, st *store.MemStore, prefix, cluster, db string, at time.Time, globalsKey string) Manifest {
	t.Helper()
	ctx := context.Background()
	m := Manifest{
		FormatVersion: FormatVersion, ToolVersion: "t", Cluster: cluster, Database: db, Kind: KindDump,
		Key: DumpKey(prefix, cluster, db, at), CreatedAt: at, DumpFormat: "custom", Compression: "zstd:3",
		SizeBytes: 3, SHA256: strings.Repeat("0", 64), Encryption: Encryption{Type: "age", Recipients: []string{"age1x"}},
		GlobalsKey: globalsKey,
	}
	if db == "" {
		m.Kind, m.Key, m.DumpFormat, m.Compression, m.GlobalsKey = KindGlobals, GlobalsKey(prefix, cluster, at), "plain", "zstd", ""
	}
	if _, err := st.Put(ctx, m.Key, bytes.NewReader([]byte("obj")), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, ManifestKeyFor(m.Key), bytes.NewReader(b), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLoadCatalog(t *testing.T) {
	t.Parallel()
	st := store.NewMem()
	ctx := context.Background()
	t0 := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	g := seed(t, st, "p", "c", "", t0, "")
	a1 := seed(t, st, "p", "c", "app", t0, g.Key)
	a2 := seed(t, st, "p", "c", "app", t0.Add(24*time.Hour), g.Key)
	b1 := seed(t, st, "p", "c", "billing", t0.Add(time.Hour), g.Key)
	// Another cluster must not leak in.
	seed(t, st, "p", "other", "app", t0, "")
	// Orphan: object without manifest.
	orphanKey := DumpKey("p", "c", "app", t0.Add(-48*time.Hour))
	st.Put(ctx, orphanKey, bytes.NewReader([]byte("x")), store.PutOptions{})
	// Dangling: manifest without object.
	dangling := seed(t, st, "p", "c", "app", t0.Add(-72*time.Hour), "")
	st.Delete(ctx, []string{dangling.Key})
	// Broken manifest and a manifest describing the wrong key.
	st.Put(ctx, ManifestKeyFor(DumpKey("p", "c", "app", t0.Add(-96*time.Hour))), bytes.NewReader([]byte("{not json")), store.PutOptions{})
	wrong := a1
	wrong.Key = a2.Key
	wb, _ := wrong.Marshal()
	st.Put(ctx, ManifestKeyFor(DumpKey("p", "c", "app", t0.Add(-120*time.Hour))), bytes.NewReader(wb), store.PutOptions{})
	// Foreign object.
	st.Put(ctx, "p/c/README.txt", bytes.NewReader([]byte("hi")), store.PutOptions{})

	cat, err := Load(ctx, st, "p", "c", "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cat.Entries) != 5 { // g, a1, a2, b1, dangling
		t.Fatalf("entries = %d: %+v", len(cat.Entries), cat.Entries)
	}
	if cat.Entries[0].Manifest.Key != a2.Key || cat.Entries[1].Manifest.Key != b1.Key {
		t.Errorf("not newest first: %s, %s", cat.Entries[0].Manifest.Key, cat.Entries[1].Manifest.Key)
	}
	if dbs := cat.Databases(); strings.Join(dbs, ",") != "app,billing" {
		t.Errorf("Databases = %v", dbs)
	}
	if got := cat.ByDatabase("app"); len(got) != 3 || got[0].Manifest.Key != a2.Key || got[2].Manifest.Key != dangling.Key || got[2].ObjectFound {
		t.Errorf("ByDatabase(app) = %+v", got)
	}
	if e, ok := cat.Latest("app"); !ok || e.Manifest.Key != a2.Key || !e.ObjectFound || e.Object.Size != 3 {
		t.Errorf("Latest(app) = %+v %v", e, ok)
	}
	if _, ok := cat.Latest("nope"); ok {
		t.Error("Latest(nope) found something")
	}
	if e, ok := cat.At("app", t0.Add(12*time.Hour)); !ok || e.Manifest.Key != a1.Key {
		t.Errorf("At(app, t0+12h) = %+v", e)
	}
	if e, ok := cat.At("app", t0.Add(-200*time.Hour)); ok {
		t.Errorf("At before everything = %+v", e)
	}
	if e, ok := cat.Find(b1.Key); !ok || e.Manifest.Database != "billing" {
		t.Errorf("Find = %+v", e)
	}
	if globals := cat.Globals(); len(globals) != 1 || globals[0].Manifest.Key != g.Key {
		t.Errorf("Globals = %+v", globals)
	}
	if len(cat.Orphans) != 1 || cat.Orphans[0].Object.Key != orphanKey || cat.Orphans[0].Info.Database != "app" {
		t.Errorf("Orphans = %+v", cat.Orphans)
	}
	if len(cat.Broken) != 2 {
		t.Errorf("Broken = %+v", cat.Broken)
	}
	if len(cat.Foreign) != 1 || cat.Foreign[0].Key != "p/c/README.txt" {
		t.Errorf("Foreign = %+v", cat.Foreign)
	}

	// Scoped to one database.
	cat, err = Load(ctx, st, "p", "c", "billing")
	if err != nil || len(cat.Entries) != 1 || len(cat.Orphans) != 0 {
		t.Errorf("Load(billing) = %d entries, %d orphans, %v", len(cat.Entries), len(cat.Orphans), err)
	}
}

func TestLoadCatalogErrors(t *testing.T) {
	t.Parallel()
	st := store.NewMem()
	st.FailList = context.DeadlineExceeded
	if _, err := Load(context.Background(), st, "p", "c", ""); err == nil {
		t.Error("list failure not propagated")
	}
	st.FailList = nil
	seed(t, st, "p", "c", "app", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "")
	st.FailGet = context.DeadlineExceeded
	cat, err := Load(context.Background(), st, "p", "c", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Entries) != 0 || len(cat.Broken) != 1 || len(cat.Orphans) != 0 {
		t.Errorf("unfetchable manifest: entries=%d broken=%d orphans=%d", len(cat.Entries), len(cat.Broken), len(cat.Orphans))
	}
}

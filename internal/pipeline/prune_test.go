package pipeline

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/retention"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

var pruneNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func put(t *testing.T, st *store.MemStore, key string, body string) {
	t.Helper()
	if _, err := st.Put(context.Background(), key, bytes.NewReader([]byte(body)), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func seedRun(t *testing.T, st *store.MemStore, at time.Time, dbs ...string) (globalsKey string) {
	t.Helper()
	g := manifest.Manifest{
		FormatVersion: manifest.FormatVersion, ToolVersion: "t", Cluster: "c", Kind: manifest.KindGlobals,
		Key: manifest.GlobalsKey("p", "c", at), CreatedAt: at, DumpFormat: "plain", Compression: "zstd",
		SizeBytes: 2, SHA256: strings.Repeat("1", 64), Encryption: manifest.Encryption{Type: "age", Passphrase: true},
	}
	put(t, st, g.Key, "gg")
	gb, _ := g.Marshal()
	put(t, st, manifest.ManifestKeyFor(g.Key), string(gb))
	for _, db := range dbs {
		m := manifest.Manifest{
			FormatVersion: manifest.FormatVersion, ToolVersion: "t", Cluster: "c", Database: db, Kind: manifest.KindDump,
			Key: manifest.DumpKey("p", "c", db, at), CreatedAt: at, DumpFormat: "custom", Compression: "zstd:3",
			SizeBytes: 5, SHA256: strings.Repeat("2", 64), Encryption: manifest.Encryption{Type: "age", Passphrase: true},
			GlobalsKey: g.Key,
		}
		put(t, st, m.Key, "ddddd")
		mb, _ := m.Marshal()
		put(t, st, manifest.ManifestKeyFor(m.Key), string(mb))
	}
	return g.Key
}

func TestPlanPrune(t *testing.T) {
	t.Parallel()
	st := store.NewMem()
	day := 24 * time.Hour
	// Ten daily runs for app and billing; billing missing on the last two days.
	for i := 0; i < 10; i++ {
		at := pruneNow.Add(-time.Duration(i)*day - 9*time.Hour)
		if i < 2 {
			seedRun(t, st, at, "app")
		} else {
			seedRun(t, st, at, "app", "billing")
		}
	}
	// Orphans: one old, one fresh. A dangling manifest, old. A fresh unreferenced globals.
	oldOrphan := manifest.DumpKey("p", "c", "app", pruneNow.Add(-30*day))
	put(t, st, oldOrphan, "o")
	freshOrphan := manifest.DumpKey("p", "c", "app", pruneNow.Add(-time.Hour))
	put(t, st, freshOrphan, "o")
	danglingAt := pruneNow.Add(-40 * day)
	seedRun(t, st, danglingAt, "app")
	st.Delete(context.Background(), []string{manifest.DumpKey("p", "c", "app", danglingAt)})
	freshGlobals := seedRun(t, st, pruneNow.Add(-30*time.Minute))

	cat, err := manifest.Load(context.Background(), st, "p", "c", "")
	if err != nil {
		t.Fatal(err)
	}
	plan := PlanPrune(cat, PruneOptions{Policy: retention.Policy{KeepLast: 3}, Now: func() time.Time { return pruneNow }})

	deleted := map[string]PruneAction{}
	for _, a := range plan.Delete {
		deleted[a.Key] = a
	}
	// app: 10 daily + dangling (excluded from retention) → keep 3, drop 7 dumps (+7 manifests).
	// billing: 8 → keep 3, drop 5. Dangling globals at day 40 kept? Its run's dump is gone, so
	// the globals object is unreferenced and old → deleted. Globals of dropped runs: days 3..9
	// for app are dropped, but billing keeps days 2,3,4 → globals of days 2,3,4 and 0,1 are kept.
	var dumps, manifests, globals, orphans int
	for _, a := range plan.Delete {
		switch a.Kind {
		case "dump":
			dumps++
		case "manifest":
			manifests++
		case "globals":
			globals++
		case "orphan":
			orphans++
		}
	}
	if dumps != 12 || orphans != 1 {
		t.Errorf("dumps=%d orphans=%d", dumps, orphans)
	}
	// Globals dropped: days 5..9 (5 runs) + the dangling run's globals = 6.
	if globals != 6 {
		t.Errorf("globals deleted = %d, want 6: %v", globals, keysOfKind(plan, "globals"))
	}
	// Manifests: 12 dumps + 6 globals + 1 dangling = 19.
	if manifests != 19 {
		t.Errorf("manifests deleted = %d, want 19", manifests)
	}
	if _, ok := deleted[oldOrphan]; !ok {
		t.Error("old orphan not deleted")
	}
	if _, ok := deleted[freshOrphan]; ok {
		t.Error("fresh orphan deleted")
	}
	if _, ok := deleted[freshGlobals]; ok {
		t.Error("fresh unreferenced globals deleted")
	}
	if _, ok := deleted[manifest.ManifestKeyFor(manifest.DumpKey("p", "c", "app", danglingAt))]; !ok {
		t.Error("old dangling manifest not deleted")
	}
	// Newest of each database kept with the "newest" reason.
	newestApp := manifest.DumpKey("p", "c", "app", pruneNow.Add(-9*time.Hour))
	if r := plan.Kept[newestApp]; len(r) == 0 || r[0] != "newest" {
		t.Errorf("newest app kept reasons = %v", r)
	}
	newestBilling := manifest.DumpKey("p", "c", "billing", pruneNow.Add(-2*day-9*time.Hour))
	if _, ok := plan.Kept[newestBilling]; !ok {
		t.Error("newest billing not kept")
	}
	if _, ok := deleted[newestBilling]; ok {
		t.Error("newest billing scheduled for deletion")
	}
	// Globals referenced by kept billing dumps (days 2..4) survive.
	for i := 0; i <= 4; i++ {
		if _, ok := deleted[manifest.GlobalsKey("p", "c", pruneNow.Add(-time.Duration(i)*day-9*time.Hour))]; ok {
			t.Errorf("globals of day %d deleted although referenced", i)
		}
	}
	if plan.Bytes() <= 0 {
		t.Error("Bytes() not computed")
	}
	// Plan is ordered oldest first.
	for i := 1; i < len(plan.Delete); i++ {
		if plan.Delete[i].Time.Before(plan.Delete[i-1].Time) {
			t.Fatal("plan not ordered by time")
		}
	}
}

func keysOfKind(p PrunePlan, kind string) []string {
	var out []string
	for _, a := range p.Delete {
		if a.Kind == kind {
			out = append(out, a.Key)
		}
	}
	return out
}

func TestPruneExecutesAndIsIdempotent(t *testing.T) {
	t.Parallel()
	st := store.NewMem()
	for i := 0; i < 5; i++ {
		seedRun(t, st, pruneNow.Add(-time.Duration(i)*24*time.Hour), "app")
	}
	before := len(st.Keys())
	opts := PruneOptions{Policy: retention.Policy{KeepLast: 2}, Now: func() time.Time { return pruneNow }}

	dry, err := Prune(context.Background(), nil, st, "p", "c", PruneOptions{Policy: opts.Policy, Now: opts.Now, DryRun: true})
	if err != nil || len(dry.Delete) == 0 || len(st.Keys()) != before {
		t.Fatalf("dry run: deleted=%d keys %d→%d err=%v", len(dry.Delete), before, len(st.Keys()), err)
	}

	plan, err := Prune(context.Background(), nil, st, "p", "c", opts)
	if err != nil {
		t.Fatal(err)
	}
	// 3 runs dropped: 3 dumps + 3 globals + 6 manifests = 12 keys.
	if len(plan.Delete) != 12 || len(st.Keys()) != before-12 {
		t.Errorf("deleted %d (plan), keys %d → %d", len(plan.Delete), before, len(st.Keys()))
	}
	again, err := Prune(context.Background(), nil, st, "p", "c", opts)
	if err != nil || len(again.Delete) != 0 {
		t.Errorf("second prune: %d deletions, %v", len(again.Delete), err)
	}
	// Nothing dangling or orphaned left behind.
	cat, _ := manifest.Load(context.Background(), st, "p", "c", "")
	for _, e := range cat.Entries {
		if !e.ObjectFound {
			t.Errorf("dangling manifest after prune: %s", e.ManifestKey)
		}
	}
	if len(cat.Orphans) != 0 {
		t.Errorf("orphans after prune: %v", cat.Orphans)
	}
}

func TestPruneEmptyAndErrors(t *testing.T) {
	t.Parallel()
	st := store.NewMem()
	plan, err := Prune(context.Background(), nil, st, "p", "empty", PruneOptions{Policy: retention.Policy{KeepLast: 1}})
	if err != nil || len(plan.Delete) != 0 || len(plan.Kept) != 0 {
		t.Errorf("empty prune: %+v %v", plan.Delete, err)
	}

	seedRun(t, st, pruneNow.Add(-48*time.Hour), "app")
	seedRun(t, st, pruneNow.Add(-24*time.Hour), "app")
	st.FailDelete = context.DeadlineExceeded
	_, err = Prune(context.Background(), nil, st, "p", "c", PruneOptions{Policy: retention.Policy{KeepLast: 1}, Now: func() time.Time { return pruneNow }})
	if err == nil {
		t.Error("delete failure not propagated")
	}
}

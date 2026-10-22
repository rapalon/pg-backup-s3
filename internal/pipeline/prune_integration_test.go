//go:build integration

package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/retention"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

func TestIntegrationPruneOnStand(t *testing.T) {
	if os.Getenv("PGBACKUP_INTEGRATION") != "1" {
		t.Skip("PGBACKUP_INTEGRATION != 1; run via test/compose.yml")
	}
	st, err := store.NewS3(store.S3Config{
		Endpoint: os.Getenv("S3_ENDPOINT"), Region: os.Getenv("S3_REGION"), Bucket: os.Getenv("S3_BUCKET"),
		AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"), PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	prefix := fmt.Sprintf("it-prune/%d", time.Now().UnixNano())
	const cluster = "stand"
	t.Cleanup(func() {
		infos, _ := st.List(ctx, prefix)
		keys := make([]string, len(infos))
		for i, o := range infos {
			keys[i] = o.Key
		}
		st.Delete(ctx, keys)
	})

	put := func(key, body string) {
		if _, err := st.Put(ctx, key, bytes.NewReader([]byte(body)), store.PutOptions{}); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}
	writeManifest := func(m manifest.Manifest) {
		b, err := m.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		put(manifest.ManifestKeyFor(m.Key), string(b))
	}

	// 40 manifests: 20 daily runs × (app dump + globals); billing every other day → 10 more.
	now := time.Now().UTC().Truncate(time.Second)
	var runTimes []time.Time
	for i := 0; i < 20; i++ {
		at := now.Add(-time.Duration(i) * 24 * time.Hour)
		runTimes = append(runTimes, at)
		g := manifest.Manifest{
			FormatVersion: 1, ToolVersion: "it", Cluster: cluster, Kind: manifest.KindGlobals,
			Key: manifest.GlobalsKey(prefix, cluster, at), CreatedAt: at, DumpFormat: "plain", Compression: "zstd",
			SizeBytes: 1, SHA256: strings.Repeat("a", 64), Encryption: manifest.Encryption{Type: "age", Passphrase: true},
		}
		put(g.Key, "g")
		writeManifest(g)
		dbs := []string{"app"}
		if i%2 == 0 {
			dbs = append(dbs, "billing")
		}
		for _, db := range dbs {
			m := manifest.Manifest{
				FormatVersion: 1, ToolVersion: "it", Cluster: cluster, Database: db, Kind: manifest.KindDump,
				Key: manifest.DumpKey(prefix, cluster, db, at), CreatedAt: at, DumpFormat: "custom", Compression: "zstd:3",
				SizeBytes: 4, SHA256: strings.Repeat("b", 64), Encryption: manifest.Encryption{Type: "age", Passphrase: true},
				GlobalsKey: g.Key,
			}
			put(m.Key, "dump")
			writeManifest(m)
		}
	}
	// An old orphan (object only) and a fresh one.
	put(manifest.DumpKey(prefix, cluster, "app", now.Add(-60*24*time.Hour)), "orphan")
	put(manifest.DumpKey(prefix, cluster, "app", now.Add(-10*time.Minute)), "orphan")

	cat, err := manifest.Load(ctx, st, prefix, cluster, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cat.Entries); n != 50 {
		t.Fatalf("seeded catalog has %d manifests, want 50", n)
	}
	if len(cat.Orphans) != 2 {
		t.Fatalf("orphans = %d", len(cat.Orphans))
	}

	policy := retention.Policy{KeepLast: 2, KeepDaily: 4, KeepWeekly: 2}
	plan, err := Prune(ctx, nil, st, prefix, cluster, PruneOptions{Policy: policy})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	t.Logf("deleted %d keys, %d bytes", len(plan.Delete), plan.Bytes())

	after, err := manifest.Load(ctx, st, prefix, cluster, "")
	if err != nil {
		t.Fatal(err)
	}
	// Expected survivors per database come from the pure policy on the seeded times.
	for _, db := range []string{"app", "billing"} {
		var items []retention.Item
		for i, at := range runTimes {
			if db == "billing" && i%2 != 0 {
				continue
			}
			items = append(items, retention.Item{Key: manifest.DumpKey(prefix, cluster, db, at), Time: at})
		}
		keep, _ := retention.Select(items, policy)
		got := after.ByDatabase(db)
		if len(got) != len(keep) {
			t.Errorf("%s: %d dumps survive, want %d", db, len(got), len(keep))
		}
		for _, k := range keep {
			if _, ok := after.Find(k.Key); !ok {
				t.Errorf("%s: kept dump %s is gone", db, k.Key)
			}
		}
		if got[0].Manifest.Key != manifest.DumpKey(prefix, cluster, db, items[0].Time) {
			t.Errorf("%s: newest dump not preserved", db)
		}
	}
	// Every surviving dump's globals still exist; no unreferenced old globals remain.
	referenced := map[string]bool{}
	for _, e := range after.Dumps() {
		referenced[e.Manifest.GlobalsKey] = true
		if _, ok := after.Find(e.Manifest.GlobalsKey); !ok {
			t.Errorf("globals %s referenced by %s is gone", e.Manifest.GlobalsKey, e.Manifest.Key)
		}
	}
	for i, g := range after.Globals() {
		if i > 0 && !referenced[g.Manifest.Key] {
			t.Errorf("unreferenced globals survived: %s", g.Manifest.Key)
		}
	}
	// Old orphan gone, fresh orphan kept; no dangling manifests.
	if len(after.Orphans) != 1 || !after.Orphans[0].Info.Time.After(now.Add(-time.Hour)) {
		t.Errorf("orphans after prune = %+v", after.Orphans)
	}
	for _, e := range after.Entries {
		if !e.ObjectFound {
			t.Errorf("dangling manifest after prune: %s", e.ManifestKey)
		}
	}
	// Second prune is a no-op.
	again, err := Prune(ctx, nil, st, prefix, cluster, PruneOptions{Policy: policy})
	if err != nil || len(again.Delete) != 0 {
		t.Errorf("second prune: %d deletions, %v", len(again.Delete), err)
	}
}

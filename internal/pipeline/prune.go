package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/retention"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

// DefaultOrphanAge is how old an unreferenced object must be before prune
// removes it; younger ones may belong to a run that is still in progress.
const DefaultOrphanAge = 24 * time.Hour

// PruneOptions control one prune.
type PruneOptions struct {
	Policy    retention.Policy
	OrphanAge time.Duration // DefaultOrphanAge when zero
	DryRun    bool
	Now       func() time.Time
}

// PruneAction is one key the plan deletes and why.
type PruneAction struct {
	Key      string
	Kind     string // "dump", "manifest", "globals" or "orphan"
	Reason   string
	Size     int64
	Database string
	Time     time.Time
}

// PrunePlan is what Prune would (or did) delete and what it keeps.
type PrunePlan struct {
	Catalog manifest.Catalog
	Delete  []PruneAction
	// Kept maps a kept dump's object key to the retention rules that kept it.
	Kept map[string][]string
}

// Bytes returns the total size of the objects to delete.
func (p PrunePlan) Bytes() int64 {
	var n int64
	for _, a := range p.Delete {
		n += a.Size
	}
	return n
}

// PlanPrune applies the retention policy to a catalog. It never selects the
// newest dump of a database, a globals object referenced by a surviving
// dump, or anything younger than OrphanAge that is not covered by a manifest.
func PlanPrune(cat manifest.Catalog, opts PruneOptions) PrunePlan {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	orphanAge := opts.OrphanAge
	if orphanAge <= 0 {
		orphanAge = DefaultOrphanAge
	}
	cutoff := now().Add(-orphanAge)

	plan := PrunePlan{Catalog: cat, Kept: map[string][]string{}}
	add := func(a PruneAction) { plan.Delete = append(plan.Delete, a) }

	// 1. Retention per database.
	referencedGlobals := map[string]bool{}
	for _, db := range cat.Databases() {
		entries := cat.ByDatabase(db)
		byKey := make(map[string]manifest.Entry, len(entries))
		items := make([]retention.Item, 0, len(entries))
		for _, e := range entries {
			if !e.ObjectFound {
				continue // dangling manifests are handled below
			}
			byKey[e.Manifest.Key] = e
			items = append(items, retention.Item{Key: e.Manifest.Key, Time: e.Manifest.CreatedAt})
		}
		keep, drop := retention.Select(items, opts.Policy)
		for _, k := range keep {
			plan.Kept[k.Key] = k.Reasons
			if g := byKey[k.Key].Manifest.GlobalsKey; g != "" {
				referencedGlobals[g] = true
			}
		}
		for _, d := range drop {
			e := byKey[d.Key]
			reason := "retention"
			add(PruneAction{Key: e.ManifestKey, Kind: "manifest", Reason: reason, Database: db, Time: e.Manifest.CreatedAt})
			add(PruneAction{Key: e.Manifest.Key, Kind: "dump", Reason: reason, Size: e.Object.Size, Database: db, Time: e.Manifest.CreatedAt})
		}
	}

	// 2. Globals: keep the newest, anything referenced by a kept dump, and
	//    anything young enough to belong to a run in progress.
	for i, e := range cat.Globals() {
		switch {
		case !e.ObjectFound:
			continue
		case i == 0, referencedGlobals[e.Manifest.Key], e.Manifest.CreatedAt.After(cutoff):
			continue
		}
		add(PruneAction{Key: e.ManifestKey, Kind: "manifest", Reason: "globals unreferenced", Time: e.Manifest.CreatedAt})
		add(PruneAction{Key: e.Manifest.Key, Kind: "globals", Reason: "globals unreferenced", Size: e.Object.Size, Time: e.Manifest.CreatedAt})
	}

	// 3. Dangling manifests (object gone) and orphans (manifest gone), old enough.
	for _, e := range cat.Entries {
		if !e.ObjectFound && e.Manifest.CreatedAt.Before(cutoff) {
			add(PruneAction{Key: e.ManifestKey, Kind: "manifest", Reason: "dangling: object missing", Database: e.Manifest.Database, Time: e.Manifest.CreatedAt})
		}
	}
	// The key timestamp is the run start, so "older than OrphanAge" means the
	// run that produced it cannot still be in progress.
	for _, o := range cat.Orphans {
		if o.Info.Time.Before(cutoff) {
			add(PruneAction{Key: o.Object.Key, Kind: "orphan", Reason: "orphan: no manifest", Size: o.Object.Size, Database: o.Info.Database, Time: o.Info.Time})
		}
	}

	sort.SliceStable(plan.Delete, func(i, j int) bool {
		if !plan.Delete[i].Time.Equal(plan.Delete[j].Time) {
			return plan.Delete[i].Time.Before(plan.Delete[j].Time)
		}
		return plan.Delete[i].Key < plan.Delete[j].Key
	})
	return plan
}

// Prune loads the catalog, plans, and (unless DryRun) deletes. Manifests are
// deleted before their objects so an interrupted prune leaves orphans, which
// the next prune cleans up, rather than manifests pointing at nothing.
func Prune(ctx context.Context, logger *slog.Logger, st store.ObjectStore, prefix, cluster string, opts PruneOptions) (PrunePlan, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	cat, err := manifest.Load(ctx, st, prefix, cluster, "")
	if err != nil {
		return PrunePlan{}, fmt.Errorf("prune: %w", err)
	}
	plan := PlanPrune(cat, opts)
	for _, b := range cat.Broken {
		logger.Warn("unreadable manifest left in place", "key", b.Key, "err", b.Err)
	}
	logger.Info("prune plan", "delete", len(plan.Delete), "bytes", plan.Bytes(), "kept_dumps", len(plan.Kept), "dry_run", opts.DryRun)
	if opts.DryRun || len(plan.Delete) == 0 {
		return plan, nil
	}

	var manifests, objects []string
	for _, a := range plan.Delete {
		if a.Kind == "manifest" {
			manifests = append(manifests, a.Key)
		} else {
			objects = append(objects, a.Key)
		}
	}
	if err := st.Delete(ctx, manifests); err != nil {
		return plan, fmt.Errorf("prune: deleting manifests: %w", err)
	}
	if err := st.Delete(ctx, objects); err != nil {
		return plan, fmt.Errorf("prune: deleting objects: %w", err)
	}
	for _, a := range plan.Delete {
		logger.Info("deleted", "key", a.Key, "kind", a.Kind, "reason", a.Reason, "bytes", a.Size)
	}
	return plan, nil
}

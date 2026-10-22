package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/config"
	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

// Selector names one backup: by object key, or by database with the latest
// backup (default) or the newest one created at or before At.
type Selector struct {
	Key      string
	Database string
	At       time.Time // zero = latest
}

// Validate checks that the selector names exactly one thing.
func (s Selector) Validate() error {
	switch {
	case s.Key != "" && s.Database != "":
		return fmt.Errorf("%w: use either --key or --db, not both", config.ErrInvalid)
	case s.Key == "" && s.Database == "":
		return fmt.Errorf("%w: --key or --db is required", config.ErrInvalid)
	case s.Key != "" && !s.At.IsZero():
		return fmt.Errorf("%w: --at only applies with --db", config.ErrInvalid)
	}
	return nil
}

// Resolve turns a selector into the manifest entry to restore. Selection
// always goes through manifests; an object without one is not a backup.
func Resolve(ctx context.Context, st store.ObjectStore, prefix, cluster string, sel Selector) (manifest.Entry, error) {
	if err := sel.Validate(); err != nil {
		return manifest.Entry{}, err
	}
	if sel.Key != "" {
		return resolveKey(ctx, st, sel.Key)
	}
	cat, err := manifest.Load(ctx, st, prefix, cluster, sel.Database)
	if err != nil {
		return manifest.Entry{}, err
	}
	var e manifest.Entry
	var ok bool
	if sel.At.IsZero() {
		e, ok = cat.Latest(sel.Database)
	} else {
		e, ok = cat.At(sel.Database, sel.At)
	}
	if !ok {
		if sel.At.IsZero() {
			return manifest.Entry{}, fmt.Errorf("no backup of database %q under %s", sel.Database, manifest.ClusterPrefix(prefix, cluster))
		}
		return manifest.Entry{}, fmt.Errorf("no backup of database %q at or before %s", sel.Database, sel.At.UTC().Format(time.RFC3339))
	}
	if !e.ObjectFound {
		return manifest.Entry{}, fmt.Errorf("backup %s has a manifest but its object is missing", e.Manifest.Key)
	}
	return e, nil
}

func resolveKey(ctx context.Context, st store.ObjectStore, key string) (manifest.Entry, error) {
	if manifest.IsManifestKey(key) {
		key = manifest.ObjectKeyFor(key)
	}
	mkey := manifest.ManifestKeyFor(key)
	rc, _, err := st.Get(ctx, mkey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return manifest.Entry{}, fmt.Errorf("no manifest for %s (expected %s)", key, mkey)
		}
		return manifest.Entry{}, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return manifest.Entry{}, err
	}
	m, err := manifest.Unmarshal(b)
	if err != nil {
		return manifest.Entry{}, err
	}
	if m.Key != key {
		return manifest.Entry{}, fmt.Errorf("manifest %s describes %s, not %s", mkey, m.Key, key)
	}
	obj, err := st.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return manifest.Entry{}, fmt.Errorf("backup object %s is missing", key)
		}
		return manifest.Entry{}, err
	}
	return manifest.Entry{Manifest: m, ManifestKey: mkey, Object: obj, ObjectFound: true}, nil
}

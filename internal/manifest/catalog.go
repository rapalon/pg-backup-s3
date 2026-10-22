package manifest

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/store"
)

// loadConcurrency bounds parallel manifest downloads in Load.
const loadConcurrency = 8

// Entry is one manifest found in the bucket together with its object.
type Entry struct {
	Manifest    Manifest
	ManifestKey string
	Object      store.ObjectInfo // listing info of the dump/globals object
	ObjectFound bool             // false for a dangling manifest
}

// Orphan is a dump or globals object that has no manifest, typically the
// remains of a run that crashed between uploading the object and writing
// the manifest.
type Orphan struct {
	Object store.ObjectInfo
	Info   KeyInfo
}

// Broken is a manifest that could not be fetched or parsed.
type Broken struct {
	Key string
	Err error
}

// Catalog is everything known about one cluster (or one database) in the
// bucket. Manifests are the source of truth; objects are cross-checked.
type Catalog struct {
	Prefix  string
	Cluster string
	Entries []Entry            // every readable manifest, newest first
	Orphans []Orphan           // objects without a manifest
	Broken  []Broken           // unreadable manifests
	Foreign []store.ObjectInfo // keys that do not follow the layout
}

// Load lists prefix/cluster (or prefix/cluster/db when db is non-empty),
// fetches every manifest and cross-checks objects against manifests.
func Load(ctx context.Context, st store.ObjectStore, prefix, cluster, db string) (Catalog, error) {
	listPrefix := ClusterPrefix(prefix, cluster)
	if db != "" {
		listPrefix = DatabasePrefix(prefix, cluster, db)
	}
	infos, err := st.List(ctx, listPrefix)
	if err != nil {
		return Catalog{}, err
	}

	cat := Catalog{Prefix: prefix, Cluster: cluster}
	byKey := make(map[string]store.ObjectInfo, len(infos))
	var manifestKeys []string
	for _, o := range infos {
		byKey[o.Key] = o
		if IsManifestKey(o.Key) {
			manifestKeys = append(manifestKeys, o.Key)
		}
	}

	// Fetch manifests with bounded concurrency; order is restored below.
	entries := make([]*Entry, len(manifestKeys))
	broken := make([]*Broken, len(manifestKeys))
	var wg sync.WaitGroup
	sem := make(chan struct{}, loadConcurrency)
	for i, key := range manifestKeys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m, err := fetchManifest(ctx, st, key)
			if err != nil {
				broken[i] = &Broken{Key: key, Err: err}
				return
			}
			e := &Entry{Manifest: m, ManifestKey: key}
			if obj, ok := byKey[m.Key]; ok {
				e.Object, e.ObjectFound = obj, true
			}
			entries[i] = e
		}()
	}
	wg.Wait()

	referenced := map[string]bool{}
	for i := range manifestKeys {
		if entries[i] != nil {
			cat.Entries = append(cat.Entries, *entries[i])
			referenced[entries[i].Manifest.Key] = true
		}
		if broken[i] != nil {
			cat.Broken = append(cat.Broken, *broken[i])
			referenced[ObjectKeyFor(broken[i].Key)] = true // do not call its object an orphan
		}
	}

	for _, o := range infos {
		if IsManifestKey(o.Key) || referenced[o.Key] {
			continue
		}
		info, err := ParseKey(prefix, o.Key)
		if err != nil {
			cat.Foreign = append(cat.Foreign, o)
			continue
		}
		cat.Orphans = append(cat.Orphans, Orphan{Object: o, Info: info})
	}

	sortEntries(cat.Entries)
	sort.Slice(cat.Orphans, func(i, j int) bool { return cat.Orphans[i].Info.Time.After(cat.Orphans[j].Info.Time) })
	return cat, nil
}

func fetchManifest(ctx context.Context, st store.ObjectStore, key string) (Manifest, error) {
	rc, _, err := st.Get(ctx, key)
	if err != nil {
		return Manifest{}, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return Manifest{}, err
	}
	m, err := Unmarshal(b)
	if err != nil {
		return Manifest{}, err
	}
	if want := ObjectKeyFor(key); m.Key != want {
		return Manifest{}, fmt.Errorf("manifest %s describes %s, expected %s", key, m.Key, want)
	}
	return m, nil
}

// sortEntries orders newest first, ties broken by key for determinism.
func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].Manifest, entries[j].Manifest
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.Key > b.Key
	})
}

// Dumps returns the database dump entries, newest first.
func (c Catalog) Dumps() []Entry { return c.ofKind(KindDump) }

// Globals returns the globals entries, newest first.
func (c Catalog) Globals() []Entry { return c.ofKind(KindGlobals) }

func (c Catalog) ofKind(k Kind) []Entry {
	var out []Entry
	for _, e := range c.Entries {
		if e.Manifest.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// Databases returns the names of every database with at least one manifest.
func (c Catalog) Databases() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range c.Dumps() {
		if !seen[e.Manifest.Database] {
			seen[e.Manifest.Database] = true
			out = append(out, e.Manifest.Database)
		}
	}
	sort.Strings(out)
	return out
}

// ByDatabase returns one database's dump entries, newest first.
func (c Catalog) ByDatabase(db string) []Entry {
	var out []Entry
	for _, e := range c.Dumps() {
		if e.Manifest.Database == db {
			out = append(out, e)
		}
	}
	return out
}

// Latest returns the newest dump of db.
func (c Catalog) Latest(db string) (Entry, bool) {
	dumps := c.ByDatabase(db)
	if len(dumps) == 0 {
		return Entry{}, false
	}
	return dumps[0], true
}

// At returns the newest dump of db created at or before t.
func (c Catalog) At(db string, t time.Time) (Entry, bool) {
	for _, e := range c.ByDatabase(db) {
		if !e.Manifest.CreatedAt.After(t) {
			return e, true
		}
	}
	return Entry{}, false
}

// Find returns the entry whose object key is key.
func (c Catalog) Find(objectKey string) (Entry, bool) {
	for _, e := range c.Entries {
		if e.Manifest.Key == objectKey {
			return e, true
		}
	}
	return Entry{}, false
}

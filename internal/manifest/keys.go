package manifest

import (
	"fmt"
	"strings"
	"time"
)

// Object key layout (see plan.md §1.2):
//
//	<prefix>/<cluster>/<db>/<YYYY>/<MM>/<db>_<ts>.dump.age
//	<prefix>/<cluster>/<db>/<YYYY>/<MM>/<db>_<ts>.dump.manifest.json
//	<prefix>/<cluster>/globals/<YYYY>/<MM>/globals_<ts>.sql.zst.age
//	<prefix>/<cluster>/globals/<YYYY>/<MM>/globals_<ts>.sql.zst.manifest.json
//
// The manifest key is the object key with ".age" replaced by
// ".manifest.json", so the two never collide even for a database that is
// literally named "globals". Database names are escaped so that "/" and "%"
// cannot break the layout.
const (
	TimeLayout     = "20060102T150405Z"
	DumpSuffix     = ".dump.age"
	GlobalsSuffix  = ".sql.zst.age"
	ManifestSuffix = ".manifest.json"
	globalsSegment = "globals"
)

// FormatTime renders a key timestamp (UTC, second precision).
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// ParseTime parses a key timestamp.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(TimeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func join(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "/")
}

// ClusterPrefix is the key prefix of everything belonging to a cluster.
func ClusterPrefix(prefix, cluster string) string { return join(prefix, cluster) + "/" }

// DatabasePrefix is the key prefix of one database's dumps and manifests.
func DatabasePrefix(prefix, cluster, db string) string {
	return join(prefix, cluster, encodeName(db)) + "/"
}

// GlobalsPrefix is the key prefix of a cluster's globals objects.
func GlobalsPrefix(prefix, cluster string) string {
	return join(prefix, cluster, globalsSegment) + "/"
}

// DumpKey builds the object key of a database dump.
func DumpKey(prefix, cluster, db string, ts time.Time) string {
	name := encodeName(db)
	u := ts.UTC()
	return join(prefix, cluster, name, u.Format("2006"), u.Format("01"), name+"_"+FormatTime(u)+DumpSuffix)
}

// GlobalsKey builds the object key of a globals file.
func GlobalsKey(prefix, cluster string, ts time.Time) string {
	u := ts.UTC()
	return join(prefix, cluster, globalsSegment, u.Format("2006"), u.Format("01"), globalsSegment+"_"+FormatTime(u)+GlobalsSuffix)
}

// ManifestKeyFor returns the manifest key of an object key.
func ManifestKeyFor(objectKey string) string {
	return strings.TrimSuffix(objectKey, ".age") + ManifestSuffix
}

// ObjectKeyFor returns the object key of a manifest key.
func ObjectKeyFor(manifestKey string) string {
	return strings.TrimSuffix(manifestKey, ManifestSuffix) + ".age"
}

// IsManifestKey reports whether key names a manifest.
func IsManifestKey(key string) bool { return strings.HasSuffix(key, ManifestSuffix) }

// KeyInfo is what ParseKey recovers from a key.
type KeyInfo struct {
	Cluster   string
	Database  string // empty for globals
	Kind      Kind
	Time      time.Time
	Manifest  bool   // key names a manifest rather than the object
	ObjectKey string // the object key (equal to the input for non-manifest keys)
}

// ParseKey recovers cluster, database, kind and timestamp from a key laid
// out by DumpKey/GlobalsKey (or their manifest keys) under prefix.
func ParseKey(prefix, key string) (KeyInfo, error) {
	rel := key
	if prefix != "" {
		p := strings.Trim(prefix, "/") + "/"
		if !strings.HasPrefix(key, p) {
			return KeyInfo{}, fmt.Errorf("key %q is not under prefix %q", key, prefix)
		}
		rel = strings.TrimPrefix(key, p)
	}
	parts := strings.Split(rel, "/")
	if len(parts) != 5 {
		return KeyInfo{}, fmt.Errorf("key %q: expected cluster/db/YYYY/MM/file", key)
	}
	cluster, segment, yyyy, mm, file := parts[0], parts[1], parts[2], parts[3], parts[4]

	info := KeyInfo{Cluster: cluster, ObjectKey: key}
	base := file
	if IsManifestKey(file) {
		info.Manifest = true
		base = strings.TrimSuffix(file, ManifestSuffix) + ".age"
		info.ObjectKey = ObjectKeyFor(key)
	}

	var name string
	switch {
	case strings.HasSuffix(base, DumpSuffix):
		info.Kind = KindDump
		name = strings.TrimSuffix(base, DumpSuffix)
	case strings.HasSuffix(base, GlobalsSuffix):
		info.Kind = KindGlobals
		name = strings.TrimSuffix(base, GlobalsSuffix)
	default:
		return KeyInfo{}, fmt.Errorf("key %q: unknown object suffix", key)
	}

	i := strings.LastIndexByte(name, '_')
	if i < 0 {
		return KeyInfo{}, fmt.Errorf("key %q: file name lacks _timestamp", key)
	}
	encoded, tsStr := name[:i], name[i+1:]
	ts, err := ParseTime(tsStr)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("key %q: %w", key, err)
	}
	info.Time = ts
	if yyyy != ts.Format("2006") || mm != ts.Format("01") {
		return KeyInfo{}, fmt.Errorf("key %q: directory %s/%s does not match timestamp %s", key, yyyy, mm, tsStr)
	}

	switch info.Kind {
	case KindDump:
		if encoded != segment {
			return KeyInfo{}, fmt.Errorf("key %q: file name %q does not match directory %q", key, encoded, segment)
		}
		info.Database = decodeName(encoded)
	case KindGlobals:
		if encoded != globalsSegment || segment != globalsSegment {
			return KeyInfo{}, fmt.Errorf("key %q: malformed globals key", key)
		}
	}
	return info, nil
}

var (
	nameEncoder = strings.NewReplacer("%", "%25", "/", "%2F")
	nameDecoder = strings.NewReplacer("%2F", "/", "%25", "%")
)

func encodeName(db string) string { return nameEncoder.Replace(db) }
func decodeName(s string) string  { return nameDecoder.Replace(s) }

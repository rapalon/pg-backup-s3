package manifest

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var ts = time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)

func valid() Manifest {
	return Manifest{
		FormatVersion: FormatVersion,
		ToolVersion:   "0.1.0",
		Cluster:       "prod",
		Database:      "app",
		Kind:          KindDump,
		Key:           DumpKey("backups", "prod", "app", ts),
		CreatedAt:     ts,
		ServerVersion: "18.6",
		PgDumpVersion: "18.6",
		DumpFormat:    "custom",
		Compression:   "zstd:3",
		SizeBytes:     12345,
		SHA256:        strings.Repeat("ab", 32),
		Encryption:    Encryption{Type: "age", Recipients: []string{"age1x"}},
		GlobalsKey:    GlobalsKey("backups", "prod", ts),
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	t.Parallel()
	m := valid()
	b, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"format_version": 1`) || !strings.HasSuffix(string(b), "}\n") {
		t.Errorf("unexpected JSON:\n%s", b)
	}
	got, err := Unmarshal(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Key != m.Key || got.SHA256 != m.SHA256 || !got.CreatedAt.Equal(m.CreatedAt) || got.GlobalsKey != m.GlobalsKey {
		t.Errorf("round trip changed fields: %+v", got)
	}

	g := valid()
	g.Kind, g.Database, g.Key, g.DumpFormat, g.GlobalsKey = KindGlobals, "", GlobalsKey("backups", "prod", ts), "plain", ""
	b, err = g.Marshal()
	if err != nil {
		t.Fatalf("globals Marshal: %v", err)
	}
	if strings.Contains(string(b), "globals_key") {
		t.Error("empty globals_key should be omitted")
	}
}

func TestUnmarshalRejects(t *testing.T) {
	t.Parallel()
	_, err := Unmarshal([]byte(`{"format_version": 2, "kind": "dump"}`))
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("format 2: %v", err)
	}
	if _, err := Unmarshal([]byte(`not json`)); err == nil {
		t.Error("garbage accepted")
	}
	if _, err := Unmarshal([]byte(`{"format_version": 1}`)); err == nil {
		t.Error("empty manifest accepted")
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Manifest){
		"bad version":        func(m *Manifest) { m.FormatVersion = 0 },
		"dump without db":    func(m *Manifest) { m.Database = "" },
		"globals with db":    func(m *Manifest) { m.Kind = KindGlobals },
		"unknown kind":       func(m *Manifest) { m.Kind = "tarball" },
		"no cluster":         func(m *Manifest) { m.Cluster = "" },
		"no key":             func(m *Manifest) { m.Key = "" },
		"zero time":          func(m *Manifest) { m.CreatedAt = time.Time{} },
		"short sha":          func(m *Manifest) { m.SHA256 = "abc" },
		"negative size":      func(m *Manifest) { m.SizeBytes = -1 },
		"wrong encryption":   func(m *Manifest) { m.Encryption.Type = "gpg" },
		"no key material":    func(m *Manifest) { m.Encryption.Recipients = nil },
		"passphrase is fine": nil,
	}
	for name, mutate := range cases {
		m := valid()
		if mutate == nil {
			m.Encryption = Encryption{Type: "age", Passphrase: true}
			if err := m.Validate(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		mutate(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: Validate succeeded", name)
		}
		if _, err := m.Marshal(); err == nil {
			t.Errorf("%s: Marshal succeeded", name)
		}
	}
}

func TestKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		prefix, cluster, db string
		wantDump, wantGlob  string
	}{
		{"backups", "prod", "app",
			"backups/prod/app/2026/10/app_20261022T030000Z.dump.age",
			"backups/prod/globals/2026/10/globals_20261022T030000Z.sql.zst.age"},
		{"", "default", "my_db",
			"default/my_db/2026/10/my_db_20261022T030000Z.dump.age",
			"default/globals/2026/10/globals_20261022T030000Z.sql.zst.age"},
		{"a/b", "c", "odd/name%",
			"a/b/c/odd%2Fname%25/2026/10/odd%2Fname%25_20261022T030000Z.dump.age",
			"a/b/c/globals/2026/10/globals_20261022T030000Z.sql.zst.age"},
	}
	for _, c := range cases {
		if got := DumpKey(c.prefix, c.cluster, c.db, ts); got != c.wantDump {
			t.Errorf("DumpKey(%q,%q,%q) = %q, want %q", c.prefix, c.cluster, c.db, got, c.wantDump)
		}
		if got := GlobalsKey(c.prefix, c.cluster, ts); got != c.wantGlob {
			t.Errorf("GlobalsKey = %q, want %q", got, c.wantGlob)
		}
		for _, key := range []string{c.wantDump, c.wantGlob} {
			info, err := ParseKey(c.prefix, key)
			if err != nil {
				t.Errorf("ParseKey(%q): %v", key, err)
				continue
			}
			if info.Cluster != c.cluster || !info.Time.Equal(ts) || info.Manifest || info.ObjectKey != key {
				t.Errorf("ParseKey(%q) = %+v", key, info)
			}
			if key == c.wantDump && (info.Kind != KindDump || info.Database != c.db) {
				t.Errorf("ParseKey(dump) = %+v", info)
			}
			if key == c.wantGlob && (info.Kind != KindGlobals || info.Database != "") {
				t.Errorf("ParseKey(globals) = %+v", info)
			}

			mk := ManifestKeyFor(key)
			if !IsManifestKey(mk) || ObjectKeyFor(mk) != key {
				t.Errorf("manifest key round trip: %q -> %q -> %q", key, mk, ObjectKeyFor(mk))
			}
			minfo, err := ParseKey(c.prefix, mk)
			if err != nil || !minfo.Manifest || minfo.ObjectKey != key || minfo.Kind != info.Kind {
				t.Errorf("ParseKey(manifest %q) = %+v, %v", mk, minfo, err)
			}
		}
	}

	if got := ManifestKeyFor("backups/prod/app/2026/10/app_20261022T030000Z.dump.age"); got != "backups/prod/app/2026/10/app_20261022T030000Z.dump.manifest.json" {
		t.Errorf("ManifestKeyFor = %q", got)
	}
	if got := ManifestKeyFor("x/globals_20261022T030000Z.sql.zst.age"); got != "x/globals_20261022T030000Z.sql.zst.manifest.json" {
		t.Errorf("ManifestKeyFor(globals) = %q", got)
	}
}

func TestDatabaseNamedGlobalsDoesNotCollide(t *testing.T) {
	t.Parallel()
	dump := DumpKey("p", "c", "globals", ts)
	glob := GlobalsKey("p", "c", ts)
	if dump == glob || ManifestKeyFor(dump) == ManifestKeyFor(glob) {
		t.Fatalf("collision: %q vs %q", dump, glob)
	}
	di, err := ParseKey("p", dump)
	if err != nil || di.Kind != KindDump || di.Database != "globals" {
		t.Errorf("dump of db globals: %+v %v", di, err)
	}
	gi, err := ParseKey("p", glob)
	if err != nil || gi.Kind != KindGlobals {
		t.Errorf("globals: %+v %v", gi, err)
	}
}

func TestPrefixes(t *testing.T) {
	t.Parallel()
	if ClusterPrefix("backups", "prod") != "backups/prod/" || ClusterPrefix("", "prod") != "prod/" {
		t.Error("ClusterPrefix")
	}
	if DatabasePrefix("b", "c", "a/b") != "b/c/a%2Fb/" {
		t.Error("DatabasePrefix")
	}
	if GlobalsPrefix("", "c") != "c/globals/" {
		t.Error("GlobalsPrefix")
	}
	if !strings.HasPrefix(DumpKey("b", "c", "a/b", ts), DatabasePrefix("b", "c", "a/b")) {
		t.Error("DumpKey not under DatabasePrefix")
	}
}

func TestParseKeyErrors(t *testing.T) {
	t.Parallel()
	bad := []string{
		"other/prod/app/2026/10/app_20261022T030000Z.dump.age",        // wrong prefix
		"backups/prod/app/app_20261022T030000Z.dump.age",              // too few segments
		"backups/prod/app/2026/10/app_20261022T030000Z.tar",           // unknown suffix
		"backups/prod/app/2026/10/app.dump.age",                       // no timestamp
		"backups/prod/app/2026/10/app_notatime.dump.age",              // bad timestamp
		"backups/prod/app/2026/11/app_20261022T030000Z.dump.age",      // month mismatch
		"backups/prod/app/2026/10/other_20261022T030000Z.dump.age",    // name mismatch
		"backups/prod/app/2026/10/app_20261022T030000Z.sql.zst.age",   // globals suffix under a db
		"backups/prod/globals/2026/10/x_20261022T030000Z.sql.zst.age", // wrong globals name
	}
	for _, key := range bad {
		if _, err := ParseKey("backups", key); err == nil {
			t.Errorf("ParseKey(%q) succeeded", key)
		}
	}
}

func TestTime(t *testing.T) {
	t.Parallel()
	local := time.Date(2026, 10, 22, 5, 0, 0, 0, time.FixedZone("X", 2*3600))
	if FormatTime(local) != "20261022T030000Z" {
		t.Errorf("FormatTime = %s", FormatTime(local))
	}
	got, err := ParseTime("20261022T030000Z")
	if err != nil || !got.Equal(ts) || got.Location() != time.UTC {
		t.Errorf("ParseTime = %v %v", got, err)
	}
	if _, err := ParseTime("2026-10-22"); err == nil {
		t.Error("ParseTime accepted the wrong layout")
	}
}

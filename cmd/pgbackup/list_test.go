package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pipeline"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

func sampleCatalog() manifest.Catalog {
	ts := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)
	m := manifest.Manifest{
		FormatVersion: 1, Cluster: "c", Database: "app", Kind: manifest.KindDump,
		Key: manifest.DumpKey("p", "c", "app", ts), CreatedAt: ts, SizeBytes: 5 << 20, SHA256: strings.Repeat("a", 64),
		Encryption: manifest.Encryption{Type: "age", Passphrase: true},
	}
	g := m
	g.Kind, g.Database, g.Key, g.SizeBytes = manifest.KindGlobals, "", manifest.GlobalsKey("p", "c", ts), 700
	short := m
	short.Key, short.CreatedAt, short.SizeBytes = manifest.DumpKey("p", "c", "app", ts.Add(-time.Hour)), ts.Add(-time.Hour), 4096
	return manifest.Catalog{
		Prefix: "p", Cluster: "c",
		Entries: []manifest.Entry{
			{Manifest: m, ManifestKey: manifest.ManifestKeyFor(m.Key), ObjectFound: true, Object: store.ObjectInfo{Key: m.Key, Size: m.SizeBytes}},
			{Manifest: g, ManifestKey: manifest.ManifestKeyFor(g.Key), ObjectFound: false},
			{Manifest: short, ManifestKey: manifest.ManifestKeyFor(short.Key), ObjectFound: true, Object: store.ObjectInfo{Key: short.Key, Size: 100}},
		},
		Orphans: []manifest.Orphan{{Object: store.ObjectInfo{Key: "p/c/app/2026/09/app_20260901T000000Z.dump.age", Size: 10}, Info: manifest.KeyInfo{Database: "app", Kind: manifest.KindDump, Time: ts.AddDate(0, -1, 0)}}},
		Broken:  []manifest.Broken{{Key: "p/c/app/2026/08/app_20260801T000000Z.dump.manifest.json", Err: errors.New("not json")}},
		Foreign: []store.ObjectInfo{{Key: "p/c/README"}},
	}
}

func TestPrintCatalog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	printCatalog(&buf, sampleCatalog(), 0)
	out := buf.String()
	for _, want := range []string{
		"CREATED", "2026-10-22T03:00:00Z", "dump", "app", "5.0 MiB", "ok",
		"globals", "700 B", "object missing",
		"size mismatch: bucket has 100 bytes",
		"orphan: no manifest", "broken manifest: not json",
		"3 manifest(s), 1 orphan(s), 1 broken, 1 foreign object(s) under p/c/",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	printCatalog(&buf, sampleCatalog(), 1)
	if strings.Contains(buf.String(), "globals") {
		t.Error("limit not applied")
	}
}

func TestWriteCatalogJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeCatalogJSON(&buf, sampleCatalog(), 0); err != nil {
		t.Fatal(err)
	}
	var out catalogJSON
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if out.Cluster != "c" || len(out.Entries) != 3 || len(out.Dangling) != 1 || len(out.Mismatch) != 1 || len(out.Orphans) != 1 || len(out.Broken) != 1 {
		t.Errorf("json = %+v", out)
	}
	if out.Entries[0].Database != "app" || out.Entries[0].FormatVersion != 1 {
		t.Errorf("entry = %+v", out.Entries[0])
	}
}

func TestPrintPrunePlan(t *testing.T) {
	t.Parallel()
	plan := pipeline.PrunePlan{
		Delete: []pipeline.PruneAction{
			{Key: "k1", Kind: "manifest", Reason: "retention", Database: "app", Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
			{Key: "k2", Kind: "dump", Reason: "retention", Database: "app", Size: 2048, Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
			{Key: "k3", Kind: "orphan", Reason: "orphan: no manifest", Size: 1},
		},
		Kept: map[string][]string{"a": {"newest"}, "b": {"last"}},
	}
	plan.Catalog.Broken = []manifest.Broken{{Key: "x", Err: errors.New("bad")}}

	var buf bytes.Buffer
	printPrunePlan(&buf, plan, true)
	out := buf.String()
	for _, want := range []string{"Would delete", "2026-09-01T00:00:00Z", "2.0 KiB", "orphan: no manifest", "would delete 3 object(s), 2.0 KiB; 2 dump(s) kept; 1 broken manifest(s) left untouched"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	printPrunePlan(&buf, pipeline.PrunePlan{}, false)
	if got := buf.String(); got != "deleted 0 object(s), 0 B; 0 dump(s) kept\n" {
		t.Errorf("empty plan output = %q", got)
	}
}

func TestHumanSize(t *testing.T) {
	t.Parallel()
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 5 << 20: "5.0 MiB", 3 << 30: "3.0 GiB", 1536: "1.5 KiB"}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestListAndPruneCommandsNeedConfig(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"list"}, {"prune"}, {"prune", "--dry-run"}, {"list", "--json"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != exitUsage {
			t.Errorf("%v: exit %d, want %d (%s)", args, code, exitUsage, stderr.String())
		}
	}
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"list", "extra"}, &bytes.Buffer{}, &stderr); code != exitUsage || !strings.Contains(stderr.String(), "unexpected argument") {
		t.Errorf("positional arg: %d %s", code, stderr.String())
	}
}

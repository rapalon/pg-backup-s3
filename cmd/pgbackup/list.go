package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/manifest"
)

func runList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("list", stderr)
	db := fs.String("db", "", "only this database")
	limit := fs.Int("limit", 0, "show at most N manifests (0 = all)")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}

	a, err := loadApp(ctx, stderr, nil)
	if err != nil {
		return err
	}
	cat, err := manifest.Load(ctx, a.store, a.cfg.S3.Prefix, a.cfg.Backup.ClusterName, *db)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeCatalogJSON(stdout, cat, *limit)
	}
	printCatalog(stdout, cat, *limit)
	return nil
}

type catalogJSON struct {
	Cluster  string              `json:"cluster"`
	Entries  []manifest.Manifest `json:"entries"`
	Dangling []string            `json:"dangling_manifests,omitempty"`
	Mismatch []string            `json:"size_mismatch,omitempty"`
	Orphans  []orphanJSON        `json:"orphans,omitempty"`
	Broken   []brokenJSON        `json:"broken_manifests,omitempty"`
}

type orphanJSON struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size_bytes"`
	LastModified time.Time `json:"last_modified"`
}

type brokenJSON struct {
	Key   string `json:"key"`
	Error string `json:"error"`
}

func writeCatalogJSON(w io.Writer, cat manifest.Catalog, limit int) error {
	out := catalogJSON{Cluster: cat.Cluster, Entries: []manifest.Manifest{}}
	for i, e := range cat.Entries {
		if limit > 0 && i >= limit {
			break
		}
		out.Entries = append(out.Entries, e.Manifest)
		switch {
		case !e.ObjectFound:
			out.Dangling = append(out.Dangling, e.ManifestKey)
		case e.Object.Size != e.Manifest.SizeBytes:
			out.Mismatch = append(out.Mismatch, e.Manifest.Key)
		}
	}
	for _, o := range cat.Orphans {
		out.Orphans = append(out.Orphans, orphanJSON{Key: o.Object.Key, Size: o.Object.Size, LastModified: o.Object.LastModified})
	}
	for _, b := range cat.Broken {
		out.Broken = append(out.Broken, brokenJSON{Key: b.Key, Error: b.Err.Error()})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printCatalog(w io.Writer, cat manifest.Catalog, limit int) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CREATED\tKIND\tDATABASE\tSIZE\tSTATUS\tKEY")
	shown := 0
	for _, e := range cat.Entries {
		if limit > 0 && shown >= limit {
			break
		}
		shown++
		status := "ok"
		switch {
		case !e.ObjectFound:
			status = "object missing"
		case e.Object.Size != e.Manifest.SizeBytes:
			status = fmt.Sprintf("size mismatch: bucket has %d bytes", e.Object.Size)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			e.Manifest.CreatedAt.UTC().Format(time.RFC3339), e.Manifest.Kind, dbLabel(e.Manifest), humanSize(e.Manifest.SizeBytes), status, e.Manifest.Key)
	}
	for _, o := range cat.Orphans {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			o.Info.Time.Format(time.RFC3339), o.Info.Kind, o.Info.Database, humanSize(o.Object.Size), "orphan: no manifest", o.Object.Key)
	}
	for _, b := range cat.Broken {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", "-", "-", "-", "-", "broken manifest: "+b.Err.Error(), b.Key)
	}
	tw.Flush()
	fmt.Fprintf(w, "%d manifest(s), %d orphan(s), %d broken, %d foreign object(s) under %s\n",
		len(cat.Entries), len(cat.Orphans), len(cat.Broken), len(cat.Foreign), manifest.ClusterPrefix(cat.Prefix, cat.Cluster))
}

func dbLabel(m manifest.Manifest) string {
	if m.Kind == manifest.KindGlobals {
		return "-"
	}
	return m.Database
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Command s3check is a tiny S3 client for the test scripts: it counts objects
// and incomplete multipart uploads under a prefix, or fetches one object.
// It uses the same store package as pgbackup, configured from S3_* variables.
//
//	s3check count <prefix>        → "objects=N incomplete=M"
//	s3check fetch <key> <file>    → downloads an object
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/rapalon/pg-backup-s3/internal/store"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: s3check count <prefix> | fetch <key> <file>")
		os.Exit(2)
	}
	st, err := store.NewS3(store.S3Config{
		Endpoint: os.Getenv("S3_ENDPOINT"), Region: os.Getenv("S3_REGION"), Bucket: os.Getenv("S3_BUCKET"),
		AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"), PathStyle: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	switch os.Args[1] {
	case "count":
		infos, err := st.List(ctx, os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		incomplete, err := st.IncompleteUploads(ctx, os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("objects=%d incomplete=%d\n", len(infos), len(incomplete))
		for _, o := range infos {
			fmt.Printf("  %s (%d bytes)\n", o.Key, o.Size)
		}
		for _, k := range incomplete {
			fmt.Printf("  INCOMPLETE %s\n", k)
		}
	case "fetch":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: s3check fetch <key> <file>")
			os.Exit(2)
		}
		rc, _, err := st.Get(ctx, os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer rc.Close()
		f, err := os.Create(os.Args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		n, err := io.Copy(f, rc)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("fetched %d bytes to %s\n", n, os.Args[3])
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand")
		os.Exit(2)
	}
}

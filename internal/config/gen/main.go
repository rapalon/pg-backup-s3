// Command gen rewrites the environment variable table in README.md from
// the specs in internal/config. Run through `go generate ./internal/config`.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/rapalon/pg-backup-s3/internal/config"
)

func main() {
	readme := flag.String("readme", "README.md", "README to update in place")
	check := flag.Bool("check", false, "exit 1 if the README is out of date instead of rewriting it")
	flag.Parse()

	b, err := os.ReadFile(*readme)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	updated, ok := config.SpliceEnvTable(string(b))
	if !ok {
		fmt.Fprintf(os.Stderr, "%s: markers %q and %q not found\n", *readme, config.EnvTableBegin, config.EnvTableEnd)
		os.Exit(1)
	}
	if updated == string(b) {
		fmt.Println("README environment table is up to date")
		return
	}
	if *check {
		fmt.Fprintln(os.Stderr, "README environment table is out of date; run: go generate ./internal/config")
		os.Exit(1)
	}
	if err := os.WriteFile(*readme, []byte(updated), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("README environment table updated")
}

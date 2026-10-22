package config

import (
	"os"
	"strings"
	"testing"
)

func TestEnvTable(t *testing.T) {
	t.Parallel()
	table := EnvTable()
	if !strings.HasPrefix(table, "| Variable | Default | Description |\n|---|---|---|\n") {
		t.Error("table header")
	}
	for _, want := range []string{
		"| `PGPASSWORD` / `PGPASSWORD_FILE` | — |",
		"Secret: redacted in logs.",
		"| `AGE_RECIPIENTS` / `AGE_RECIPIENTS_FILE` | — |",
		"| `S3_PART_SIZE` | `32MiB` |",
		"| `BACKUP_SCHEDULE` | — |",
	} {
		if !strings.Contains(table, want) {
			t.Errorf("table lacks %q", want)
		}
	}
	if strings.Contains(table, "| `AGE_RECIPIENTS` / `AGE_RECIPIENTS_FILE` | — | Comma- or newline-separated age public keys (age1...). Required for backup unless AGE_PASSPHRASE is set. Secret") {
		t.Error("public recipients must not be marked secret")
	}
	if n := strings.Count(table, "\n"); n != len(specs)+2 {
		t.Errorf("table has %d lines, want %d", n, len(specs)+2)
	}

	doc := "intro\n" + EnvTableBegin + "\nold\n" + EnvTableEnd + "\noutro\n"
	out, ok := SpliceEnvTable(doc)
	if !ok || !strings.HasPrefix(out, "intro\n"+EnvTableBegin+"\n| Variable") || !strings.HasSuffix(out, EnvTableEnd+"\noutro\n") {
		t.Errorf("splice = %q", out)
	}
	if _, ok := SpliceEnvTable("no markers"); ok {
		t.Error("splice without markers reported success")
	}
}

// TestReadmeEnvTableIsCurrent fails when README.md does not contain exactly
// the table EnvTable renders, so documentation cannot drift from the code.
func TestReadmeEnvTableIsCurrent(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Skipf("README.md not readable: %v", err)
	}
	readme := string(b)
	updated, ok := SpliceEnvTable(readme)
	if !ok {
		t.Fatal("README.md lacks the env table markers")
	}
	if updated != readme {
		t.Fatal("README.md environment table is out of date; run: go generate ./internal/config")
	}
}

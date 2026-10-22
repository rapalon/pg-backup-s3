package pg

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHelperProcess is not a test: fakeRunner re-executes the test binary
// with -test.run=^TestHelperProcess$ -- <binary> <args...> so unit tests can
// exercise the Runner without PostgreSQL installed.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("PG_FAKE_HELPER") != "1" {
		return
	}
	defer os.Exit(0)

	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "helper: no binary name")
		os.Exit(99)
	}
	binary, rest := args[0], args[1:]
	switch binary {
	case "fake":
		fakeGeneric(rest)
	case "pg_dump":
		fakePgDump(rest)
	case "pg_dumpall":
		fmt.Printf("ARGS %s\n", strings.Join(rest, " "))
		fmt.Printf("PGOPTIONS=%s\n", os.Getenv("PGOPTIONS"))
	case "psql":
		fakePsql(rest)
	case "pg_restore":
		fakePgRestore(rest)
	case "vacuumdb":
		fmt.Printf("ARGS %s\n", strings.Join(rest, " "))
	default:
		fmt.Fprintf(os.Stderr, "helper: unknown binary %q\n", binary)
		os.Exit(98)
	}
}

func fakeGeneric(args []string) {
	if len(args) == 0 {
		os.Exit(97)
	}
	switch args[0] {
	case "ok":
		fmt.Println("hello")
	case "exit":
		fmt.Fprintln(os.Stderr, "first warning")
		fmt.Fprintln(os.Stderr, "second warning")
		code := 1
		if len(args) > 1 {
			fmt.Sscanf(args[1], "%d", &code)
		}
		os.Exit(code)
	case "stderr-partial":
		fmt.Fprint(os.Stderr, "no newline at end")
		os.Exit(2)
	case "stderr-many":
		line := strings.Repeat("x", 1023) + "\n"
		for i := 0; i < 100; i++ { // 100 KiB, more than stderrTailLimit
			fmt.Fprintf(os.Stderr, "%03d%s", i, line[3:])
		}
		os.Exit(1)
	case "echo":
		io.Copy(os.Stdout, os.Stdin)
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
	case "hang-term":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		fmt.Println("ready")
		select {
		case <-ch:
			os.Exit(0)
		case <-time.After(30 * time.Second):
			os.Exit(96)
		}
	case "hang-ignore":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		time.Sleep(30 * time.Second)
		os.Exit(95)
	default:
		os.Exit(94)
	}
}

func fakePgDump(args []string) {
	for _, a := range args {
		if a == "--version" {
			v := os.Getenv("FAKE_PG_DUMP_VERSION")
			if v == "" {
				v = "18.6"
			}
			fmt.Printf("pg_dump (PostgreSQL) %s\n", v)
			return
		}
	}
	fmt.Printf("ARGS %s\n", strings.Join(args, " "))
	fmt.Printf("PGOPTIONS=%s\n", os.Getenv("PGOPTIONS"))
	fmt.Printf("PGPASSWORD=%s\n", os.Getenv("PGPASSWORD"))
	fmt.Printf("S3_SECRET_KEY=%s\n", os.Getenv("S3_SECRET_KEY"))
}

func fakePgRestore(args []string) {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "--list"):
		io.Copy(io.Discard, io.LimitReader(os.Stdin, 64)) // like the real one: reads the TOC, not the whole stream
		fmt.Println(";\n; Archive created at 2026-10-22 03:00:00 UTC\n;\n123; 1259 16384 TABLE public t postgres")
	case strings.Contains(joined, "--dbname=ignore-mode"):
		io.Copy(io.Discard, os.Stdin)
		fmt.Fprintln(os.Stderr, "pg_restore: warning: errors ignored on restore: 3")
		os.Exit(1)
	case strings.Contains(joined, "--dbname=fail-mode"):
		fmt.Fprintln(os.Stderr, "pg_restore: error: could not execute query")
		os.Exit(1)
	default:
		io.Copy(io.Discard, os.Stdin)
	}
}

func fakePsql(args []string) {
	var command, dbname, file string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--command="); ok {
			command = v
		}
		if v, ok := strings.CutPrefix(a, "--dbname="); ok {
			dbname = v
		}
		if v, ok := strings.CutPrefix(a, "--file="); ok {
			file = v
		}
	}
	if file == "-" {
		// Script mode: one ERROR per statement line, exit 0 like ON_ERROR_STOP=0.
		b, _ := io.ReadAll(os.Stdin)
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line != "" {
				fmt.Fprintf(os.Stderr, "psql:<stdin>:1: ERROR:  role already exists: %s\n", line)
			}
		}
		return
	}
	switch {
	case strings.Contains(command, "pg_database where datname = 'exists'"):
		fmt.Println("1")
	case strings.Contains(command, "pg_database where datname"):
		// no rows
	case strings.Contains(command, "from pg_class"):
		fmt.Println("7")
	case strings.Contains(command, "pg_terminate_backend"), strings.HasPrefix(command, "create database"), strings.HasPrefix(command, "drop database"):
		// ok, no output
	case strings.Contains(command, "server_version"):
		fmt.Println("16.15 (Debian 16.15-1.pgdg120+1)")
	case strings.Contains(command, "pg_database"):
		fmt.Println("app")
		fmt.Println("postgres")
		fmt.Println("zeta")
	case strings.Contains(command, "multi"):
		fmt.Println("a\tb")
		fmt.Println("c\td")
	case strings.Contains(command, "dbname"):
		fmt.Println(dbname)
	case strings.Contains(command, "nothing"):
		// no rows
	case strings.Contains(command, "fail"):
		fmt.Fprintln(os.Stderr, "ERROR:  relation \"nope\" does not exist")
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "helper psql: unexpected command %q\n", command)
		os.Exit(93)
	}
}

// fakeRunner returns a Runner whose children are this test binary in helper
// mode, and a buffer receiving its JSON log output.
func fakeRunner(t *testing.T, extraEnv ...string) (*Runner, *strings.Builder) {
	t.Helper()
	logs := &strings.Builder{}
	r := NewRunner(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	r.KillGrace = 300 * time.Millisecond
	r.lookPath = func(string) (string, error) { return os.Args[0], nil }
	r.argPrefix = []string{"-test.run=^TestHelperProcess$", "--"}
	// GOCOVERDIR keeps a `go test -cover` build of the helper from printing
	// "warning: GOCOVERDIR not set" to stderr, which the stderr tests inspect.
	coverDir := t.TempDir()
	r.baseEnv = func() []string {
		env := []string{"PG_FAKE_HELPER=1", "PATH=" + os.Getenv("PATH"), "S3_SECRET_KEY=must-not-leak", "GOCOVERDIR=" + coverDir}
		return append(env, extraEnv...)
	}
	return r, logs
}

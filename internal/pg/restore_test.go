package pg

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRestoreOptionsArgs(t *testing.T) {
	t.Parallel()
	o := RestoreOptions{Database: "dst", Clean: true, NoOwner: true, NoPrivileges: true, SchemaOnly: true,
		Tables: []string{"a", "public.b"}, ExitOnError: true, SingleTransaction: true}
	got := strings.Join(o.args(), " ")
	want := "--dbname=dst --no-password --clean --if-exists --no-owner --no-privileges --schema-only --table=a --table=public.b --exit-on-error --single-transaction"
	if got != want {
		t.Errorf("args = %q\nwant   %q", got, want)
	}
	if got := strings.Join((RestoreOptions{Database: "d", DataOnly: true}).args(), " "); got != "--dbname=d --no-password --data-only" {
		t.Errorf("data-only args = %q", got)
	}
}

func TestRestoreIgnoredErrors(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	// The fake pg_restore prints "errors ignored on restore: 3" and exits 1
	// when it sees --ignore-mode in its arguments.
	res, err := r.Restore(context.Background(), Conn{}, RestoreOptions{Database: "ignore-mode"}, strings.NewReader("PGDMP"))
	if err != nil || res.IgnoredErrors != 3 {
		t.Errorf("ignored errors: res=%+v err=%v", res, err)
	}
	// With ExitOnError the same exit is a failure.
	_, err = r.Restore(context.Background(), Conn{}, RestoreOptions{Database: "ignore-mode", ExitOnError: true}, strings.NewReader("PGDMP"))
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Errorf("exit-on-error: err=%v", err)
	}
	// A real failure without the ignored line stays a failure.
	_, err = r.Restore(context.Background(), Conn{}, RestoreOptions{Database: "fail-mode"}, strings.NewReader("PGDMP"))
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Errorf("fail-mode: err=%v", err)
	}
}

func TestRestoreFileAndJobs(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	res, err := r.RestoreFile(context.Background(), Conn{}, RestoreOptions{Database: "echo-args"}, "/tmp/x.dump", 4)
	if err != nil {
		t.Fatalf("RestoreFile: %v", err)
	}
	if res.IgnoredErrors != 0 {
		t.Errorf("res = %+v", res)
	}
	// echo-args mode makes the fake print its argv to stderr; the last line is
	// captured by the ExitError only on failure, so use ListArchive's stdout path instead.
	toc, err := r.ListArchive(context.Background(), strings.NewReader("PGDMP..."))
	if err != nil || !strings.Contains(toc, "Archive created at") {
		t.Errorf("ListArchive: %q %v", toc, err)
	}
}

func TestRunSQLScriptCountsErrors(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	n, err := r.RunSQLScript(context.Background(), Conn{}, "postgres", strings.NewReader("CREATE ROLE a;\nCREATE ROLE b;\n"))
	if err != nil || n != 2 {
		t.Errorf("RunSQLScript: n=%d err=%v", n, err)
	}
}

func TestAdminHelpers(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	ctx := context.Background()

	if QuoteIdent(`we"ird`) != `"we""ird"` || QuoteLiteral("it's") != `'it''s'` {
		t.Error("quoting")
	}
	ok, err := r.DatabaseExists(ctx, Conn{}, "exists")
	if err != nil || !ok {
		t.Errorf("DatabaseExists(exists) = %v %v", ok, err)
	}
	ok, err = r.DatabaseExists(ctx, Conn{}, "nothing")
	if err != nil || ok {
		t.Errorf("DatabaseExists(nothing) = %v %v", ok, err)
	}
	n, err := r.UserObjectCount(ctx, Conn{}, "any")
	if err != nil || n != 7 {
		t.Errorf("UserObjectCount = %d %v", n, err)
	}
	for name, fn := range map[string]func() error{
		"terminate": func() error { return r.TerminateConnections(ctx, Conn{}, "x") },
		"create":    func() error { return r.CreateDatabase(ctx, Conn{}, "x", "owner") },
		"drop":      func() error { return r.DropDatabase(ctx, Conn{}, "x", true) },
		"analyze":   func() error { return r.AnalyzeDatabase(ctx, Conn{}, "x") },
	} {
		if err := fn(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestOnStderrHook(t *testing.T) {
	t.Parallel()
	r, _ := fakeRunner(t)
	var lines []string
	err := r.Run(context.Background(), Spec{Binary: "fake", Args: []string{"exit", "0"}, OnStderr: func(l string) { lines = append(lines, l) }})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "|") != "first warning|second warning" {
		t.Errorf("lines = %v", lines)
	}
	_ = io.EOF
}

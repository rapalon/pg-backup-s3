package pipeline

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pg"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

func TestRunMixedResults(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.db.waitErr["beta"] = &pg.ExitError{Binary: "pg_dump", Code: 1}
	stateDir := t.TempDir()

	report, err := Run(context.Background(), h.deps, RunOptions{
		AllDatabases: true,
		Exclude:      []string{"gamma"},
		Parallel:     2,
		Retry:        instantRetry(3),
		StateDir:     stateDir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Globals.Err != nil || report.Globals.Name != "globals" {
		t.Errorf("globals = %+v", report.Globals)
	}
	if len(report.Databases) != 2 {
		t.Fatalf("databases = %d, want 2 (gamma excluded)", len(report.Databases))
	}
	byName := map[string]Result{}
	for _, r := range report.Databases {
		byName[r.Name] = r
	}
	if a := byName["alpha"]; a.Err != nil || a.Attempts != 1 || a.Manifest.GlobalsKey != report.Globals.Manifest.Key {
		t.Errorf("alpha = %+v", a)
	}
	if b := byName["beta"]; b.Err == nil || b.Attempts != 1 {
		t.Errorf("beta should fail once without retry: %+v", b)
	}
	if failed := report.Failed(); len(failed) != 1 || failed[0].Name != "beta" || report.OK() {
		t.Errorf("Failed() = %v", failed)
	}
	if started := h.db.startedDumps(); len(started) != 2 || strings.Contains(strings.Join(started, ","), "gamma") {
		t.Errorf("dumps started: %v", started)
	}
	if keys := h.store.Keys(); len(keys) != 4 {
		t.Errorf("expected globals + alpha objects and manifests, got %v", keys)
	}
	if _, err := os.Stat(filepath.Join(stateDir, HeartbeatFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("heartbeat written although a database failed")
	}
	if !strings.Contains(h.logs.String(), `"msg":"backup failed","name":"beta"`) {
		t.Error("failure not logged")
	}
}

func TestRunAllOKWritesHeartbeat(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	stateDir := t.TempDir()
	report, err := Run(context.Background(), h.deps, RunOptions{
		Databases: []string{"gamma", "alpha"},
		Retry:     instantRetry(1),
		StateDir:  stateDir,
		Timeout:   time.Minute,
	})
	if err != nil || !report.OK() {
		t.Fatalf("Run: %v, failed=%v", err, report.Failed())
	}
	if report.Databases[0].Name != "gamma" || report.Databases[1].Name != "alpha" {
		t.Errorf("results are not in request order: %v", report.Databases)
	}
	hb, err := ReadHeartbeat(stateDir)
	if err != nil {
		t.Fatalf("ReadHeartbeat: %v", err)
	}
	if hb.Before(fixedTime) {
		t.Errorf("heartbeat %v is before the run", hb)
	}
	if report.Duration <= 0 {
		t.Error("duration not measured")
	}
}

func TestRunRetriesTransientStoreErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	flaky := &flakyStore{ObjectStore: h.store, left: 2}
	h.deps.Store = flaky

	report, err := Run(context.Background(), h.deps, RunOptions{Databases: []string{"gamma"}, Retry: instantRetry(3)})
	if err != nil {
		t.Fatal(err)
	}
	// The first two Puts (globals object) fail; globals needs 3 attempts.
	if report.Globals.Err != nil || report.Globals.Attempts != 3 {
		t.Errorf("globals = err %v attempts %d", report.Globals.Err, report.Globals.Attempts)
	}
	if report.Databases[0].Err != nil || report.Databases[0].Attempts != 1 {
		t.Errorf("gamma = %+v", report.Databases[0])
	}
	if !strings.Contains(h.logs.String(), "retrying after transient error") {
		t.Error("retry not logged")
	}
}

func TestRunGivesUpAfterAttempts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.Store = &flakyStore{ObjectStore: h.store, left: 100}
	report, err := Run(context.Background(), h.deps, RunOptions{Databases: []string{"gamma"}, Retry: instantRetry(2)})
	if err != nil {
		t.Fatal(err)
	}
	if report.Globals.Attempts != 2 || report.Globals.Err == nil {
		t.Errorf("globals = %+v", report.Globals)
	}
	if report.Databases[0].Attempts != 2 || report.Databases[0].Err == nil {
		t.Errorf("gamma = %+v", report.Databases[0])
	}
	if len(h.store.Keys()) != 0 {
		t.Error("objects left behind")
	}
}

func TestRunLevelFailures(t *testing.T) {
	t.Parallel()

	t.Run("no databases selected", func(t *testing.T) {
		h := newHarness(t)
		if _, err := Run(context.Background(), h.deps, RunOptions{}); err == nil {
			t.Error("want error")
		}
	})
	t.Run("everything excluded", func(t *testing.T) {
		h := newHarness(t)
		_, err := Run(context.Background(), h.deps, RunOptions{AllDatabases: true, Exclude: []string{"alpha", "beta", "gamma"}})
		if err == nil || !strings.Contains(err.Error(), "no databases left") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("list fails", func(t *testing.T) {
		h := newHarness(t)
		h.db.listErr = errors.New("psql: connection refused")
		if _, err := Run(context.Background(), h.deps, RunOptions{AllDatabases: true}); err == nil {
			t.Error("want error")
		}
	})
	t.Run("client older than server", func(t *testing.T) {
		h := newHarness(t)
		h.db.client = pg.Version{Major: 16, Minor: 9}
		_, err := Run(context.Background(), h.deps, RunOptions{AllDatabases: true})
		if err == nil || !strings.Contains(err.Error(), "older than the server") {
			t.Errorf("err = %v", err)
		}
		if len(h.db.startedDumps()) != 0 {
			t.Error("dumps started despite version mismatch")
		}
	})
	t.Run("globals failure does not stop databases", func(t *testing.T) {
		h := newHarness(t)
		h.db.waitErr["globals"] = &pg.ExitError{Binary: "pg_dumpall", Code: 1}
		report, err := Run(context.Background(), h.deps, RunOptions{Databases: []string{"gamma"}, Retry: instantRetry(1)})
		if err != nil {
			t.Fatal(err)
		}
		if report.Globals.Err == nil || report.Databases[0].Err != nil {
			t.Errorf("report = %+v", report)
		}
		if report.Databases[0].Manifest.GlobalsKey != "" {
			t.Error("manifest references a globals object that does not exist")
		}
	})
}

func TestRunTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// A store that never returns until the context dies.
	h.deps.Store = &blockingStore{ObjectStore: h.store}
	start := time.Now()
	report, err := Run(context.Background(), h.deps, RunOptions{Databases: []string{"gamma"}, Retry: instantRetry(3), Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("run did not respect the timeout")
	}
	if report.Globals.Err == nil || !errors.Is(report.Globals.Err, context.DeadlineExceeded) {
		t.Errorf("globals err = %v, want deadline exceeded", report.Globals.Err)
	}
	if report.Globals.Attempts != 1 {
		t.Errorf("deadline errors must not be retried, attempts = %d", report.Globals.Attempts)
	}
}

type blockingStore struct{ store.ObjectStore }

func (b *blockingStore) Put(ctx context.Context, key string, r io.Reader, opts store.PutOptions) (store.PutResult, error) {
	<-ctx.Done()
	return store.PutResult{}, ctx.Err()
}

func TestRetryPolicy(t *testing.T) {
	t.Parallel()
	p := DefaultRetryPolicy(5)
	p.Jitter = func(d time.Duration) time.Duration { return d }
	want := []time.Duration{0, 5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := p.Delay(i + 1); got != w {
			t.Errorf("Delay(%d) = %s, want %s", i+1, got, w)
		}
	}
	// Default jitter stays within ±20 %.
	q := DefaultRetryPolicy(2)
	for i := 0; i < 100; i++ {
		d := q.Delay(2)
		if d < 4*time.Second || d > 6*time.Second {
			t.Fatalf("jittered delay %s outside 4s..6s", d)
		}
	}
}

func TestRetry(t *testing.T) {
	t.Parallel()
	var slept []time.Duration
	p := RetryPolicy{Attempts: 4, Base: time.Second, Factor: 2, Max: time.Minute,
		Jitter: func(d time.Duration) time.Duration { return d },
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}}

	calls := 0
	attempts, err := Retry(context.Background(), nil, p, "op", func(context.Context) error {
		calls++
		if calls < 3 {
			return &transientErr{}
		}
		return nil
	})
	if err != nil || attempts != 3 || calls != 3 {
		t.Errorf("transient then success: attempts=%d calls=%d err=%v", attempts, calls, err)
	}
	if len(slept) != 2 || slept[0] != time.Second || slept[1] != 2*time.Second {
		t.Errorf("sleeps = %v", slept)
	}

	calls = 0
	attempts, err = Retry(context.Background(), nil, p, "op", func(context.Context) error {
		calls++
		return &pg.ExitError{Binary: "pg_dump", Code: 2}
	})
	if attempts != 1 || calls != 1 || err == nil {
		t.Errorf("non-transient: attempts=%d calls=%d err=%v", attempts, calls, err)
	}

	calls = 0
	attempts, err = Retry(context.Background(), nil, p, "op", func(context.Context) error {
		calls++
		return &transientErr{}
	})
	if attempts != 4 || calls != 4 || err == nil {
		t.Errorf("exhausted: attempts=%d calls=%d err=%v", attempts, calls, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.Sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	calls = 0
	attempts, err = Retry(ctx, nil, p, "op", func(context.Context) error {
		calls++
		return &transientErr{}
	})
	if attempts != 1 || calls != 1 || err == nil {
		t.Errorf("cancelled during sleep: attempts=%d calls=%d err=%v", attempts, calls, err)
	}

	if attempts, err := Retry(context.Background(), nil, RetryPolicy{}, "op", func(context.Context) error { return nil }); attempts != 1 || err != nil {
		t.Errorf("zero policy: attempts=%d err=%v", attempts, err)
	}
}

func TestIsTransient(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":            {nil, false},
		"exit error":     {&pg.ExitError{}, false},
		"wrapped exit":   {errors.Join(errors.New("x"), &pg.ExitError{}), false},
		"integrity":      {ErrIntegrity, false},
		"not found":      {store.ErrNotFound, false},
		"bucket":         {store.ErrBucketNotFound, false},
		"compression":    {pg.ErrCompressionUnsupported, false},
		"cancelled":      {context.Canceled, false},
		"deadline":       {context.DeadlineExceeded, false},
		"network-ish":    {errors.New("dial tcp: connection refused"), true},
		"transient type": {&transientErr{}, true},
	}
	for name, c := range cases {
		if got := IsTransient(c.err); got != c.want {
			t.Errorf("%s: IsTransient = %v, want %v", name, got, c.want)
		}
	}
}

func TestHeartbeat(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "nested", "state")
	if _, err := ReadHeartbeat(dir); err == nil {
		t.Error("ReadHeartbeat on a missing file succeeded")
	}
	when := time.Date(2026, 10, 22, 3, 4, 5, 999, time.FixedZone("X", 3600))
	if err := WriteHeartbeat(dir, when); err != nil {
		t.Fatalf("WriteHeartbeat: %v", err)
	}
	got, err := ReadHeartbeat(dir)
	if err != nil || !got.Equal(when.Truncate(time.Second)) || got.Location() != time.UTC {
		t.Errorf("ReadHeartbeat = %v %v", got, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left in state dir: %v", entries)
	}
	os.WriteFile(filepath.Join(dir, HeartbeatFile), []byte("garbage"), 0o644)
	if _, err := ReadHeartbeat(dir); err == nil {
		t.Error("garbage heartbeat parsed")
	}
}

func TestReportFailedOrder(t *testing.T) {
	t.Parallel()
	r := Report{
		Globals:   Result{Name: "globals", Err: errors.New("g")},
		Databases: []Result{{Name: "a"}, {Name: "b", Err: errors.New("b")}, {Name: "c", Manifest: manifest.Manifest{Key: "k"}}},
	}
	f := r.Failed()
	if len(f) != 2 || f[0].Name != "globals" || f[1].Name != "b" || r.OK() {
		t.Errorf("Failed() = %v", f)
	}
}

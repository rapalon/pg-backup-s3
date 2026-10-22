package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// minimal is the smallest environment Load accepts.
var minimal = map[string]string{
	"S3_ENDPOINT":   "http://garage:3900",
	"S3_BUCKET":     "pgbackup-test",
	"S3_ACCESS_KEY": "GKtest",
	"S3_SECRET_KEY": "hunter2",
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func files(m map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		v, ok := m[path]
		if !ok {
			return nil, fmt.Errorf("open %s: no such file or directory", path)
		}
		return []byte(v), nil
	}
}

func with(over map[string]string) map[string]string {
	m := make(map[string]string, len(minimal)+len(over))
	for k, v := range minimal {
		m[k] = v
	}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func mustLoad(t *testing.T, e map[string]string, f map[string]string) Config {
	t.Helper()
	c, err := Load(env(e), files(f))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	c := mustLoad(t, minimal, nil)

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"PGHOST", c.Postgres.Host, "localhost"},
		{"PGPORT", c.Postgres.Port, 5432},
		{"PGUSER", c.Postgres.User, "postgres"},
		{"PGPASSWORD", c.Postgres.Password, ""},
		{"PGDATABASE", c.Postgres.Database, "postgres"},
		{"PGSSLMODE", c.Postgres.SSLMode, "prefer"},
		{"CLUSTER_NAME", c.Backup.ClusterName, "default"},
		{"AllDatabases", c.Backup.AllDatabases, true},
		{"Databases", len(c.Backup.Databases), 0},
		{"BACKUP_COMPRESSION", c.Backup.Compression, "zstd:3"},
		{"BACKUP_LOCK_WAIT_TIMEOUT", c.Backup.LockWaitTimeout, time.Minute},
		{"BACKUP_TIMEOUT", c.Backup.Timeout, 6 * time.Hour},
		{"BACKUP_PARALLEL", c.Backup.Parallel, 1},
		{"BACKUP_RETRIES", c.Backup.Retries, 3},
		{"S3.Endpoint", c.S3.Endpoint, "http://garage:3900"},
		{"S3.UseTLS", c.S3.UseTLS, false},
		{"S3_REGION", c.S3.Region, "us-east-1"},
		{"S3_PREFIX", c.S3.Prefix, ""},
		{"S3_PATH_STYLE", c.S3.PathStyle, true},
		{"S3_PART_SIZE", c.S3.PartSize, int64(32 << 20)},
		{"S3_TLS_INSECURE", c.S3.TLSInsecure, false},
		{"BACKUP_SCHEDULE", c.Schedule.Backup, ""},
		{"VERIFY_SCHEDULE", c.Schedule.Verify, ""},
		{"BACKUP_TZ", c.Schedule.Location.String(), "UTC"},
		{"RUN_ON_START", c.Schedule.RunOnStart, false},
		{"PRUNE_AFTER_BACKUP", c.Schedule.PruneAfterBackup, true},
		{"RETENTION_KEEP_LAST", c.Retention.KeepLast, 7},
		{"RETENTION_KEEP_DAILY", c.Retention.KeepDaily, 0},
		{"RETENTION_KEEP_WEEKLY", c.Retention.KeepWeekly, 0},
		{"RETENTION_KEEP_MONTHLY", c.Retention.KeepMonthly, 0},
		{"STATE_DIR", c.Runtime.StateDir, "/tmp/pgbackup"},
		{"SHUTDOWN_TIMEOUT", c.Runtime.ShutdownTimeout, 30 * time.Second},
		{"HEALTH_MAX_AGE", c.Runtime.HealthMaxAge, time.Duration(0)},
		{"LOG_LEVEL", c.Runtime.LogLevel, slog.LevelInfo},
		{"LOG_FORMAT", c.Runtime.LogFormat, "json"},
		{"METRICS_ADDR", c.Runtime.MetricsAddr, ""},
		{"NOTIFY_METHOD", c.Runtime.NotifyMethod, "POST"},
		{"PGBACKUP_YES", c.Runtime.Yes, false},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s = %v, want %v", ck.name, ck.got, ck.want)
		}
	}
}

func TestLoadValues(t *testing.T) {
	t.Parallel()
	e := map[string]string{
		"PGHOST":                   "db.internal",
		"PGPORT":                   "6432",
		"PGUSER":                   "backup",
		"PGPASSWORD_FILE":          "/run/secrets/pgpass",
		"PGDATABASE":               "maint",
		"PGSSLMODE":                "VERIFY-FULL",
		"CLUSTER_NAME":             "prod",
		"BACKUP_DATABASES":         " app, auth ,, billing ",
		"BACKUP_COMPRESSION":       "gzip:6",
		"BACKUP_LOCK_WAIT_TIMEOUT": "0s",
		"BACKUP_TIMEOUT":           "90m",
		"BACKUP_PARALLEL":          "4",
		"BACKUP_RETRIES":           "0",
		"AGE_RECIPIENTS_FILE":      "/run/secrets/recipients",
		"AGE_IDENTITY":             "AGE-SECRET-KEY-1TEST",
		"S3_ENDPOINT":              "https://S3.Example.com/",
		"S3_REGION":                "eu-central-1",
		"S3_BUCKET":                "backups",
		"S3_PREFIX":                "/pg/prod/",
		"S3_ACCESS_KEY_FILE":       "/run/secrets/ak",
		"S3_SECRET_KEY_FILE":       "/run/secrets/sk",
		"S3_PATH_STYLE":            "false",
		"S3_PART_SIZE":             "64MB",
		"S3_TLS_INSECURE":          "true",
		"BACKUP_SCHEDULE":          "0 3 * * *",
		"BACKUP_TZ":                "Europe/Zurich",
		"VERIFY_SCHEDULE":          "@weekly",
		"RUN_ON_START":             "1",
		"PRUNE_AFTER_BACKUP":       "false",
		"RETENTION_KEEP_LAST":      "3",
		"RETENTION_KEEP_DAILY":     "14",
		"RETENTION_KEEP_WEEKLY":    "8",
		"RETENTION_KEEP_MONTHLY":   "12",
		"STATE_DIR":                "/var/lib/pgbackup",
		"SHUTDOWN_TIMEOUT":         "1m",
		"HEALTH_MAX_AGE":           "26h",
		"LOG_LEVEL":                "debug",
		"LOG_FORMAT":               "text",
		"METRICS_ADDR":             ":9090",
		"NOTIFY_URL_SUCCESS":       "https://hc.example.com/ping/ok",
		"NOTIFY_URL_FAILURE":       "http://hc.example.com/ping/fail",
		"NOTIFY_METHOD":            "get",
		"PGBACKUP_YES":             "true",
	}
	f := map[string]string{
		"/run/secrets/pgpass":     "pw-from-file\n",
		"/run/secrets/recipients": "age1aaa,age1bbb\r\n",
		"/run/secrets/ak":         "AKFILE",
		"/run/secrets/sk":         "SKFILE\n\n",
	}
	c := mustLoad(t, e, f)

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Host", c.Postgres.Host, "db.internal"},
		{"Port", c.Postgres.Port, 6432},
		{"User", c.Postgres.User, "backup"},
		{"Password", c.Postgres.Password, "pw-from-file"},
		{"Database", c.Postgres.Database, "maint"},
		{"SSLMode", c.Postgres.SSLMode, "verify-full"},
		{"ClusterName", c.Backup.ClusterName, "prod"},
		{"AllDatabases", c.Backup.AllDatabases, false},
		{"Databases", strings.Join(c.Backup.Databases, "|"), "app|auth|billing"},
		{"Compression", c.Backup.Compression, "gzip:6"},
		{"LockWaitTimeout", c.Backup.LockWaitTimeout, time.Duration(0)},
		{"Timeout", c.Backup.Timeout, 90 * time.Minute},
		{"Parallel", c.Backup.Parallel, 4},
		{"Retries", c.Backup.Retries, 0},
		{"Recipients", c.Encryption.Recipients, "age1aaa,age1bbb"},
		{"Identity", c.Encryption.Identity, "AGE-SECRET-KEY-1TEST"},
		{"Endpoint", c.S3.Endpoint, "https://S3.Example.com"},
		{"UseTLS", c.S3.UseTLS, true},
		{"Region", c.S3.Region, "eu-central-1"},
		{"Bucket", c.S3.Bucket, "backups"},
		{"Prefix", c.S3.Prefix, "pg/prod"},
		{"AccessKey", c.S3.AccessKey, "AKFILE"},
		{"SecretKey", c.S3.SecretKey, "SKFILE"},
		{"PathStyle", c.S3.PathStyle, false},
		{"PartSize", c.S3.PartSize, int64(64_000_000)},
		{"TLSInsecure", c.S3.TLSInsecure, true},
		{"Schedule.Backup", c.Schedule.Backup, "0 3 * * *"},
		{"Schedule.Verify", c.Schedule.Verify, "@weekly"},
		{"Location", c.Schedule.Location.String(), "Europe/Zurich"},
		{"RunOnStart", c.Schedule.RunOnStart, true},
		{"PruneAfterBackup", c.Schedule.PruneAfterBackup, false},
		{"KeepLast", c.Retention.KeepLast, 3},
		{"KeepDaily", c.Retention.KeepDaily, 14},
		{"KeepWeekly", c.Retention.KeepWeekly, 8},
		{"KeepMonthly", c.Retention.KeepMonthly, 12},
		{"StateDir", c.Runtime.StateDir, "/var/lib/pgbackup"},
		{"ShutdownTimeout", c.Runtime.ShutdownTimeout, time.Minute},
		{"HealthMaxAge", c.Runtime.HealthMaxAge, 26 * time.Hour},
		{"LogLevel", c.Runtime.LogLevel, slog.LevelDebug},
		{"LogFormat", c.Runtime.LogFormat, "text"},
		{"MetricsAddr", c.Runtime.MetricsAddr, ":9090"},
		{"NotifyURLSuccess", c.Runtime.NotifyURLSuccess, "https://hc.example.com/ping/ok"},
		{"NotifyURLFailure", c.Runtime.NotifyURLFailure, "http://hc.example.com/ping/fail"},
		{"NotifyMethod", c.Runtime.NotifyMethod, "GET"},
		{"Yes", c.Runtime.Yes, true},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s = %v, want %v", ck.name, ck.got, ck.want)
		}
	}
}

func TestLoadExcludes(t *testing.T) {
	t.Parallel()
	c := mustLoad(t, with(map[string]string{
		"BACKUP_DATABASES":         "ALL",
		"BACKUP_EXCLUDE_DATABASES": "postgres, scratch",
	}), nil)
	if !c.Backup.AllDatabases {
		t.Fatal("AllDatabases = false, want true")
	}
	if got := strings.Join(c.Backup.ExcludeDatabases, "|"); got != "postgres|scratch" {
		t.Errorf("ExcludeDatabases = %q", got)
	}
}

func TestLoadErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		env   map[string]string
		files map[string]string
		want  []string // substrings that must all appear in the error
	}{
		{
			name: "empty environment reports every required variable",
			env:  map[string]string{},
			want: []string{"S3_ENDPOINT: is required", "S3_BUCKET: is required", "S3_ACCESS_KEY: is required", "S3_SECRET_KEY: is required"},
		},
		{name: "port not integer", env: with(map[string]string{"PGPORT": "abc"}), want: []string{"PGPORT", "not an integer"}},
		{name: "port out of range", env: with(map[string]string{"PGPORT": "70000"}), want: []string{"PGPORT", "outside 1..65535"}},
		{name: "sslmode", env: with(map[string]string{"PGSSLMODE": "maybe"}), want: []string{"PGSSLMODE", "not one of"}},
		{name: "cluster name with slash", env: with(map[string]string{"CLUSTER_NAME": "a/b"}), want: []string{"CLUSTER_NAME"}},
		{name: "compression algorithm", env: with(map[string]string{"BACKUP_COMPRESSION": "brotli"}), want: []string{"BACKUP_COMPRESSION"}},
		{name: "compression level", env: with(map[string]string{"BACKUP_COMPRESSION": "zstd:"}), want: []string{"BACKUP_COMPRESSION"}},
		{name: "databases only commas", env: with(map[string]string{"BACKUP_DATABASES": ",,"}), want: []string{"BACKUP_DATABASES"}},
		{name: "databases all plus names", env: with(map[string]string{"BACKUP_DATABASES": "all,app"}), want: []string{"`all` cannot be combined"}},
		{name: "excludes with explicit list", env: with(map[string]string{"BACKUP_DATABASES": "app", "BACKUP_EXCLUDE_DATABASES": "x"}), want: []string{"BACKUP_EXCLUDE_DATABASES", "only applies"}},
		{name: "lock wait timeout", env: with(map[string]string{"BACKUP_LOCK_WAIT_TIMEOUT": "soon"}), want: []string{"BACKUP_LOCK_WAIT_TIMEOUT", "not a duration"}},
		{name: "backup timeout too small", env: with(map[string]string{"BACKUP_TIMEOUT": "10s"}), want: []string{"BACKUP_TIMEOUT", "below the minimum"}},
		{name: "parallel zero", env: with(map[string]string{"BACKUP_PARALLEL": "0"}), want: []string{"BACKUP_PARALLEL", "outside 1..64"}},
		{name: "retries negative", env: with(map[string]string{"BACKUP_RETRIES": "-1"}), want: []string{"BACKUP_RETRIES"}},
		{name: "recipients and passphrase", env: with(map[string]string{"AGE_RECIPIENTS": "age1x", "AGE_PASSPHRASE": "p"}), want: []string{"AGE_RECIPIENTS", "choose one"}},
		{name: "secret and file both set", env: with(map[string]string{"PGPASSWORD": "a", "PGPASSWORD_FILE": "/x"}), files: map[string]string{"/x": "b"}, want: []string{"set either PGPASSWORD or PGPASSWORD_FILE"}},
		{name: "secret file missing", env: with(map[string]string{"PGPASSWORD_FILE": "/nope"}), want: []string{"PGPASSWORD_FILE", "no such file"}},
		{name: "endpoint without scheme", env: with(map[string]string{"S3_ENDPOINT": "garage:3900"}), want: []string{"S3_ENDPOINT", "scheme must be http or https"}},
		{name: "endpoint ftp", env: with(map[string]string{"S3_ENDPOINT": "ftp://x"}), want: []string{"S3_ENDPOINT", "scheme"}},
		{name: "endpoint no host", env: with(map[string]string{"S3_ENDPOINT": "http://"}), want: []string{"S3_ENDPOINT", "no host"}},
		{name: "endpoint with path", env: with(map[string]string{"S3_ENDPOINT": "http://h/bucket"}), want: []string{"S3_ENDPOINT", "must not contain a path"}},
		{name: "endpoint with credentials", env: with(map[string]string{"S3_ENDPOINT": "http://u:p@h"}), want: []string{"S3_ENDPOINT", "credentials"}},
		{name: "endpoint with query", env: with(map[string]string{"S3_ENDPOINT": "http://h?x=1"}), want: []string{"S3_ENDPOINT", "query"}},
		{name: "endpoint unparsable", env: with(map[string]string{"S3_ENDPOINT": "http://h:port"}), want: []string{"S3_ENDPOINT"}},
		{name: "path style not boolean", env: with(map[string]string{"S3_PATH_STYLE": "yes"}), want: []string{"S3_PATH_STYLE", "not a boolean"}},
		{name: "part size too small", env: with(map[string]string{"S3_PART_SIZE": "1MiB"}), want: []string{"S3_PART_SIZE", "outside 5MiB..5GiB"}},
		{name: "part size garbage", env: with(map[string]string{"S3_PART_SIZE": "lots"}), want: []string{"S3_PART_SIZE", "not a size"}},
		{name: "backup schedule", env: with(map[string]string{"BACKUP_SCHEDULE": "every day"}), want: []string{"BACKUP_SCHEDULE", "not a valid cron expression"}},
		{name: "verify schedule", env: with(map[string]string{"VERIFY_SCHEDULE": "61 * * * *"}), want: []string{"VERIFY_SCHEDULE"}},
		{name: "time zone", env: with(map[string]string{"BACKUP_TZ": "Mars/Olympus"}), want: []string{"BACKUP_TZ", "unknown time zone"}},
		{name: "retention negative", env: with(map[string]string{"RETENTION_KEEP_LAST": "-1"}), want: []string{"RETENTION_KEEP_LAST"}},
		{name: "shutdown timeout", env: with(map[string]string{"SHUTDOWN_TIMEOUT": "x"}), want: []string{"SHUTDOWN_TIMEOUT"}},
		{name: "log level", env: with(map[string]string{"LOG_LEVEL": "verbose"}), want: []string{"LOG_LEVEL", "not one of"}},
		{name: "log format", env: with(map[string]string{"LOG_FORMAT": "xml"}), want: []string{"LOG_FORMAT"}},
		{name: "metrics addr", env: with(map[string]string{"METRICS_ADDR": "9090"}), want: []string{"METRICS_ADDR", "host:port"}},
		{name: "notify url", env: with(map[string]string{"NOTIFY_URL_SUCCESS": "ftp://x"}), want: []string{"NOTIFY_URL_SUCCESS", "http(s) URL"}},
		{name: "notify url no host", env: with(map[string]string{"NOTIFY_URL_FAILURE": "http://"}), want: []string{"NOTIFY_URL_FAILURE"}},
		{name: "notify method", env: with(map[string]string{"NOTIFY_METHOD": "PUT"}), want: []string{"NOTIFY_METHOD"}},
		{name: "yes not boolean", env: with(map[string]string{"PGBACKUP_YES": "maybe"}), want: []string{"PGBACKUP_YES"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(env(tc.env), files(tc.files))
			if err == nil {
				t.Fatal("Load succeeded, want error")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error does not wrap ErrInvalid: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestLoadErrorsAreJoined(t *testing.T) {
	t.Parallel()
	_, err := Load(env(with(map[string]string{"PGPORT": "x", "LOG_LEVEL": "y", "S3_PART_SIZE": "z"})), files(nil))
	if err == nil {
		t.Fatal("want error")
	}
	for _, w := range []string{"PGPORT", "LOG_LEVEL", "S3_PART_SIZE"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("joined error lacks %s: %v", w, err)
		}
	}
}

func TestValidateFor(t *testing.T) {
	t.Parallel()
	base := mustLoad(t, minimal, nil)
	withRecipients := mustLoad(t, with(map[string]string{"AGE_RECIPIENTS": "age1x"}), nil)
	withPassphrase := mustLoad(t, with(map[string]string{"AGE_PASSPHRASE": "p"}), nil)
	withIdentity := mustLoad(t, with(map[string]string{"AGE_IDENTITY": "AGE-SECRET-KEY-1X"}), nil)
	serveReady := mustLoad(t, with(map[string]string{"AGE_RECIPIENTS": "age1x", "BACKUP_SCHEDULE": "@daily"}), nil)

	tests := []struct {
		name    string
		err     error
		wantErr string
	}{
		{"backup without keys", base.ValidateForBackup(), "AGE_RECIPIENTS or AGE_PASSPHRASE"},
		{"backup with recipients", withRecipients.ValidateForBackup(), ""},
		{"backup with passphrase", withPassphrase.ValidateForBackup(), ""},
		{"serve without keys", base.ValidateForServe(), "AGE_RECIPIENTS or AGE_PASSPHRASE"},
		{"serve without schedule", withRecipients.ValidateForServe(), "BACKUP_SCHEDULE is required"},
		{"serve ready", serveReady.ValidateForServe(), ""},
		{"restore without identity", base.ValidateForRestore(), "AGE_IDENTITY or AGE_PASSPHRASE"},
		{"restore with identity", withIdentity.ValidateForRestore(), ""},
		{"restore with passphrase", withPassphrase.ValidateForRestore(), ""},
	}
	for _, tc := range tests {
		switch {
		case tc.wantErr == "" && tc.err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, tc.err)
		case tc.wantErr != "" && tc.err == nil:
			t.Errorf("%s: want error containing %q", tc.name, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(tc.err.Error(), tc.wantErr):
			t.Errorf("%s: error %q does not contain %q", tc.name, tc.err, tc.wantErr)
		case tc.err != nil && !errors.Is(tc.err, ErrInvalid):
			t.Errorf("%s: error does not wrap ErrInvalid", tc.name)
		}
	}
}

func TestLoadHealth(t *testing.T) {
	t.Parallel()
	h, err := LoadHealth(env(map[string]string{"STATE_DIR": "/var/lib/pgbackup", "BACKUP_SCHEDULE": "@daily", "BACKUP_TZ": "Europe/Zurich", "HEALTH_MAX_AGE": "2h"}))
	if err != nil || h.StateDir != "/var/lib/pgbackup" || h.Schedule != "@daily" || h.Location.String() != "Europe/Zurich" || h.MaxAge != 2*time.Hour {
		t.Errorf("LoadHealth = %+v %v", h, err)
	}
	h, err = LoadHealth(env(nil)) // no S3 variables needed
	if err != nil || h.StateDir != "/tmp/pgbackup" || h.Schedule != "" || h.MaxAge != 0 {
		t.Errorf("LoadHealth(defaults) = %+v %v", h, err)
	}
	if _, err := LoadHealth(env(map[string]string{"BACKUP_SCHEDULE": "nope"})); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad schedule: %v", err)
	}
}

func TestRedactedAndString(t *testing.T) {
	t.Parallel()
	secrets := map[string]string{
		"PGPASSWORD":     "pg-secret-1",
		"AGE_PASSPHRASE": "age-secret-2",
		"S3_ACCESS_KEY":  "ak-secret-3",
		"S3_SECRET_KEY":  "sk-secret-4",
	}
	c := mustLoad(t, with(secrets), nil)
	c.Encryption.Identity = "identity-secret-5" // not combinable with passphrase through Load; set directly

	r := c.Redacted()
	for name, got := range map[string]string{
		"Password":   r.Postgres.Password,
		"Passphrase": r.Encryption.Passphrase,
		"Identity":   r.Encryption.Identity,
		"AccessKey":  r.S3.AccessKey,
		"SecretKey":  r.S3.SecretKey,
	} {
		if got != redacted {
			t.Errorf("Redacted().%s = %q, want %q", name, got, redacted)
		}
	}
	if r.S3.Bucket != "pgbackup-test" {
		t.Errorf("Redacted changed a non-secret field: %q", r.S3.Bucket)
	}
	if c.Postgres.Password != "pg-secret-1" {
		t.Error("Redacted modified the receiver")
	}

	s := c.String()
	for _, v := range secrets {
		if strings.Contains(s, v) {
			t.Errorf("String() leaks %q", v)
		}
	}
	if strings.Contains(s, "identity-secret-5") {
		t.Error("String() leaks the identity")
	}
	if !strings.Contains(s, "pgbackup-test") {
		t.Error("String() lacks non-secret content")
	}
	if !strings.Contains(fmt.Sprintf("%v", c), redacted) {
		t.Error("fmt verb v formatting does not go through String()")
	}

	// Unset secrets stay empty rather than becoming "***".
	empty := mustLoad(t, minimal, nil).Redacted()
	if empty.Postgres.Password != "" || empty.Encryption.Passphrase != "" {
		t.Error("unset secrets must stay empty after redaction")
	}
}

func TestRecipientsAreNotRedacted(t *testing.T) {
	t.Parallel()
	c := mustLoad(t, with(map[string]string{"AGE_RECIPIENTS": "age1public"}), nil)
	if c.Redacted().Encryption.Recipients != "age1public" {
		t.Error("public recipients must stay visible in logs")
	}
}

func TestParseSize(t *testing.T) {
	t.Parallel()
	ok := map[string]int64{
		"33554432": 33554432,
		"32MiB":    32 << 20,
		"32mib":    32 << 20,
		"64MB":     64_000_000,
		"1.5GiB":   3 << 29,
		"5m":       5 << 20,
		"10k":      10 << 10,
		"2G":       2 << 30,
		"1GB":      1_000_000_000,
		"100KB":    100_000,
		"512B":     512,
		"0":        0,
		" 8MiB ":   8 << 20,
	}
	for in, want := range ok {
		got, err := parseSize(in)
		if err != nil {
			t.Errorf("parseSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, in := range []string{"", "-1", "-5MiB", "abc", "1.5.5MiB", "MiB", "5 MiB x"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) succeeded, want error", in)
		}
	}
}

func TestFormatSize(t *testing.T) {
	t.Parallel()
	cases := map[int64]string{
		5 << 20: "5MiB",
		5 << 30: "5GiB",
		2048:    "2KiB",
		1536:    "1536B",
		0:       "0B",
	}
	for in, want := range cases {
		if got := formatSize(in); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestSpecs(t *testing.T) {
	t.Parallel()
	all := Specs()
	if len(all) == 0 {
		t.Fatal("no specs")
	}
	all[0].Name = "MUTATED"
	if Specs()[0].Name == "MUTATED" {
		t.Error("Specs() must return a copy")
	}
	for _, s := range Specs() {
		if s.Doc == "" {
			t.Errorf("%s has no documentation", s.Name)
		}
		if s.Name != strings.ToUpper(s.Name) {
			t.Errorf("%s is not upper case", s.Name)
		}
	}
	if !specIndex["PGPASSWORD"].AcceptsFile() || !specIndex["AGE_RECIPIENTS"].AcceptsFile() || specIndex["PGHOST"].AcceptsFile() {
		t.Error("AcceptsFile is wrong for PGPASSWORD / AGE_RECIPIENTS / PGHOST")
	}
}

func TestReaderPanicsOnProgrammingErrors(t *testing.T) {
	t.Parallel()
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: want panic", name)
			}
		}()
		f()
	}
	r := &reader{getenv: env(nil), readFile: files(nil), seen: map[string]bool{}}
	mustPanic("unknown variable", func() { r.str("NOT_A_VARIABLE") })
	mustPanic("secret on plain variable", func() { r.secret("PGHOST") })
}

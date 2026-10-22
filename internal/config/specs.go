package config

// Spec describes one environment variable: its name, default and documentation.
// The table below is the single source of truth for defaults; Load reads every
// variable through it and the README table is generated from it.
type Spec struct {
	Name    string
	Default string
	Doc     string
	// Secret variables are redacted in logs and accept a NAME_FILE alternative.
	Secret bool
	// File variables accept a NAME_FILE alternative without being redacted
	// (for example public keys). Secret implies File.
	File bool
}

// AcceptsFile reports whether NAME_FILE is read for this variable.
func (s Spec) AcceptsFile() bool { return s.Secret || s.File }

// Specs returns the documented environment variables in display order.
func Specs() []Spec {
	out := make([]Spec, len(specs))
	copy(out, specs)
	return out
}

var specs = []Spec{
	// PostgreSQL
	{Name: "PGHOST", Default: "localhost", Doc: "PostgreSQL host (libpq)."},
	{Name: "PGPORT", Default: "5432", Doc: "PostgreSQL port (libpq)."},
	{Name: "PGUSER", Default: "postgres", Doc: "PostgreSQL role used for dumps and restores (libpq)."},
	{Name: "PGPASSWORD", Secret: true, Doc: "Password for PGUSER; passed to child processes through their environment, never on the command line."},
	{Name: "PGDATABASE", Default: "postgres", Doc: "Maintenance database for cluster-level work: listing databases, CREATE/DROP DATABASE, globals. PGUSER needs CONNECT on it."},
	{Name: "PGSSLMODE", Default: "prefer", Doc: "libpq sslmode: disable, allow, prefer, require, verify-ca or verify-full."},
	{Name: "CLUSTER_NAME", Default: "default", Doc: "Logical name of the source cluster; second path segment of every object key."},
	{Name: "BACKUP_DATABASES", Default: "all", Doc: "Comma-separated database names, or `all` for every non-template database that allows connections."},
	{Name: "BACKUP_EXCLUDE_DATABASES", Doc: "Comma-separated databases to skip when BACKUP_DATABASES=all."},

	// Dump
	{Name: "BACKUP_COMPRESSION", Default: "zstd:3", Doc: "pg_dump --compress value: zstd[:level], gzip[:level], lz4[:level] or none. zstd and lz4 need a PostgreSQL 16+ client."},
	{Name: "BACKUP_LOCK_WAIT_TIMEOUT", Default: "1m", Doc: "pg_dump --lock-wait-timeout; how long to wait for table locks before giving up."},
	{Name: "BACKUP_TIMEOUT", Default: "6h", Doc: "Deadline for one whole backup run (all databases)."},
	{Name: "BACKUP_PARALLEL", Default: "1", Doc: "How many databases are dumped at the same time."},
	{Name: "BACKUP_RETRIES", Default: "3", Doc: "Attempts per database on transient (network/S3) errors; pg_dump failures are never retried."},

	// Encryption
	{Name: "AGE_RECIPIENTS", File: true, Doc: "Comma- or newline-separated age public keys (age1...). Required for backup unless AGE_PASSPHRASE is set."},
	{Name: "AGE_PASSPHRASE", Secret: true, Doc: "Passphrase for scrypt-based age encryption; alternative to AGE_RECIPIENTS. Also used to decrypt on restore."},
	{Name: "AGE_IDENTITY", Secret: true, Doc: "age private key(s) (AGE-SECRET-KEY-1...) for restore and verify."},

	// S3
	{Name: "S3_ENDPOINT", Doc: "Endpoint URL, e.g. https://s3.example.com or http://garage:3900. The scheme selects TLS. Required."},
	{Name: "S3_REGION", Default: "us-east-1", Doc: "Region used for request signing; some servers validate it."},
	{Name: "S3_BUCKET", Doc: "Bucket name. Required; the bucket must already exist."},
	{Name: "S3_PREFIX", Doc: "Key prefix inside the bucket, without leading or trailing slash."},
	{Name: "S3_ACCESS_KEY", Secret: true, Doc: "Access key ID. Required."},
	{Name: "S3_SECRET_KEY", Secret: true, Doc: "Secret access key. Required."},
	{Name: "S3_PATH_STYLE", Default: "true", Doc: "Use path-style addressing (bucket in the path). Most self-hosted servers need true; AWS accepts both."},
	{Name: "S3_PART_SIZE", Default: "32MiB", Doc: "Multipart part size, 5MiB..5GiB. Bounds memory use per upload."},
	{Name: "S3_TLS_INSECURE", Default: "false", Doc: "Skip TLS certificate verification. Test stands only."},

	// Schedule and retention
	{Name: "BACKUP_SCHEDULE", Doc: "Cron expression (5 fields) for scheduled backups; required by `serve`."},
	{Name: "BACKUP_TZ", Default: "UTC", Doc: "IANA time zone the schedules are evaluated in."},
	{Name: "VERIFY_SCHEDULE", Doc: "Optional cron expression for `verify --full` of the latest backup of every database."},
	{Name: "RUN_ON_START", Default: "false", Doc: "Run one backup immediately when `serve` starts."},
	{Name: "PRUNE_AFTER_BACKUP", Default: "true", Doc: "Apply the retention policy after every successful scheduled backup."},
	{Name: "RETENTION_KEEP_LAST", Default: "7", Doc: "Always keep the newest N backups of each database."},
	{Name: "RETENTION_KEEP_DAILY", Default: "0", Doc: "Keep the newest backup of each of the last N days (0 disables)."},
	{Name: "RETENTION_KEEP_WEEKLY", Default: "0", Doc: "Keep the newest backup of each of the last N ISO weeks (0 disables)."},
	{Name: "RETENTION_KEEP_MONTHLY", Default: "0", Doc: "Keep the newest backup of each of the last N months (0 disables)."},

	// Runtime and observability
	{Name: "STATE_DIR", Default: "/tmp/pgbackup", Doc: "Directory for the heartbeat file read by `health`."},
	{Name: "SHUTDOWN_TIMEOUT", Default: "30s", Doc: "How long `serve` waits for a running job after SIGTERM before exiting."},
	{Name: "HEALTH_MAX_AGE", Default: "0", Doc: "`health` fails if the last success is older than this. 0 = twice the schedule interval, capped at 26h."},
	{Name: "LOG_LEVEL", Default: "info", Doc: "debug, info, warn or error."},
	{Name: "LOG_FORMAT", Default: "json", Doc: "json or text."},
	{Name: "METRICS_ADDR", Doc: "host:port for /healthz and /metrics in `serve`; empty disables the HTTP server."},
	{Name: "NOTIFY_URL_SUCCESS", Doc: "URL pinged after a successful run."},
	{Name: "NOTIFY_URL_FAILURE", Doc: "URL pinged after a failed run."},
	{Name: "NOTIFY_METHOD", Default: "POST", Doc: "POST (JSON summary body) or GET."},
	{Name: "PGBACKUP_YES", Default: "false", Doc: "Equivalent to --yes on restore; for non-interactive automation."},
}

var specIndex = func() map[string]Spec {
	m := make(map[string]Spec, len(specs))
	for _, s := range specs {
		if _, dup := m[s.Name]; dup {
			panic("config: duplicate spec " + s.Name)
		}
		m[s.Name] = s
	}
	return m
}()

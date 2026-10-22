// Package config turns environment variables into a validated Config.
//
// Every variable is described in specs.go; Load reads them through that table
// so defaults and documentation cannot drift from the code. Secrets accept a
// NAME_FILE alternative pointing to a file (Docker and Kubernetes secrets).
// Load reports every problem at once, wrapped in ErrInvalid.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// ErrInvalid wraps every validation error returned by Load. Commands map it
// to exit code 2.
var ErrInvalid = errors.New("invalid configuration")

const (
	minPartSize = 5 << 20 // S3 minimum multipart part size
	maxPartSize = 5 << 30 // S3 maximum part size
	redacted    = "***"
)

var compressionRE = regexp.MustCompile(`^(zstd|gzip|lz4|none)(:\d+)?$`)

// Config is the complete, validated configuration of one process.
type Config struct {
	Postgres   Postgres
	Backup     Backup
	Encryption Encryption
	S3         S3
	Schedule   Schedule
	Retention  Retention
	Runtime    Runtime
}

// Postgres holds libpq connection settings.
type Postgres struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string // maintenance database
	SSLMode  string
}

// Backup holds dump settings.
type Backup struct {
	ClusterName      string
	AllDatabases     bool
	Databases        []string // explicit list when AllDatabases is false
	ExcludeDatabases []string // only meaningful when AllDatabases is true
	Compression      string   // pg_dump --compress value, e.g. "zstd:3"
	LockWaitTimeout  time.Duration
	Timeout          time.Duration
	Parallel         int
	Retries          int
}

// Encryption holds age key material as raw strings; internal/crypt parses them.
type Encryption struct {
	Recipients string
	Passphrase string
	Identity   string
}

// S3 holds object store settings.
type S3 struct {
	Endpoint    string // normalised URL without trailing slash
	UseTLS      bool   // derived from the endpoint scheme
	Region      string
	Bucket      string
	Prefix      string // no leading or trailing slash; may be empty
	AccessKey   string
	SecretKey   string
	PathStyle   bool
	PartSize    int64
	TLSInsecure bool
}

// Schedule holds scheduler settings for `serve`.
type Schedule struct {
	Backup           string // cron expression; empty when not configured
	Verify           string // cron expression; empty disables
	Location         *time.Location
	RunOnStart       bool
	PruneAfterBackup bool
}

// Retention holds the pruning policy. Zero disables a rule.
type Retention struct {
	KeepLast    int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
}

// Runtime holds process-level settings.
type Runtime struct {
	StateDir         string
	ShutdownTimeout  time.Duration
	HealthMaxAge     time.Duration // 0 = derive from the schedule
	LogLevel         slog.Level
	LogFormat        string // json or text
	MetricsAddr      string // empty disables the HTTP server
	NotifyURLSuccess string
	NotifyURLFailure string
	NotifyMethod     string // POST or GET
	Yes              bool
}

// Load reads the configuration through getenv and readFile (for NAME_FILE
// secrets). It validates everything that does not depend on the subcommand
// and returns all problems joined under ErrInvalid.
func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	r := &reader{getenv: getenv, readFile: readFile, seen: map[string]bool{}}
	var c Config

	c.Postgres = Postgres{
		Host:     r.str("PGHOST"),
		Port:     r.integer("PGPORT", 1, 65535),
		User:     r.str("PGUSER"),
		Password: r.secret("PGPASSWORD"),
		Database: r.str("PGDATABASE"),
		SSLMode:  r.oneOf("PGSSLMODE", "disable", "allow", "prefer", "require", "verify-ca", "verify-full"),
	}

	c.Backup = Backup{
		ClusterName:     r.str("CLUSTER_NAME"),
		Compression:     r.str("BACKUP_COMPRESSION"),
		LockWaitTimeout: r.duration("BACKUP_LOCK_WAIT_TIMEOUT", 0),
		Timeout:         r.duration("BACKUP_TIMEOUT", time.Minute),
		Parallel:        r.integer("BACKUP_PARALLEL", 1, 64),
		Retries:         r.integer("BACKUP_RETRIES", 0, 100),
	}
	if strings.ContainsAny(c.Backup.ClusterName, "/\\ ") {
		r.errorf("CLUSTER_NAME", "%q must not contain slashes or spaces", c.Backup.ClusterName)
	}
	if !compressionRE.MatchString(c.Backup.Compression) {
		r.errorf("BACKUP_COMPRESSION", "%q must match zstd|gzip|lz4|none with an optional :level", c.Backup.Compression)
	}
	dbs := r.list("BACKUP_DATABASES")
	excl := r.list("BACKUP_EXCLUDE_DATABASES")
	switch {
	case len(dbs) == 1 && strings.EqualFold(dbs[0], "all"):
		c.Backup.AllDatabases = true
		c.Backup.ExcludeDatabases = excl
	case len(dbs) == 0:
		r.errorf("BACKUP_DATABASES", "must be `all` or a comma-separated list of databases")
	default:
		for _, d := range dbs {
			if strings.EqualFold(d, "all") {
				r.errorf("BACKUP_DATABASES", "`all` cannot be combined with database names")
			}
		}
		c.Backup.Databases = dbs
		if len(excl) > 0 {
			r.errorf("BACKUP_EXCLUDE_DATABASES", "only applies when BACKUP_DATABASES=all")
		}
	}

	c.Encryption = Encryption{
		Recipients: r.secret("AGE_RECIPIENTS"),
		Passphrase: r.secret("AGE_PASSPHRASE"),
		Identity:   r.secret("AGE_IDENTITY"),
	}
	if c.Encryption.Recipients != "" && c.Encryption.Passphrase != "" {
		r.errorf("AGE_RECIPIENTS", "cannot be combined with AGE_PASSPHRASE; choose one encryption mode")
	}

	c.S3 = S3{
		Region:      r.str("S3_REGION"),
		Bucket:      r.str("S3_BUCKET"),
		Prefix:      strings.Trim(r.str("S3_PREFIX"), "/"),
		AccessKey:   r.secret("S3_ACCESS_KEY"),
		SecretKey:   r.secret("S3_SECRET_KEY"),
		PathStyle:   r.boolean("S3_PATH_STYLE"),
		PartSize:    r.size("S3_PART_SIZE", minPartSize, maxPartSize),
		TLSInsecure: r.boolean("S3_TLS_INSECURE"),
	}
	c.S3.Endpoint, c.S3.UseTLS = parseEndpoint(r, r.str("S3_ENDPOINT"))
	if c.S3.Bucket == "" {
		r.errorf("S3_BUCKET", "is required")
	}
	if c.S3.AccessKey == "" {
		r.errorf("S3_ACCESS_KEY", "is required")
	}
	if c.S3.SecretKey == "" {
		r.errorf("S3_SECRET_KEY", "is required")
	}

	c.Schedule = Schedule{
		Backup:           parseCron(r, "BACKUP_SCHEDULE"),
		Verify:           parseCron(r, "VERIFY_SCHEDULE"),
		Location:         parseLocation(r, "BACKUP_TZ"),
		RunOnStart:       r.boolean("RUN_ON_START"),
		PruneAfterBackup: r.boolean("PRUNE_AFTER_BACKUP"),
	}

	c.Retention = Retention{
		KeepLast:    r.integer("RETENTION_KEEP_LAST", 0, 100000),
		KeepDaily:   r.integer("RETENTION_KEEP_DAILY", 0, 100000),
		KeepWeekly:  r.integer("RETENTION_KEEP_WEEKLY", 0, 100000),
		KeepMonthly: r.integer("RETENTION_KEEP_MONTHLY", 0, 100000),
	}

	c.Runtime = Runtime{
		StateDir:         r.str("STATE_DIR"),
		ShutdownTimeout:  r.duration("SHUTDOWN_TIMEOUT", 0),
		HealthMaxAge:     r.duration("HEALTH_MAX_AGE", 0),
		LogLevel:         parseLogLevel(r, "LOG_LEVEL"),
		LogFormat:        r.oneOf("LOG_FORMAT", "json", "text"),
		MetricsAddr:      parseHostPort(r, "METRICS_ADDR"),
		NotifyURLSuccess: parseHTTPURL(r, "NOTIFY_URL_SUCCESS"),
		NotifyURLFailure: parseHTTPURL(r, "NOTIFY_URL_FAILURE"),
		NotifyMethod:     r.oneOf("NOTIFY_METHOD", "POST", "GET"),
		Yes:              r.boolean("PGBACKUP_YES"),
	}

	for _, s := range specs {
		if !r.seen[s.Name] {
			panic("config: Load does not read " + s.Name)
		}
	}
	return c, r.err()
}

// Health is the subset of the configuration the `health` command needs, so
// a probe does not have to carry S3 credentials to ask "is the last backup
// recent enough?".
type Health struct {
	StateDir string
	Schedule string
	Location *time.Location
	MaxAge   time.Duration // 0 = derive from the schedule
}

// LoadHealth reads only the health-related variables.
func LoadHealth(getenv func(string) string) (Health, error) {
	r := &reader{getenv: getenv, readFile: func(string) ([]byte, error) { return nil, os.ErrNotExist }, seen: map[string]bool{}}
	h := Health{
		StateDir: r.str("STATE_DIR"),
		Schedule: parseCron(r, "BACKUP_SCHEDULE"),
		Location: parseLocation(r, "BACKUP_TZ"),
		MaxAge:   r.duration("HEALTH_MAX_AGE", 0),
	}
	return h, r.err()
}

// ValidateForBackup reports what `backup` and `serve` need beyond Load.
func (c Config) ValidateForBackup() error {
	if c.Encryption.Recipients == "" && c.Encryption.Passphrase == "" {
		return fmt.Errorf("%w: AGE_RECIPIENTS or AGE_PASSPHRASE is required for backup", ErrInvalid)
	}
	return nil
}

// ValidateForServe reports what `serve` needs beyond ValidateForBackup.
func (c Config) ValidateForServe() error {
	if err := c.ValidateForBackup(); err != nil {
		return err
	}
	if c.Schedule.Backup == "" {
		return fmt.Errorf("%w: BACKUP_SCHEDULE is required for serve", ErrInvalid)
	}
	return nil
}

// ValidateForRestore reports what `restore` and `verify` need beyond Load.
func (c Config) ValidateForRestore() error {
	if c.Encryption.Identity == "" && c.Encryption.Passphrase == "" {
		return fmt.Errorf("%w: AGE_IDENTITY or AGE_PASSPHRASE is required to decrypt", ErrInvalid)
	}
	return nil
}

// Redacted returns a copy with every secret replaced by "***" when set.
func (c Config) Redacted() Config {
	hide := func(s *string) {
		if *s != "" {
			*s = redacted
		}
	}
	out := c
	hide(&out.Postgres.Password)
	hide(&out.Encryption.Passphrase)
	hide(&out.Encryption.Identity)
	hide(&out.S3.AccessKey)
	hide(&out.S3.SecretKey)
	return out
}

// String prints the redacted configuration; it is the only stringer so a
// Config can never leak secrets through %v or a logger.
func (c Config) String() string {
	// plain has no String method, so formatting it does not recurse.
	type plain Config
	return fmt.Sprintf("%+v", plain(c.Redacted()))
}

func parseEndpoint(r *reader, v string) (endpoint string, useTLS bool) {
	const name = "S3_ENDPOINT"
	if v == "" {
		r.errorf(name, "is required (e.g. https://s3.example.com)")
		return "", false
	}
	u, err := url.Parse(v)
	if err != nil {
		r.errorf(name, "%v", err)
		return "", false
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		r.errorf(name, "scheme must be http or https, got %q", u.Scheme)
	case u.Host == "":
		r.errorf(name, "%q has no host", v)
	case u.RawQuery != "" || u.Fragment != "" || u.User != nil:
		r.errorf(name, "%q must not contain credentials, a query or a fragment", v)
	case strings.Trim(u.Path, "/") != "":
		r.errorf(name, "%q must not contain a path", v)
	}
	return u.Scheme + "://" + u.Host, u.Scheme == "https"
}

func parseCron(r *reader, name string) string {
	v := r.str(name)
	if v == "" {
		return ""
	}
	if _, err := cron.ParseStandard(v); err != nil {
		r.errorf(name, "%q is not a valid cron expression: %v", v, err)
		return ""
	}
	return v
}

func parseLocation(r *reader, name string) *time.Location {
	v := r.str(name)
	loc, err := time.LoadLocation(v)
	if err != nil {
		r.errorf(name, "unknown time zone %q", v)
		return time.UTC
	}
	return loc
}

func parseLogLevel(r *reader, name string) slog.Level {
	switch r.oneOf(name, "info", "debug", "warn", "error") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func parseHostPort(r *reader, name string) string {
	v := r.str(name)
	if v == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(v); err != nil {
		r.errorf(name, "%q is not host:port", v)
		return ""
	}
	return v
}

func parseHTTPURL(r *reader, name string) string {
	v := r.str(name)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		r.errorf(name, "%q is not an http(s) URL", v)
		return ""
	}
	return v
}

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"filippo.io/age"

	"github.com/rapalon/pg-backup-s3/internal/config"
	"github.com/rapalon/pg-backup-s3/internal/crypt"
	"github.com/rapalon/pg-backup-s3/internal/manifest"
	"github.com/rapalon/pg-backup-s3/internal/pg"
	"github.com/rapalon/pg-backup-s3/internal/pipeline"
	"github.com/rapalon/pg-backup-s3/internal/store"
	"github.com/rapalon/pg-backup-s3/internal/version"
)

// osGetenv and osReadFile are the configuration sources; tests replace them.
var (
	osGetenv   = os.Getenv
	osReadFile = os.ReadFile
)

// app holds what every command needs after the configuration is loaded.
type app struct {
	cfg    config.Config
	logger *slog.Logger
	store  *store.S3Store
	runner *pg.Runner
	conn   pg.Conn
}

// loadApp reads the configuration, applies the command's extra validation,
// builds the logger, the store (and pings the bucket) and the runner.
func loadApp(ctx context.Context, stderr io.Writer, validate func(config.Config) error) (*app, error) {
	cfg, err := config.Load(osGetenv, osReadFile)
	if err != nil {
		return nil, err
	}
	if validate != nil {
		if err := validate(cfg); err != nil {
			return nil, err
		}
	}
	logger := newLogger(stderr, cfg.Runtime.LogLevel, cfg.Runtime.LogFormat)
	logger.Debug("configuration loaded", "config", cfg.String(), "version", version.Version)

	st, err := store.NewS3(store.S3Config{
		Endpoint:    cfg.S3.Endpoint,
		Region:      cfg.S3.Region,
		Bucket:      cfg.S3.Bucket,
		AccessKey:   cfg.S3.AccessKey,
		SecretKey:   cfg.S3.SecretKey,
		PathStyle:   cfg.S3.PathStyle,
		PartSize:    cfg.S3.PartSize,
		TLSInsecure: cfg.S3.TLSInsecure,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", config.ErrInvalid, err)
	}
	if err := st.Ping(ctx); err != nil {
		if errors.Is(err, store.ErrBucketNotFound) {
			return nil, &exitError{code: exitUsage, err: err}
		}
		return nil, err
	}

	return &app{
		cfg:    cfg,
		logger: logger,
		store:  st,
		runner: pg.NewRunner(logger),
		conn: pg.Conn{
			Host:     cfg.Postgres.Host,
			Port:     cfg.Postgres.Port,
			User:     cfg.Postgres.User,
			Password: cfg.Postgres.Password,
			Database: cfg.Postgres.Database,
			SSLMode:  cfg.Postgres.SSLMode,
		},
	}, nil
}

// backupDeps builds the pipeline dependencies for backup commands.
func (a *app) backupDeps() (pipeline.Deps, error) {
	recipients, err := crypt.RecipientsFor(a.cfg.Encryption.Recipients, a.cfg.Encryption.Passphrase)
	if err != nil {
		return pipeline.Deps{}, fmt.Errorf("%w: %v", config.ErrInvalid, err)
	}
	enc := manifest.Encryption{Type: "age"}
	if a.cfg.Encryption.Passphrase != "" {
		enc.Passphrase = true
	} else {
		for _, r := range recipients {
			if x, ok := r.(*age.X25519Recipient); ok {
				enc.Recipients = append(enc.Recipients, x.String())
			}
		}
	}
	return pipeline.Deps{
		Logger:          a.logger,
		DB:              a.runner,
		Store:           a.store,
		Conn:            a.conn,
		Recipients:      recipients,
		Encryption:      enc,
		Prefix:          a.cfg.S3.Prefix,
		Cluster:         a.cfg.Backup.ClusterName,
		Compression:     a.cfg.Backup.Compression,
		LockWaitTimeout: a.cfg.Backup.LockWaitTimeout,
		PartSize:        a.cfg.S3.PartSize,
		ToolVersion:     version.Version,
	}, nil
}

// runOptions builds the run options from the configuration; dbs, when
// non-empty, overrides BACKUP_DATABASES.
func (a *app) runOptions(dbs []string) pipeline.RunOptions {
	opts := pipeline.RunOptions{
		AllDatabases: a.cfg.Backup.AllDatabases,
		Databases:    a.cfg.Backup.Databases,
		Exclude:      a.cfg.Backup.ExcludeDatabases,
		Parallel:     a.cfg.Backup.Parallel,
		Retry:        pipeline.DefaultRetryPolicy(a.cfg.Backup.Retries),
		Timeout:      a.cfg.Backup.Timeout,
		StateDir:     a.cfg.Runtime.StateDir,
	}
	if len(dbs) > 0 {
		opts.AllDatabases = false
		opts.Databases = dbs
		opts.Exclude = nil
	}
	return opts
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

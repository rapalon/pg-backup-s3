// Package manifest defines the JSON document stored next to every backup
// object and the layout of object keys in the bucket.
//
// Manifests are plaintext. list, verify, restore and prune work from them,
// never from guessing key names; the dump object is only ever located
// through its manifest's Key field.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// FormatVersion is the manifest schema version this binary writes and reads.
const FormatVersion = 1

// ErrUnsupportedFormat is returned by Unmarshal for a newer or unknown schema.
var ErrUnsupportedFormat = errors.New("unsupported manifest format_version")

// Kind distinguishes database dumps from the cluster globals file.
type Kind string

const (
	KindDump    Kind = "dump"
	KindGlobals Kind = "globals"
)

// Encryption records how the object was encrypted. Only public material.
type Encryption struct {
	Type string `json:"type"` // always "age"
	// Recipients are the X25519 public keys the object was encrypted to.
	// Empty when Passphrase is true.
	Recipients []string `json:"recipients"`
	// Passphrase is true for scrypt (passphrase) encryption.
	Passphrase bool `json:"passphrase,omitempty"`
}

// Manifest describes one stored backup object (schema format_version 1).
type Manifest struct {
	FormatVersion   int        `json:"format_version"`
	ToolVersion     string     `json:"tool_version"`
	Cluster         string     `json:"cluster"`
	Database        string     `json:"database"` // empty for globals
	Kind            Kind       `json:"kind"`
	Key             string     `json:"key"`
	CreatedAt       time.Time  `json:"created_at"`
	DurationSeconds float64    `json:"duration_seconds"`
	ServerVersion   string     `json:"server_version"`
	PgDumpVersion   string     `json:"pg_dump_version"`
	DumpFormat      string     `json:"dump_format"` // "custom" or "plain"
	Compression     string     `json:"compression"`
	SizeBytes       int64      `json:"size_bytes"`
	SHA256          string     `json:"sha256"` // of the stored ciphertext
	Encryption      Encryption `json:"encryption"`
	GlobalsKey      string     `json:"globals_key,omitempty"`
}

// Marshal renders the manifest as indented JSON.
func (m Manifest) Marshal() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Unmarshal parses a manifest and rejects unsupported schema versions.
func Unmarshal(b []byte) (Manifest, error) {
	var probe struct {
		FormatVersion int `json:"format_version"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if probe.FormatVersion != FormatVersion {
		return Manifest{}, fmt.Errorf("%w: %d (this build reads %d)", ErrUnsupportedFormat, probe.FormatVersion, FormatVersion)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Validate checks the fields every consumer relies on.
func (m Manifest) Validate() error {
	var problems []error
	add := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }
	if m.FormatVersion != FormatVersion {
		add("format_version %d is not %d", m.FormatVersion, FormatVersion)
	}
	switch m.Kind {
	case KindDump:
		if m.Database == "" {
			add("database is empty for a dump manifest")
		}
	case KindGlobals:
		if m.Database != "" {
			add("database must be empty for a globals manifest")
		}
	default:
		add("unknown kind %q", m.Kind)
	}
	if m.Cluster == "" {
		add("cluster is empty")
	}
	if m.Key == "" {
		add("key is empty")
	}
	if m.CreatedAt.IsZero() {
		add("created_at is zero")
	}
	if len(m.SHA256) != 64 {
		add("sha256 %q is not a hex SHA-256", m.SHA256)
	}
	if m.SizeBytes < 0 {
		add("size_bytes is negative")
	}
	if m.Encryption.Type != "age" {
		add("encryption.type %q is not age", m.Encryption.Type)
	}
	if !m.Encryption.Passphrase && len(m.Encryption.Recipients) == 0 {
		add("encryption has neither recipients nor passphrase")
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("manifest invalid: %w", errors.Join(problems...))
}

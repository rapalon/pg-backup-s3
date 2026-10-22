package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HeartbeatFile holds the RFC 3339 time of the last fully successful run.
const HeartbeatFile = "last_success"

// WriteHeartbeat records t atomically (temp file + rename) in dir.
func WriteHeartbeat(dir string, t time.Time) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, HeartbeatFile+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.WriteString(t.UTC().Format(time.RFC3339) + "\n")
	serr := tmp.Sync()
	cerr := tmp.Close()
	for _, e := range []error{werr, serr, cerr} {
		if e != nil {
			os.Remove(tmpName)
			return e
		}
	}
	if err := os.Rename(tmpName, filepath.Join(dir, HeartbeatFile)); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// ReadHeartbeat returns the recorded time, or an error if there is none.
func ReadHeartbeat(dir string) (time.Time, error) {
	b, err := os.ReadFile(filepath.Join(dir, HeartbeatFile))
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}, fmt.Errorf("heartbeat: %w", err)
	}
	return t, nil
}

package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewLogger(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	l := newLogger(&buf, slog.LevelInfo, "json")
	l.Debug("hidden")
	l.Info("shown", "k", "v")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not one JSON record: %q: %v", buf.String(), err)
	}
	if rec["msg"] != "shown" || rec["k"] != "v" || rec["level"] != "INFO" {
		t.Errorf("record = %v", rec)
	}

	buf.Reset()
	l = newLogger(&buf, slog.LevelDebug, "text")
	l.Debug("dbg")
	if !strings.Contains(buf.String(), "level=DEBUG msg=dbg") {
		t.Errorf("text output = %q", buf.String())
	}
}

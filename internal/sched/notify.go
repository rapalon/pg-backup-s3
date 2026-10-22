package sched

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/pipeline"
)

// Summary is the JSON body sent to the notification URLs.
type Summary struct {
	Event           string    `json:"event"` // "backup" or "verify"
	Result          string    `json:"result"`
	Cluster         string    `json:"cluster"`
	StartedAt       time.Time `json:"started_at"`
	DurationSeconds float64   `json:"duration_seconds"`
	Databases       int       `json:"databases,omitempty"`
	Failed          []string  `json:"failed,omitempty"`
	Bytes           int64     `json:"bytes,omitempty"`
	Error           string    `json:"error,omitempty"`
	ToolVersion     string    `json:"tool_version,omitempty"`
}

// Notifier pings a URL after every run. Failures are logged, never returned:
// a notification must not change the outcome of a backup.
type Notifier struct {
	SuccessURL string
	FailureURL string
	Method     string // POST (JSON body) or GET
	Client     *http.Client
	Logger     *slog.Logger
}

func (n Notifier) enabled() bool { return n.SuccessURL != "" || n.FailureURL != "" }

// Notify sends the summary to the URL matching its result.
func (n Notifier) Notify(ctx context.Context, s Summary) {
	url := n.FailureURL
	if s.Result == "success" {
		url = n.SuccessURL
	}
	if url == "" {
		return
	}
	log := n.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	var req *http.Request
	var err error
	if n.Method == "GET" {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	} else {
		body, _ := json.Marshal(s)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		log.Warn("notification not sent", "url", url, "err", err)
		return
	}
	req.Header.Set("User-Agent", "pgbackup")
	resp, err := client.Do(req)
	if err != nil {
		log.Warn("notification failed", "url", url, "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Warn("notification rejected", "url", url, "status", resp.StatusCode)
		return
	}
	log.Debug("notification sent", "url", url, "result", s.Result)
}

func summarize(cluster, toolVersion string, report pipeline.Report, err error) Summary {
	s := Summary{Event: "backup", Cluster: cluster, StartedAt: report.Time, DurationSeconds: report.Duration.Seconds(),
		Databases: len(report.Databases), ToolVersion: toolVersion, Result: "success"}
	for _, d := range report.Databases {
		s.Bytes += d.Manifest.SizeBytes
	}
	switch {
	case err != nil:
		s.Result, s.Error = "failure", err.Error()
	case !report.OK():
		s.Result = "failure"
		for _, f := range report.Failed() {
			s.Failed = append(s.Failed, f.Name)
		}
	}
	return s
}

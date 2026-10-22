// Package pg runs the PostgreSQL client tools (pg_dump, pg_dumpall,
// pg_restore, psql, vacuumdb) safely and observably. It never parses archive
// formats itself: it orchestrates the tools that ship with PostgreSQL.
package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultKillGrace is how long a child gets after SIGTERM before SIGKILL.
const DefaultKillGrace = 10 * time.Second

// stderrTailLimit bounds how much stderr is kept for error messages.
const stderrTailLimit = 64 << 10

// inheritedEnv lists the host variables children receive. Everything else,
// in particular our own S3 and age secrets, stays out of child processes.
var inheritedEnv = []string{"PATH", "HOME", "TMPDIR", "TZ", "LANG", "LC_ALL", "LC_CTYPE", "LC_MESSAGES"}

// Spec describes one child process.
type Spec struct {
	Binary string   // name on PATH, e.g. "pg_dump"
	Args   []string // arguments, never containing secrets
	Env    []string // KEY=VALUE pairs appended to the inherited environment
	Stdin  io.Reader
	Stdout io.Writer
	// OnStderr, when set, receives every complete stderr line in addition
	// to the logging and the tail kept for errors.
	OnStderr func(line string)
}

// ExitError reports a child that exited with a non-zero status.
type ExitError struct {
	Binary     string
	Code       int    // -1 when killed by a signal
	StderrTail string // last stderrTailLimit bytes of stderr
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s exited with code %d", e.Binary, e.Code)
	if last := lastLine(e.StderrTail); last != "" {
		msg += ": " + last
	}
	return msg
}

// Process is a started child whose stdout is streamed to the caller.
type Process struct {
	// Stdout is the child's standard output. Read it to EOF, then call Wait.
	// Closing it early makes the child fail with EPIPE.
	Stdout io.ReadCloser
	wait   func() error
	once   sync.Once
	err    error
}

// NewProcess wraps a stdout stream and a wait function as a Process. The
// Runner does this for real children; fakes in other packages' tests use it.
func NewProcess(stdout io.ReadCloser, wait func() error) *Process {
	return &Process{Stdout: stdout, wait: wait}
}

// Wait waits for the child and returns nil, an *ExitError, or the context
// error when the context was cancelled. It is safe to call more than once.
func (p *Process) Wait() error {
	p.once.Do(func() { p.err = p.wait() })
	return p.err
}

// Runner starts PostgreSQL client tools.
type Runner struct {
	Logger    *slog.Logger
	KillGrace time.Duration

	// test hooks
	lookPath  func(string) (string, error)
	argPrefix []string // inserted before Binary; used to re-exec the test binary
	baseEnv   func() []string

	versions sync.Map // binary -> Version
}

// NewRunner returns a Runner that finds binaries on PATH.
func NewRunner(logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Runner{
		Logger:    logger,
		KillGrace: DefaultKillGrace,
		lookPath:  exec.LookPath,
		baseEnv:   hostEnv,
	}
}

func hostEnv() []string {
	var env []string
	for _, k := range inheritedEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Run starts the child, waits for it and returns like Process.Wait.
func (r *Runner) Run(ctx context.Context, spec Spec) error {
	p, err := r.start(ctx, spec, false)
	if err != nil {
		return err
	}
	return p.Wait()
}

// Start starts the child with its stdout piped to the returned Process.
func (r *Runner) Start(ctx context.Context, spec Spec) (*Process, error) {
	return r.start(ctx, spec, true)
}

func (r *Runner) start(ctx context.Context, spec Spec, pipeStdout bool) (*Process, error) {
	path, err := r.lookPath(spec.Binary)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", spec.Binary, err)
	}

	args := make([]string, 0, len(r.argPrefix)+1+len(spec.Args))
	if len(r.argPrefix) > 0 {
		args = append(args, r.argPrefix...)
		args = append(args, spec.Binary)
	}
	args = append(args, spec.Args...)

	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(r.baseEnv(), spec.Env...)
	cmd.Stdin = spec.Stdin
	sink := newStderrSink(r.Logger, spec.Binary, spec.OnStderr)
	cmd.Stderr = sink
	cmd.WaitDelay = r.KillGrace
	cmd.Cancel = func() error {
		// Ask nicely first; exec sends SIGKILL itself once WaitDelay passes.
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}

	var stdout io.ReadCloser
	if pipeStdout {
		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("%s: stdout pipe: %w", spec.Binary, err)
		}
	} else {
		cmd.Stdout = spec.Stdout
	}

	r.Logger.Debug("exec", "binary", spec.Binary, "args", spec.Args)
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", spec.Binary, err)
	}

	wait := func() error {
		err := cmd.Wait()
		sink.flush()
		r.Logger.Debug("exited", "binary", spec.Binary, "duration", time.Since(started), "err", err)
		return r.wrapExit(ctx, spec.Binary, err, sink)
	}
	return &Process{Stdout: stdout, wait: wait}, nil
}

func (r *Runner) wrapExit(ctx context.Context, binary string, err error, sink *stderrSink) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", binary, ctx.Err())
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &ExitError{Binary: binary, Code: ee.ExitCode(), StderrTail: sink.tail()}
	}
	return fmt.Errorf("%s: %w", binary, err)
}

// stderrSink logs each complete stderr line and keeps a bounded tail.
type stderrSink struct {
	logger  *slog.Logger
	binary  string
	onLine  func(string)
	mu      sync.Mutex
	pending []byte
	tailBuf []byte
}

func newStderrSink(logger *slog.Logger, binary string, onLine func(string)) *stderrSink {
	return &stderrSink{logger: logger, binary: binary, onLine: onLine}
}

func (s *stderrSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, p...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		s.emit(s.pending[:i])
		s.pending = s.pending[i+1:]
	}
	return len(p), nil
}

// flush logs a trailing partial line once the process has exited.
func (s *stderrSink) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 0 {
		s.emit(s.pending)
		s.pending = nil
	}
}

func (s *stderrSink) emit(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if len(line) == 0 {
		return
	}
	s.logger.Warn(string(line), "binary", s.binary)
	if s.onLine != nil {
		s.onLine(string(line))
	}
	s.tailBuf = append(s.tailBuf, line...)
	s.tailBuf = append(s.tailBuf, '\n')
	if len(s.tailBuf) > stderrTailLimit {
		s.tailBuf = s.tailBuf[len(s.tailBuf)-stderrTailLimit:]
	}
}

func (s *stderrSink) tail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.tailBuf)
}

// lastLine returns the last non-blank line of s, trimmed.
func lastLine(s string) string {
	s = strings.TrimRight(s, " \t\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

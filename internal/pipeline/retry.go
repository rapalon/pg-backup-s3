package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"

	"github.com/rapalon/pg-backup-s3/internal/crypt"
	"github.com/rapalon/pg-backup-s3/internal/pg"
	"github.com/rapalon/pg-backup-s3/internal/store"
)

// RetryPolicy is exponential backoff with jitter.
type RetryPolicy struct {
	Attempts int           // total attempts, at least 1
	Base     time.Duration // delay after the first failure
	Factor   float64       // growth per failure
	Max      time.Duration // delay cap

	// Sleep waits for d or until ctx is done. Tests make it instant.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter scales a delay; nil means ±20 % uniform.
	Jitter func(d time.Duration) time.Duration
}

// DefaultRetryPolicy is plan.md's policy: 5 s base, ×2, capped at 5 min.
func DefaultRetryPolicy(attempts int) RetryPolicy {
	return RetryPolicy{Attempts: attempts, Base: 5 * time.Second, Factor: 2, Max: 5 * time.Minute}
}

// Delay returns the wait before attempt number next (1-based: the delay
// before the second attempt is Delay(2)).
func (p RetryPolicy) Delay(next int) time.Duration {
	if next < 2 {
		return 0
	}
	d := float64(p.Base) * math.Pow(p.Factor, float64(next-2))
	if max := float64(p.Max); p.Max > 0 && d > max {
		d = max
	}
	delay := time.Duration(d)
	if p.Jitter != nil {
		return p.Jitter(delay)
	}
	return time.Duration(float64(delay) * (0.8 + 0.4*rand.Float64()))
}

func (p RetryPolicy) sleep(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Retry runs fn up to policy.Attempts times, retrying only transient errors.
// It returns the number of attempts made and the last error.
func Retry(ctx context.Context, logger *slog.Logger, policy RetryPolicy, name string, fn func(ctx context.Context) error) (int, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	attempts := max(policy.Attempts, 1)
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = fn(ctx)
		if err == nil {
			return attempt, nil
		}
		if attempt == attempts || !IsTransient(err) || ctx.Err() != nil {
			return attempt, err
		}
		delay := policy.Delay(attempt + 1)
		logger.Warn("retrying after transient error", "op", name, "attempt", attempt, "of", attempts, "delay", delay, "err", err)
		if serr := policy.sleep(ctx, delay); serr != nil {
			return attempt, err
		}
	}
	return attempts, err
}

// IsTransient reports whether an error is worth retrying. Failures of the
// PostgreSQL tools, integrity failures, missing key material and a dead
// context are final; everything else (network, S3 5xx, timeouts inside the
// SDK) is assumed transient.
func IsTransient(err error) bool {
	var exitErr *pg.ExitError
	switch {
	case err == nil:
		return false
	case errors.As(err, &exitErr):
		return false
	case errors.Is(err, ErrIntegrity),
		errors.Is(err, crypt.ErrNoRecipients),
		errors.Is(err, crypt.ErrNoIdentities),
		errors.Is(err, store.ErrNotFound),
		errors.Is(err, store.ErrBucketNotFound),
		errors.Is(err, pg.ErrCompressionUnsupported),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return false
	}
	return true
}

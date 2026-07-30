// Package retry provides bounded exponential backoff with jitter,
// permanent-error classification, per-attempt timeouts, and context
// cancellation (docs/ARCHITECTURE.md §5 gateway client, docs/TESTING.md unit
// layer). Errors are retryable by default; wrap with Permanent to stop
// immediately.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// PermanentError marks an error that must not be retried.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err so Do stops retrying and returns it as-is.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err is marked permanent.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

type Policy struct {
	MaxAttempts    int           // total attempts including the first; <=0 means 1
	BaseDelay      time.Duration // delay before the second attempt
	MaxDelay       time.Duration // cap for the exponential growth
	Multiplier     float64       // growth factor per attempt; <=1 defaults to 2
	JitterFraction float64       // ±fraction applied to each delay, 0..1
	AttemptTimeout time.Duration // per-attempt timeout; 0 disables

	// randFloat overrides jitter randomness in tests.
	randFloat func() float64
}

// DefaultPolicy matches the bounded-backoff requirement for gateway
// registration retries.
func DefaultPolicy() Policy {
	return Policy{
		MaxAttempts:    5,
		BaseDelay:      500 * time.Millisecond,
		MaxDelay:       30 * time.Second,
		Multiplier:     2,
		JitterFraction: 0.2,
		AttemptTimeout: 30 * time.Second,
	}
}

// Delay returns the pre-jitter backoff before attempt n (n=1 is the
// delay between the first and second attempt).
func (p Policy) Delay(attempt int) time.Duration {
	if attempt < 1 || p.BaseDelay <= 0 {
		return 0
	}
	multiplier := p.Multiplier
	if multiplier <= 1 {
		multiplier = 2
	}
	delay := float64(p.BaseDelay) * math.Pow(multiplier, float64(attempt-1))
	if p.MaxDelay > 0 && delay > float64(p.MaxDelay) {
		delay = float64(p.MaxDelay)
	}
	return time.Duration(delay)
}

func (p Policy) jittered(attempt int) time.Duration {
	delay := p.Delay(attempt)
	if delay == 0 || p.JitterFraction <= 0 {
		return delay
	}
	random := p.randFloat
	if random == nil {
		random = rand.Float64
	}
	// Spread uniformly over [1-f, 1+f].
	factor := 1 + p.JitterFraction*(2*random()-1)
	return time.Duration(float64(delay) * factor)
}

// Do runs op until it succeeds, returns a permanent error, exhausts
// MaxAttempts, or ctx is cancelled. Each attempt receives a context
// bounded by AttemptTimeout when set.
func Do(ctx context.Context, p Policy, op func(context.Context) error) error {
	attempts := p.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return joinCancel(err, lastErr)
		}
		lastErr = runAttempt(ctx, p, op)
		if lastErr == nil {
			return nil
		}
		if IsPermanent(lastErr) {
			return lastErr
		}
		if attempt == attempts {
			break
		}
		if err := sleep(ctx, p.jittered(attempt)); err != nil {
			return joinCancel(err, lastErr)
		}
	}
	return fmt.Errorf("after %d attempts: %w", attempts, lastErr)
}

func runAttempt(ctx context.Context, p Policy, op func(context.Context) error) error {
	if p.AttemptTimeout <= 0 {
		return op(ctx)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, p.AttemptTimeout)
	defer cancel()
	return op(attemptCtx)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func joinCancel(cancelErr, lastErr error) error {
	if lastErr == nil {
		return cancelErr
	}
	return fmt.Errorf("%w (last attempt error: %v)", cancelErr, lastErr)
}

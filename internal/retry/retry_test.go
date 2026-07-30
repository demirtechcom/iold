package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func fastPolicy() Policy {
	return Policy{
		MaxAttempts:    4,
		BaseDelay:      time.Millisecond,
		MaxDelay:       8 * time.Millisecond,
		Multiplier:     2,
		JitterFraction: 0,
	}
}

func TestSucceedsAfterTransientFailures(t *testing.T) {
	calls := 0
	err := Do(context.Background(), fastPolicy(), func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestPermanentErrorStopsImmediately(t *testing.T) {
	sentinel := errors.New("bad credentials")
	calls := 0
	err := Do(context.Background(), fastPolicy(), func(context.Context) error {
		calls++
		return Permanent(sentinel)
	})
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("lost underlying error: %v", err)
	}
	if !IsPermanent(err) {
		t.Fatalf("error lost permanent classification: %v", err)
	}
}

func TestExhaustionReturnsLastErrorWithAttemptCount(t *testing.T) {
	sentinel := errors.New("still down")
	calls := 0
	err := Do(context.Background(), fastPolicy(), func(context.Context) error {
		calls++
		return sentinel
	})
	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("lost underlying error: %v", err)
	}
}

func TestDelayGrowsExponentiallyAndCaps(t *testing.T) {
	p := fastPolicy()
	want := []time.Duration{
		time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond,
		8 * time.Millisecond, 8 * time.Millisecond, // capped at MaxDelay
	}
	for i, expected := range want {
		if got := p.Delay(i + 1); got != expected {
			t.Fatalf("Delay(%d) = %v, want %v", i+1, got, expected)
		}
	}
	if p.Delay(0) != 0 {
		t.Fatal("Delay(0) should be 0")
	}
}

func TestJitterStaysWithinFraction(t *testing.T) {
	p := fastPolicy()
	p.JitterFraction = 0.5
	for _, tc := range []struct {
		random float64
		want   time.Duration
	}{
		{0, 500 * time.Microsecond},    // lower bound: base * (1-0.5)
		{1, 1500 * time.Microsecond},   // upper bound: base * (1+0.5)
		{0.5, 1000 * time.Microsecond}, // midpoint: unchanged
	} {
		p.randFloat = func() float64 { return tc.random }
		if got := p.jittered(1); got != tc.want {
			t.Fatalf("jittered(1) with rand=%v = %v, want %v", tc.random, got, tc.want)
		}
	}
}

func TestCancellationDuringBackoff(t *testing.T) {
	p := fastPolicy()
	p.BaseDelay = 10 * time.Second // would block without cancellation
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- Do(ctx, p, func(context.Context) error { return errors.New("transient") })
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Do did not return promptly after cancellation")
	}
}

func TestPerAttemptTimeoutClassifiedRetryable(t *testing.T) {
	p := fastPolicy()
	p.AttemptTimeout = 10 * time.Millisecond
	calls := 0
	err := Do(context.Background(), p, func(ctx context.Context) error {
		calls++
		<-ctx.Done() // simulate an op blocked until its deadline
		return ctx.Err()
	})
	if calls != p.MaxAttempts {
		t.Fatalf("calls = %d, want %d (timeouts should retry)", calls, p.MaxAttempts)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

func TestParentCancellationBeatsAttemptTimeout(t *testing.T) {
	p := fastPolicy()
	p.AttemptTimeout = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Do(ctx, p, func(context.Context) error {
		t.Fatal("op ran despite cancelled parent context")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

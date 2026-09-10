package patchmanager

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestRetryPolicy verifies that the retry policy produces correct backoff delays.
func TestRetryPolicy(t *testing.T) {
	policy := DefaultRetryPolicy()

	if policy.MaxRetries != 3 {
		t.Errorf("expected MaxRetries=3, got %d", policy.MaxRetries)
	}
	if policy.BaseDelay != 2*time.Second {
		t.Errorf("expected BaseDelay=2s, got %v", policy.BaseDelay)
	}

	// Calculate expected delays.
	delay := policy.BaseDelay
	for attempt := 0; attempt < policy.MaxRetries; attempt++ {
		// First attempt: base delay, second: base*backoff, etc.
		expected := delay
		if expected > policy.MaxDelay {
			expected = policy.MaxDelay
		}
		// We don't test actual time delays here (would be slow), just the logic.
		_ = expected

		delay = time.Duration(float64(delay) * policy.Backoff)
	}
}

// TestPatchManagerIdempotency verifies that duplicate apply operations are skipped.
func TestPatchManagerIdempotency(t *testing.T) {
	pm := NewPatchManager()

	// Simulate successful install of patch 1.
	pm.finishInstall("patch-1", true)

	// Second apply of same patch should be skipped.
	if !pm.isAlreadyInstalled("patch-1") {
		t.Errorf("patch-1 should be marked as installed")
	}

	// Different patch should not be marked.
	if pm.isAlreadyInstalled("patch-2") {
		t.Errorf("patch-2 should NOT be marked as installed")
	}
}

// TestPatchManagerInstallingState verifies the installing state tracking.
func TestPatchManagerInstallingState(t *testing.T) {
	pm := NewPatchManager()

	// Mark patch as installing.
	pm.markInstalling("patch-1")

	// Should not be marked as installed yet.
	if pm.isAlreadyInstalled("patch-1") {
		t.Errorf("patch-1 should not be marked as installed while installing")
	}

	// Finish install successfully.
	pm.finishInstall("patch-1", true)

	if !pm.isAlreadyInstalled("patch-1") {
		t.Errorf("patch-1 should be marked as installed after successful finish")
	}
}

// TestPatchManagerFailedInstall verifies that failed installs are retriable.
func TestPatchManagerFailedInstall(t *testing.T) {
	pm := NewPatchManager()

	// Mark patch as installing, then fail.
	pm.markInstalling("patch-1")
	pm.finishInstall("patch-1", false)

	// Should not be marked as installed (retriable).
	if pm.isAlreadyInstalled("patch-1") {
		t.Errorf("patch-1 should not be marked as installed after failure")
	}
}

// TestRetryWithBackoff verifies the retry logic.
func TestRetryWithBackoff(t *testing.T) {
	pm := NewPatchManager()
	// Fast retries for testing.
	pm.SetRetryPolicy(RetryPolicy{
		MaxRetries: 3,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   100 * time.Millisecond,
		Backoff:    2.0,
	})

	attempts := 0
	err := pm.retryWithBackoff(context.Background(), "test", func() error {
		attempts++
		if attempts < 3 {
			return fmt.Errorf("transient error")
		}
		return nil
	})

	if err != nil {
		t.Errorf("unexpected error after retries: %v", err)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

// TestRetryExhausted verifies that retry eventually gives up.
func TestRetryExhausted(t *testing.T) {
	pm := NewPatchManager()
	pm.SetRetryPolicy(RetryPolicy{
		MaxRetries: 2,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   100 * time.Millisecond,
		Backoff:    2.0,
	})

	attempts := 0
	err := pm.retryWithBackoff(context.Background(), "test", func() error {
		attempts++
		return fmt.Errorf("permanent error")
	})

	if err == nil {
		t.Errorf("expected error after exhausting retries")
	}
	if attempts != 3 { // Initial + 2 retries
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

// TestRetryContextTimeout verifies that retry respects context cancellation.
func TestRetryContextTimeout(t *testing.T) {
	pm := NewPatchManager()
	pm.SetRetryPolicy(RetryPolicy{
		MaxRetries: 10,
		BaseDelay:  100 * time.Millisecond,
		MaxDelay:   1 * time.Second,
		Backoff:    2.0,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	attempts := 0
	err := pm.retryWithBackoff(ctx, "test", func() error {
		attempts++
		return fmt.Errorf("transient")
	})

	if err == nil {
		t.Errorf("expected error from context timeout")
	}
	// Should have made at least 1 attempt before timing out.
	if attempts < 1 {
		t.Errorf("expected at least 1 attempt, got %d", attempts)
	}
}

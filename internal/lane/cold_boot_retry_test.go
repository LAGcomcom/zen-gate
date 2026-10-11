package lane

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestColdBootRetryGivesAFailedListingOneMoreChance(t *testing.T) {
	calls := 0
	fetch := func(context.Context) ([]string, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("listing refused")
		}
		return []string{"a", "b"}, nil
	}
	rows, err := fetchListingWithColdBootRetry(context.Background(), 0, fetch)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want the retry's rows", rows)
	}
	if calls != 2 {
		t.Errorf("fetch called %d times, want 2", calls)
	}
}

func TestColdBootRetryStopsAtTwoAttempts(t *testing.T) {
	calls := 0
	first := errors.New("listing refused")
	fetch := func(context.Context) ([]string, error) {
		calls++
		return nil, first
	}
	rows, err := fetchListingWithColdBootRetry(context.Background(), 0, fetch)
	if !errors.Is(err, first) {
		t.Fatalf("err = %v, want the fetch error", err)
	}
	if rows != nil {
		t.Errorf("rows = %v, want nil", rows)
	}
	if calls != 2 {
		t.Errorf("fetch called %d times, want 2: one retry, not a loop", calls)
	}
}

func TestColdBootRetryLeavesACachedSourceAlone(t *testing.T) {
	calls := 0
	fetch := func(context.Context) ([]string, error) {
		calls++
		return nil, errors.New("listing refused")
	}
	if _, err := fetchListingWithColdBootRetry(context.Background(), 3, fetch); err == nil {
		t.Fatal("err = nil, want the fetch error")
	}
	if calls != 1 {
		t.Errorf("fetch called %d times, want 1: a cached slice already covers the blip", calls)
	}
}

func TestColdBootRetryAcceptsAnEmptyButSuccessfulListing(t *testing.T) {
	calls := 0
	fetch := func(context.Context) ([]string, error) {
		calls++
		return nil, nil
	}
	rows, err := fetchListingWithColdBootRetry(context.Background(), 0, fetch)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rows != nil {
		t.Errorf("rows = %v, want nil", rows)
	}
	if calls != 1 {
		t.Errorf("fetch called %d times, want 1: an empty listing is an answer", calls)
	}
}

func TestColdBootRetryHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	fetch := func(context.Context) ([]string, error) {
		calls++
		return nil, errors.New("listing refused")
	}
	start := time.Now()
	if _, err := fetchListingWithColdBootRetry(ctx, 0, fetch); err == nil {
		t.Fatal("err = nil, want the fetch error")
	}
	if calls != 1 {
		t.Errorf("fetch called %d times, want 1: a cancelled refresh must not wait", calls)
	}
	if elapsed := time.Since(start); elapsed > coldBootRetryDelay {
		t.Errorf("returned after %v, want it to skip the wait once the context is done", elapsed)
	}
}

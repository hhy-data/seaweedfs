package filer

import (
	"context"
	"errors"
	"testing"
)

type testCacheInvalidator struct {
	invalidated []string
}

func (inv *testCacheInvalidator) InvalidateCache(fileId string) {
	inv.invalidated = append(inv.invalidated, fileId)
}

func testLookupFn(urls []string) func(ctx context.Context, fileId string) ([]string, error) {
	return func(ctx context.Context, fileId string) ([]string, error) {
		return urls, nil
	}
}

func TestRetryFetchWithFreshLocationsRetriesOnChangedUrls(t *testing.T) {
	inv := &testCacheInvalidator{}
	originalErr := errors.New("read failed")
	refetched := false

	err := retryFetchWithFreshLocations(context.Background(), inv,
		testLookupFn([]string{"http://new:8080/5,abc"}), "5,abc",
		[]string{"http://old:8080/5,abc"}, originalErr,
		func(newUrls []string) error {
			refetched = true
			if len(newUrls) != 1 || newUrls[0] != "http://new:8080/5,abc" {
				t.Errorf("unexpected refetch urls: %v", newUrls)
			}
			return nil
		})

	if err != nil {
		t.Errorf("expected refetch error, got %v", err)
	}
	if !refetched {
		t.Error("expected refetch to be called with new locations")
	}
	if len(inv.invalidated) != 1 || inv.invalidated[0] != "5,abc" {
		t.Errorf("expected cache invalidation for 5,abc, got %v", inv.invalidated)
	}
}

func TestRetryFetchWithFreshLocationsSkipsSameUrls(t *testing.T) {
	inv := &testCacheInvalidator{}
	originalErr := errors.New("read failed")
	refetched := false

	err := retryFetchWithFreshLocations(context.Background(), inv,
		testLookupFn([]string{"http://same:8080/5,abc"}), "5,abc",
		[]string{"http://same:8080/5,abc"}, originalErr,
		func(newUrls []string) error {
			refetched = true
			return nil
		})

	if !errors.Is(err, originalErr) {
		t.Errorf("expected original error, got %v", err)
	}
	if refetched {
		t.Error("expected no refetch when locations unchanged")
	}
}

func TestRetryFetchWithFreshLocationsNilInvalidator(t *testing.T) {
	originalErr := errors.New("read failed")
	refetched := false

	err := retryFetchWithFreshLocations(context.Background(), nil,
		testLookupFn([]string{"http://new:8080/5,abc"}), "5,abc",
		[]string{"http://old:8080/5,abc"}, originalErr,
		func(newUrls []string) error {
			refetched = true
			return nil
		})

	if !errors.Is(err, originalErr) {
		t.Errorf("expected original error, got %v", err)
	}
	if refetched {
		t.Error("expected no refetch without an invalidator")
	}
}

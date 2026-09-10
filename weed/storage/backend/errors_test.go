package backend

import (
	"errors"
	"fmt"
	"testing"
)

// Verifies that the sentinel survives both single- and double-wrapping
// so volume server handlers can route on errors.Is.
func TestErrTierBackendUnavailable_Is(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"bare", ErrTierBackendUnavailable},
		{"wrap-once", fmt.Errorf("udm read: %w", ErrTierBackendUnavailable)},
		{"wrap-twice", fmt.Errorf("volume %d: %w", 7, fmt.Errorf("udm read: %w", ErrTierBackendUnavailable))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, ErrTierBackendUnavailable) {
				t.Fatalf("errors.Is mismatch for %v", tc.err)
			}
		})
	}
}

// Other backend failures must NOT classify as tier-unavailable so the
// volume server does not silently reroute genuine errors to peer replicas.
func TestErrTierBackendUnavailable_NotConfused(t *testing.T) {
	other := errors.New("some other backend error")
	if errors.Is(other, ErrTierBackendUnavailable) {
		t.Fatal("unrelated error should not match ErrTierBackendUnavailable")
	}

	wrapped := fmt.Errorf("udm: %w", other)
	if errors.Is(wrapped, ErrTierBackendUnavailable) {
		t.Fatal("wrapped unrelated error should not match ErrTierBackendUnavailable")
	}
}

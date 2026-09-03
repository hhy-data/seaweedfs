package backend

import "errors"

// ErrTierBackendUnavailable signals that a tiered-storage backend could not
// satisfy a read because the remote store was unreachable, disabled for
// reads, or failed to recall data into the local cache.
//
// Callers (typically the volume server's read path) can use errors.Is to
// decide whether to fail locally or fall back to a peer replica that may
// still hold a hot .dat copy.
var ErrTierBackendUnavailable = errors.New("tier backend unavailable")

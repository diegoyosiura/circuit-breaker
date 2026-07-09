package circuitbreaker

import "errors"

// ErrStopped is returned by Do when the circuit breaker was stopped via
// Stop() and no rate-limit tokens remain. Before this sentinel existed,
// callers without a context deadline would block forever (CB.md A3).
var ErrStopped = errors.New("circuit breaker stopped")

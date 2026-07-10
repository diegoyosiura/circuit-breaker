package circuitbreaker

import "errors"

// ErrStopped is returned by Do when the circuit breaker was stopped via
// Stop() and no rate-limit tokens remain. Before this sentinel existed,
// callers without a context deadline would block forever (CB.md A3).
var ErrStopped = errors.New("circuit breaker stopped")

// retriesExhaustedError preserves the historical error text byte-for-byte
// (consumers may compare err.Error()) while exposing the last attempt's
// error via Unwrap — errors.Is/As now reach the real cause (CB.md A13/F7).
type retriesExhaustedError struct{ last error }

func (e *retriesExhaustedError) Error() string { return "request failed after retries" }
func (e *retriesExhaustedError) Unwrap() error { return e.last }
func (e *retriesExhaustedError) Is(target error) bool {
	_, ok := target.(*retriesExhaustedError)
	return ok
}

// ErrRetriesExhausted is the sentinel for errors.Is when every attempt
// failed with a retryable error. The concrete error also unwraps to the
// last underlying failure.
var ErrRetriesExhausted error = &retriesExhaustedError{}

// ErrCircuitOpen is returned by Do when the opt-in state machine
// (WithBreaker) is OPEN or half-open with all probe slots taken: the request
// fast-fails WITHOUT touching the downstream, giving it time to recover and
// letting the caller trigger a fallback. Never returned by breakers created
// without WithBreaker.
var ErrCircuitOpen = errors.New("circuit breaker is open")

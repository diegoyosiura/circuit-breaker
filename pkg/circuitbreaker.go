package circuitbreaker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"
)

// circuitBreaker implements a resilient HTTP request handler with the following features:
// - Maximum number of concurrent requests
// - Rate-limiting using a token bucket algorithm
// - Automatic retries for transient network failures
// - Context-aware cancellation and timeout support
type circuitBreaker struct {
	Name          string        // Circuit identifier
	MaxConcurrent int           // Maximum concurrent requests allowed
	MaxRetries    int           // Number of retry attempts allowed for retryable failures
	sem           chan struct{} // Semaphore to control concurrent request slots
	tokens        chan struct{} // Token bucket channel to regulate request rate
	tokenInterval time.Duration // Interval between tokens being added to the bucket
	tokenStop     chan struct{} // Channel to signal shutdown of the token generator

	tokenWaitGroup sync.WaitGroup // WaitGroup to ensure graceful token goroutine shutdown
	stopOnce       sync.Once      // Ensures Stop() only closes the channel once
}

// NewCircuitBreaker initializes a new ICircuitBreaker instance with concurrency, rate, and retry controls.
// - name: Identifier for this circuit
// - maxConcurrent: Max number of simultaneous requests
// - maxRequests: Max number of requests per time window
// - windowSeconds: Duration of the rate-limiting window
// - maxRetries: Max number of retry attempts on failure
func NewCircuitBreaker(name string, maxConcurrent, maxRequests int, windowSeconds int, maxRetries int) ICircuitBreaker {
	cb := &circuitBreaker{
		Name:          name,
		MaxConcurrent: maxConcurrent,
		MaxRetries:    maxRetries,
	}

	if maxConcurrent > 0 {
		cb.sem = make(chan struct{}, maxConcurrent)
	}

	if maxRequests > 0 && windowSeconds > 0 {
		cb.tokens = make(chan struct{}, maxRequests)
		interval := (time.Duration(windowSeconds) * time.Second) / time.Duration(maxRequests)
		if interval <= 0 {
			interval = time.Nanosecond
		}
		cb.tokenInterval = interval
		cb.tokenStop = make(chan struct{})

		for i := 0; i < cap(cb.tokens); i++ {
			cb.tokens <- struct{}{}
		}

		cb.startTokenBucket()
	}

	return cb
}

// startTokenBucket starts a background goroutine that generates tokens at regular intervals
// to enforce the request rate limit using a token bucket mechanism.
func (cb *circuitBreaker) startTokenBucket() {
	if cb.tokens == nil || cb.tokenStop == nil || cb.tokenInterval <= 0 {
		return
	}

	cb.tokenWaitGroup.Add(1)
	go func() {
		defer cb.tokenWaitGroup.Done()
		ticker := time.NewTicker(cb.tokenInterval)
		defer ticker.Stop()

		for {
			select {
			case <-cb.tokenStop:
				return // Stop signal received
			case <-ticker.C:
				select {
				case cb.tokens <- struct{}{}:
				default:
					// Token bucket is full; drop the token (non-blocking)
				}
			}
		}
	}()
}

// Do executes the HTTP request, ensuring that:
// - Concurrency and rate limits are respected
// - Retry logic is applied on retryable errors
// - Context cancellation or timeout is observed
func (cb *circuitBreaker) Do(req *http.Request, cl *http.Client) (*http.Response, error) {
	if cb.sem != nil {
		// Wait for a concurrency slot or return if context is cancelled
		select {
		case cb.sem <- struct{}{}:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		defer func() { <-cb.sem }() // Release concurrency slot
	}

	for attempt := 0; attempt <= cb.MaxRetries; attempt++ {
		if err := cb.waitForToken(req.Context()); err != nil {
			return nil, err
		}

		// Clone the request to avoid reuse issues with Body on retries
		newReq := req.Clone(req.Context())

		// Attempt the request
		resp, err := cl.Do(newReq)
		if err == nil {
			return resp, nil // Success
		}

		// Return immediately if the error is not retryable
		if !isRetryable(err) {
			return nil, err
		}

		// If retries remain, back off slightly before trying again
		if attempt < cb.MaxRetries {
			time.Sleep(500 * time.Millisecond)
		}
	}

	// All retry attempts failed
	return nil, errors.New("request failed after retries")
}

// Stop gracefully shuts down the token bucket goroutine.
// Should be called when the circuit is no longer needed.
func (cb *circuitBreaker) Stop() {
	if cb.tokenStop == nil {
		return
	}

	cb.stopOnce.Do(func() {
		close(cb.tokenStop)
	})
	cb.tokenWaitGroup.Wait()
}

func (cb *circuitBreaker) waitForToken(ctx context.Context) error {
	if cb.tokens == nil {
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-cb.tokens:
		return nil
	}
}

// isRetryable checks if an error is considered transient/retryable,
// such as timeouts, connection resets, or temporary network failures.
func isRetryable(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Temporary() || netErr.Timeout()
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}

	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}

	return false
}

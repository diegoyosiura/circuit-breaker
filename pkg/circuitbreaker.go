package circuitbreaker

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"sync"
	"time"
)

// Limites de sanidade do token bucket (F5/D5 do PLANO.md).
const (
	minTokenInterval  = time.Millisecond // piso do intervalo do ticker
	maxBucketCapacity = 1_000_000        // teto do burst para configs degeneradas
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
	tokenInterval time.Duration // Interval between refill ticks
	tokensPerTick int           // Tokens restored per tick (>1 when the interval is clamped)
	tokenStop     chan struct{} // Channel to signal shutdown of the token generator

	backoff time.Duration // Delay between retry attempts (default 500ms)

	tokenWaitGroup sync.WaitGroup // WaitGroup to ensure graceful token goroutine shutdown
	stopOnce       sync.Once      // Ensures Stop() only closes the channel once

	metricsMu sync.RWMutex
	metrics   map[string]map[string]*endpointState
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
		backoff:       500 * time.Millisecond,
	}

	if maxConcurrent > 0 {
		cb.sem = make(chan struct{}, maxConcurrent)
	}

	if maxRequests > 0 && windowSeconds > 0 {
		// Teto de capacidade para configs degeneradas: sem ele, maxRequests
		// gigante custa dezenas de segundos de pré-fill no construtor e anula
		// o rate limit na prática (CB.md A16). Configs sãs não são afetadas.
		capacity := maxRequests
		if capacity > maxBucketCapacity {
			capacity = maxBucketCapacity
		}
		cb.tokens = make(chan struct{}, capacity)

		interval := (time.Duration(windowSeconds) * time.Second) / time.Duration(maxRequests)
		cb.tokensPerTick = 1
		if interval < minTokenInterval {
			// Piso de 1ms com reposição em lote: preserva a taxa nominal sem
			// transformar o ticker num busy-loop de ~1 core (CB.md A16).
			perSecond := float64(maxRequests) / float64(windowSeconds)
			cb.tokensPerTick = int(math.Ceil(perSecond * minTokenInterval.Seconds()))
			if cb.tokensPerTick > capacity {
				cb.tokensPerTick = capacity // nunca repõe mais que o bucket comporta
			}
			interval = minTokenInterval
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
			refill:
				for range cb.tokensPerTick {
					select {
					case cb.tokens <- struct{}{}:
					default:
						// Bucket cheio: nada mais a repor neste tick — sem o
						// break rotulado, um lote grande viraria milhões de
						// sends falhos por tick (busy-loop disfarçado).
						break refill
					}
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

	host := req.URL.Host
	endpoint := req.URL.Path
	if endpoint == "" {
		endpoint = "/"
	}

	var lastErr error
	for attempt := 0; attempt <= cb.MaxRetries; attempt++ {
		start := time.Now()
		if err := cb.waitForToken(req.Context()); err != nil {
			cb.recordFailure(host, endpoint, start, time.Now())
			return nil, err
		}

		cb.recordAttempt(host, endpoint, start, time.Now())
		newReq := req.Clone(req.Context())
		// Clone copies the Body by reference: the first attempt consumes the
		// reader, so every retry must rewind it via GetBody (CB.md A2 —
		// without this, retries resend an empty body).
		if attempt > 0 && req.Body != nil {
			body, bodyErr := req.GetBody()
			if bodyErr != nil {
				return nil, bodyErr
			}
			newReq.Body = body
		}

		// Attempt the request
		resp, err := cl.Do(newReq)
		if err == nil {
			cb.recordSuccess(host, endpoint, start, time.Now())
			return resp, nil // Success
		}

		lastErr = err
		cb.recordFailure(host, endpoint, start, time.Now())
		// Return immediately if the error is not retryable
		if !isRetryable(err) {
			return nil, err
		}

		// A non-rewindable body (GetBody == nil) cannot be replayed: retrying
		// would resend a consumed reader (hard failure or silent empty body),
		// so the attempt's error is returned instead [D2].
		if req.Body != nil && req.GetBody == nil {
			return nil, err
		}

		// If retries remain, back off slightly before trying again.
		// The wait observes the request context: a cancelled caller returns
		// immediately instead of sleeping through the backoff (CB.md A6).
		if attempt < cb.MaxRetries {
			cb.recordRetry(host, endpoint, start, time.Now())
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(cb.backoff):
			}
		}
	}

	// All retry attempts failed. The text is frozen (informal contract);
	// the last cause is recoverable via errors.Is/As (F7).
	return nil, &retriesExhaustedError{last: lastErr}
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

	// Fast path: tokens já disponíveis são atendidos mesmo após Stop()
	// (preserva o comportamento observável de "sobras ainda atendem").
	select {
	case <-cb.tokens:
		return nil
	default:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-cb.tokenStop:
		// Stop() fechou o canal: desbloqueia todos os waiters em vez de
		// pendurar para sempre goroutines (e slots do semáforo) [A3].
		return ErrStopped
	case <-cb.tokens:
		return nil
	}
}

// isRetryable classifies transient failures. Behaviour frozen by the
// characterization table (T0.2, CB-TESTES.md scenario 24):
//   - retryable: net.Error with Timeout() or Temporary() true (genuine
//     timeouts, os.ErrDeadlineExceeded, temporary conditions)
//   - NOT retryable: ECONNRESET/ECONNREFUSED (their Timeout/Temporary are
//     false — retrying a refused connection would hammer a downed service),
//     generic errors, and retryable errors wrapped with %w inside the
//     transport (url.Error probes e.Err by direct type-assertion).
//
// The former branches below this check (*net.OpError, errors.Is ECONNRESET/
// ECONNREFUSED/ErrDeadlineExceeded) were unreachable dead code: every such
// error already satisfies net.Error and is decided here [F8/D1].
//
//nolint:staticcheck // Temporary() é deprecated, mas removê-lo mudaria o
// comportamento efetivo em produção — decisão D1 do PLANO.md.
func isRetryable(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Temporary() || netErr.Timeout()
	}
	return false
}

// Metrics returns a snapshot of the aggregated metrics per host and endpoint.
// Every field of the returned structs is a fresh copy — no aliasing with the
// live state (CB.md A1).
func (cb *circuitBreaker) Metrics() map[string]map[string]EndpointMetrics {
	cb.metricsMu.RLock()
	defer cb.metricsMu.RUnlock()

	result := make(map[string]map[string]EndpointMetrics, len(cb.metrics))
	for host, endpoints := range cb.metrics {
		endpointCopy := make(map[string]EndpointMetrics, len(endpoints))
		for endpoint, state := range endpoints {
			endpointCopy[endpoint] = state.snapshot()
		}
		result[host] = endpointCopy
	}

	return result
}

func (cb *circuitBreaker) recordAttempt(host, endpoint string, start, end time.Time) {
	cb.updateMetrics(host, endpoint, func(s *endpointState) {
		s.totalRequests++
		s.req.record(start, end)
	})
}

func (cb *circuitBreaker) recordSuccess(host, endpoint string, start, end time.Time) {
	cb.updateMetrics(host, endpoint, func(s *endpointState) {
		s.successfulRequests++
		s.success.record(start, end)
	})
}

func (cb *circuitBreaker) recordFailure(host, endpoint string, start, end time.Time) {
	cb.updateMetrics(host, endpoint, func(s *endpointState) {
		s.failedRequests++
		s.failure.record(start, end)
	})
}

func (cb *circuitBreaker) recordRetry(host, endpoint string, start, end time.Time) {
	cb.updateMetrics(host, endpoint, func(s *endpointState) {
		s.retryCount++
		s.retry.record(start, end)
	})
}

// updateMetrics applies the update to the endpoint state AND to the per-host
// "::root" aggregate, under the metrics lock. Each record is O(1): ring
// buffers for the means and a fixed bucket wheel for the window counts
// (metrics_state.go) — the previous slices grew without bound and made every
// Do() progressively slower (CB.md A4).
func (cb *circuitBreaker) updateMetrics(host, endpoint string, updateFn func(*endpointState)) {
	cb.metricsMu.Lock()
	defer cb.metricsMu.Unlock()

	if cb.metrics == nil {
		cb.metrics = make(map[string]map[string]*endpointState)
	}

	hostMetrics, ok := cb.metrics[host]
	if !ok {
		hostMetrics = make(map[string]*endpointState)
		cb.metrics[host] = hostMetrics
		hostMetrics["::root"] = &endpointState{}
	}

	state, ok := hostMetrics[endpoint]
	if !ok {
		state = &endpointState{}
		hostMetrics[endpoint] = state
	}

	updateFn(state)
	updateFn(hostMetrics["::root"])
}

package circuitbreaker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"slices"
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

	backoff time.Duration // Delay between retry attempts (default 500ms)

	tokenWaitGroup sync.WaitGroup // WaitGroup to ensure graceful token goroutine shutdown
	stopOnce       sync.Once      // Ensures Stop() only closes the channel once

	metricsMu sync.RWMutex
	metrics   map[string]map[string]*EndpointMetrics
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

	host := req.URL.Host
	endpoint := req.URL.Path
	if endpoint == "" {
		endpoint = "/"
	}

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

		// If retries remain, back off slightly before trying again
		if attempt < cb.MaxRetries {
			cb.recordRetry(host, endpoint, start, time.Now())
			time.Sleep(cb.backoff)
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

// Metrics returns a snapshot of the aggregated metrics per host and endpoint.
func (cb *circuitBreaker) Metrics() map[string]map[string]EndpointMetrics {
	cb.metricsMu.RLock()
	defer cb.metricsMu.RUnlock()

	result := make(map[string]map[string]EndpointMetrics, len(cb.metrics))
	for host, endpoints := range cb.metrics {
		endpointCopy := make(map[string]EndpointMetrics, len(endpoints))
		for endpoint, metrics := range endpoints {
			snapshot := *metrics
			// Deep-copy: sem isto os campos slice do snapshot compartilham
			// backing array com os dados vivos (caller e breaker se corrompem
			// mutuamente).
			snapshot.TimeRequests = slices.Clone(metrics.TimeRequests)
			snapshot.TimeSuccessfulRequests = slices.Clone(metrics.TimeSuccessfulRequests)
			snapshot.TimeFailedRequests = slices.Clone(metrics.TimeFailedRequests)
			snapshot.TimeRetry = slices.Clone(metrics.TimeRetry)
			snapshot.StartTimeRequests = slices.Clone(metrics.StartTimeRequests)
			snapshot.StartTimeSuccessfulRequests = slices.Clone(metrics.StartTimeSuccessfulRequests)
			snapshot.StartTimeFailedRequests = slices.Clone(metrics.StartTimeFailedRequests)
			snapshot.StartTimeRetry = slices.Clone(metrics.StartTimeRetry)
			endpointCopy[endpoint] = snapshot
		}
		result[host] = endpointCopy
	}

	return result
}

func (cb *circuitBreaker) recordAttempt(host, endpoint string, start, end time.Time) {
	cb.updateMetrics(host, endpoint, func(m *EndpointMetrics) {
		m.TotalRequests++
		m.TimeRequests = append(m.TimeRequests, end.Sub(start).Seconds())
		m.StartTimeRequests = append(m.StartTimeRequests, start)
		m.MeanRequests, _ = avgLastNItens(m.TimeRequests, 20)

		m.Ratio01Requests = repsRatio(m.StartTimeRequests, 1)
		m.Ratio05Requests = repsRatio(m.StartTimeRequests, 5)
		m.Ratio10Requests = repsRatio(m.StartTimeRequests, 10)
	})
}

func (cb *circuitBreaker) recordSuccess(host, endpoint string, start, end time.Time) {
	cb.updateMetrics(host, endpoint, func(m *EndpointMetrics) {
		m.SuccessfulRequests++
		m.TimeSuccessfulRequests = append(m.TimeSuccessfulRequests, end.Sub(start).Seconds())
		m.StartTimeSuccessfulRequests = append(m.StartTimeSuccessfulRequests, start)
		m.MeanSuccessfulRequests, _ = avgLastNItens(m.TimeSuccessfulRequests, 20)

		m.Ratio01SuccessfulRequests = repsRatio(m.StartTimeSuccessfulRequests, 1)
		m.Ratio05SuccessfulRequests = repsRatio(m.StartTimeSuccessfulRequests, 5)
		m.Ratio10SuccessfulRequests = repsRatio(m.StartTimeSuccessfulRequests, 10)
	})
}

func (cb *circuitBreaker) recordFailure(host, endpoint string, start, end time.Time) {
	cb.updateMetrics(host, endpoint, func(m *EndpointMetrics) {
		m.FailedRequests++
		m.TimeFailedRequests = append(m.TimeFailedRequests, end.Sub(start).Seconds())
		m.StartTimeFailedRequests = append(m.StartTimeFailedRequests, start)
		m.MeanFailedRequests, _ = avgLastNItens(m.TimeFailedRequests, 20)

		m.Ratio01FailedRequests = repsRatio(m.StartTimeFailedRequests, 1)
		m.Ratio05FailedRequests = repsRatio(m.StartTimeFailedRequests, 5)
		m.Ratio10FailedRequests = repsRatio(m.StartTimeFailedRequests, 10)
	})
}

func (cb *circuitBreaker) recordRetry(host, endpoint string, start, end time.Time) {
	cb.updateMetrics(host, endpoint, func(m *EndpointMetrics) {
		m.RetryCount++
		m.TimeRetry = append(m.TimeRetry, end.Sub(start).Seconds())
		m.StartTimeRetry = append(m.StartTimeRetry, start)
		m.MeanRetry, _ = avgLastNItens(m.TimeRetry, 20)

		m.Ratio01Retry = repsRatio(m.StartTimeRetry, 1)
		m.Ratio05Retry = repsRatio(m.StartTimeRetry, 5)
		m.Ratio10Retry = repsRatio(m.StartTimeRetry, 10)
	})
}

func (cb *circuitBreaker) updateMetrics(host, endpoint string, updateFn func(*EndpointMetrics)) {
	cb.metricsMu.Lock()
	defer cb.metricsMu.Unlock()

	if cb.metrics == nil {
		cb.metrics = make(map[string]map[string]*EndpointMetrics)
	}

	hostMetrics, ok := cb.metrics[host]
	if !ok {
		hostMetrics = make(map[string]*EndpointMetrics)
		cb.metrics[host] = hostMetrics
		cb.metrics[host]["::root"] = &EndpointMetrics{}
	}

	endpointMetrics, ok := hostMetrics[endpoint]
	if !ok {
		endpointMetrics = &EndpointMetrics{}
		hostMetrics[endpoint] = endpointMetrics
	}

	updateFn(endpointMetrics)
	updateFn(cb.metrics[host]["::root"])
	pruneMetrics(endpointMetrics)
	pruneMetrics(cb.metrics[host]["::root"])
}

// Retenção das amostras internas (F4/D4): as médias usam as últimas
// meanWindow amostras e os ratios a janela de até 10 min — nada além disso
// precisa ser retido. Antes desta poda os slices cresciam sem teto (leak de
// memória + custo O(n) por request, CB.md A4).
const (
	meanWindow    = 20               // amostras usadas por avgLastNItens
	pruneTrigger  = 4 * meanWindow   // amortiza a cópia da poda
	ratioWindow   = 10 * time.Minute // maior janela de repsRatio
	startsHardCap = 100_000          // teto absoluto de segurança
)

func pruneMetrics(m *EndpointMetrics) {
	m.TimeRequests = pruneFloats(m.TimeRequests)
	m.TimeSuccessfulRequests = pruneFloats(m.TimeSuccessfulRequests)
	m.TimeFailedRequests = pruneFloats(m.TimeFailedRequests)
	m.TimeRetry = pruneFloats(m.TimeRetry)

	cutoff := time.Now().Add(-ratioWindow)
	m.StartTimeRequests = pruneTimes(m.StartTimeRequests, cutoff)
	m.StartTimeSuccessfulRequests = pruneTimes(m.StartTimeSuccessfulRequests, cutoff)
	m.StartTimeFailedRequests = pruneTimes(m.StartTimeFailedRequests, cutoff)
	m.StartTimeRetry = pruneTimes(m.StartTimeRetry, cutoff)
}

func pruneFloats(xs []float64) []float64 {
	if len(xs) <= pruneTrigger {
		return xs
	}
	out := make([]float64, meanWindow)
	copy(out, xs[len(xs)-meanWindow:])
	return out
}

func pruneTimes(ts []time.Time, cutoff time.Time) []time.Time {
	if len(ts) > startsHardCap {
		ts = ts[len(ts)-startsHardCap:]
	}
	// Entradas são cronológicas (pós-F1 nada reordena): descarta o prefixo
	// fora da janela, copiando apenas quando ao menos metade expirou.
	expired := 0
	for expired < len(ts) && ts[expired].Before(cutoff) {
		expired++
	}
	if expired == 0 || expired < len(ts)/2 {
		return ts
	}
	out := make([]time.Time, len(ts)-expired)
	copy(out, ts[expired:])
	return out
}

func avgLastNItens(xs []float64, size int) (avg float64, count int) {
	if len(xs) == 0 {
		return 0, 0
	}
	start := 0
	if len(xs) > size {
		start = len(xs) - size
	}
	sum := 0.0
	for _, v := range xs[start:] {
		sum += v
	}
	count = len(xs) - start
	return sum / float64(count), count
}

// repsRatio counts how many timestamps fall inside the trailing window.
// It must NOT mutate ts: the slices are shared with snapshots returned by
// Metrics(), and the previous in-place sort caused a data race (CB.md A1).
func repsRatio(ts []time.Time, windowMinutes int) int64 {
	if len(ts) == 0 || windowMinutes <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-time.Duration(windowMinutes) * time.Minute)
	var count int64
	for _, t := range ts {
		if !t.Before(cutoff) {
			count++
		}
	}
	return count
}

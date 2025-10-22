package circuitbreaker_test

import (
	circuitbreaker "circuit-breaker/pkg"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type temporaryError struct{}

func (temporaryError) Error() string   { return "temporary" }
func (temporaryError) Timeout() bool   { return false }
func (temporaryError) Temporary() bool { return true }

func TestCircuitBreaker_BasicRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)

	cb := circuitbreaker.NewCircuitBreaker("test", 2, 4, 1, 0)
	t.Cleanup(cb.Stop)
	cl := server.Client()

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := cb.Do(req, cl)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestCircuitBreaker_RetryOnTemporaryError(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("retry", 1, 10, 1, 3)
	t.Cleanup(cb.Stop)

	var attempts int32
	const failCount = 2
	cl := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if atomic.AddInt32(&attempts, 1) <= failCount {
				return nil, temporaryError{}
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
		Timeout: 2 * time.Second,
	}

	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := cb.Do(req, cl)
	if err != nil {
		t.Fatalf("expected request to succeed after retries, got %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if got := atomic.LoadInt32(&attempts); got != failCount+1 {
		t.Fatalf("expected %d attempts, got %d", failCount+1, got)
	}
}

func TestCircuitBreaker_ContextCancellationWhileWaitingForToken(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("ctx", 1, 1, 10, 0)
	t.Cleanup(cb.Stop)

	cl := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
		Timeout: time.Second,
	}

	baseReq, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := cb.Do(baseReq, cl)
	if err != nil {
		t.Fatalf("expected initial request to succeed, got %v", err)
	}
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	reqWithTimeout := baseReq.Clone(ctx)
	_, err = cb.Do(reqWithTimeout, cl)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded error, got %v", err)
	}
}

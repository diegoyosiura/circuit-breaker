package circuitbreaker_test

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// roundTripFunc adapts a function into an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// netError is a configurable net.Error implementation.
type netError struct {
	msg       string
	timeout   bool
	temporary bool
}

func (e netError) Error() string   { return e.msg }
func (e netError) Timeout() bool   { return e.timeout }
func (e netError) Temporary() bool { return e.temporary }

func okResponse(r *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
		Request:    r,
	}
}

// countingTransport counts calls, optionally failing the first failN calls
// with err and sleeping delay before answering.
type countingTransport struct {
	mu    sync.Mutex
	calls int
	failN int
	err   error
	delay time.Duration
}

func (t *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.calls++
	n := t.calls
	failN, err, delay := t.failN, t.err, t.delay
	t.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if n <= failN {
		return nil, err
	}
	return okResponse(r), nil
}

func (t *countingTransport) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

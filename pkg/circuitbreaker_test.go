package circuitbreaker_test

import (
	"circuit-breaker/pkg"
	"net/http"
	"testing"
	"time"
)

func TestCircuitBreaker_BasicRequest(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("test", 5, 10, 10, 1)

	req, err := http.NewRequest("GET", "http://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Replace with test server or mock if needed
	timeout := time.After(2 * time.Second)
	done := make(chan bool)

	go func() {
		// No actual call; just testing the blocking and token logic
		cb.Do(req)
		done <- true
	}()

	select {
	case <-timeout:
		t.Error("CircuitBreaker did not complete in time")
	case <-done:
		// pass
	}
}

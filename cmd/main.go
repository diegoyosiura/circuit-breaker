//go:build !unittest

package main

import (
	"fmt"

	"github.com/diegoyosiura/circuit-breaker/internal"
	circuitbreaker "github.com/diegoyosiura/circuit-breaker/pkg"

	"net/http"
	"sync"
	"time"
)

// main runs a benchmark-style test against a local fake HTTP server.
// It demonstrates how to:
// - Start and interact with a simulated API server (FakeServer)
// - Use the CircuitBreaker with concurrency and rate control
// - Spawn multiple parallel HTTP requests (using goroutines)
// - Measure and display total execution time and request results
func main() {
	// Start a local fake server on localhost:8080
	// The FakeServer simulates processing delays and tracks statistics
	fs := internal.NewFakeServer("localhost", 8080)
	go fs.Listen()

	// Allow server time to fully start before sending traffic
	time.Sleep(5 * time.Second)

	// Initialize the CircuitBreaker manager
	m := circuitbreaker.NewManager()

	// Create or retrieve a named CircuitBreaker:
	// name: "google"
	// maxConcurrent: 50 → up to 50 parallel requests
	// maxRequests: 200 → up to 200 requests every 30 seconds (rate limit)
	// windowSeconds: 30
	// maxRetries: 5 → retry failed requests up to 5 times
	cb := m.NewCircuitBreaker("google", 50, 200, 30, 5)

	var wg sync.WaitGroup
	total := 3000 // Total number of requests to simulate

	start := time.Now() // Start measuring total time

	// Spawn 3000 concurrent requests to the /teste endpoint
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// Create a new HTTP GET request to the test endpoint
			req, _ := http.NewRequest("GET", "http://localhost:8080/teste", nil)
			cl := &http.Client{Timeout: 10 * time.Second}
			// Execute the request through the CircuitBreaker
			resp, err := cb.Do(req, cl)

			// Print result or error
			if err != nil {
				fmt.Printf("[Req %04d] ERROR: %v\n", i, err)
				return
			}
			defer resp.Body.Close()
			fmt.Printf("[Req %04d] OK: %s\n", i, resp.Status)
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Display the total elapsed time for all requests
	fmt.Println("Total time:", time.Since(start))
}

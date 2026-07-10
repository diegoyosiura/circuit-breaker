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
	// Start a local fake server on localhost:8080. Start() binds
	// synchronously: when it returns nil the server is ready — no sleep.
	fs := internal.NewFakeServer("localhost", 8080)
	if err := fs.Start(); err != nil {
		fmt.Println("não foi possível subir o FakeServer:", err)
		return
	}
	defer fs.Close()

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
			req, _ := http.NewRequest("GET", fmt.Sprintf("http://%s/teste", fs.Addr()), nil)
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

	// Stop the breakers registered in the manager (graceful shutdown).
	if lc, ok := m.(circuitbreaker.IManagerLifecycle); ok {
		lc.StopAll()
	}

	// Display the total elapsed time for all requests
	fmt.Println("Total time:", time.Since(start))
}

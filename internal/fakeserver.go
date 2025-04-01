package internal

import (
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

// FakeServer simulates an HTTP server used to test request throughput,
// concurrency limits, and delay handling in a realistic environment.
// It tracks metrics such as:
// - Current simultaneous connections
// - Maximum simultaneous connections
// - Requests per second (RPS)
// - Maximum RPS
// - Average RPS over time
type FakeServer struct {
	mu                   *sync.Mutex    // Ensures thread-safe access to metrics
	mux                  *http.ServeMux // HTTP multiplexer for routing requests
	currentConnections   int            // Number of requests currently being handled
	maxSimultaneous      int            // Peak of simultaneous connections
	requestsPerSecond    int            // Number of requests received in the current second
	maxRequestsPerSecond int            // Highest RPS recorded
	totalRequests        int            // Cumulative request count since start
	startTime            time.Time      // Time the server was initialized
	server               *http.Server   // Underlying HTTP server instance
}

// NewFakeServer initializes a new instance of FakeServer.
// It sets up a basic HTTP server listening on the given address and port,
// and starts background goroutines to track statistics.
func NewFakeServer(server string, port int) *FakeServer {
	mux := http.NewServeMux()

	fs := &FakeServer{
		mu:                   &sync.Mutex{},
		mux:                  mux,
		currentConnections:   0,
		maxSimultaneous:      0,
		requestsPerSecond:    0,
		maxRequestsPerSecond: 0,
		totalRequests:        0,
		startTime:            time.Now(),
		server: &http.Server{
			Addr:    fmt.Sprintf("%s:%d", server, port),
			Handler: mux,
		},
	}

	// Register the handler for the /teste endpoint
	mux.HandleFunc("/teste", fs.handle)

	// Launch background goroutines to monitor metrics
	go fs.reset() // Updates maxRPS every second
	go fs.stats() // Prints stats every 5 seconds

	return fs
}

// handle is the HTTP handler for /teste.
// It simulates a processing delay between 1-5 seconds,
// and updates connection counters and request metrics.
func (f *FakeServer) handle(w http.ResponseWriter, r *http.Request) {
	// Lock before updating shared state
	f.mu.Lock()
	f.currentConnections++
	f.requestsPerSecond++
	f.totalRequests++

	// Track peak of simultaneous connections
	if f.currentConnections > f.maxSimultaneous {
		f.maxSimultaneous = f.currentConnections
	}
	f.mu.Unlock()

	// Ensure connection count is decremented when finished
	defer func() {
		f.mu.Lock()
		f.currentConnections--
		f.mu.Unlock()
	}()

	// Simulate variable processing time (1–5 seconds)
	delay := time.Duration(rand.Intn(5)+1) * time.Second
	time.Sleep(delay)

	// Respond to the client
	fmt.Fprintf(w, "Resposta OK (delay %v)\n", delay)
}

// reset is a background goroutine that runs every second.
// It checks whether the current RPS exceeds the maximum recorded,
// and resets the per-second request counter.
func (f *FakeServer) reset() {
	for {
		time.Sleep(1 * time.Second)
		f.mu.Lock()

		// Update max RPS if current second exceeds previous max
		if f.requestsPerSecond > f.maxRequestsPerSecond {
			f.maxRequestsPerSecond = f.requestsPerSecond
		}

		// Reset current second counter
		f.requestsPerSecond = 0
		f.mu.Unlock()
	}
}

// stats is a background goroutine that runs every 5 seconds.
// It prints a snapshot of:
// - Maximum simultaneous connections observed
// - Maximum requests per second recorded
// - Average requests per second since server start
func (f *FakeServer) stats() {
	for {
		time.Sleep(5 * time.Second)
		f.mu.Lock()

		// Calculate average RPS since startup
		elapsed := time.Since(f.startTime).Seconds()
		var avg float64
		if elapsed > 0 {
			avg = float64(f.totalRequests) / elapsed
		}

		// Print collected statistics
		fmt.Printf("📊 Máx. simultâneas: %d | Máx. req/s: %d | Média req/s: %.2f\n",
			f.maxSimultaneous, f.maxRequestsPerSecond, avg)

		f.mu.Unlock()
	}
}

// Listen starts the HTTP server and blocks until it stops.
// This should be called after NewFakeServer() to begin accepting requests.
func (f *FakeServer) Listen() {
	fmt.Println("Servidor falso ouvindo em http://localhost:8080")
	_ = f.server.ListenAndServe() // Starts the HTTP server (errors are ignored here)
}

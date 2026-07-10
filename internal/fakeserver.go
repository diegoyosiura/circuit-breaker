package internal

import (
	"fmt"
	"math/rand"
	"net"
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
//
// The listen address is parameterizable: pass port 0 to bind an ephemeral
// port chosen by the OS, which allows multiple FakeServers to run side by
// side without collisions. The effective address is available via Addr()
// after a successful Start()/Listen().
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
	listener             net.Listener   // Set after a successful bind
	minDelay             time.Duration  // Lower bound of the simulated processing delay
	maxDelay             time.Duration  // Upper bound of the simulated processing delay
	done                 chan struct{}  // Stops the reset/stats goroutines
	closeOnce            sync.Once      // Ensures Close() runs once
}

// NewFakeServer initializes a new instance of FakeServer.
// It sets up a basic HTTP server for the given address and port (port 0
// requests an ephemeral port from the OS) and starts background goroutines
// to track statistics. Call Start() or Listen() to begin accepting requests.
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
		minDelay:             1 * time.Second,
		maxDelay:             5 * time.Second,
		done:                 make(chan struct{}),
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

// SetDelay overrides the simulated processing delay bounds (default 1–5s).
// Use equal values for a fixed delay. Must be called before traffic arrives.
func (f *FakeServer) SetDelay(min, max time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if max < min {
		max = min
	}
	f.minDelay, f.maxDelay = min, max
}

// handle is the HTTP handler for /teste.
// It simulates a processing delay within the configured bounds,
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
	minDelay, maxDelay := f.minDelay, f.maxDelay
	f.mu.Unlock()

	// Ensure connection count is decremented when finished
	defer func() {
		f.mu.Lock()
		f.currentConnections--
		f.mu.Unlock()
	}()

	// Simulate variable processing time within [minDelay, maxDelay]
	delay := minDelay
	if maxDelay > minDelay {
		delay += time.Duration(rand.Int63n(int64(maxDelay - minDelay + 1)))
	}
	time.Sleep(delay)

	// Respond to the client
	fmt.Fprintf(w, "Resposta OK (delay %v)\n", delay)
}

// bind opens the TCP listener and records the effective address.
// Unlike the previous implementation, a bind failure (e.g. port already
// in use) surfaces as an error instead of being silently swallowed.
func (f *FakeServer) bind() (net.Listener, error) {
	l, err := net.Listen("tcp", f.server.Addr)
	if err != nil {
		return nil, fmt.Errorf("FakeServer: falha ao escutar em %s: %w", f.server.Addr, err)
	}
	f.mu.Lock()
	f.listener = l
	f.mu.Unlock()
	fmt.Printf("Servidor falso ouvindo em http://%s\n", l.Addr())
	return l, nil
}

// Start binds the listener synchronously and serves in the background.
// When it returns nil the server is ready to accept connections — no
// sleep-based synchronization is needed. The bind error, if any, is
// returned to the caller.
func (f *FakeServer) Start() error {
	l, err := f.bind()
	if err != nil {
		return err
	}
	go func() { _ = f.server.Serve(l) }()
	return nil
}

// Addr returns the effective listen address (host:port) after a successful
// Start()/Listen(), or "" before binding. With port 0 this is how the
// OS-chosen ephemeral port is discovered.
func (f *FakeServer) Addr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listener == nil {
		return ""
	}
	return f.listener.Addr().String()
}

// Close stops the HTTP server and the metric goroutines (reset/stats).
// Safe to call multiple times.
func (f *FakeServer) Close() {
	f.closeOnce.Do(func() {
		close(f.done)
		_ = f.server.Close()
	})
}

// reset is a background goroutine that runs every second.
// It checks whether the current RPS exceeds the maximum recorded,
// and resets the per-second request counter. It stops on Close().
func (f *FakeServer) reset() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-ticker.C:
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
}

// stats is a background goroutine that runs every 5 seconds.
// It prints a snapshot of:
// - Maximum simultaneous connections observed
// - Maximum requests per second recorded
// - Average requests per second since server start
// It stops on Close().
func (f *FakeServer) stats() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-ticker.C:
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
}

// Listen starts the HTTP server and blocks until it stops (historical
// behavior, kept for compatibility with existing callers). Unlike the
// previous version, a bind failure is reported instead of ignored, and
// the printed address is the effective one (including ephemeral ports).
func (f *FakeServer) Listen() {
	l, err := f.bind()
	if err != nil {
		fmt.Println(err)
		return
	}
	_ = f.server.Serve(l)
}

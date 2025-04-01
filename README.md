# Circuit Breaker and Test Harness (Go)

![Production](https://img.shields.io/badge/status-production-brightgreen)
![Go Version](https://img.shields.io/badge/go-1.20+-blue)
![Tests](https://github.com/diegoyosiura/circuit-breaker/actions/workflows/tests.yml/badge.svg)
![Coverage](https://img.shields.io/codecov/c/github/your-org/your-repo)

This repository demonstrates a **fully-featured Circuit Breaker** written in Go, along with a **simulated HTTP server** to test request handling under constraints such as:

- **Rate limiting** (token bucket algorithm)
- **Concurrency control** (via semaphores)
- **Retry logic** (on transient network errors)
- **Context support** (timeouts and cancellations)
- **Live metrics** through the FakeServer

---

## ✨ Features

### Circuit Breaker (`pkg/circuitbreaker.go`)
- Limits max **concurrent HTTP requests**.
- Enforces **rate limit** using a **token bucket**.
- Retries requests **on network failures** (e.g., timeout, connection refused).
- Observes **context cancellation** or timeout.

### Manager (`pkg/manager.go`)
- Keeps a **registry of named CircuitBreakers**.
- Ensures a single instance per name (singleton pattern).
- Thread-safe with `sync.Mutex`.

### Fake Server (`internal/fakeserver.go`)
- Simulates real-world delay (1–5s per request).
- Tracks and logs:
    - Current and maximum simultaneous connections
    - Requests per second (RPS)
    - Maximum RPS
    - Average RPS

---

## ⚙️ How It Works

### Token Bucket (Rate Limiting)
- Tokens are added to a buffered channel (`cb.tokens`) at regular intervals.
- Each request consumes one token.
- If no tokens are available, the request **waits** (or is canceled by context).

### Semaphore (Concurrency Limit)
- A buffered channel (`cb.sem`) is used to block when max parallel requests are reached.

### Retry with Context Awareness
- On transient failure, requests are retried up to `MaxRetries`.
- Between retries, a short backoff is applied.
- If the context expires or is canceled, retry loop is exited.

---

## 🔧 Usage Example

```go
func main() {
    fs := internal.NewFakeServer("localhost", 8080)
    go fs.Listen()
    time.Sleep(5 * time.Second) // Give server time to start

    manager := circuitbreaker.NewManager()
    cb := manager.NewCircuitBreaker("google", 50, 200, 30, 5) // name, concurrency, max reqs, window (s), retries

    var wg sync.WaitGroup
    total := 1000 // Total number of requests to simulate

    for i := 0; i < total; i++ {
        wg.Add(1)
        go func(i int) {
            defer wg.Done()
            req, _ := http.NewRequest("GET", "http://localhost:8080/teste", nil)
            resp, err := cb.Do(req)
            if err != nil {
                fmt.Printf("[Req %04d] ERROR: %v\n", i, err)
                return
            }
            defer resp.Body.Close()
            fmt.Printf("[Req %04d] OK: %s\n", i, resp.Status)
        }(i)
    }

    wg.Wait()
}
```

---

## ✅ Metrics Output

Every 5 seconds, the FakeServer logs:

```
📊 Max simultaneous: 50 | Max req/s: 40 | Avg req/s: 1.66
```

This helps validate if the CircuitBreaker is enforcing:
- Maximum concurrency
- Requests per second (as calculated from maxRequests/windowSeconds)

---

## ⚠️ Notes & Recommendations

- Use `cb.Stop()` to gracefully shut down token generators when your app exits.
- Tune `MaxRequests` and `WindowSeconds` to control **rate**.
- Tune `MaxConcurrent` to control **parallelism**.
- The system assumes requests are **stateless** and idempotent if retries are enabled.

---

## 🚀 Extending This Project

- Export metrics to **Prometheus**
- Add **backoff strategies** (exponential, jittered)
- Support other transports (e.g. gRPC)
- Add circuit "open/close" state tracking

---

## ✨ Authors

Developed by Diego Yosiura. Contributions welcome!

---

## 📝 License

This project is open-sourced under the MIT License.


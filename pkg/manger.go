package circuitbreaker

import "sync"

// Manager is responsible for managing multiple CircuitBreaker instances.
// It acts as a centralized registry, ensuring that each named CircuitBreaker
// is only instantiated once and reused for all future requests.
//
// This design enables:
// - Centralized lifecycle management
// - Thread-safe access to named CircuitBreakers
// - Avoidance of redundant instantiations
type manager struct {
	mu sync.RWMutex
	cb map[string]ICircuitBreaker
}

// NewManager initializes and returns a new Manager instance.
// It prepares an empty map and a mutex for thread-safe management.
func NewManager() IManager {
	return &manager{
		cb: make(map[string]ICircuitBreaker),
	}
}

// NewCircuitBreaker creates and stores a new CircuitBreaker if one
// with the given name doesn't already exist.
//
// Parameters:
// - name: unique name for the circuit
// - maxConcurrent: maximum concurrent HTTP requests allowed
// - maxRequests: max number of requests in the rate-limit window
// - windowSeconds: duration of the rate-limit window
// - maxRetries: number of retry attempts for failed requests
//
// Returns:
// - A pointer to the existing or newly created CircuitBreaker instance
func (m *manager) NewCircuitBreaker(
	name string,
	maxConcurrent, maxRequests int,
	windowSeconds int,
	maxRetries int,
) ICircuitBreaker {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cb, ok := m.cb[name]; ok {
		return cb
	}

	cb := NewCircuitBreaker(name, maxConcurrent, maxRequests, windowSeconds, maxRetries)
	m.cb[name] = cb
	return cb
}

// GetCircuitBreaker retrieves a CircuitBreaker by name from the manager.
//
// Returns:
// - The CircuitBreaker pointer if it exists
// - nil if no CircuitBreaker with the given name was found
func (m *manager) GetCircuitBreaker(name string) ICircuitBreaker {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if cb, ok := m.cb[name]; ok {
		return cb
	}
	return nil
}

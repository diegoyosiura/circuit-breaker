package circuitbreaker

import (
	"sync"
)

// Manager is responsible for managing multiple CircuitBreaker instances.
// It acts as a centralized registry, ensuring that each named CircuitBreaker
// is only instantiated once and reused for all future requests.
//
// This design enables:
// - Centralized lifecycle management
// - Thread-safe access to named CircuitBreakers
// - Avoidance of redundant instantiations
type Manager struct {
	mu *sync.Mutex                // Mutex to protect concurrent access to the map
	cb map[string]*CircuitBreaker // Map to store CircuitBreakers by name
}

// NewManager initializes and returns a new Manager instance.
// It prepares an empty map and a mutex for thread-safe management.
func NewManager() *Manager {
	return &Manager{
		mu: &sync.Mutex{},
		cb: make(map[string]*CircuitBreaker),
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
func (m *Manager) NewCircuitBreaker(
	name string,
	maxConcurrent, maxRequests int,
	windowSeconds int,
	maxRetries int,
) *CircuitBreaker {
	m.mu.Lock()         // Lock access to the map to ensure thread safety
	defer m.mu.Unlock() // Always unlock after operation

	// If the circuit doesn't exist yet, create and store it
	if _, ok := m.cb[name]; !ok {
		m.cb[name] = NewCircuitBreaker(name, maxConcurrent, maxRequests, windowSeconds, maxRetries)
	}
	return m.cb[name]
}

// GetCircuitBreaker retrieves a CircuitBreaker by name from the manager.
//
// Returns:
// - The CircuitBreaker pointer if it exists
// - nil if no CircuitBreaker with the given name was found
func (m *Manager) GetCircuitBreaker(name string) *CircuitBreaker {
	m.mu.Lock()         // Ensure thread-safe read
	defer m.mu.Unlock() // Release lock after access

	// Lookup the circuit by name
	if _, ok := m.cb[name]; !ok {
		return nil
	}
	return m.cb[name]
}

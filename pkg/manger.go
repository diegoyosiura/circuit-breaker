package circuitbreaker

import (
	"fmt"
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
// breakerConfig guarda os parâmetros com que cada breaker foi criado —
// base do modo estrito (R3), que acusa configurações divergentes.
type breakerConfig struct {
	maxConcurrent, maxRequests, windowSeconds, maxRetries int
}

type manager struct {
	mu  sync.RWMutex
	cb  map[string]ICircuitBreaker
	cfg map[string]breakerConfig
}

// NewManager initializes and returns a new Manager instance.
// It prepares an empty map and a mutex for thread-safe management.
func NewManager() IManager {
	return &manager{
		cb:  make(map[string]ICircuitBreaker),
		cfg: make(map[string]breakerConfig),
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
	m.cfg[name] = breakerConfig{maxConcurrent, maxRequests, windowSeconds, maxRetries}
	return cb
}

// NewCircuitBreakerStrict é a variante get-or-create que RECUSA configuração
// divergente: se o nome já existe com outros parâmetros, retorna erro em vez
// de silenciosamente devolver a instância antiga (R3 — o comportamento
// silencioso de NewCircuitBreaker é preservado por compatibilidade).
// Exposto via type assertion para IManagerStrict.
func (m *manager) NewCircuitBreakerStrict(
	name string,
	maxConcurrent, maxRequests int,
	windowSeconds int,
	maxRetries int,
) (ICircuitBreaker, error) {
	want := breakerConfig{maxConcurrent, maxRequests, windowSeconds, maxRetries}

	m.mu.Lock()
	defer m.mu.Unlock()

	if cb, ok := m.cb[name]; ok {
		if got := m.cfg[name]; got != want {
			return nil, fmt.Errorf(
				"circuit breaker %q já existe com configuração divergente (existente: %+v, solicitada: %+v)",
				name, got, want)
		}
		return cb, nil
	}

	cb := NewCircuitBreaker(name, maxConcurrent, maxRequests, windowSeconds, maxRetries)
	m.cb[name] = cb
	m.cfg[name] = want
	return cb, nil
}

// List devolve os nomes registrados, em ordem indefinida (R2).
func (m *manager) List() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.cb))
	for name := range m.cb {
		names = append(names, name)
	}
	return names
}

// Remove para o breaker (Stop) e o retira do registro (R2). No-op para nome
// inexistente. O Stop ocorre fora do lock: ele aguarda o shutdown da
// goroutine do token bucket.
func (m *manager) Remove(name string) {
	m.mu.Lock()
	cb, ok := m.cb[name]
	if ok {
		delete(m.cb, name)
		delete(m.cfg, name)
	}
	m.mu.Unlock()

	if ok {
		cb.Stop()
	}
}

// StopAll para todos os breakers registrados (R2), mantendo-os no registro —
// útil no shutdown gracioso da aplicação. Stop é idempotente, então StopAll
// também é.
func (m *manager) StopAll() {
	m.mu.RLock()
	all := make([]ICircuitBreaker, 0, len(m.cb))
	for _, cb := range m.cb {
		all = append(all, cb)
	}
	m.mu.RUnlock()

	for _, cb := range all {
		cb.Stop()
	}
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

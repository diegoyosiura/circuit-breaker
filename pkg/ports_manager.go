package circuitbreaker

type IManager interface {
	NewCircuitBreaker(name string, maxConcurrent, maxRequests int, windowSeconds int, maxRetries int) ICircuitBreaker
	GetCircuitBreaker(name string) ICircuitBreaker
}

// Package circuitbreaker re-exporta a API pública de pkg/ na raiz do módulo,
// permitindo o import limpo:
//
//	import "github.com/diegoyosiura/circuit-breaker"
//
// sem o alias manual que o path histórico exige (o pacote em pkg/ chama-se
// circuitbreaker, mas o path termina em /pkg). O import histórico
// "github.com/diegoyosiura/circuit-breaker/pkg" continua 100% válido: os
// tipos abaixo são ALIASES (type X = Y) — idênticos entre os dois caminhos,
// valores fluem livremente entre eles. Nenhuma das duas portas será removida.
package circuitbreaker

import (
	"time"

	pkg "github.com/diegoyosiura/circuit-breaker/pkg"
)

// Tipos do contrato congelado e das extensões — aliases idênticos aos de pkg/.
type (
	ICircuitBreaker   = pkg.ICircuitBreaker
	IManager          = pkg.IManager
	IManagerLifecycle = pkg.IManagerLifecycle
	IManagerStrict    = pkg.IManagerStrict
	EndpointMetrics   = pkg.EndpointMetrics
	Option            = pkg.Option
	BreakerState      = pkg.BreakerState
	StateReporter     = pkg.StateReporter
	BreakerSpec       = pkg.BreakerSpec
)

// Estados da máquina opcional (WithBreaker).
const (
	StateDisabled = pkg.StateDisabled
	StateClosed   = pkg.StateClosed
	StateOpen     = pkg.StateOpen
	StateHalfOpen = pkg.StateHalfOpen
)

// Sentinelas — os MESMOS valores de pkg/ (errors.Is intercambiável).
var (
	ErrStopped          = pkg.ErrStopped
	ErrRetriesExhausted = pkg.ErrRetriesExhausted
	ErrCircuitOpen      = pkg.ErrCircuitOpen
)

// NewCircuitBreaker cria um breaker com o contrato clássico congelado.
func NewCircuitBreaker(name string, maxConcurrent, maxRequests int, windowSeconds int, maxRetries int) ICircuitBreaker {
	return pkg.NewCircuitBreaker(name, maxConcurrent, maxRequests, windowSeconds, maxRetries)
}

// NewCircuitBreakerWithOptions cria um breaker com as opções da Fase 4
// (opt-in; sem opções é idêntico a NewCircuitBreaker).
func NewCircuitBreakerWithOptions(name string, maxConcurrent, maxRequests int, windowSeconds int, maxRetries int, opts ...Option) ICircuitBreaker {
	return pkg.NewCircuitBreakerWithOptions(name, maxConcurrent, maxRequests, windowSeconds, maxRetries, opts...)
}

// NewManager cria o registro nomeado de breakers.
func NewManager() IManager { return pkg.NewManager() }

// Opções da Fase 4 — forwards diretos.
func WithBreaker(consecutiveFailures int, openFor time.Duration, halfOpenProbes int) Option {
	return pkg.WithBreaker(consecutiveFailures, openFor, halfOpenProbes)
}

func WithStatusCodeFailure(min int) Option { return pkg.WithStatusCodeFailure(min) }

func WithRetryPolicy(fn func(error) bool) Option { return pkg.WithRetryPolicy(fn) }

func WithExponentialBackoff(base, max time.Duration, jitter bool) Option {
	return pkg.WithExponentialBackoff(base, max, jitter)
}

func WithDefaultTimeout(d time.Duration) Option { return pkg.WithDefaultTimeout(d) }

func WithBurst(n int) Option { return pkg.WithBurst(n) }

func WithRetryAfter(maxWait time.Duration) Option { return pkg.WithRetryAfter(maxWait) }

// ConfigureManager registra um mapa nome→BreakerSpec de uma só vez
// (validação tudo-ou-nada; ver pkg.ConfigureManager).
func ConfigureManager(m IManager, specs map[string]BreakerSpec) (map[string]ICircuitBreaker, error) {
	return pkg.ConfigureManager(m, specs)
}

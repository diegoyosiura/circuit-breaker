package circuitbreaker

// Opções da Fase 4 (PLANO.md) — todas OPT-IN e inertes por default.
// NewCircuitBreakerWithOptions sem opções é comportamentalmente idêntico a
// NewCircuitBreaker (travado por teste); cada With* liga exatamente um
// comportamento novo. O contrato congelado (ICircuitBreaker/IManager) não é
// tocado: tudo aqui é aditivo.

import (
	"math/rand/v2"
	"time"
)

// Option configura um circuit breaker criado via NewCircuitBreakerWithOptions.
type Option func(*circuitBreaker)

// expBackoffConfig parametriza o backoff exponencial (WithExponentialBackoff).
type expBackoffConfig struct {
	base   time.Duration
	max    time.Duration
	jitter bool
}

// WithBreaker liga a máquina de estados closed/open/half-open (ver state.go):
// após `consecutiveFailures` falhas consecutivas o circuito ABRE e Do()
// retorna ErrCircuitOpen imediatamente (fast-fail, sem tocar o downstream)
// por `openFor`; depois, até `halfOpenProbes` sondas simultâneas são
// admitidas — sucesso fecha, falha reabre. O estado é consultável via a
// interface opcional StateReporter.
func WithBreaker(consecutiveFailures int, openFor time.Duration, halfOpenProbes int) Option {
	return func(cb *circuitBreaker) {
		cb.state = newBreakerState(consecutiveFailures, openFor, halfOpenProbes)
	}
}

// WithStatusCodeFailure classifica respostas com StatusCode >= min como
// FALHA nas métricas (FailedRequests em vez de SuccessfulRequests) e na
// máquina de estados. A resposta continua sendo devolvida ao chamador com
// err == nil — a política muda a CLASSIFICAÇÃO, não o retorno nem o retry.
// Uso típico: WithStatusCodeFailure(500).
func WithStatusCodeFailure(min int) Option {
	return func(cb *circuitBreaker) {
		cb.statusFailMin = min
	}
}

// WithRetryPolicy substitui a classificação de retryabilidade padrão
// (isRetryable: net.Error com Timeout/Temporary). Permite, por exemplo,
// retentar ECONNREFUSED ou usar errors.As para alcançar causas embrulhadas
// com %w — decisões deliberadamente NÃO tomadas no default (D1).
func WithRetryPolicy(fn func(error) bool) Option {
	return func(cb *circuitBreaker) {
		cb.retryPolicy = fn
	}
}

// WithExponentialBackoff troca o backoff fixo (500ms) por exponencial:
// tentativa n espera min(max, base·2ⁿ); com jitter, o valor é sorteado em
// [d/2, d] para descorrelacionar rebanhos de clientes.
func WithExponentialBackoff(base, max time.Duration, jitter bool) Option {
	return func(cb *circuitBreaker) {
		if base <= 0 {
			base = 100 * time.Millisecond
		}
		if max < base {
			max = base
		}
		cb.expBackoff = &expBackoffConfig{base: base, max: max, jitter: jitter}
	}
}

// WithDefaultTimeout aplica um teto de duração APENAS quando o chamador não
// definiu nenhum: se cl.Timeout == 0 E o request não tem deadline, o contexto
// é envolvido com context.WithTimeout(d). Chamadas com client/deadline
// próprios não são afetadas — por isso é seguro; imposto por default seria
// mudança de comportamento para chamadas longas legítimas (CB.md A10).
func WithDefaultTimeout(d time.Duration) Option {
	return func(cb *circuitBreaker) {
		cb.defaultTimeout = d
	}
}

// NewCircuitBreakerWithOptions cria um ICircuitBreaker com os mesmos
// parâmetros posicionais de NewCircuitBreaker mais opções da Fase 4.
// Sem opções, o comportamento é idêntico ao de NewCircuitBreaker.
func NewCircuitBreakerWithOptions(
	name string,
	maxConcurrent, maxRequests int,
	windowSeconds int,
	maxRetries int,
	opts ...Option,
) ICircuitBreaker {
	cb := newCircuitBreaker(name, maxConcurrent, maxRequests, windowSeconds, maxRetries)
	for _, opt := range opts {
		opt(cb)
	}
	return cb
}

// backoffFor calcula a espera antes da tentativa attempt+1.
func (cb *circuitBreaker) backoffFor(attempt int) time.Duration {
	if cb.expBackoff == nil {
		return cb.backoff
	}
	d := cb.expBackoff.base << attempt
	if d <= 0 || d > cb.expBackoff.max { // <<= pode estourar
		d = cb.expBackoff.max
	}
	if cb.expBackoff.jitter && d > 1 {
		d = d/2 + rand.N(d/2)
	}
	return d
}

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
//
// ATENÇÃO: valores <= 0 são sanitizados para os MÍNIMOS (threshold=1,
// openFor=30s, probes=1) — WithBreaker(0, 0, 0) é o breaker mais agressivo
// possível, não um no-op [hunt API-07].
//
// A admissão é decidida na ENTRADA de Do(): chamadas já admitidas (na fila
// do semáforo/token) prosseguem mesmo que o circuito abra durante a espera
// [hunt SM-4 — comportamento deliberado, comum às implementações canônicas].
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

// WithBurst desacopla a CAPACIDADE do bucket (rajada) da TAXA de reposição.
// Sem esta opção, capacidade = maxRequests: após qualquer período ocioso, a
// primeira janela passa até ~2x maxRequests — o que VIOLA limites rígidos de
// APIs externas ("200 req/min" da CCEE vira ~400 no pior caso). Regra segura
// para limite rígido L por janela W: maxRequests + burst <= L, com
// windowSeconds = W. Ex.: L=200/min → (maxRequests=199, windowSeconds=60,
// WithBurst(1)) usa ~99,5% do orçamento sem jamais exceder em NENHUMA janela
// deslizante. Valores <= 0 viram 1 (taxa pura, sem rajada).
//
// Nota para taxas > 1000 req/s (tick engrossado de 25ms): a taxa sustentada
// fica limitada a burst/25ms; use burst >= taxa*0,025 nesses casos.
func WithBurst(n int) Option {
	return func(cb *circuitBreaker) {
		if n < 1 {
			n = 1
		}
		cb.burst = n
	}
}

// WithRetryAfter honra o header Retry-After de respostas 429/503:
//   - estende um gate GLOBAL do breaker — todas as chamadas (novas e retries)
//     aguardam o prazo pedido pela API antes de voltar a bater;
//   - a própria chamada re-tenta após o prazo, se restarem tentativas e o
//     corpo for rebobinável; senão, a resposta 429/503 é devolvida ao chamador
//     (com o gate já armado para as próximas).
//
// maxWait limita a espera (headers de minutos/horas não penduram chamadas);
// <= 0 vira 5 minutos. A espera respeita o contexto do request. Combine com
// WithStatusCodeFailure(429)+WithBreaker para o freio de emergência completo.
func WithRetryAfter(maxWait time.Duration) Option {
	return func(cb *circuitBreaker) {
		if maxWait <= 0 {
			maxWait = 5 * time.Minute
		}
		cb.retryAfterMax = maxWait
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
		if opt != nil { // Option nil explícita é ignorada [hunt OPT-02]
			opt(cb)
		}
	}
	cb.initTokenBucket() // pós-options: WithBurst altera a capacidade
	return cb
}

// backoffFor calcula a espera antes da tentativa attempt+1. A duplicação é
// iterativa com parada no teto: shift direto (base << attempt) pode dar wrap
// para um positivo pequeno e escapar do clamp [hunt OPT-03].
func (cb *circuitBreaker) backoffFor(attempt int) time.Duration {
	if cb.expBackoff == nil {
		return cb.backoff
	}
	d := cb.expBackoff.base
	for i := 0; i < attempt && d < cb.expBackoff.max; i++ {
		d *= 2
	}
	if d <= 0 || d > cb.expBackoff.max {
		d = cb.expBackoff.max
	}
	if cb.expBackoff.jitter && d > 1 {
		d = d/2 + rand.N(d/2)
	}
	return d
}

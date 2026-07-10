package circuitbreaker

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Limites de sanidade do token bucket (F5/D5 do PLANO.md).
// maxEndpointsPerHost limita a cardinalidade de métricas por host (inclui as
// chaves sintéticas ::root/::other); excedentes agregam em "::other".
const maxEndpointsPerHost = 1026

const (
	minTokenInterval  = time.Millisecond      // abaixo disto o ticker é engrossado
	clampedTick       = 25 * time.Millisecond // tick real quando clampado (≤40 acordadas/s)
	maxBucketCapacity = 1_000_000             // teto do burst para configs degeneradas
)

// circuitBreaker implements a resilient HTTP request handler with the following features:
// - Maximum number of concurrent requests
// - Rate-limiting using a token bucket algorithm
// - Automatic retries for transient network failures
// - Context-aware cancellation and timeout support
type circuitBreaker struct {
	Name          string        // Circuit identifier
	MaxConcurrent int           // Maximum concurrent requests allowed
	MaxRetries    int           // Number of retry attempts allowed for retryable failures
	sem           chan struct{} // Semaphore to control concurrent request slots
	tokens        chan struct{} // Token bucket channel to regulate request rate
	tokenInterval time.Duration // Interval between refill ticks
	tokensPerTick int           // Tokens restored per tick (>1 when the interval is clamped)
	tokenStop     chan struct{} // Channel to signal shutdown of the token generator

	backoff time.Duration // Delay between retry attempts (default 500ms)

	// Fase 4 (opt-in via NewCircuitBreakerWithOptions; nil/zero = inerte)
	state          *breakerState     // máquina closed/open/half-open (WithBreaker)
	statusFailMin  int               // status >= min conta como falha (WithStatusCodeFailure)
	retryPolicy    func(error) bool  // substitui isRetryable (WithRetryPolicy)
	expBackoff     *expBackoffConfig // backoff exponencial (WithExponentialBackoff)
	defaultTimeout time.Duration     // teto quando o chamador não define nenhum (WithDefaultTimeout)

	// Anti-bloqueio (opt-in; zero = inerte)
	maxRequests     int           // guardado para initTokenBucket (pós-options)
	windowSeconds   int           // idem
	burst           int           // capacidade do bucket desacoplada da taxa (WithBurst)
	retryAfterMax   time.Duration // teto de espera do Retry-After (WithRetryAfter; 0 = off)
	retryAfterUntil atomic.Int64  // gate global (unixnano): 429/503 pausa TODO o breaker

	tokenWaitGroup sync.WaitGroup // WaitGroup to ensure graceful token goroutine shutdown
	stopOnce       sync.Once      // Ensures Stop() only closes the channel once

	metricsMu sync.RWMutex
	metrics   map[string]map[string]*endpointState
}

// NewCircuitBreaker initializes a new ICircuitBreaker instance with concurrency, rate, and retry controls.
// - name: Identifier for this circuit
// - maxConcurrent: Max number of simultaneous requests
// - maxRequests: Max number of requests per time window
// - windowSeconds: Duration of the rate-limiting window
// - maxRetries: Max number of retry attempts on failure
func NewCircuitBreaker(name string, maxConcurrent, maxRequests int, windowSeconds int, maxRetries int) ICircuitBreaker {
	cb := newCircuitBreaker(name, maxConcurrent, maxRequests, windowSeconds, maxRetries)
	cb.initTokenBucket()
	return cb
}

// newCircuitBreaker é o núcleo concreto compartilhado por NewCircuitBreaker
// e NewCircuitBreakerWithOptions.
func newCircuitBreaker(name string, maxConcurrent, maxRequests int, windowSeconds int, maxRetries int) *circuitBreaker {
	cb := &circuitBreaker{
		Name:          name,
		MaxConcurrent: maxConcurrent,
		MaxRetries:    maxRetries,
		backoff:       500 * time.Millisecond,
	}

	if maxConcurrent > 0 {
		cb.sem = make(chan struct{}, maxConcurrent)
	}

	cb.maxRequests = maxRequests
	cb.windowSeconds = windowSeconds

	return cb
}

// initTokenBucket cria e liga o token bucket. É chamado pelos construtores
// públicos DEPOIS de as options serem aplicadas — WithBurst precisa alterar a
// capacidade antes do pré-fill e do início do refiller.
func (cb *circuitBreaker) initTokenBucket() {
	maxRequests, windowSeconds := cb.maxRequests, cb.windowSeconds
	if maxRequests <= 0 || windowSeconds <= 0 {
		return
	}

	// Capacidade = burst. Default: maxRequests (comportamento histórico);
	// WithBurst desacopla — burst pequeno permite usar ~todo o orçamento de
	// APIs com limite rígido (regra: maxRequests + burst <= limite/janela).
	// Teto para configs degeneradas: sem ele, capacidade gigante custa
	// dezenas de segundos de pré-fill e anula o rate limit (CB.md A16).
	capacity := maxRequests
	if cb.burst > 0 {
		capacity = cb.burst
	}
	if capacity > maxBucketCapacity {
		capacity = maxBucketCapacity
	}
	cb.tokens = make(chan struct{}, capacity)

	interval := (time.Duration(windowSeconds) * time.Second) / time.Duration(maxRequests)
	cb.tokensPerTick = 1
	if interval < minTokenInterval {
		// Tick engrossado (25ms) com reposição em lote: preserva a taxa
		// nominal (erro de arredondamento <4% acima de 1000 req/s) e
		// reduz as acordadas do refiller de 1000/s para 40/s (CB.md A16;
		// configs com intervalo >= 1ms não são afetadas).
		perSecond := float64(maxRequests) / float64(windowSeconds)
		cb.tokensPerTick = int(math.Round(perSecond * clampedTick.Seconds()))
		if cb.tokensPerTick < 1 {
			cb.tokensPerTick = 1
		}
		if cb.tokensPerTick > capacity {
			cb.tokensPerTick = capacity // nunca repõe mais que o bucket comporta
		}
		interval = clampedTick
	}
	cb.tokenInterval = interval
	cb.tokenStop = make(chan struct{})

	for i := 0; i < cap(cb.tokens); i++ {
		cb.tokens <- struct{}{}
	}

	cb.startTokenBucket()
}

// startTokenBucket starts a background goroutine that generates tokens at regular intervals
// to enforce the request rate limit using a token bucket mechanism.
func (cb *circuitBreaker) startTokenBucket() {
	if cb.tokens == nil || cb.tokenStop == nil || cb.tokenInterval <= 0 {
		return
	}

	cb.tokenWaitGroup.Add(1)
	go func() {
		defer cb.tokenWaitGroup.Done()
		ticker := time.NewTicker(cb.tokenInterval)
		defer ticker.Stop()

		for {
			select {
			case <-cb.tokenStop:
				return // Stop signal received
			case <-ticker.C:
			refill:
				for range cb.tokensPerTick {
					select {
					case cb.tokens <- struct{}{}:
					default:
						// Bucket cheio: nada mais a repor neste tick — sem o
						// break rotulado, um lote grande viraria milhões de
						// sends falhos por tick (busy-loop disfarçado).
						break refill
					}
				}
			}
		}
	}()
}

// Do executes the HTTP request, ensuring that:
//   - Concurrency and rate limits are respected (the semaphore slot is held
//     for the WHOLE call, including every retry and backoff wait)
//   - Retry logic is applied on retryable errors (net.Error with Timeout or
//     Temporary true — see isRetryable); bodies are replayed via GetBody,
//     and non-rewindable bodies are never retried
//   - Context cancellation or timeout is observed, including during backoff
//
// cl may be nil (http.DefaultClient is used); callers SHOULD supply a client
// with Timeout or a request deadline — a server that accepts the connection
// and never answers would otherwise pin a semaphore slot indefinitely.
// A response is returned whenever the transport succeeds, REGARDLESS of the
// HTTP status: 4xx/5xx count as successes in the metrics; inspect
// resp.StatusCode. After Stop(), Do returns ErrStopped once the remaining
// tokens are exhausted.
func (cb *circuitBreaker) Do(req *http.Request, cl *http.Client) (finalResp *http.Response, finalErr error) {
	// Requests malformados retornam erro em vez de panic [hunt API-05].
	if req == nil || req.URL == nil {
		return nil, errors.New("circuitbreaker: nil *http.Request or nil request URL")
	}

	// R1: nil client falls back to http.DefaultClient instead of panicking.
	// Callers SHOULD provide a client with Timeout (or a request deadline):
	// a hung server otherwise pins the semaphore slot forever (CB.md A10).
	if cl == nil {
		cl = http.DefaultClient
	}

	// WithDefaultTimeout: aplica-se APENAS quando o chamador não definiu
	// nenhum limite (client sem Timeout e request sem deadline). O cancel do
	// contexto NÃO pode rodar no retorno de Do: isso mataria a leitura do
	// Body pelo chamador (o transporte aborta a conexão de ctx cancelado)
	// [hunt SM-2]. Em sucesso, o cancel é amarrado ao Close() do Body; o
	// próprio timeout garante que o contexto não vaza indefinidamente.
	if cb.defaultTimeout > 0 && cl.Timeout == 0 {
		if _, has := req.Context().Deadline(); !has {
			ctx, cancel := context.WithTimeout(req.Context(), cb.defaultTimeout)
			req = req.WithContext(ctx)
			defer func() {
				if finalErr != nil || finalResp == nil || finalResp.Body == nil {
					cancel()
					return
				}
				finalResp.Body = &cancelOnClose{ReadCloser: finalResp.Body, cancel: cancel}
			}()
		}
	}

	// lastErr guarda a última falha REAL de transporte: usada para escalar
	// desfechos neutros (tempo esgotado no backoff/token APÓS falhas reais)
	// para falha na máquina de estados [hunt DO-INT-2].
	var lastErr error

	// WithBreaker: fast-fail ANTES de consumir semáforo/token — o propósito
	// do circuito aberto é não gastar recurso algum com downstream doente.
	probe := false
	var probeGen uint64
	if cb.state != nil {
		var allowErr error
		probe, probeGen, allowErr = cb.state.allow(time.Now())
		if allowErr != nil {
			return nil, allowErr
		}
		defer func() {
			// Panic no transporte não pode virar "sucesso" (finalResp/finalErr
			// zerados): conta como falha e o panic segue propagando [hunt API-04].
			if r := recover(); r != nil {
				cb.state.report(probe, probeGen, outcomeFailure)
				panic(r)
			}
			o := classifyOutcome(finalResp, finalErr, cb.statusFailMin)
			// Escalação ESTREITA [DO-INT-2]: apenas deadline que estourou após
			// falhas reais de transporte vira falha (o downstream falhou E está
			// lento). Cancel explícito e erros locais permanecem neutros.
			if o == outcomeNeutral && lastErr != nil && errors.Is(finalErr, context.DeadlineExceeded) {
				o = outcomeFailure
			}
			cb.state.report(probe, probeGen, o)
		}()
	}

	if cb.sem != nil {
		// Wait for a concurrency slot or return if context is cancelled
		select {
		case cb.sem <- struct{}{}:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		defer func() { <-cb.sem }() // Release concurrency slot
	}

	host := req.URL.Host
	endpoint := req.URL.Path
	if endpoint == "" {
		endpoint = "/"
	}

	for attempt := 0; attempt <= cb.MaxRetries; attempt++ {
		start := time.Now()
		// WithRetryAfter: se a API pediu pausa (gate global), espera aqui —
		// vale para chamadas novas e para retries.
		if err := cb.waitRetryAfterGate(req.Context()); err != nil {
			cb.recordFailure(host, endpoint, start, time.Now())
			if !errors.Is(err, ErrStopped) { // espelha o caminho do token
				cb.recordTokenCancel(host, endpoint)
			}
			return nil, err
		}
		if err := cb.waitForToken(req.Context()); err != nil {
			// Semântica original preservada (D3): o cancelamento conta como
			// falha SEM tentativa (failed pode exceder total). O contador
			// aditivo abaixo permite distinguir cancelamentos locais de
			// falhas remotas nos dashboards [R4].
			cb.recordFailure(host, endpoint, start, time.Now())
			if !errors.Is(err, ErrStopped) {
				cb.recordTokenCancel(host, endpoint)
			}
			return nil, err
		}

		cb.recordAttempt(host, endpoint, start, time.Now())
		newReq := req.Clone(req.Context())
		// Clone copies the Body by reference: the first attempt consumes the
		// reader, so every retry must rewind it via GetBody (CB.md A2 —
		// without this, retries resend an empty body).
		if attempt > 0 && req.Body != nil {
			body, bodyErr := req.GetBody()
			if bodyErr != nil {
				// Registra a falha da tentativa (senão total > ok+fail) e marca
				// como erro LOCAL — não é evidência sobre o downstream [DO-INT-4].
				cb.recordFailure(host, endpoint, start, time.Now())
				return nil, &localOpError{err: bodyErr}
			}
			newReq.Body = body
		}

		// Attempt the request
		resp, err := cl.Do(newReq)
		if err == nil {
			// WithStatusCodeFailure: classifica 4xx/5xx como falha nas
			// métricas (a resposta continua sendo devolvida com err == nil).
			if cb.statusFailMin > 0 && resp.StatusCode >= cb.statusFailMin {
				cb.recordFailure(host, endpoint, start, time.Now())
			} else {
				cb.recordSuccess(host, endpoint, start, time.Now())
			}

			// WithRetryAfter: 429/503 com header estende o gate global e,
			// havendo tentativas restantes e corpo rebobinável, aguarda o
			// que a API pediu (limitado a retryAfterMax) e re-tenta.
			if cb.retryAfterMax > 0 && retryAfterStatus(resp.StatusCode) {
				if delay, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
					if delay > cb.retryAfterMax {
						delay = cb.retryAfterMax
					}
					until := time.Now().Add(delay)
					cb.extendRetryAfterGate(until)

					if attempt < cb.MaxRetries && (req.Body == nil || req.GetBody != nil) {
						drainAndClose(resp)
						cb.recordRetry(host, endpoint, start, time.Now())
						// Piso de backoff: "Retry-After: 0" (ou data no
						// passado) não pode virar marteladas imediatas na
						// API que acabou de pedir pausa [hunt RA-04].
						wait := time.Until(until)
						if floor := cb.backoffFor(attempt); wait < floor {
							wait = floor
						}
						select {
						case <-req.Context().Done():
							return nil, req.Context().Err()
						case <-time.After(wait):
						}
						continue
					}
				}
			}
			return resp, nil // Success (transport-level)
		}

		lastErr = err
		cb.recordFailure(host, endpoint, start, time.Now())
		// Return immediately if the error is not retryable
		// (WithRetryPolicy substitui a classificação padrão)
		retryable := isRetryable
		if cb.retryPolicy != nil {
			retryable = cb.retryPolicy
		}
		if !retryable(err) {
			return nil, err
		}

		// A non-rewindable body (GetBody == nil) cannot be replayed: retrying
		// would resend a consumed reader (hard failure or silent empty body),
		// so the attempt's error is returned instead [D2].
		if req.Body != nil && req.GetBody == nil {
			return nil, err
		}

		// If retries remain, back off slightly before trying again.
		// The wait observes the request context: a cancelled caller returns
		// immediately instead of sleeping through the backoff (CB.md A6).
		if attempt < cb.MaxRetries {
			cb.recordRetry(host, endpoint, start, time.Now())
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(cb.backoffFor(attempt)):
			}
		}
	}

	// All retry attempts failed. The text is frozen (informal contract);
	// the last cause is recoverable via errors.Is/As (F7).
	return nil, &retriesExhaustedError{last: lastErr}
}

// retryAfterStatus indica os códigos que carregam Retry-After por convenção.
func retryAfterStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

// parseRetryAfter aceita os dois formatos do header (segundos ou HTTP-date).
func parseRetryAfter(h string, now time.Time) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0, false
		}
		// Satura ANTES de multiplicar: um header hostil/bugado gigante
		// (ex.: "10000000000") estouraria o int64 para NEGATIVO, escapando
		// do clamp e desarmando o gate — a proteção anti-bloqueio falharia
		// aberta exatamente sob o sinal de bloqueio [hunt AB-01].
		if int64(secs) > int64(math.MaxInt64)/int64(time.Second) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(h); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// extendRetryAfterGate estende o gate GLOBAL do breaker: uma resposta de
// bloqueio (429/503 + Retry-After) pausa TODAS as chamadas até `until` —
// continuar batendo numa API que pediu pausa é o caminho mais curto para o
// bloqueio de conta.
func (cb *circuitBreaker) extendRetryAfterGate(until time.Time) {
	n := until.UnixNano()
	for {
		cur := cb.retryAfterUntil.Load()
		if cur >= n || cb.retryAfterUntil.CompareAndSwap(cur, n) {
			return
		}
	}
}

// waitRetryAfterGate bloqueia (respeitando ctx e Stop) enquanto o gate
// estiver ativo. O laço RE-CHECA o gate ao acordar: outra resposta 429/503
// pode tê-lo estendido durante o sono — sem o loop, chamadas atravessariam
// a pausa estendida [hunt AB-02]. Stop() desbloqueia via tokenStop quando o
// breaker tem rate limit (canal nil nunca dispara no select; sem rate limit,
// a espera é limitada por retryAfterMax) [hunt RA-03].
func (cb *circuitBreaker) waitRetryAfterGate(ctx context.Context) error {
	if cb.retryAfterMax <= 0 {
		return nil
	}
	for {
		d := time.Until(time.Unix(0, cb.retryAfterUntil.Load()))
		if d <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cb.tokenStop:
			return ErrStopped
		case <-time.After(d):
			// volta ao topo: o gate pode ter sido estendido enquanto dormia
		}
	}
}

// drainAndClose consome (limitado) e fecha o corpo antes de um retry —
// obrigatório para reutilizar a conexão e não vazar o body.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// cancelOnClose amarra o cancel do contexto de WithDefaultTimeout ao Close()
// do Body: o chamador consegue ler a resposta inteira e o contexto é liberado
// no fechamento (ou, no pior caso, quando o próprio timeout dispara).
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// Stop gracefully shuts down the token bucket goroutine. Idempotent and
// safe for concurrent use. After Stop, in-flight and future Do calls are
// released with ErrStopped once the tokens remaining in the bucket are
// consumed. A breaker without rate limit has nothing to stop (no-op).
func (cb *circuitBreaker) Stop() {
	if cb.tokenStop == nil {
		return
	}

	cb.stopOnce.Do(func() {
		close(cb.tokenStop)
	})
	cb.tokenWaitGroup.Wait()
}

func (cb *circuitBreaker) waitForToken(ctx context.Context) error {
	if cb.tokens == nil {
		return nil
	}

	// Contexto já cancelado não consome token: chamadas mortas-na-chegada
	// não podem queimar budget de rate de chamadas vivas [hunt TB-02].
	if err := ctx.Err(); err != nil {
		return err
	}

	// Fast path: tokens já disponíveis são atendidos mesmo após Stop()
	// (preserva o comportamento observável de "sobras ainda atendem").
	select {
	case <-cb.tokens:
		return nil
	default:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-cb.tokenStop:
		// Stop() fechou o canal: desbloqueia todos os waiters em vez de
		// pendurar para sempre goroutines (e slots do semáforo) [A3].
		return ErrStopped
	case <-cb.tokens:
		return nil
	}
}

// isRetryable classifies transient failures. Behaviour frozen by the
// characterization table (T0.2, CB-TESTES.md scenario 24):
//   - retryable: net.Error with Timeout() or Temporary() true (genuine
//     timeouts, os.ErrDeadlineExceeded, temporary conditions)
//   - NOT retryable: ECONNRESET/ECONNREFUSED (their Timeout/Temporary are
//     false — retrying a refused connection would hammer a downed service),
//     generic errors, and retryable errors wrapped with %w inside the
//     transport (url.Error probes e.Err by direct type-assertion).
//
// The former branches below this check (*net.OpError, errors.Is ECONNRESET/
// ECONNREFUSED/ErrDeadlineExceeded) were unreachable dead code: every such
// error already satisfies net.Error and is decided here [F8/D1].
//
//nolint:staticcheck // Temporary() é deprecated, mas removê-lo mudaria o
// comportamento efetivo em produção — decisão D1 do PLANO.md.
func isRetryable(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Temporary() || netErr.Timeout()
	}
	return false
}

// Metrics returns a snapshot of the aggregated metrics per host and endpoint.
// Every field of the returned structs is a fresh copy — no aliasing with the
// live state (CB.md A1).
func (cb *circuitBreaker) Metrics() map[string]map[string]EndpointMetrics {
	cb.metricsMu.RLock()
	defer cb.metricsMu.RUnlock()

	result := make(map[string]map[string]EndpointMetrics, len(cb.metrics))
	for host, endpoints := range cb.metrics {
		endpointCopy := make(map[string]EndpointMetrics, len(endpoints))
		for endpoint, state := range endpoints {
			endpointCopy[endpoint] = state.snapshot()
		}
		result[host] = endpointCopy
	}

	return result
}

func (cb *circuitBreaker) recordAttempt(host, endpoint string, start, end time.Time) {
	cb.updateMetrics(host, endpoint, func(s *endpointState) {
		s.totalRequests++
		s.req.record(start, end)
	})
}

func (cb *circuitBreaker) recordSuccess(host, endpoint string, start, end time.Time) {
	cb.updateMetrics(host, endpoint, func(s *endpointState) {
		s.successfulRequests++
		s.success.record(start, end)
	})
}

func (cb *circuitBreaker) recordFailure(host, endpoint string, start, end time.Time) {
	cb.updateMetrics(host, endpoint, func(s *endpointState) {
		s.failedRequests++
		s.failure.record(start, end)
	})
}

func (cb *circuitBreaker) recordTokenCancel(host, endpoint string) {
	cb.updateMetrics(host, endpoint, func(s *endpointState) {
		s.tokenWaitCancellations++
	})
}

func (cb *circuitBreaker) recordRetry(host, endpoint string, start, end time.Time) {
	cb.updateMetrics(host, endpoint, func(s *endpointState) {
		s.retryCount++
		s.retry.record(start, end)
	})
}

// updateMetrics applies the update to the endpoint state AND to the per-host
// "::root" aggregate, under the metrics lock. Each record is O(1): ring
// buffers for the means and a fixed bucket wheel for the window counts
// (metrics_state.go) — the previous slices grew without bound and made every
// Do() progressively slower (CB.md A4).
func (cb *circuitBreaker) updateMetrics(host, endpoint string, updateFn func(*endpointState)) {
	cb.metricsMu.Lock()
	defer cb.metricsMu.Unlock()

	if cb.metrics == nil {
		cb.metrics = make(map[string]map[string]*endpointState)
	}

	// Um path real igual às chaves sintéticas não pode aliasar os agregados
	// [hunt MW-2]. (Paths de URLs reais começam com "/", mas URLs artesanais
	// podem produzir qualquer coisa.)
	if endpoint == "::root" || endpoint == "::other" {
		endpoint = "/" + endpoint
	}

	hostMetrics, ok := cb.metrics[host]
	if !ok {
		hostMetrics = make(map[string]*endpointState)
		cb.metrics[host] = hostMetrics
		hostMetrics["::root"] = &endpointState{}
	}

	state, ok := hostMetrics[endpoint]
	if !ok {
		// Cardinalidade limitada por host: paths únicos ilimitados (ex.:
		// /user/{id}) reteriam ~14KB cada para sempre [hunt API-03]. Ao
		// atingir o teto, novos endpoints agregam em "::other".
		if len(hostMetrics) >= maxEndpointsPerHost {
			endpoint = "::other"
			state = hostMetrics[endpoint]
			if state == nil {
				state = &endpointState{}
				hostMetrics[endpoint] = state
			}
		} else {
			state = &endpointState{}
			hostMetrics[endpoint] = state
		}
	}

	updateFn(state)
	updateFn(hostMetrics["::root"])
}

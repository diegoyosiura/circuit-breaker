package circuitbreaker

// Máquina de estados closed/open/half-open (Fase 4 do PLANO.md).
//
// OPT-IN e inerte por default: só existe quando o breaker é criado via
// NewCircuitBreakerWithOptions(..., WithBreaker(...)). Sem a opção, nenhum
// caminho de execução muda — garantido pelos testes de característica.
//
// Semântica (padrão Fowler):
//   - Closed: operação normal; falhas consecutivas são contadas.
//   - Open: após `threshold` falhas consecutivas, Do() falha rápido com
//     ErrCircuitOpen SEM tocar o downstream, por `openFor`.
//   - Half-open: passado `openFor`, até `maxProbes` requisições-sonda são
//     admitidas; uma sonda bem-sucedida fecha o circuito, uma falha reabre.
//
// O que conta como falha: erro de transporte devolvido por Do e, com
// WithStatusCodeFailure, respostas com status >= min. Cancelamentos LOCAIS
// (contexto cancelado esperando token/backoff, ErrStopped) são NEUTROS —
// não abrem o circuito, pois não evidenciam falha do downstream.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// BreakerState é o estado corrente da máquina (StateDisabled quando o
// breaker foi criado sem WithBreaker).
type BreakerState int

const (
	StateDisabled BreakerState = iota
	StateClosed
	StateOpen
	StateHalfOpen
)

func (s BreakerState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "disabled"
	}
}

// StateReporter é uma interface OPCIONAL implementada pelo breaker deste
// pacote — não faz parte de ICircuitBreaker (contrato congelado). Descubra
// por type assertion:
//
//	if sr, ok := cb.(circuitbreaker.StateReporter); ok { _ = sr.State() }
type StateReporter interface {
	State() BreakerState
}

// outcome classifica o desfecho de uma chamada Do para a máquina de estados.
type outcome int

const (
	outcomeNeutral outcome = iota // cancelamento local: não conta
	outcomeSuccess
	outcomeFailure
)

// classifyOutcome decide o desfecho para a máquina de estados:
//   - err == nil: sucesso (ou falha, se status >= statusFailMin);
//   - context.Canceled EM QUALQUER PONTO da cadeia (inclusive dentro de
//     *url.Error, quando o chamador cancela com a requisição em voo) é
//     NEUTRO — cancelamento explícito é sempre ação do chamador, nunca
//     evidência de falha do downstream [hunt TB-01/SM-3/DO-INT-3];
//   - erro local (GetBody que falhou ao rebobinar) é neutro;
//   - demais erros de transporte (*url.Error, incl. timeouts) = falha;
//   - deadline/ErrStopped fora do transporte = neutro (espera local). O Do()
//     ESCALA neutro→falha quando houve falha real de transporte antes do
//     tempo esgotar [hunt DO-INT-2].
func classifyOutcome(resp *http.Response, err error, statusFailMin int) outcome {
	if err == nil {
		if statusFailMin > 0 && resp != nil && resp.StatusCode >= statusFailMin {
			return outcomeFailure
		}
		return outcomeSuccess
	}
	if errors.Is(err, context.Canceled) {
		return outcomeNeutral
	}
	var le *localOpError
	if errors.As(err, &le) {
		return outcomeNeutral
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return outcomeFailure
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrStopped) {
		return outcomeNeutral
	}
	return outcomeFailure
}

// localOpError marca erros produzidos LOCALMENTE pelo breaker (ex.: GetBody
// falhou ao rebobinar o corpo) — não são evidência sobre o downstream.
type localOpError struct{ err error }

func (e *localOpError) Error() string { return e.err.Error() }
func (e *localOpError) Unwrap() error { return e.err }

// breakerState guarda a máquina; mutex próprio (contenção mínima: duas
// operações O(1) por chamada Do quando habilitada).
type breakerState struct {
	mu sync.Mutex

	threshold int           // falhas consecutivas para abrir
	openFor   time.Duration // tempo em open antes de admitir sondas
	maxProbes int           // sondas simultâneas em half-open

	cur         BreakerState
	consecFails int
	openedAt    time.Time
	probes      int       // sondas em voo em half-open
	gen         uint64    // geração do half-open corrente [hunt SM-1]
	halfOpenAt  time.Time // início do half-open corrente [hunt API-02]
}

func newBreakerState(threshold int, openFor time.Duration, maxProbes int) *breakerState {
	if threshold < 1 {
		threshold = 1
	}
	if openFor <= 0 {
		openFor = 30 * time.Second
	}
	if maxProbes < 1 {
		maxProbes = 1
	}
	return &breakerState{threshold: threshold, openFor: openFor, maxProbes: maxProbes, cur: StateClosed}
}

// allow decide a admissão da chamada. Retorna (probe, gen, nil) para admitir
// — probe=true quando a chamada é uma sonda de half-open, com a GERAÇÃO do
// half-open que a admitiu — ou ErrCircuitOpen.
func (b *breakerState) allow(now time.Time) (bool, uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.cur {
	case StateOpen:
		if now.Sub(b.openedAt) < b.openFor {
			return false, 0, ErrCircuitOpen
		}
		b.enterHalfOpen(now)
		fallthrough
	case StateHalfOpen:
		if b.probes >= b.maxProbes {
			// Sondas penduradas (chamador sem timeout) não podem travar o
			// breaker para sempre: decorrido mais um openFor inteiro sem
			// nenhuma resolução, a geração expira e novas sondas são
			// admitidas [hunt API-02]. Reports das sondas velhas viram
			// no-op pela checagem de geração.
			if now.Sub(b.halfOpenAt) < b.openFor {
				return false, 0, ErrCircuitOpen
			}
			b.enterHalfOpen(now)
		}
		b.probes++
		return true, b.gen, nil
	default: // StateClosed
		return false, 0, nil
	}
}

// enterHalfOpen inicia uma NOVA geração de half-open: sondas de gerações
// anteriores perdem o direito de decrementar o contador ou transicionar o
// estado [hunt SM-1: sem isso, probes fica negativo e maxProbes é violado].
func (b *breakerState) enterHalfOpen(now time.Time) {
	b.cur = StateHalfOpen
	b.probes = 0
	b.gen++
	b.halfOpenAt = now
}

// report registra o desfecho de uma chamada admitida. gen identifica a
// geração de half-open que admitiu a sonda: sondas de gerações passadas são
// ignoradas por completo (não decrementam probes nem transicionam o estado).
func (b *breakerState) report(probe bool, gen uint64, o outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if probe && gen != b.gen {
		return // sonda de uma geração anterior de half-open [hunt SM-1]
	}

	switch b.cur {
	case StateClosed:
		switch o {
		case outcomeSuccess:
			b.consecFails = 0
		case outcomeFailure:
			b.consecFails++
			if b.consecFails >= b.threshold {
				b.cur = StateOpen
				b.openedAt = time.Now()
			}
		}
	case StateHalfOpen:
		if !probe {
			return // chamada antiga (admitida em closed) concluindo agora
		}
		switch o {
		case outcomeSuccess:
			b.cur = StateClosed
			b.consecFails = 0
		case outcomeFailure:
			b.cur = StateOpen
			b.openedAt = time.Now()
		case outcomeNeutral:
			b.probes-- // libera o slot de sonda sem transição
		}
	case StateOpen:
		// chamada antiga concluindo após reabertura: nada a fazer
	}
}

func (b *breakerState) current() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur
}

// State implementa StateReporter (StateDisabled sem WithBreaker).
func (cb *circuitBreaker) State() BreakerState {
	if cb.state == nil {
		return StateDisabled
	}
	return cb.state.current()
}

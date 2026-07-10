package circuitbreaker_test

// Testes do kit anti-bloqueio: WithBurst, WithRetryAfter, BreakerSpec/
// ConfigureManager e a receita completa do freio de emergência (README).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	circuitbreaker "github.com/diegoyosiura/circuit-breaker/pkg"
)

func doWithDeadline(t *testing.T, cb circuitbreaker.ICircuitBreaker, cl *http.Client, url string, d time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := cb.Do(req.WithContext(ctx), cl)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

// WithBurst: a capacidade do bucket é o burst, não maxRequests.
func TestBurst_CapacityDecoupledFromRate(t *testing.T) {
	// taxa 10/s (1 token a cada 100ms) com burst de apenas 2
	cb := circuitbreaker.NewCircuitBreakerWithOptions("b1", 0, 10, 1, 0,
		circuitbreaker.WithBurst(2))
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}

	immediate := 0
	for range 5 {
		if doWithDeadline(t, cb, cl, "http://b1.test/x", 2*time.Millisecond) == nil {
			immediate++
		}
	}
	if immediate != 2 {
		t.Fatalf("burst=2 deve permitir exatamente 2 imediatas: %d", immediate)
	}

	// a TAXA continua a de maxRequests: próximo token em ~100ms (10/s)
	time.Sleep(150 * time.Millisecond)
	if err := doWithDeadline(t, cb, cl, "http://b1.test/x", 2*time.Millisecond); err != nil {
		t.Fatalf("reposição pela taxa nominal deveria ter recarregado: %v", err)
	}
}

// Regra anti-bloqueio: maxRequests + burst <= limite da API. Simulação da
// CCEE (limite 200/min) com (199, 60, WithBurst(1)): NENHUMA janela pode
// exceder 200 — aqui validamos o pior caso pós-ocioso: burst inicial = 1.
func TestBurst_HardLimitRecipe(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreakerWithOptions("b2", 0, 199, 60, 0,
		circuitbreaker.WithBurst(1))
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}

	immediate := 0
	for range 4 {
		if doWithDeadline(t, cb, cl, "http://b2.test/x", 2*time.Millisecond) == nil {
			immediate++
		}
	}
	if immediate != 1 {
		t.Fatalf("WithBurst(1): rajada pós-ociosa deve ser 1 (era %d); pior janela = 1+199 = 200 ✓", immediate)
	}
}

// Sanitização e default inalterado.
func TestBurst_SanitizeAndDefault(t *testing.T) {
	// n <= 0 vira 1
	cb := circuitbreaker.NewCircuitBreakerWithOptions("b3", 0, 100, 1, 0,
		circuitbreaker.WithBurst(-7))
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}
	immediate := 0
	for range 3 {
		if doWithDeadline(t, cb, cl, "http://b3.test/x", 2*time.Millisecond) == nil {
			immediate++
		}
	}
	if immediate != 1 {
		t.Fatalf("WithBurst(<=0) deve virar 1: %d", immediate)
	}

	// sem a opção: capacidade = maxRequests (comportamento histórico)
	cb2 := circuitbreaker.NewCircuitBreaker("b3d", 0, 5, 60, 0)
	defer cb2.Stop()
	immediate = 0
	for range 7 {
		if doWithDeadline(t, cb2, cl, "http://b3d.test/x", 2*time.Millisecond) == nil {
			immediate++
		}
	}
	if immediate != 5 {
		t.Fatalf("default: burst == maxRequests (5): %d", immediate)
	}
}

// WithRetryAfter: honra o header (cap por maxWait) e re-tenta a própria chamada.
func TestRetryAfter_RetriesAfterHeader(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			resp := okResponse(r)
			resp.StatusCode = http.StatusTooManyRequests
			resp.Header.Set("Retry-After", "1") // 1s — o cap de 60ms encurta
			return resp, nil
		}
		return okResponse(r), nil
	})
	cb := circuitbreaker.NewCircuitBreakerWithOptions("ra1", 0, 0, 0, 2,
		circuitbreaker.WithRetryAfter(60*time.Millisecond))
	defer cb.Stop()

	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, "http://ra1.test/x", nil)
	resp, err := cb.Do(req, &http.Client{Transport: tr})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("deveria suceder no retry: %d", resp.StatusCode)
	}
	mu.Lock()
	c := calls
	mu.Unlock()
	if c != 2 {
		t.Fatalf("esperava 2 tentativas: %d", c)
	}
	if elapsed < 55*time.Millisecond || elapsed > 800*time.Millisecond {
		t.Fatalf("espera deveria ser ~60ms (cap sobre 1s do header): %v", elapsed)
	}
}

// O gate é GLOBAL: um 429 pausa também as chamadas paralelas do breaker.
func TestRetryAfter_GateBlocksOtherCalls(t *testing.T) {
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := okResponse(r)
		resp.StatusCode = http.StatusTooManyRequests
		resp.Header.Set("Retry-After", "1")
		return resp, nil
	})
	cb := circuitbreaker.NewCircuitBreakerWithOptions("ra2", 0, 0, 0, 0, // sem retries
		circuitbreaker.WithRetryAfter(90*time.Millisecond))
	defer cb.Stop()
	cl := &http.Client{Transport: tr}

	// 1ª chamada: recebe o 429 (sem retries) e ARMA o gate de ~90ms
	req, _ := http.NewRequest(http.MethodGet, "http://ra2.test/x", nil)
	resp, err := cb.Do(req, cl)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()

	// 2ª chamada com deadline MENOR que o gate: deve morrer esperando,
	// SEM tocar o transport de novo
	err = doWithDeadline(t, cb, cl, "http://ra2.test/x", 25*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("esperava DeadlineExceeded no gate: %v", err)
	}

	// 3ª chamada com folga: passa depois do gate
	start := time.Now()
	resp, err = cb.Do(req, cl)
	if err != nil {
		t.Fatalf("pós-gate: %v", err)
	}
	_ = resp.Body.Close()
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Fatalf("deveria ter aguardado o restante do gate: %v", waited)
	}
}

// Corpo não-rebobinável: devolve o 429 ao chamador (gate armado), sem retry.
func TestRetryAfter_NonRewindableBodyNoRetry(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		resp := okResponse(r)
		resp.StatusCode = http.StatusTooManyRequests
		resp.Header.Set("Retry-After", "1")
		return resp, nil
	})
	cb := circuitbreaker.NewCircuitBreakerWithOptions("ra3", 0, 0, 0, 3,
		circuitbreaker.WithRetryAfter(50*time.Millisecond))
	defer cb.Stop()

	req, _ := http.NewRequest(http.MethodPost, "http://ra3.test/x", strings.NewReader("X"))
	req.GetBody = nil
	resp, err := cb.Do(req, &http.Client{Transport: tr})
	if err != nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("deveria devolver o 429: resp=%v err=%v", resp, err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("corpo não-rebobinável não pode ser retentado: %d", calls)
	}
}

// Sem header (ou opção desligada): comportamento intacto.
func TestRetryAfter_NoHeaderNoRetry(t *testing.T) {
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := okResponse(r)
		resp.StatusCode = http.StatusTooManyRequests
		return resp, nil // SEM Retry-After
	})
	cb := circuitbreaker.NewCircuitBreakerWithOptions("ra4", 0, 0, 0, 3,
		circuitbreaker.WithRetryAfter(50*time.Millisecond))
	defer cb.Stop()

	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, "http://ra4.test/x", nil)
	resp, err := cb.Do(req, &http.Client{Transport: tr})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || time.Since(start) > 100*time.Millisecond {
		t.Fatal("sem header: devolve o 429 direto, sem espera")
	}
}

// Cancelamento durante a espera do Retry-After retorna imediatamente.
func TestRetryAfter_CtxCancelDuringWait(t *testing.T) {
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := okResponse(r)
		resp.StatusCode = http.StatusServiceUnavailable // 503 também honra
		resp.Header.Set("Retry-After", "30")
		return resp, nil
	})
	cb := circuitbreaker.NewCircuitBreakerWithOptions("ra5", 0, 0, 0, 3,
		circuitbreaker.WithRetryAfter(10*time.Second))
	defer cb.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(40 * time.Millisecond); cancel() }()
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, "http://ra5.test/x", nil)
	_, err := cb.Do(req.WithContext(ctx), &http.Client{Transport: tr})
	if !errors.Is(err, context.Canceled) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("cancel deve interromper a espera do Retry-After: err=%v elapsed=%v", err, time.Since(start))
	}
}

// ConfigureManager: criação em lote, idempotência, divergência e Options.
func TestConfigureManager(t *testing.T) {
	m := circuitbreaker.NewManager()
	specs := map[string]circuitbreaker.BreakerSpec{
		"ccee": {MaxRequests: 199, WindowSeconds: 60, MaxRetries: 2,
			Options: []circuitbreaker.Option{
				circuitbreaker.WithBurst(1),
				circuitbreaker.WithBreaker(3, time.Minute, 1),
			}},
		"omie": {MaxConcurrent: 1, MaxRetries: 2},
	}

	breakers, err := circuitbreaker.ConfigureManager(m, specs)
	if err != nil {
		t.Fatalf("ConfigureManager: %v", err)
	}
	if len(breakers) != 2 || breakers["ccee"] == nil || breakers["omie"] == nil {
		t.Fatalf("esperava 2 breakers: %v", breakers)
	}
	if m.GetCircuitBreaker("ccee") != breakers["ccee"] {
		t.Fatal("breaker deve estar registrado no manager")
	}
	// Options aplicadas: ccee tem máquina de estados
	if sr, ok := breakers["ccee"].(circuitbreaker.StateReporter); !ok || sr.State() != circuitbreaker.StateClosed {
		t.Fatal("Options da spec devem ser aplicadas (WithBreaker → StateClosed)")
	}

	// Idempotente: mesma spec devolve as mesmas instâncias
	again, err := circuitbreaker.ConfigureManager(m, specs)
	if err != nil || again["ccee"] != breakers["ccee"] {
		t.Fatalf("reaplicação idêntica deve ser idempotente: %v", err)
	}

	// Divergência posicional: erro e NADA muda (tudo-ou-nada)
	bad := map[string]circuitbreaker.BreakerSpec{
		"novo": {MaxConcurrent: 3},
		"omie": {MaxConcurrent: 99}, // diverge
	}
	if _, err := circuitbreaker.ConfigureManager(m, bad); err == nil {
		t.Fatal("divergência deveria retornar erro")
	}
	if m.GetCircuitBreaker("novo") != nil {
		t.Fatal("tudo-ou-nada: 'novo' não pode ter sido criado")
	}

	if lc, ok := m.(circuitbreaker.IManagerLifecycle); ok {
		lc.StopAll()
	}
}

// RECEITA COMPLETA (README): freio de emergência + as duas métricas de ouro.
func TestRecipe_EmergencyBrakeAndGoldenMetrics(t *testing.T) {
	// API que começou a bloquear: sempre 429 (sem Retry-After, pior caso)
	blocked := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := okResponse(r)
		resp.StatusCode = http.StatusTooManyRequests
		return resp, nil
	})

	m := circuitbreaker.NewManager()
	breakers, err := circuitbreaker.ConfigureManager(m, map[string]circuitbreaker.BreakerSpec{
		"api-bloqueando": {MaxRequests: 100, WindowSeconds: 1, MaxRetries: 0,
			Options: []circuitbreaker.Option{
				circuitbreaker.WithBurst(10),
				circuitbreaker.WithRetryAfter(time.Minute),
				circuitbreaker.WithStatusCodeFailure(429), // 429 = falha
				circuitbreaker.WithBreaker(3, time.Hour, 1), // 3 seguidas → PARA
			}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cb := breakers["api-bloqueando"]
	defer cb.Stop()
	cl := &http.Client{Transport: blocked}

	// 3 respostas 429 abrem o circuito (freio de emergência)
	req, _ := http.NewRequest(http.MethodGet, "http://brake.test/x", nil)
	for range 3 {
		resp, err := cb.Do(req, cl)
		if err != nil {
			t.Fatalf("429 volta com err==nil: %v", err)
		}
		_ = resp.Body.Close()
	}
	if _, err := cb.Do(req, cl); !errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		t.Fatalf("freio: 4ª chamada deveria fast-failar: %v", err)
	}

	// Métrica de ouro 1: ratio_01_failed detecta a API recusando
	mtr := cb.Metrics()["brake.test"]["/x"]
	if mtr.Ratio01FailedRequests != 3 || mtr.FailedRequests != 3 {
		t.Fatalf("ratio_01_failed deveria acusar os 3 bloqueios: %+v", mtr)
	}

	// Métrica de ouro 2: token_wait_cancellations detecta saturação do
	// PRÓPRIO orçamento (antes de a API reclamar)
	sat := circuitbreaker.NewCircuitBreakerWithOptions("saturado", 0, 1, 3600, 0,
		circuitbreaker.WithBurst(1))
	defer sat.Stop()
	ok := &http.Client{Transport: &countingTransport{}}
	req2, _ := http.NewRequest(http.MethodGet, "http://sat.test/x", nil)
	resp, err := sat.Do(req2, ok)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for range 4 {
		_ = doWithDeadline(t, sat, ok, "http://sat.test/x", 15*time.Millisecond)
	}
	if got := sat.Metrics()["sat.test"]["/x"].TokenWaitCancellations; got != 4 {
		t.Fatalf("token_wait_cancellations deveria acusar a saturação: %d", got)
	}

	_ = fmt.Sprintf // mantém fmt no import se refactors removerem usos
}

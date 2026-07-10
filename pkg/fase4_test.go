package circuitbreaker_test

// Testes da Fase 4 (opt-in): máquina de estados, políticas configuráveis e
// defaultTimeout. O primeiro teste prova a INÉRCIA: sem opções, nada muda.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"syscall"
	"testing"
	"time"

	circuitbreaker "github.com/diegoyosiura/circuit-breaker/pkg"
)

// Inércia: NewCircuitBreakerWithOptions SEM opções é idêntico ao clássico —
// State() == StateDisabled, nunca ErrCircuitOpen, 5xx segue como sucesso.
func TestFase4_InertByDefault(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreakerWithOptions("inert", 0, 0, 0, 0)
	defer cb.Stop()

	sr, ok := cb.(circuitbreaker.StateReporter)
	if !ok {
		t.Fatal("breaker deveria implementar StateReporter")
	}
	if sr.State() != circuitbreaker.StateDisabled {
		t.Fatalf("sem WithBreaker, State deve ser StateDisabled: %v", sr.State())
	}

	// 10 falhas consecutivas jamais abrem o circuito sem WithBreaker.
	tr := &countingTransport{failN: 1 << 30, err: errors.New("boom")}
	cl := &http.Client{Transport: tr}
	req, _ := http.NewRequest(http.MethodGet, "http://inert.test/x", nil)
	for range 10 {
		_, err := cb.Do(req, cl)
		if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
			t.Fatal("breaker sem WithBreaker nunca pode retornar ErrCircuitOpen")
		}
	}
	if tr.count() != 10 {
		t.Fatalf("todas as chamadas devem atingir o transport: %d", tr.count())
	}

	// 500 continua contando como sucesso sem WithStatusCodeFailure.
	tr500 := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := okResponse(r)
		resp.StatusCode = 500
		return resp, nil
	})
	resp, err := cb.Do(mustReq(t, "http://inert.test/y"), &http.Client{Transport: tr500})
	if err != nil {
		t.Fatalf("500 deve retornar sem erro: %v", err)
	}
	_ = resp.Body.Close()
	m := cb.Metrics()["inert.test"]["/y"]
	if m.SuccessfulRequests != 1 || m.FailedRequests != 0 {
		t.Fatalf("sem a opção, 5xx conta como sucesso: %+v", m)
	}
}

func mustReq(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// WithBreaker: abre após N falhas consecutivas; aberto = fast-fail sem tocar
// o transport nem as métricas.
func TestFase4_OpensAfterThresholdAndFastFails(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreakerWithOptions("open", 0, 0, 0, 0,
		circuitbreaker.WithBreaker(3, time.Hour, 1))
	defer cb.Stop()
	sr := cb.(circuitbreaker.StateReporter)

	tr := &countingTransport{failN: 1 << 30, err: netError{msg: "temp", timeout: true}}
	cl := &http.Client{Transport: tr}
	circuitbreaker.SetBackoffForTest(cb, time.Millisecond)

	for i := range 3 { // três falhas reais (maxRetries=0 → 1 tentativa cada)
		if _, err := cb.Do(mustReq(t, "http://open.test/x"), cl); err == nil {
			t.Fatalf("chamada %d deveria falhar", i)
		}
	}
	if sr.State() != circuitbreaker.StateOpen {
		t.Fatalf("após 3 falhas consecutivas o circuito deve ABRIR: %v", sr.State())
	}
	callsBefore := tr.count()
	totalBefore := cb.Metrics()["open.test"]["/x"].TotalRequests

	start := time.Now()
	_, err := cb.Do(mustReq(t, "http://open.test/x"), cl)
	if !errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		t.Fatalf("circuito aberto deve retornar ErrCircuitOpen: %v", err)
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Fatalf("fast-fail deve ser imediato: %v", el)
	}
	if tr.count() != callsBefore {
		t.Fatal("fast-fail não pode tocar o transport")
	}
	if got := cb.Metrics()["open.test"]["/x"].TotalRequests; got != totalBefore {
		t.Fatalf("fast-fail não consome tentativa nas métricas: %d→%d", totalBefore, got)
	}
}

// WithBreaker: half-open após openFor; sonda com sucesso FECHA, com falha REABRE.
func TestFase4_HalfOpenRecoveryAndReopen(t *testing.T) {
	var mu sync.Mutex
	failing := true
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		f := failing
		mu.Unlock()
		if f {
			return nil, netError{msg: "temp", timeout: true}
		}
		return okResponse(r), nil
	})
	cl := &http.Client{Transport: tr}

	cb := circuitbreaker.NewCircuitBreakerWithOptions("half", 0, 0, 0, 0,
		circuitbreaker.WithBreaker(2, 60*time.Millisecond, 1))
	defer cb.Stop()
	sr := cb.(circuitbreaker.StateReporter)
	circuitbreaker.SetBackoffForTest(cb, time.Millisecond)

	// Abre com 2 falhas.
	for range 2 {
		_, _ = cb.Do(mustReq(t, "http://half.test/x"), cl)
	}
	if sr.State() != circuitbreaker.StateOpen {
		t.Fatalf("deveria estar aberto: %v", sr.State())
	}

	// Ainda dentro de openFor: fast-fail.
	if _, err := cb.Do(mustReq(t, "http://half.test/x"), cl); !errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		t.Fatalf("dentro de openFor: esperava ErrCircuitOpen, got %v", err)
	}

	// Passa openFor com downstream AINDA doente: sonda falha → reabre.
	time.Sleep(70 * time.Millisecond)
	if _, err := cb.Do(mustReq(t, "http://half.test/x"), cl); err == nil ||
		errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		t.Fatalf("a sonda deve atingir o transport e falhar de verdade: %v", err)
	}
	if sr.State() != circuitbreaker.StateOpen {
		t.Fatalf("sonda falha → reabre: %v", sr.State())
	}

	// Downstream se recupera: sonda sucede → fecha.
	mu.Lock()
	failing = false
	mu.Unlock()
	time.Sleep(70 * time.Millisecond)
	resp, err := cb.Do(mustReq(t, "http://half.test/x"), cl)
	if err != nil {
		t.Fatalf("sonda deveria suceder: %v", err)
	}
	_ = resp.Body.Close()
	if sr.State() != circuitbreaker.StateClosed {
		t.Fatalf("sonda com sucesso → fecha: %v", sr.State())
	}

	// Fechado: tráfego normal flui.
	resp, err = cb.Do(mustReq(t, "http://half.test/x"), cl)
	if err != nil {
		t.Fatalf("fechado deve operar normalmente: %v", err)
	}
	_ = resp.Body.Close()
}

// WithStatusCodeFailure(500): a resposta 500 é devolvida (err==nil), mas conta
// como FALHA nas métricas e alimenta a máquina de estados.
func TestFase4_StatusCodeFailure(t *testing.T) {
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := okResponse(r)
		resp.StatusCode = 500
		return resp, nil
	})
	cb := circuitbreaker.NewCircuitBreakerWithOptions("s5", 0, 0, 0, 0,
		circuitbreaker.WithStatusCodeFailure(500),
		circuitbreaker.WithBreaker(2, time.Hour, 1))
	defer cb.Stop()
	sr := cb.(circuitbreaker.StateReporter)

	for range 2 {
		resp, err := cb.Do(mustReq(t, "http://s5.test/x"), &http.Client{Transport: tr})
		if err != nil {
			t.Fatalf("a resposta deve ser devolvida com err==nil: %v", err)
		}
		if resp.StatusCode != 500 {
			t.Fatalf("status: %d", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	m := cb.Metrics()["s5.test"]["/x"]
	if m.FailedRequests != 2 || m.SuccessfulRequests != 0 {
		t.Fatalf("com a opção, 5xx conta como falha: %+v", m)
	}
	if sr.State() != circuitbreaker.StateOpen {
		t.Fatalf("2× 500 devem abrir o circuito: %v", sr.State())
	}
}

// WithRetryPolicy: política custom pode retentar ECONNREFUSED (o default não).
func TestFase4_RetryPolicyOverridesDefault(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	tr := &countingTransport{failN: 1 << 30, err: refused}
	cb := circuitbreaker.NewCircuitBreakerWithOptions("rp", 0, 0, 0, 1,
		circuitbreaker.WithRetryPolicy(func(err error) bool {
			return errors.Is(err, syscall.ECONNREFUSED)
		}))
	defer cb.Stop()
	circuitbreaker.SetBackoffForTest(cb, time.Millisecond)

	_, _ = cb.Do(mustReq(t, "http://rp.test/x"), &http.Client{Transport: tr})
	if tr.count() != 2 {
		t.Fatalf("política custom deveria retentar ECONNREFUSED (2 chamadas): %d", tr.count())
	}
}

// WithExponentialBackoff: esperas crescem geometricamente até o teto.
func TestFase4_ExponentialBackoff(t *testing.T) {
	tr := &countingTransport{failN: 1 << 30, err: netError{msg: "t", timeout: true}}
	cb := circuitbreaker.NewCircuitBreakerWithOptions("eb", 0, 0, 0, 3,
		circuitbreaker.WithExponentialBackoff(20*time.Millisecond, 200*time.Millisecond, false))
	defer cb.Stop()

	start := time.Now()
	_, err := cb.Do(mustReq(t, "http://eb.test/x"), &http.Client{Transport: tr})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("esperava exaustão")
	}
	// backoffs: 20 + 40 + 80 = 140ms (4 tentativas, 3 esperas)
	if elapsed < 120*time.Millisecond || elapsed > 400*time.Millisecond {
		t.Fatalf("esperas exponenciais 20+40+80ms esperadas, elapsed=%v", elapsed)
	}
	if tr.count() != 4 {
		t.Fatalf("tentativas: %d", tr.count())
	}
}

// WithDefaultTimeout: aplica-se só quando o chamador não definiu limite algum.
func TestFase4_DefaultTimeout(t *testing.T) {
	mute := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done() // servidor mudo: só retorna quando o ctx morre
		return nil, r.Context().Err()
	})
	cb := circuitbreaker.NewCircuitBreakerWithOptions("dt", 0, 0, 0, 0,
		circuitbreaker.WithDefaultTimeout(80*time.Millisecond))
	defer cb.Stop()

	// Caso 1: client sem Timeout e request sem deadline → teto aplicado.
	start := time.Now()
	_, err := cb.Do(mustReq(t, "http://dt.test/x"), &http.Client{Transport: mute})
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("esperava DeadlineExceeded do teto default: %v", err)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("teto de 80ms não aplicado: %v", elapsed)
	}

	// Caso 2: request COM deadline próprio (mais curto) → o do chamador vence.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err = cb.Do(mustReq(t, "http://dt.test/x").WithContext(ctx), &http.Client{Transport: mute})
	elapsed = time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || elapsed > 70*time.Millisecond {
		t.Fatalf("deadline do chamador deve prevalecer (~30ms): err=%v elapsed=%v", err, elapsed)
	}
}

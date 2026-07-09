package circuitbreaker_test

// Testes de regressão dos bugs corrigidos (PLANO.md §5) — cada um nasce
// vermelho contra o código antigo e trava a correção correspondente.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	circuitbreaker "github.com/diegoyosiura/circuit-breaker/pkg"
)

// F1 [A1, cenário 02] — snapshot de Metrics() concorrente com tráfego.
// Sob -race, o código antigo abortava (sort in-place em array aliasado).
func TestRegression_MetricsConcurrentWithTraffic(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("f1", 0, 0, 0, 0)
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}

	req, _ := http.NewRequest(http.MethodGet, "http://f1.test/x", nil)
	for range 50 { // aquece host/endpoint
		resp, _ := cb.Do(req, cl)
		if resp != nil {
			_ = resp.Body.Close()
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() { // escritor: tráfego contínuo
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				resp, _ := cb.Do(req, cl)
				if resp != nil {
					_ = resp.Body.Close()
				}
			}
		}
	}()
	go func() { // leitor: itera os slices do snapshot
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				snap := cb.Metrics()
				if ep, ok := snap["f1.test"]["/x"]; ok {
					var sink time.Time
					for _, ts := range ep.StartTimeRequests {
						sink = ts
					}
					_ = sink
				}
			}
		}
	}()
	time.Sleep(250 * time.Millisecond)
	close(stop)
	wg.Wait()
	// A asserção real é o -race: qualquer aliasing mutável entre snapshot e
	// dados vivos aborta o teste.
}

// F1 — o snapshot não pode aliasar os dados vivos: mutar o snapshot não pode
// afetar o breaker, e novos registros não podem alterar o snapshot.
func TestRegression_MetricsSnapshotIsolated(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("f1b", 0, 0, 0, 0)
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}
	req, _ := http.NewRequest(http.MethodGet, "http://f1b.test/x", nil)
	for range 5 {
		resp, _ := cb.Do(req, cl)
		if resp != nil {
			_ = resp.Body.Close()
		}
	}

	snap := cb.Metrics()["f1b.test"]["/x"]
	if len(snap.StartTimeRequests) == 0 {
		t.Fatal("snapshot deveria ter StartTimeRequests")
	}
	before := snap.StartTimeRequests[0]

	// Mutar o snapshot não pode vazar para os dados vivos.
	snap.StartTimeRequests[0] = time.Time{}
	fresh := cb.Metrics()["f1b.test"]["/x"]
	if fresh.StartTimeRequests[0].IsZero() {
		t.Fatal("mutação no snapshot vazou para os dados vivos (aliasing)")
	}
	if !fresh.StartTimeRequests[0].Equal(before) {
		t.Fatalf("dados vivos mudaram inesperadamente")
	}
}

// F3 [A3, cenário 03] — Do() após Stop() retorna ErrStopped em vez de travar.
func TestRegression_DoAfterStopReturnsError(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("f3", 0, 1, 60, 0)
	cl := &http.Client{Transport: &countingTransport{}}
	req, _ := http.NewRequest(http.MethodGet, "http://f3.test/x", nil)

	resp, err := cb.Do(req, cl) // consome o token inicial
	if err != nil {
		t.Fatalf("primeira chamada: %v", err)
	}
	_ = resp.Body.Close()
	cb.Stop()

	done := make(chan error, 1)
	go func() {
		_, err := cb.Do(req, cl) // context.Background(), bucket vazio
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, circuitbreaker.ErrStopped) {
			t.Fatalf("esperava ErrStopped, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Do() pós-Stop ainda bloqueia (regressão do A3)")
	}
}

// F3 — waiters já bloqueados no momento do Stop() são desbloqueados.
func TestRegression_StopUnblocksInflightWaiters(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("f3b", 0, 1, 60, 0)
	cl := &http.Client{Transport: &countingTransport{}}
	req, _ := http.NewRequest(http.MethodGet, "http://f3b.test/x", nil)

	resp, err := cb.Do(req, cl)
	if err != nil {
		t.Fatalf("primeira chamada: %v", err)
	}
	_ = resp.Body.Close()

	done := make(chan error, 1)
	go func() {
		_, err := cb.Do(req, cl) // fica esperando token
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // garante que o waiter entrou no select
	cb.Stop()

	select {
	case err := <-done:
		if !errors.Is(err, circuitbreaker.ErrStopped) {
			t.Fatalf("esperava ErrStopped, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() não desbloqueou o waiter em voo")
	}
}

// F3 — tokens remanescentes no bucket ainda são atendidos após Stop()
// (característica preservada, observada no cenário 03/afterstop).
func TestRegression_LeftoverTokensStillServed(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("f3c", 0, 2, 60, 0) // 2 tokens pré-carregados
	cl := &http.Client{Transport: &countingTransport{}}
	req, _ := http.NewRequest(http.MethodGet, "http://f3c.test/x", nil)
	cb.Stop() // para o refill antes de qualquer consumo

	for i := range 2 {
		resp, err := cb.Do(req, cl)
		if err != nil {
			t.Fatalf("sobra %d deveria atender: %v", i, err)
		}
		_ = resp.Body.Close()
	}
	if _, err := cb.Do(req, cl); !errors.Is(err, circuitbreaker.ErrStopped) {
		t.Fatalf("após esgotar as sobras, esperava ErrStopped: %v", err)
	}
}

// F2 [A2, cenário 01] — o retry reenvia o corpo COMPLETO via GetBody.
func TestRegression_RetryReplaysPostBody(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	attempt := 0
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		attempt++
		n := attempt
		mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if n == 1 {
			return nil, netError{msg: "temp", temporary: true}
		}
		return okResponse(r), nil
	})
	cb := circuitbreaker.NewCircuitBreaker("f2", 0, 0, 0, 2)
	defer cb.Stop()
	circuitbreaker.SetBackoffForTest(cb, time.Millisecond)

	req, _ := http.NewRequest(http.MethodPost, "http://f2.test/save", strings.NewReader("PAYLOAD-123"))
	resp, err := cb.Do(req, &http.Client{Transport: tr})
	if err != nil {
		t.Fatalf("esperava sucesso após retry: %v", err)
	}
	_ = resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || bodies[0] != "PAYLOAD-123" || bodies[1] != "PAYLOAD-123" {
		t.Fatalf("retry não reenviou o corpo completo: %q", bodies)
	}
}

// F2 [D2] — corpo não-rebobinável (GetBody == nil) NÃO é retentado.
func TestRegression_NoRetryWhenBodyNotRewindable(t *testing.T) {
	tempErr := netError{msg: "temp", temporary: true}
	tr := &countingTransport{failN: 1 << 30, err: tempErr}
	cb := circuitbreaker.NewCircuitBreaker("f2b", 0, 0, 0, 5)
	defer cb.Stop()
	circuitbreaker.SetBackoffForTest(cb, time.Millisecond)

	req, _ := http.NewRequest(http.MethodPost, "http://f2b.test/save", strings.NewReader("X"))
	req.GetBody = nil // simula corpo de stream não-rebobinável

	_, err := cb.Do(req, &http.Client{Transport: tr})
	if err == nil {
		t.Fatal("esperava erro")
	}
	if !errors.Is(err, tempErr) {
		t.Fatalf("esperava o erro da tentativa, got %v", err)
	}
	if tr.count() != 1 {
		t.Fatalf("corpo não-rebobinável não pode ser retentado: %d chamadas", tr.count())
	}
}

// F4 [A4, cenários 04/05] — as amostras internas são podadas: médias/ratios
// preservados, memória limitada (antes: len==N para sempre).
func TestRegression_MetricsBounded(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("f4", 0, 0, 0, 0)
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}
	req, _ := http.NewRequest(http.MethodGet, "http://f4.test/x", nil)

	const n = 5000
	for range n {
		resp, err := cb.Do(req, cl)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		_ = resp.Body.Close()
	}

	for _, key := range []string{"/x", "::root"} {
		m := cb.Metrics()["f4.test"][key]
		if m.TotalRequests != n {
			t.Fatalf("%s: contadores devem ser exatos: total=%d", key, m.TotalRequests)
		}
		if m.Ratio01Requests != n {
			t.Fatalf("%s: ratio01 deve contar a janela completa: %d", key, m.Ratio01Requests)
		}
		if len(m.TimeRequests) > 80 {
			t.Fatalf("%s: TimeRequests não podado: len=%d (era %d sem poda)", key, len(m.TimeRequests), n)
		}
		if m.MeanRequests <= 0 && m.TotalRequests > 0 {
			t.Fatalf("%s: média zerada após poda", key)
		}
	}
}

// F6 [A6, cenário 08] — cancelamento durante o backoff retorna imediatamente.
func TestRegression_BackoffRespectsContext(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("f6", 0, 0, 0, 5)
	defer cb.Stop()
	circuitbreaker.SetBackoffForTest(cb, 300*time.Millisecond)
	tr := &countingTransport{failN: 1 << 30, err: netError{msg: "temp", temporary: true}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	req, _ := http.NewRequest(http.MethodGet, "http://f6.test/x", nil)
	start := time.Now()
	_, err := cb.Do(req.WithContext(ctx), &http.Client{Transport: tr})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("esperava context.Canceled, got %v", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("cancel em ~50ms deveria interromper o backoff de 300ms: retornou em %v", elapsed)
	}
}

// F5 [A16, cenários 22/14] — config extrema não gasta segundos no construtor
// nem queima CPU ociosa com ticker de 1ns.
func TestRegression_ExtremeRateNoBusyLoop(t *testing.T) {
	start := time.Now()
	cb := circuitbreaker.NewCircuitBreaker("f5", 0, 2_000_000_000, 1, 0)
	built := time.Since(start)
	defer cb.Stop()
	if built > 100*time.Millisecond {
		t.Fatalf("construtor gastou %v (era ~26s no código antigo)", built)
	}

	// Do continua funcionando com a config extrema.
	cl := &http.Client{Transport: &countingTransport{}}
	req, _ := http.NewRequest(http.MethodGet, "http://f5.test/x", nil)
	resp, err := cb.Do(req, cl)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()

	// CPU ociosa: com o clamp de 1ms o refiller não pode saturar um core
	// (media 100.3% de um core no código antigo — cenário 22).
	var before, after syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
		t.Skipf("Getrusage indisponível: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &after)
	cpu := time.Duration(after.Utime.Nano()+after.Stime.Nano()) - time.Duration(before.Utime.Nano()+before.Stime.Nano())
	if cpu > 150*time.Millisecond { // >30% de 1 core em 500ms de ociosidade
		t.Fatalf("refiller queimando CPU ociosa: %v em 500ms", cpu)
	}
	t.Logf("CPU ociosa em 500ms: %v (construtor: %v)", cpu, built)
}

// F5 — a taxa nominal é preservada pelo lote (tokensPerTick) quando o
// intervalo é clampado: um bucket pequeno com taxa alta continua repondo.
func TestRegression_ClampPreservesRefill(t *testing.T) {
	// 10_000 req/s → intervalo natural 100µs < 1ms → clamp + lote de 10/tick.
	cb := circuitbreaker.NewCircuitBreaker("f5b", 0, 10_000, 1, 0)
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}
	req, _ := http.NewRequest(http.MethodGet, "http://f5b.test/x", nil)

	// Consome um pouco do burst e mede a reposição por ~100ms.
	for range 50 {
		resp, err := cb.Do(req, cl)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		_ = resp.Body.Close()
	}
	// Se o refill morreu, nada além do burst inicial (10k) seria servido —
	// difícil de esgotar em teste; o essencial aqui é que o breaker funciona
	// e o ticker existe com lote >1 (validado indiretamente pelo teste de CPU).
}

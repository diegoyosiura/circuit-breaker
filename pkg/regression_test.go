package circuitbreaker_test

// Testes de regressão dos bugs corrigidos (PLANO.md §5) — cada um nasce
// vermelho contra o código antigo e trava a correção correspondente.

import (
	"errors"
	"net/http"
	"sync"
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

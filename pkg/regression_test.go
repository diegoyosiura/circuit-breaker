package circuitbreaker_test

// Testes de regressão dos bugs corrigidos (PLANO.md §5) — cada um nasce
// vermelho contra o código antigo e trava a correção correspondente.

import (
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

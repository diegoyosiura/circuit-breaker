package circuitbreaker_test

// Tier F do PLANO.md §8.1 (T1–T4, T6): testes funcionais de garantias
// centrais que nenhum bug específico cobre — bulkhead, concorrência do
// manager e do Stop, taxa sustentada e ciclo de vida de goroutines.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circuitbreaker "github.com/diegoyosiura/circuit-breaker/pkg"
)

// T1 — o pico de requisições em voo nunca excede maxConcurrent, e um
// contexto cancelado esperando SLOT retorna ctx.Err() sem tocar o transport.
func TestFunctional_SemaphorePeakNeverExceedsMax(t *testing.T) {
	const maxConcurrent, total = 5, 50

	var inFlight, peak, entered atomic.Int64
	release := make(chan struct{})
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cur := inFlight.Add(1)
		entered.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		<-release // segura todos em voo até o teste liberar
		inFlight.Add(-1)
		return okResponse(r), nil
	})

	cb := circuitbreaker.NewCircuitBreaker("t1", maxConcurrent, 0, 0, 0)
	defer cb.Stop()
	cl := &http.Client{Transport: tr}

	var wg sync.WaitGroup
	for i := range total {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, "http://t1.test/x", nil)
			resp, err := cb.Do(req, cl)
			if err != nil {
				t.Errorf("req %d: %v", i, err)
				return
			}
			_ = resp.Body.Close()
		}(i)
	}

	// Regime: espera os primeiros maxConcurrent entrarem e verifica o teto.
	deadline := time.Now().Add(2 * time.Second)
	for entered.Load() < maxConcurrent && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// Subteste: com o semáforo CHEIO, um ctx expirado esperando slot retorna
	// ctx.Err() sem nunca tocar o transport.
	before := entered.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	req, _ := http.NewRequest(http.MethodGet, "http://t1.test/x", nil)
	_, err := cb.Do(req.WithContext(ctx), cl)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("esperando slot com ctx expirado: esperava DeadlineExceeded, got %v", err)
	}
	if entered.Load() != before {
		t.Fatal("requisição cancelada no semáforo não pode tocar o transport")
	}

	close(release)
	wg.Wait()
	if p := peak.Load(); p != maxConcurrent {
		t.Fatalf("pico em voo = %d (esperado exatamente %d)", p, maxConcurrent)
	}
	if entered.Load() != total {
		t.Fatalf("todas as %d devem concluir: %d", total, entered.Load())
	}
}

// T2 — acesso concorrente ao manager devolve UMA instância por nome (-race).
func TestFunctional_ManagerConcurrentNewAndGet(t *testing.T) {
	m := circuitbreaker.NewManager()
	const goroutines, names = 100, 4

	instances := make([]circuitbreaker.ICircuitBreaker, goroutines)
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("svc-%d", i%names)
			instances[i] = m.NewCircuitBreaker(name, 1, 10, 1, 0)
			_ = m.GetCircuitBreaker(name)
		}(i)
	}
	wg.Wait()

	for i := range goroutines {
		want := m.GetCircuitBreaker(fmt.Sprintf("svc-%d", i%names))
		if instances[i] != want {
			t.Fatalf("goroutine %d recebeu instância diferente da registrada", i)
		}
	}
	if lc, ok := m.(circuitbreaker.IManagerLifecycle); ok {
		lc.StopAll()
	}
}

// T3 — Stop() concorrente é idempotente: sem panic, sem deadlock, sem race.
func TestFunctional_StopConcurrentIdempotent(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("t3", 0, 10, 1, 0)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cb.Stop()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop concorrente travou")
	}
}

// T4 — passada a primeira janela (burst), a taxa converge para
// maxRequests/windowSeconds (tolerância ±50% contra flake de scheduler).
func TestFunctional_SustainedRate(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("medição de taxa (distorcida sob -race)")
	}
	const perSecond = 20
	cb := circuitbreaker.NewCircuitBreaker("t4", 0, perSecond, 1, 0)
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}

	drain := func(window time.Duration) int {
		successes := 0
		deadline := time.Now().Add(window)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
			req, _ := http.NewRequest(http.MethodGet, "http://t4.test/x", nil)
			resp, err := cb.Do(req.WithContext(ctx), cl)
			cancel()
			if err == nil {
				successes++
				_ = resp.Body.Close()
			}
		}
		return successes
	}

	_ = drain(1 * time.Second) // 1ª janela: burst inicial + refill (~2x)
	sustained := drain(1 * time.Second)
	lo, hi := perSecond/2, perSecond*3/2
	if sustained < lo || sustained > hi {
		t.Fatalf("taxa sustentada fora de [%d,%d]: %d req/s", lo, hi, sustained)
	}
	t.Logf("taxa sustentada: %d req/s (nominal %d)", sustained, perSecond)
}

// T6 — goroutines do ticker não vazam quando o ciclo de vida é respeitado:
// N breakers criados via manager, StopAll → contagem volta ao patamar.
func TestFunctional_NoGoroutineLeakWithStopAll(t *testing.T) {
	before := runtime.NumGoroutine()

	m := circuitbreaker.NewManager()
	const n = 100
	for i := range n {
		m.NewCircuitBreaker(fmt.Sprintf("leak-%d", i), 0, 10, 1, 0)
	}
	time.Sleep(20 * time.Millisecond)
	during := runtime.NumGoroutine()
	if during < before+n {
		t.Fatalf("esperava ~%d goroutines de ticker ativas: antes=%d durante=%d", n, before, during)
	}

	lc, ok := m.(circuitbreaker.IManagerLifecycle)
	if !ok {
		t.Fatal("manager deveria implementar IManagerLifecycle")
	}
	lc.StopAll()
	time.Sleep(50 * time.Millisecond)

	after := runtime.NumGoroutine()
	if after > before+10 {
		t.Fatalf("goroutines vazaram após StopAll: antes=%d depois=%d", before, after)
	}
	t.Logf("goroutines: antes=%d durante=%d depois=%d", before, during, after)
}

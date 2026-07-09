package circuitbreaker_test

// Testes de característica (Fase 0 do PLANO.md): congelam a semântica
// observável ANTES de qualquer correção. Se um refactor quebrar um destes
// testes, mudou comportamento sem querer. Alterações deliberadas de
// característica exigem PR dedicado (ex.: F7 atualiza T0.7).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	circuitbreaker "github.com/diegoyosiura/circuit-breaker/pkg"
)

// T0.1 — matriz de contadores por cenário (CB.md §6.6).
func TestCharacterization_CounterMatrix(t *testing.T) {
	newReq := func(path string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, "http://matrix.test"+path, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		return req
	}
	get := func(cb circuitbreaker.ICircuitBreaker, path string) circuitbreaker.EndpointMetrics {
		m, ok := cb.Metrics()["matrix.test"][path]
		if !ok {
			t.Fatalf("sem métricas para %s", path)
		}
		return m
	}

	t.Run("sucesso direto", func(t *testing.T) {
		cb := circuitbreaker.NewCircuitBreaker("t01a", 0, 0, 0, 3)
		defer cb.Stop()
		cl := &http.Client{Transport: &countingTransport{}}
		resp, err := cb.Do(newReq("/a"), cl)
		if err != nil {
			t.Fatalf("esperava sucesso: %v", err)
		}
		_ = resp.Body.Close()
		m := get(cb, "/a")
		if m.TotalRequests != 1 || m.SuccessfulRequests != 1 || m.FailedRequests != 0 || m.RetryCount != 0 {
			t.Fatalf("contadores divergem: %+v", m)
		}
	})

	t.Run("uma falha retryable e sucesso", func(t *testing.T) {
		cb := circuitbreaker.NewCircuitBreaker("t01b", 0, 0, 0, 3)
		defer cb.Stop()
		circuitbreaker.SetBackoffForTest(cb, time.Millisecond)
		cl := &http.Client{Transport: &countingTransport{failN: 1, err: netError{msg: "temp", temporary: true}}}
		resp, err := cb.Do(newReq("/b"), cl)
		if err != nil {
			t.Fatalf("esperava sucesso após retry: %v", err)
		}
		_ = resp.Body.Close()
		m := get(cb, "/b")
		if m.TotalRequests != 2 || m.SuccessfulRequests != 1 || m.FailedRequests != 1 || m.RetryCount != 1 {
			t.Fatalf("contadores divergem: %+v", m)
		}
	})

	t.Run("erro nao-retryable retorna imediato preservando a causa", func(t *testing.T) {
		cb := circuitbreaker.NewCircuitBreaker("t01c", 0, 0, 0, 5)
		defer cb.Stop()
		boom := errors.New("boom")
		tr := &countingTransport{failN: 1 << 30, err: boom}
		cl := &http.Client{Transport: tr}
		start := time.Now()
		_, err := cb.Do(newReq("/c"), cl)
		elapsed := time.Since(start)
		if err == nil || !errors.Is(err, boom) {
			t.Fatalf("esperava a causa original, got %v", err)
		}
		if tr.count() != 1 {
			t.Fatalf("esperava 1 chamada, got %d", tr.count())
		}
		if elapsed > 400*time.Millisecond {
			t.Fatalf("não deveria dormir backoff: %v", elapsed)
		}
		m := get(cb, "/c")
		if m.TotalRequests != 1 || m.SuccessfulRequests != 0 || m.FailedRequests != 1 || m.RetryCount != 0 {
			t.Fatalf("contadores divergem: %+v", m)
		}
	})

	t.Run("exaustao de retries", func(t *testing.T) {
		cb := circuitbreaker.NewCircuitBreaker("t01d", 0, 0, 0, 2)
		defer cb.Stop()
		circuitbreaker.SetBackoffForTest(cb, time.Millisecond)
		tr := &countingTransport{failN: 1 << 30, err: netError{msg: "temp", temporary: true}}
		cl := &http.Client{Transport: tr}
		_, err := cb.Do(newReq("/d"), cl)
		if err == nil {
			t.Fatal("esperava erro de exaustão")
		}
		if tr.count() != 3 {
			t.Fatalf("esperava 3 tentativas, got %d", tr.count())
		}
		m := get(cb, "/d")
		if m.TotalRequests != 3 || m.FailedRequests != 3 || m.RetryCount != 2 || m.SuccessfulRequests != 0 {
			t.Fatalf("contadores divergem: %+v", m)
		}
	})

	t.Run("cancelamento esperando token: failed sem attempt", func(t *testing.T) {
		cb := circuitbreaker.NewCircuitBreaker("t01e", 0, 1, 3600, 0)
		defer cb.Stop()
		cl := &http.Client{Transport: &countingTransport{}}
		resp, err := cb.Do(newReq("/e"), cl) // consome o único token
		if err != nil {
			t.Fatalf("primeira chamada deveria suceder: %v", err)
		}
		_ = resp.Body.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err = cb.Do(newReq("/e").WithContext(ctx), cl)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("esperava DeadlineExceeded, got %v", err)
		}
		m := get(cb, "/e")
		// Característica vigente (documentada em CB.md A15): o cancelamento na
		// espera de token conta como falha SEM tentativa — failed pode exceder
		// o esperado por chamadas; total permanece 1.
		if m.TotalRequests != 1 || m.FailedRequests != 1 || m.SuccessfulRequests != 1 {
			t.Fatalf("contadores divergem: %+v", m)
		}
	})
}

// T0.2 — tabela EFETIVA de retryabilidade (CB-TESTES.md cenário 24).
// A classificação é inferida caixa-preta: 1 chamada = não-retryable,
// 2 chamadas (maxRetries=1) = retryable.
func TestCharacterization_EffectiveRetryTable(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		retryable bool
	}{
		{"OpError dial ECONNREFUSED", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, false},
		{"OpError read ECONNRESET", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, false},
		{"Errno ECONNREFUSED puro", syscall.ECONNREFUSED, false},
		{"os.ErrDeadlineExceeded", os.ErrDeadlineExceeded, true},
		{"net.Error Timeout", netError{msg: "t", timeout: true}, true},
		{"net.Error Temporary", netError{msg: "tmp", temporary: true}, true},
		{"retryable embrulhado com %w NAO é retentado", fmt.Errorf("wrap: %w", netError{msg: "t", timeout: true}), false},
		{"erro generico", errors.New("generic"), false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cb := circuitbreaker.NewCircuitBreaker(fmt.Sprintf("t02-%d", i), 0, 0, 0, 1)
			defer cb.Stop()
			circuitbreaker.SetBackoffForTest(cb, time.Millisecond)
			tr := &countingTransport{failN: 1 << 30, err: tc.err}
			cl := &http.Client{Transport: tr}
			req, _ := http.NewRequest(http.MethodGet, "http://retry.test/x", nil)
			_, _ = cb.Do(req, cl)
			want := 1
			if tc.retryable {
				want = 2
			}
			if got := tr.count(); got != want {
				t.Fatalf("classificação efetiva divergiu: %d chamadas (esperado %d)", got, want)
			}
		})
	}
}

// T0.3 — burst da primeira janela ~2× maxRequests (CB.md §6.4, cenário 26).
func TestCharacterization_BurstFirstWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("janela de 2s")
	}
	const maxRequests = 20
	cb := circuitbreaker.NewCircuitBreaker("t03", 0, maxRequests, 2, 0)
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}

	successes := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		req, _ := http.NewRequest(http.MethodGet, "http://burst.test/x", nil)
		resp, err := cb.Do(req.WithContext(ctx), cl)
		cancel()
		if err == nil {
			successes++
			_ = resp.Body.Close()
		}
	}
	lo, hi := int(1.7*maxRequests), int(2.2*maxRequests)
	if successes < lo || successes > hi {
		t.Fatalf("burst da 1ª janela fora de [%d,%d]: %d", lo, hi, successes)
	}
	t.Logf("burst medido: %d/%d = %.2fx", successes, maxRequests, float64(successes)/maxRequests)
}

// T0.4 — semântica de médias (últimas 20), ratios (contagem em janela) e ::root (soma).
func TestCharacterization_MeanAndRatioSemantics(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("t04", 0, 0, 0, 0)
	defer cb.Stop()

	do := func(cl *http.Client, path string, n int) {
		for range n {
			req, _ := http.NewRequest(http.MethodGet, "http://sem.test"+path, nil)
			resp, err := cb.Do(req, cl)
			if err != nil {
				t.Fatalf("Do %s: %v", path, err)
			}
			_ = resp.Body.Close()
		}
	}

	fast := &http.Client{Transport: &countingTransport{}}
	do(fast, "/a", 10)
	do(fast, "/b", 20)

	metrics := cb.Metrics()["sem.test"]
	a, b, root := metrics["/a"], metrics["/b"], metrics["::root"]
	if a.TotalRequests != 10 || b.TotalRequests != 20 {
		t.Fatalf("totais por endpoint divergem: a=%d b=%d", a.TotalRequests, b.TotalRequests)
	}
	if root.TotalRequests != a.TotalRequests+b.TotalRequests {
		t.Fatalf("::root deve somar endpoints: root=%d", root.TotalRequests)
	}
	if a.Ratio01Requests != 10 || b.Ratio01Requests != 20 || root.Ratio01Requests != 30 {
		t.Fatalf("ratio01 (contagem na janela de 1min) diverge: a=%d b=%d root=%d",
			a.Ratio01Requests, b.Ratio01Requests, root.Ratio01Requests)
	}

	// Média = últimas 20 amostras: 10 lentas (120ms) seguidas de 20 rápidas →
	// a média de sucesso reflete só as rápidas.
	slow := &http.Client{Transport: &countingTransport{delay: 120 * time.Millisecond}}
	do(slow, "/c", 10)
	do(fast, "/c", 20)
	c := cb.Metrics()["sem.test"]["/c"]
	if c.MeanSuccessfulRequests >= 0.06 {
		t.Fatalf("média deveria cobrir só as últimas 20 (rápidas): %.4fs", c.MeanSuccessfulRequests)
	}
	if c.SuccessfulRequests != 30 {
		t.Fatalf("sucessos: %d", c.SuccessfulRequests)
	}
}

// T0.5 — manager: get-or-create com parâmetros ignorados; Get inexistente → nil.
func TestCharacterization_ManagerGetOrCreate(t *testing.T) {
	m := circuitbreaker.NewManager()
	cb1 := m.NewCircuitBreaker("svc", 1, 1, 60, 0)
	cb2 := m.NewCircuitBreaker("svc", 100, 1000, 1, 5) // config divergente é ignorada
	if cb1 != cb2 {
		t.Fatal("esperava a MESMA instância para o mesmo nome")
	}
	if m.GetCircuitBreaker("nope") != nil {
		t.Fatal("Get de nome inexistente deve retornar nil")
	}
	cb1.Stop()
}

// T0.6 — modo ilimitado: sem goroutine de ticker, Do funciona, Stop no-op idempotente.
func TestCharacterization_UnlimitedMode(t *testing.T) {
	before := runtime.NumGoroutine()
	cbs := make([]circuitbreaker.ICircuitBreaker, 0, 50)
	for i := range 50 {
		cbs = append(cbs, circuitbreaker.NewCircuitBreaker(fmt.Sprintf("t06-%d", i), 0, 0, 0, 0))
	}
	time.Sleep(20 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+10 {
		t.Fatalf("modo ilimitado não deveria criar goroutines: antes=%d depois=%d", before, after)
	}

	cl := &http.Client{Transport: &countingTransport{}}
	req, _ := http.NewRequest(http.MethodGet, "http://unl.test/x", nil)
	resp, err := cbs[0].Do(req, cl)
	if err != nil {
		t.Fatalf("Do ilimitado: %v", err)
	}
	_ = resp.Body.Close()

	for _, cb := range cbs {
		cb.Stop()
		cb.Stop() // idempotente
	}
}

// T0.7 — texto do erro de exaustão e comportamento de errors.Is/As.
// Comportamento ORIGINAL congelado: texto exato e causa NÃO recuperável.
// (O F7 do PLANO.md atualiza este teste deliberadamente: o texto permanece
// idêntico e a causa passa a ser recuperável — único ajuste de característica
// previsto, feito em PR dedicado.)
func TestCharacterization_ExhaustionErrorText(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("t07", 0, 0, 0, 1)
	defer cb.Stop()
	circuitbreaker.SetBackoffForTest(cb, time.Millisecond)
	cause := netError{msg: "sentinela", temporary: true}
	cl := &http.Client{Transport: &countingTransport{failN: 1 << 30, err: cause}}
	req, _ := http.NewRequest(http.MethodGet, "http://exh.test/x", nil)
	_, err := cb.Do(req, cl)
	if err == nil {
		t.Fatal("esperava erro")
	}
	if err.Error() != "request failed after retries" {
		t.Fatalf("texto do erro é contrato informal e não pode mudar: %q", err.Error())
	}
	var ne net.Error
	if errors.As(err, &ne) {
		t.Fatalf("característica original: a causa NÃO é recuperável via errors.As (será mudado pelo F7)")
	}
}

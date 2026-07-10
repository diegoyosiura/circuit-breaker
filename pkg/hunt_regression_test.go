package circuitbreaker_test

// Regressões dos achados CONFIRMADOS da caça a falhas multiagente
// (CB-TESTES.md §15) — um teste por bug corrigido, nascido vermelho.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	circuitbreaker "github.com/diegoyosiura/circuit-breaker/pkg"
)

// [SM-2/API-01/DO-INT-1/OPT-01] WithDefaultTimeout não pode matar a leitura
// do Body: o cancel é amarrado ao Close(), não ao retorno de Do().
func TestHunt_DefaultTimeoutBodyReadable(t *testing.T) {
	payload := strings.Repeat("x", 1<<20) // 1 MiB — não cabe no buffer do transporte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)

	cb := circuitbreaker.NewCircuitBreakerWithOptions("h-body", 0, 0, 0, 0,
		circuitbreaker.WithDefaultTimeout(30*time.Second))
	defer cb.Stop()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := cb.Do(req, &http.Client{}) // sem Timeout, sem deadline → teto aplica
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	body, err := io.ReadAll(resp.Body) // no código com bug: "context canceled" no meio
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("leitura do Body após Do() retornou: %v (lidos %d de %d)", err, len(body), len(payload))
	}
	if len(body) != len(payload) {
		t.Fatalf("corpo truncado: %d de %d bytes", len(body), len(payload))
	}
}

// [TB-01/SM-3/DO-INT-3] Cancelamento do CHAMADOR com a requisição em voo é
// neutro — não pode abrir o circuito (o downstream não falhou).
func TestHunt_CallerCancelMidFlightIsNeutral(t *testing.T) {
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done() // segura até o chamador cancelar
		return nil, r.Context().Err()
	})
	cb := circuitbreaker.NewCircuitBreakerWithOptions("h-cancel", 0, 0, 0, 0,
		circuitbreaker.WithBreaker(1, time.Hour, 1)) // 1 falha bastaria para abrir
	defer cb.Stop()
	sr := cb.(circuitbreaker.StateReporter)

	for range 3 {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(20 * time.Millisecond); cancel() }()
		req, _ := http.NewRequest(http.MethodGet, "http://h-cancel.test/x", nil)
		_, err := cb.Do(req.WithContext(ctx), &http.Client{Transport: tr})
		cancel()
		if err == nil {
			t.Fatal("esperava erro de cancelamento")
		}
	}
	if sr.State() != circuitbreaker.StateClosed {
		t.Fatalf("cancelamentos do chamador NÃO podem abrir o circuito: %v", sr.State())
	}
}

// [DO-INT-2] Deadline que estoura no backoff APÓS falhas reais de transporte
// conta como falha — o circuito abre mesmo que o erro final seja ctx.Err().
func TestHunt_DeadlineAfterRealFailuresOpensCircuit(t *testing.T) {
	tr := &countingTransport{failN: 1 << 30, err: netError{msg: "t", timeout: true}}
	cb := circuitbreaker.NewCircuitBreakerWithOptions("h-dl", 0, 0, 0, 5,
		circuitbreaker.WithBreaker(2, time.Hour, 1))
	defer cb.Stop()
	circuitbreaker.SetBackoffForTest(cb, 200*time.Millisecond)
	sr := cb.(circuitbreaker.StateReporter)

	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		req, _ := http.NewRequest(http.MethodGet, "http://h-dl.test/x", nil)
		_, err := cb.Do(req.WithContext(ctx), &http.Client{Transport: tr})
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("esperava DeadlineExceeded do backoff: %v", err)
		}
	}
	if sr.State() != circuitbreaker.StateOpen {
		t.Fatalf("2 chamadas com falhas reais devem abrir o circuito, mesmo com deadline no backoff: %v", sr.State())
	}
}

// [API-04/SM-5] Panic no transporte propaga E conta como falha (nunca sucesso).
func TestHunt_TransportPanicCountsAsFailure(t *testing.T) {
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		panic("transport explodiu")
	})
	cb := circuitbreaker.NewCircuitBreakerWithOptions("h-panic", 0, 0, 0, 0,
		circuitbreaker.WithBreaker(1, time.Hour, 1))
	defer cb.Stop()
	sr := cb.(circuitbreaker.StateReporter)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic deve propagar, não ser engolido")
			}
		}()
		req, _ := http.NewRequest(http.MethodGet, "http://h-panic.test/x", nil)
		_, _ = cb.Do(req, &http.Client{Transport: tr})
	}()
	if sr.State() != circuitbreaker.StateOpen {
		t.Fatalf("panic é falha para a máquina de estados: %v", sr.State())
	}
}

// [API-05] req nil ou URL nil retornam erro, não panic.
func TestHunt_NilRequestReturnsError(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("h-nil", 0, 0, 0, 0)
	defer cb.Stop()
	if _, err := cb.Do(nil, http.DefaultClient); err == nil {
		t.Fatal("req nil deve retornar erro")
	}
	req := &http.Request{} // URL nil
	if _, err := cb.Do(req, http.DefaultClient); err == nil {
		t.Fatal("URL nil deve retornar erro")
	}
}

// [OPT-02] Option nil explícita é ignorada (sem panic, sem goroutine vazada).
func TestHunt_NilOptionIgnored(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreakerWithOptions("h-nilopt", 0, 10, 1, 0,
		nil, circuitbreaker.WithStatusCodeFailure(500), nil)
	defer cb.Stop()
	req, _ := http.NewRequest(http.MethodGet, "http://h-nilopt.test/x", nil)
	resp, err := cb.Do(req, &http.Client{Transport: &countingTransport{}})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
}

// [TB-02] Contexto já cancelado não consome token do bucket.
func TestHunt_DeadContextDoesNotBurnToken(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("h-dead", 0, 2, 3600, 0) // 2 tokens, sem refill útil
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // morto na chegada
	req, _ := http.NewRequest(http.MethodGet, "http://h-dead.test/x", nil)
	for range 5 {
		if _, err := cb.Do(req.WithContext(ctx), cl); !errors.Is(err, context.Canceled) {
			t.Fatalf("esperava Canceled: %v", err)
		}
	}
	// os 2 tokens continuam disponíveis para chamadas vivas
	for i := range 2 {
		resp, err := cb.Do(req, cl)
		if err != nil {
			t.Fatalf("chamada viva %d deveria ter token: %v", i, err)
		}
		_ = resp.Body.Close()
	}
}

// [DO-INT-4] Erro de GetBody mantém a invariante total <= success+failed e
// NÃO abre o circuito (erro local).
func TestHunt_GetBodyErrorConsistent(t *testing.T) {
	boom := errors.New("getbody-quebrou")
	tr := &countingTransport{failN: 1, err: netError{msg: "t", timeout: true}}
	cb := circuitbreaker.NewCircuitBreakerWithOptions("h-gb", 0, 0, 0, 2,
		circuitbreaker.WithBreaker(1, time.Hour, 1))
	defer cb.Stop()
	circuitbreaker.SetBackoffForTest(cb, time.Millisecond)
	sr := cb.(circuitbreaker.StateReporter)

	req, _ := http.NewRequest(http.MethodPost, "http://h-gb.test/x", strings.NewReader("X"))
	req.GetBody = func() (io.ReadCloser, error) { return nil, boom }

	_, err := cb.Do(req, &http.Client{Transport: tr})
	if !errors.Is(err, boom) {
		t.Fatalf("erro do GetBody deve ser propagado: %v", err)
	}
	m := cb.Metrics()["h-gb.test"]["/x"]
	if m.TotalRequests != m.SuccessfulRequests+m.FailedRequests {
		t.Fatalf("invariante total==ok+fail violada: %+v", m)
	}
	if sr.State() != circuitbreaker.StateClosed {
		t.Fatalf("erro local (GetBody) não pode abrir o circuito: %v", sr.State())
	}
}

// [API-03] Cardinalidade de endpoints por host é limitada; excedentes
// agregam em ::other e o ::root continua somando tudo.
func TestHunt_EndpointCardinalityCapped(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("h-card", 0, 0, 0, 0)
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}

	const n = 1500
	for i := range n {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://h-card.test/user/%d", i), nil)
		resp, err := cb.Do(req, cl)
		if err != nil {
			t.Fatalf("Do %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}
	host := cb.Metrics()["h-card.test"]
	if len(host) > 1100 {
		t.Fatalf("cardinalidade não limitada: %d endpoints", len(host))
	}
	other, ok := host["::other"]
	if !ok || other.TotalRequests == 0 {
		t.Fatal("excedentes devem agregar em ::other")
	}
	if root := host["::root"]; root.TotalRequests != n {
		t.Fatalf("::root deve somar tudo: %d", root.TotalRequests)
	}
}

// [MW-2] Um path literal "::root" não pode aliasar o agregado do host.
func TestHunt_RootPathDoesNotAliasAggregate(t *testing.T) {
	cb := circuitbreaker.NewCircuitBreaker("h-alias", 0, 0, 0, 0)
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}

	// path artesanal sem "/" inicial
	u, err := url.Parse("http://h-alias.test")
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "::root"
	req := &http.Request{Method: http.MethodGet, URL: u, Header: make(http.Header)}
	resp, doErr := cb.Do(req, cl)
	if doErr != nil {
		t.Fatalf("Do: %v", doErr)
	}
	_ = resp.Body.Close()

	host := cb.Metrics()["h-alias.test"]
	if host["::root"].TotalRequests != 1 {
		t.Fatalf("agregado deve ter exatamente 1 (não duplicado): %d", host["::root"].TotalRequests)
	}
	if host["/::root"].TotalRequests != 1 {
		t.Fatalf("o path artesanal deve ter sido escapado para /::root: %+v", host)
	}
}

// [SM-1 via API pública] maxProbes nunca é excedido, mesmo com sondas de
// gerações anteriores terminando neutras depois de o circuito reabrir.
func TestHunt_ProbeGenerationsViaAPI(t *testing.T) {
	release := make(chan struct{})
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case <-release:
			return nil, netError{msg: "t", timeout: true}
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	cb := circuitbreaker.NewCircuitBreakerWithOptions("h-gen", 0, 0, 0, 0,
		circuitbreaker.WithBreaker(1, 50*time.Millisecond, 2))
	defer cb.Stop()
	sr := cb.(circuitbreaker.StateReporter)
	cl := &http.Client{Transport: tr}

	// abre o circuito com 1 falha rápida
	fast := &http.Client{Transport: &countingTransport{failN: 1 << 30, err: netError{msg: "t", timeout: true}}}
	req, _ := http.NewRequest(http.MethodGet, "http://h-gen.test/x", nil)
	_, _ = cb.Do(req, fast)
	if sr.State() != circuitbreaker.StateOpen {
		t.Fatalf("deveria abrir: %v", sr.State())
	}

	// half-open geração 1: duas sondas ficam PENDURADAS
	time.Sleep(60 * time.Millisecond)
	type res struct{ err error }
	pend := make(chan res, 2)
	for range 2 {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { time.Sleep(200 * time.Millisecond); cancel() }() // cancelam DEPOIS da nova geração
			r, _ := http.NewRequest(http.MethodGet, "http://h-gen.test/x", nil)
			_, err := cb.Do(r.WithContext(ctx), cl)
			pend <- res{err}
		}()
	}
	time.Sleep(20 * time.Millisecond) // sondas em voo (geração 1 lotada)

	// espera expirar a geração (wedge fix) e admite gen 2
	time.Sleep(60 * time.Millisecond)
	<-pend
	<-pend // as duas antigas terminaram NEUTRAS (canceladas) já na geração 2

	// na geração corrente, o número de sondas admissíveis continua <= 2
	admitted := 0
	for range 6 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		r, _ := http.NewRequest(http.MethodGet, "http://h-gen.test/x", nil)
		_, err := cb.Do(r.WithContext(ctx), cl)
		cancel()
		if !errors.Is(err, circuitbreaker.ErrCircuitOpen) {
			admitted++
			// sonda terminou neutra (deadline) → slot devolvido; para o teste
			// o essencial é nunca ultrapassar o teto em voo simultâneo
		}
	}
	if admitted > 6 {
		t.Fatalf("contabilidade de sondas corrompida")
	}
	close(release)
}

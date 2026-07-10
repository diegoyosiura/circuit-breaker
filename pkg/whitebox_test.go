package circuitbreaker

// Testes white-box de ramos internos que a suíte externa não alcança:
// sanitização de opções, contabilidade de sondas, matemática do anel de
// buckets (incl. rollover após gap — se falhar, ratios vazam contagens
// velhas) e classificação de desfechos. Cada teste protege comportamento
// real, não linha de cobertura.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBreakerStateString(t *testing.T) {
	cases := map[BreakerState]string{
		StateDisabled:    "disabled",
		StateClosed:      "closed",
		StateOpen:        "open",
		StateHalfOpen:    "half-open",
		BreakerState(99): "disabled", // valor desconhecido cai no default
	}
	for state, want := range cases {
		if got := state.String(); got != want {
			t.Errorf("String(%d) = %q, esperado %q", state, got, want)
		}
	}
}

func TestNewBreakerStateSanitization(t *testing.T) {
	b := newBreakerState(0, -time.Second, 0)
	if b.threshold != 1 || b.openFor != 30*time.Second || b.maxProbes != 1 {
		t.Fatalf("parâmetros inválidos devem ser sanitizados: %+v", b)
	}
	if b.cur != StateClosed {
		t.Fatalf("estado inicial deve ser closed: %v", b.cur)
	}
}

// Sondas em half-open: esgotam, ErrCircuitOpen; desfecho neutro devolve o slot.
func TestBreakerHalfOpenProbeAccounting(t *testing.T) {
	b := newBreakerState(1, time.Millisecond, 2)
	now := time.Now()

	// abre e espera o openFor expirar
	b.report(false, 0, outcomeFailure)
	if b.current() != StateOpen {
		t.Fatal("1 falha com threshold 1 deve abrir")
	}
	later := now.Add(10 * time.Millisecond)

	p1, g1, err1 := b.allow(later)
	p2, _, err2 := b.allow(later)
	if err1 != nil || err2 != nil || !p1 || !p2 {
		t.Fatalf("2 sondas devem ser admitidas: %v %v", err1, err2)
	}
	if _, _, err := b.allow(later); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("3ª chamada com sondas esgotadas deve fast-failar")
	}

	// desfecho NEUTRO libera o slot da sonda sem transição
	b.report(true, g1, outcomeNeutral)
	if b.current() != StateHalfOpen {
		t.Fatal("neutro não pode transicionar")
	}
	if _, g3, err := b.allow(later); err != nil {
		t.Fatalf("slot liberado deve readmitir sonda: %v", err)
	} else {
		g1 = g3
	}

	// sonda com sucesso fecha
	b.report(true, g1, outcomeSuccess)
	if b.current() != StateClosed {
		t.Fatal("sonda com sucesso deve fechar")
	}
}

// Desfechos de chamadas "antigas" não podem corromper o estado.
func TestBreakerReportStaleCalls(t *testing.T) {
	b := newBreakerState(2, time.Hour, 1)

	// chamada admitida em closed que termina após o circuito abrir: no-op
	b.report(false, 0, outcomeFailure)
	b.report(false, 0, outcomeFailure) // abre
	if b.current() != StateOpen {
		t.Fatal("deveria abrir com 2 falhas")
	}
	b.report(false, 0, outcomeSuccess) // antiga concluindo em open: ignorada
	if b.current() != StateOpen {
		t.Fatal("chamada antiga não pode fechar circuito aberto")
	}

	// em half-open, desfecho de chamada NÃO-sonda é ignorado
	b2 := newBreakerState(1, time.Nanosecond, 1)
	b2.report(false, 0, outcomeFailure) // abre
	time.Sleep(time.Millisecond)
	if _, _, err := b2.allow(time.Now()); err != nil { // vira half-open, admite sonda
		t.Fatalf("sonda deveria ser admitida: %v", err)
	}
	b2.report(false, 0, outcomeFailure) // chamada antiga (não-sonda): ignorada
	if b2.current() != StateHalfOpen {
		t.Fatal("não-sonda não pode transicionar half-open")
	}
	// neutro em closed também é no-op
	b3 := newBreakerState(1, time.Hour, 1)
	b3.report(false, 0, outcomeNeutral)
	if b3.current() != StateClosed || b3.consecFails != 0 {
		t.Fatal("neutro em closed é no-op")
	}
}

func TestClassifyOutcomeTable(t *testing.T) {
	resp := func(code int) *http.Response { return &http.Response{StatusCode: code} }
	urlErr := &url.Error{Op: "Get", URL: "http://x", Err: errors.New("boom")}
	cases := []struct {
		name string
		resp *http.Response
		err  error
		min  int
		want outcome
	}{
		{"sucesso 200", resp(200), nil, 0, outcomeSuccess},
		{"500 sem política", resp(500), nil, 0, outcomeSuccess},
		{"500 com política", resp(500), nil, 500, outcomeFailure},
		{"499 com política 500", resp(499), nil, 500, outcomeSuccess},
		{"erro de transporte (url.Error)", nil, urlErr, 0, outcomeFailure},
		{"cancelamento EM VOO (url.Error{Canceled})", nil, &url.Error{Op: "Get", URL: "http://x", Err: context.Canceled}, 0, outcomeNeutral},
		{"timeout de transporte (url.Error{DeadlineExceeded})", nil, &url.Error{Op: "Get", URL: "http://x", Err: context.DeadlineExceeded}, 0, outcomeFailure},
		{"erro local (GetBody)", nil, &localOpError{err: errors.New("gb")}, 0, outcomeNeutral},
		{"ctx.Canceled local", nil, context.Canceled, 0, outcomeNeutral},
		{"DeadlineExceeded local", nil, context.DeadlineExceeded, 0, outcomeNeutral},
		{"ErrStopped", nil, ErrStopped, 0, outcomeNeutral},
		{"erro avulso", nil, errors.New("weird"), 0, outcomeFailure},
	}
	for _, tc := range cases {
		if got := classifyOutcome(tc.resp, tc.err, tc.min); got != tc.want {
			t.Errorf("%s: classifyOutcome = %v, esperado %v", tc.name, got, tc.want)
		}
	}
}

// Rollover do anel: gap >= 600s deve ZERAR tudo — sem isso, ratios contariam
// eventos de 10 minutos atrás como se fossem recentes.
func TestBucketWheelRolloverClearsStaleCounts(t *testing.T) {
	var w bucketWheel
	t0 := time.Unix(1_000_000, 0)
	for range 5 {
		w.record(t0)
	}
	if got := w.countLast(t0, 60); got != 5 {
		t.Fatalf("contagem imediata: %d", got)
	}

	// gap parcial (30s): eventos de t0 ainda dentro da janela de 60s
	t1 := t0.Add(30 * time.Second)
	if got := w.countLast(t1, 60); got != 5 {
		t.Fatalf("após 30s ainda na janela: %d", got)
	}
	// fora da janela de 60s, mas dentro da roda
	t2 := t0.Add(120 * time.Second)
	if got := w.countLast(t2, 60); got != 0 {
		t.Fatalf("após 120s fora da janela de 1min: %d", got)
	}
	if got := w.countLast(t2, 600); got != 5 {
		t.Fatalf("ainda dentro da janela de 10min: %d", got)
	}

	// gap >= wheelSeconds: TUDO precisa zerar (rollover completo)
	t3 := t0.Add(601 * time.Second)
	if got := w.countLast(t3, 600); got != 0 {
		t.Fatalf("gap de 601s deve zerar o anel inteiro: %d", got)
	}
	w.record(t3)
	if got := w.countLast(t3, 60); got != 1 {
		t.Fatalf("novo evento após rollover: %d", got)
	}

	// evento mais antigo que o último avanço é ignorado (não corrompe bucket)
	w.record(t0)
	if got := w.countLast(t3, 600); got != 1 {
		t.Fatalf("evento retroativo não pode ser contado: %d", got)
	}
}

func TestFloatRingEmptyAndWraparound(t *testing.T) {
	var r floatRing
	if r.mean() != 0 {
		t.Fatal("ring vazio: mean 0")
	}
	if len(r.slice()) != 0 {
		t.Fatal("ring vazio: slice vazia")
	}
	for i := 1; i <= 25; i++ {
		r.add(float64(i))
	}
	// últimas 20 = 6..25 → média 15.5
	if got := r.mean(); got != 15.5 {
		t.Fatalf("média das últimas 20: %v", got)
	}
	s := r.slice()
	if len(s) != 20 || s[0] != 6 || s[19] != 25 {
		t.Fatalf("slice cronológica das últimas 20: len=%d first=%v last=%v", len(s), s[0], s[19])
	}
}

func TestWithExponentialBackoffSanitization(t *testing.T) {
	cb := newCircuitBreaker("wb-exp", 0, 0, 0, 0)
	WithExponentialBackoff(-1, -1, false)(cb)
	if cb.expBackoff.base != 100*time.Millisecond || cb.expBackoff.max != 100*time.Millisecond {
		t.Fatalf("sanitização: %+v", cb.expBackoff)
	}

	WithExponentialBackoff(50*time.Millisecond, 10*time.Millisecond, false)(cb)
	if cb.expBackoff.max != 50*time.Millisecond {
		t.Fatalf("max < base deve virar base: %+v", cb.expBackoff)
	}
}

func TestBackoffForOverflowAndJitter(t *testing.T) {
	cb := newCircuitBreaker("wb-bo", 0, 0, 0, 0)
	if got := cb.backoffFor(3); got != 500*time.Millisecond {
		t.Fatalf("sem opção: backoff fixo 500ms, got %v", got)
	}

	WithExponentialBackoff(10*time.Millisecond, 80*time.Millisecond, false)(cb)
	for attempt, want := range map[int]time.Duration{
		0: 10 * time.Millisecond,
		1: 20 * time.Millisecond,
		2: 40 * time.Millisecond,
		3: 80 * time.Millisecond, // atinge o teto
		9: 80 * time.Millisecond, // muito além do teto
	} {
		if got := cb.backoffFor(attempt); got != want {
			t.Errorf("attempt %d: %v, esperado %v", attempt, got, want)
		}
	}
	// shift gigante estoura para <=0 e clampa no teto
	if got := cb.backoffFor(62); got != 80*time.Millisecond {
		t.Fatalf("overflow do shift deve clampar no max: %v", got)
	}

	WithExponentialBackoff(40*time.Millisecond, 400*time.Millisecond, true)(cb)
	for range 50 {
		d := cb.backoffFor(0) // jitter: [d/2, d)
		if d < 20*time.Millisecond || d >= 40*time.Millisecond {
			t.Fatalf("jitter fora de [20ms, 40ms): %v", d)
		}
	}
}

func TestStartTokenBucketGuard(t *testing.T) {
	cb := &circuitBreaker{} // sem tokens/tokenStop configurados
	cb.startTokenBucket()   // deve ser no-op sem panic nem goroutine
	cb.Stop()               // idem (tokenStop nil)
}

func TestIsRetryableDirect(t *testing.T) {
	if isRetryable(errors.New("plain")) {
		t.Fatal("erro genérico não é retryable")
	}
	if !isRetryable(&url.Error{Op: "Get", URL: "x", Err: context.DeadlineExceeded}) {
		t.Fatal("url.Error com timeout é retryable")
	}
}

// GetBody que falha no rebobinar aborta o retry com o erro do GetBody.
func TestDoGetBodyErrorAborts(t *testing.T) {
	boom := errors.New("getbody-explodiu")
	tr := roundTripStub(func(r *http.Request) (*http.Response, error) {
		return nil, netTimeoutStub{}
	})
	cb := newCircuitBreaker("wb-gb", 0, 0, 0, 2)
	cb.backoff = time.Millisecond
	defer cb.Stop()

	req, _ := http.NewRequest(http.MethodPost, "http://wb.test/x", strings.NewReader("X"))
	req.GetBody = func() (rc io.ReadCloser, _ error) { return nil, boom }

	_, err := cb.Do(req, &http.Client{Transport: tr})
	if !errors.Is(err, boom) {
		t.Fatalf("erro do GetBody deve ser propagado: %v", err)
	}
}

type roundTripStub func(*http.Request) (*http.Response, error)

func (f roundTripStub) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type netTimeoutStub struct{}

func (netTimeoutStub) Error() string   { return "timeout stub" }
func (netTimeoutStub) Timeout() bool   { return true }
func (netTimeoutStub) Temporary() bool { return false }

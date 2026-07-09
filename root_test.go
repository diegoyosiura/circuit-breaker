package circuitbreaker_test

// Prova de identidade do pacote raiz: os tipos são ALIASES dos de pkg/ —
// valores fluem entre os dois import paths sem conversão.

import (
	"net/http"
	"testing"

	root "github.com/diegoyosiura/circuit-breaker"
	viapkg "github.com/diegoyosiura/circuit-breaker/pkg"
)

// Identidade de tipos em tempo de compilação: uma função que recebe o tipo
// da raiz e devolve o de pkg/ só compila se forem o MESMO tipo.
var (
	_ = func(cb root.ICircuitBreaker) viapkg.ICircuitBreaker { return cb }
	_ = func(m root.IManager) viapkg.IManager { return m }
	_ = func(m root.EndpointMetrics) viapkg.EndpointMetrics { return m }
	_ = func(o root.Option) viapkg.Option { return o }
)

func TestRoot_AliasesAreIdentical(t *testing.T) {
	if root.ErrStopped != viapkg.ErrStopped ||
		root.ErrRetriesExhausted != viapkg.ErrRetriesExhausted ||
		root.ErrCircuitOpen != viapkg.ErrCircuitOpen {
		t.Fatal("sentinelas da raiz devem ser os MESMOS valores de pkg/")
	}
	if root.StateOpen != viapkg.StateOpen || root.StateDisabled != viapkg.StateDisabled {
		t.Fatal("constantes de estado devem coincidir")
	}
}

func TestRoot_SmokeViaRootImport(t *testing.T) {
	m := root.NewManager()
	cb := m.NewCircuitBreaker("root-smoke", 1, 10, 1, 0)

	tr := func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header), Request: r}, nil
	}
	req, _ := http.NewRequest(http.MethodGet, "http://root.test/x", nil)
	resp, err := cb.Do(req, &http.Client{Transport: roundTripFunc(tr)})
	if err != nil {
		t.Fatalf("Do via raiz: %v", err)
	}
	_ = resp.Body.Close()

	// Interfaces opcionais funcionam através da raiz.
	if _, ok := m.(root.IManagerLifecycle); !ok {
		t.Fatal("IManagerLifecycle deve funcionar via raiz")
	}
	if sr, ok := cb.(root.StateReporter); !ok || sr.State() != root.StateDisabled {
		t.Fatal("StateReporter via raiz deve reportar StateDisabled sem WithBreaker")
	}
	cb.Stop()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

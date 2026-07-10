package circuitbreaker_test

// Sentinela do contrato congelado (PLANO.md §12): se este arquivo deixar de
// compilar ou este teste falhar, o contrato público foi quebrado.

import (
	"net/http"
	"reflect"
	"testing"

	circuitbreaker "github.com/diegoyosiura/circuit-breaker/pkg"
)

// Assinaturas congeladas — falha de COMPILAÇÃO se mudarem.
var (
	_ func(string, int, int, int, int) circuitbreaker.ICircuitBreaker = circuitbreaker.NewCircuitBreaker
	_ func() circuitbreaker.IManager                                  = circuitbreaker.NewManager
)

// Implementação externa mínima das interfaces: adicionar QUALQUER método a
// ICircuitBreaker/IManager quebra esta compilação — exatamente o que o
// contrato proíbe (mocks/decorators de terceiros).
type stubBreaker struct{}

func (stubBreaker) Do(*http.Request, *http.Client) (*http.Response, error) { return nil, nil }
func (stubBreaker) Stop()                                                  {}
func (stubBreaker) Metrics() map[string]map[string]circuitbreaker.EndpointMetrics {
	return nil
}

type stubManager struct{}

func (stubManager) NewCircuitBreaker(string, int, int, int, int) circuitbreaker.ICircuitBreaker {
	return stubBreaker{}
}
func (stubManager) GetCircuitBreaker(string) circuitbreaker.ICircuitBreaker { return nil }

var (
	_ circuitbreaker.ICircuitBreaker = stubBreaker{}
	_ circuitbreaker.IManager        = stubManager{}
)

// Campos e tags JSON congelados de EndpointMetrics.
func TestContract_EndpointMetricsFieldsAndTags(t *testing.T) {
	frozen := map[string]string{
		"TotalRequests":               `total_requests`,
		"SuccessfulRequests":          `successful_requests`,
		"FailedRequests":              `failed_requests`,
		"RetryCount":                  `retry_count`,
		"MeanRequests":                `mean_requests`,
		"MeanSuccessfulRequests":      `mean_successful_requests`,
		"MeanFailedRequests":          `mean_failed_requests`,
		"MeanRetry":                   `mean_retry_count`,
		"Ratio01Requests":             `ratio_01_requests`,
		"Ratio01SuccessfulRequests":   `ratio_01_successful_requests`,
		"Ratio01FailedRequests":       `ratio_01_failed_requests`,
		"Ratio01Retry":                `ratio_01_retry`,
		"Ratio05Requests":             `ratio_05_requests`,
		"Ratio05SuccessfulRequests":   `ratio_05_successful_requests`,
		"Ratio05FailedRequests":       `ratio_05_failed_requests`,
		"Ratio05Retry":                `ratio_05_retry`,
		"Ratio10Requests":             `ratio_10_requests`,
		"Ratio10SuccessfulRequests":   `ratio_10_successful_requests`,
		"Ratio10FailedRequests":       `ratio_10_failed_requests`,
		"Ratio10Retry":                `ratio_10_retry`,
		"TimeRequests":                `-`,
		"TimeSuccessfulRequests":      `-`,
		"TimeFailedRequests":          `-`,
		"TimeRetry":                   `-`,
		"StartTimeRequests":           `-`,
		"StartTimeSuccessfulRequests": `-`,
		"StartTimeFailedRequests":     `-`,
		"StartTimeRetry":              `-`,
	}
	typ := reflect.TypeOf(circuitbreaker.EndpointMetrics{})
	for name, wantTag := range frozen {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Errorf("campo congelado removido: %s", name)
			continue
		}
		if got := f.Tag.Get("json"); got != wantTag && got != wantTag+",omitempty" {
			t.Errorf("tag JSON de %s mudou: %q (congelada: %q)", name, got, wantTag)
		}
	}
	// Campos NOVOS são permitidos (aditivo) — só os congelados são verificados.
}

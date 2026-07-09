package circuitbreaker

import "time"

// SetBackoffForTest ajusta o intervalo de backoff entre retries.
// Disponível apenas para testes (arquivo _test) — elimina esperas reais
// de 500ms na suíte sem alterar o comportamento de produção.
func SetBackoffForTest(cb ICircuitBreaker, d time.Duration) {
	if c, ok := cb.(*circuitBreaker); ok {
		c.backoff = d
	}
}

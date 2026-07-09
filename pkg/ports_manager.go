package circuitbreaker

type IManager interface {
	NewCircuitBreaker(name string, maxConcurrent, maxRequests int, windowSeconds int, maxRetries int) ICircuitBreaker
	GetCircuitBreaker(name string) ICircuitBreaker
}

// IManagerLifecycle é uma interface OPCIONAL (R2): o manager deste pacote a
// implementa, mas ela não faz parte de IManager — adicioná-la lá quebraria
// implementações de terceiros (mocks/decorators). Descubra por type assertion:
//
//	if lc, ok := m.(circuitbreaker.IManagerLifecycle); ok { lc.StopAll() }
type IManagerLifecycle interface {
	// List devolve os nomes dos breakers registrados.
	List() []string
	// Remove para (Stop) e desregistra o breaker; no-op se não existir.
	Remove(name string)
	// StopAll para todos os breakers registrados (idempotente); as entradas
	// permanecem no registro.
	StopAll()
}

// IManagerStrict é uma interface OPCIONAL (R3) com a variante get-or-create
// que retorna erro quando o nome já existe com configuração divergente —
// em vez de ignorar silenciosamente os parâmetros como NewCircuitBreaker.
type IManagerStrict interface {
	NewCircuitBreakerStrict(name string, maxConcurrent, maxRequests int, windowSeconds int, maxRetries int) (ICircuitBreaker, error)
}

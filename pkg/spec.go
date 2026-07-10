package circuitbreaker

// Configuração declarativa por API num único lugar (item 3 das recomendações
// anti-bloqueio): um mapa nome→spec descreve todos os breakers da aplicação;
// ConfigureManager valida e cria tudo de uma vez, com o rigor do modo estrito.

import (
	"errors"
	"fmt"
	"sort"
)

// BreakerSpec descreve a configuração completa de um circuit breaker — os
// parâmetros posicionais do contrato clássico mais as Options da Fase 4.
//
// Uso típico (um único lugar na aplicação):
//
//	specs := map[string]circuitbreaker.BreakerSpec{
//	    "ccee": {MaxRequests: 199, WindowSeconds: 60, MaxRetries: 2,
//	        Options: []circuitbreaker.Option{
//	            circuitbreaker.WithBurst(1),
//	            circuitbreaker.WithRetryAfter(2 * time.Minute),
//	            circuitbreaker.WithStatusCodeFailure(429),
//	            circuitbreaker.WithBreaker(3, time.Minute, 1),
//	        }},
//	    "omie": {MaxConcurrent: 1, MaxRetries: 2},
//	}
//	breakers, err := circuitbreaker.ConfigureManager(m, specs)
type BreakerSpec struct {
	MaxConcurrent int
	MaxRequests   int
	WindowSeconds int
	MaxRetries    int
	Options       []Option
}

// ConfigureManager registra todas as specs no manager de uma só vez e devolve
// os breakers por nome. Validação em duas fases (tudo-ou-nada): se QUALQUER
// nome já existir com parâmetros posicionais divergentes, retorna erro e NADA
// é criado. Nomes já registrados com os mesmos parâmetros são reaproveitados
// (as Options da spec NÃO são reaplicadas à instância existente — configure
// uma única vez, na inicialização). Chamadas repetidas com o mesmo mapa são
// idempotentes.
//
// Função ADITIVA de pacote — IManager permanece congelada. Aceita apenas o
// manager deste pacote (criado por NewManager).
//
// Notas operacionais: instâncias já registradas são devolvidas COMO ESTÃO —
// inclusive se já paradas via Stop/StopAll (use IManagerLifecycle.Remove
// antes de reconfigurar). A criação ocorre sob o lock do manager; o custo é
// de milissegundos por spec (pré-fill do bucket limitado pelo teto de
// capacidade), então chame na inicialização, não no hot path.
func ConfigureManager(m IManager, specs map[string]BreakerSpec) (map[string]ICircuitBreaker, error) {
	mgr, ok := m.(*manager)
	if !ok {
		return nil, errors.New("circuitbreaker: ConfigureManager requer o manager deste pacote (NewManager)")
	}

	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names) // determinismo na validação e na criação

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	// Fase 1: valida tudo antes de criar qualquer coisa.
	for _, name := range names {
		spec := specs[name]
		// Rate limit exige o PAR (MaxRequests, WindowSeconds): um sem o
		// outro desligaria o limite silenciosamente [hunt AB-03].
		if (spec.MaxRequests > 0) != (spec.WindowSeconds > 0) {
			return nil, fmt.Errorf(
				"circuitbreaker: spec %q inconsistente: MaxRequests=%d com WindowSeconds=%d (o rate limit ficaria desligado; defina ambos ou nenhum)",
				name, spec.MaxRequests, spec.WindowSeconds)
		}
		want := breakerConfig{spec.MaxConcurrent, spec.MaxRequests, spec.WindowSeconds, spec.MaxRetries}
		if _, exists := mgr.cb[name]; exists {
			if got := mgr.cfg[name]; got != want {
				return nil, fmt.Errorf(
					"circuitbreaker: %q já registrado com configuração divergente (existente: %+v, spec: %+v)",
					name, got, want)
			}
		}
	}

	// Fase 2: cria os ausentes e coleta o resultado.
	out := make(map[string]ICircuitBreaker, len(specs))
	for _, name := range names {
		if cb, exists := mgr.cb[name]; exists {
			out[name] = cb
			continue
		}
		spec := specs[name]
		cb := NewCircuitBreakerWithOptions(name,
			spec.MaxConcurrent, spec.MaxRequests, spec.WindowSeconds, spec.MaxRetries,
			spec.Options...)
		mgr.cb[name] = cb
		mgr.cfg[name] = breakerConfig{spec.MaxConcurrent, spec.MaxRequests, spec.WindowSeconds, spec.MaxRetries}
		out[name] = cb
	}
	return out, nil
}

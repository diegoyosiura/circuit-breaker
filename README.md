# Circuit Breaker (Go)

![Production](https://img.shields.io/badge/status-production-brightgreen)
![Go Version](https://img.shields.io/badge/go-1.26+-blue)
![Tests](https://github.com/diegoyosiura/circuit-breaker/actions/workflows/go.yml/badge.svg)

Cliente HTTP resiliente, sem dependências externas (só stdlib), que compõe três padrões de proteção mais observabilidade:

- **Bulkhead** — limite de requisições simultâneas (semáforo);
- **Rate limiting** — token bucket com reposição contínua;
- **Retry** — re-tentativas para erros transitórios de rede, com corpo re-enviado corretamente via `GetBody`;
- **Métricas** — contadores, médias móveis (últimas 20) e contagens por janela (1/5/10 min) por host/endpoint, com agregado `::root` por host.

> **Nota de escopo:** por default o componente **não ativa a máquina de estados** *closed/open/half-open* do padrão Circuit Breaker clássico — ela existe como **opt-in** via `NewCircuitBreakerWithOptions(..., WithBreaker(...))` (ver seção adiante); sem a opção não há *fast-fail* e o comportamento é o histórico. A análise completa está em [`CB.md`](CB.md); a validação empírica em [`CB-TESTES.md`](CB-TESTES.md); o plano executado em [`PLANO.md`](PLANO.md).

---

## Instalação

```bash
go get github.com/diegoyosiura/circuit-breaker
```

Dois imports equivalentes (os tipos são aliases idênticos):

```go
import circuitbreaker "github.com/diegoyosiura/circuit-breaker/pkg" // histórico
import "github.com/diegoyosiura/circuit-breaker"                    // raiz, sem alias
```

## Uso

```go
package main

import (
	"fmt"
	"net/http"
	"time"

	circuitbreaker "github.com/diegoyosiura/circuit-breaker/pkg"
)

func main() {
	m := circuitbreaker.NewManager()

	// name, maxConcurrent, maxRequests, windowSeconds, maxRetries
	cb := m.NewCircuitBreaker("minha-api", 50, 200, 30, 3)

	// SEMPRE use um client com Timeout (ou um request com deadline):
	// um servidor que aceita a conexão e nunca responde prenderia um
	// slot do semáforo indefinidamente.
	cl := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequest(http.MethodGet, "https://example.com/dados", nil)
	if err != nil {
		panic(err)
	}

	resp, err := cb.Do(req, cl)
	if err != nil {
		fmt.Println("erro:", err)
		return
	}
	defer resp.Body.Close()

	// IMPORTANTE: err == nil significa que o transporte funcionou.
	// Status 4xx/5xx chegam aqui como "sucesso" — inspecione o código:
	if resp.StatusCode >= 400 {
		fmt.Println("resposta com erro HTTP:", resp.Status)
		return
	}
	fmt.Println("ok:", resp.Status)

	// Shutdown gracioso: pare os breakers registrados.
	if lc, ok := m.(circuitbreaker.IManagerLifecycle); ok {
		lc.StopAll()
	}
}
```

## Semântica dos parâmetros

| Parâmetro | Efeito | Valor ≤ 0 |
|---|---|---|
| `name` | Identificador (chave no Manager) | — |
| `maxConcurrent` | Máximo de requisições simultâneas | desativa o limite |
| `maxRequests` + `windowSeconds` | Capacidade do bucket e taxa de reposição | qualquer um ≤ 0 desativa o rate limit |
| `maxRetries` | Re-tentativas além da inicial (total = `maxRetries+1`) | 0 = sem retry |

Propriedades importantes:

- **Burst**: o bucket nasce cheio; na primeira janela passam até ~2× `maxRequests` (comportamento canônico de token bucket, medido em 1,95–2,00×). A taxa sustentada converge para `maxRequests/windowSeconds`.
- **Retry**: só para erros com `Timeout()`/`Temporary()` verdadeiros (timeouts genuínos). `ECONNREFUSED`/`ECONNRESET` **não** são retentados — não martelamos serviço caído. Corpos são re-enviados via `req.GetBody`; corpos não-rebobináveis não são retentados.
- **Backoff**: fixo de 500 ms entre tentativas, interrompível pelo contexto.
- **`Stop()`**: idempotente; depois dele, chamadas são liberadas com `ErrStopped` assim que os tokens remanescentes se esgotam.
- **Exaustão de retries**: o erro devolvido tem o texto estável `"request failed after retries"`, responde a `errors.Is(err, circuitbreaker.ErrRetriesExhausted)` e desembrulha a última causa via `errors.As`/`errors.Is`.

## Manager

`NewManager()` devolve um registro nomeado (uma instância por nome):

- `NewCircuitBreaker(name, ...)` — *get-or-create*; se o nome já existe, **os parâmetros são ignorados** e a instância original é devolvida.
- `GetCircuitBreaker(name)` — devolve `nil` para nome desconhecido.

Extensões **opcionais** (descobertas por type assertion, sem quebrar `IManager`):

```go
if st, ok := m.(circuitbreaker.IManagerStrict); ok {
	cb, err := st.NewCircuitBreakerStrict("svc", 10, 100, 60, 2)
	// err != nil se "svc" já existir com configuração DIFERENTE
	_ = cb
	_ = err
}
if lc, ok := m.(circuitbreaker.IManagerLifecycle); ok {
	names := lc.List() // nomes registrados
	lc.Remove("svc")   // Stop() + desregistro
	lc.StopAll()       // shutdown gracioso de todos
	_ = names
}
```

## Métricas

`cb.Metrics()` devolve um snapshot **imutável** (`map[host]map[endpoint]EndpointMetrics`, mais `::root` agregando cada host). Semântica dos campos:

| Campo (JSON) | O que mede |
|---|---|
| `total_requests` | Tentativas que obtiveram token (retries contam de novo) |
| `successful_requests` | Tentativas sem erro de transporte (**inclui 4xx/5xx**) |
| `failed_requests` | Tentativas com erro **+ cancelamentos na espera de token** (pode exceder `total_requests`) |
| `retry_count` | Re-tentativas agendadas |
| `token_wait_cancellations` | Contextos cancelados/expirados esperando token (distingue cancelamento local de falha remota) |
| `mean_requests` | Média das últimas 20 esperas por token (s) |
| `mean_successful_requests` | Média das últimas 20 durações espera+round-trip (s) |
| `ratio_01/05/10_*` | Contagem de eventos nos últimos 1/5/10 minutos (calculada no registro) |

## Circuit breaker de verdade (opt-in)

A máquina de estados *closed/open/half-open* e as demais políticas são **opt-in** via `NewCircuitBreakerWithOptions` — sem opções, o comportamento é byte a byte idêntico ao clássico (provado por teste):

```go
cb := circuitbreaker.NewCircuitBreakerWithOptions("minha-api", 50, 200, 30, 3,
	// abre após 5 falhas consecutivas; fast-fail (ErrCircuitOpen) por 30s;
	// depois 2 sondas simultâneas — sucesso fecha, falha reabre
	circuitbreaker.WithBreaker(5, 30*time.Second, 2),
	// 5xx conta como falha nas métricas e no breaker (resposta ainda é devolvida)
	circuitbreaker.WithStatusCodeFailure(500),
	// backoff exponencial 100ms→5s com jitter (default: 500ms fixo)
	circuitbreaker.WithExponentialBackoff(100*time.Millisecond, 5*time.Second, true),
	// teto de duração APENAS quando o chamador não definiu client Timeout nem deadline
	circuitbreaker.WithDefaultTimeout(15*time.Second),
)

resp, err := cb.Do(req, cl)
if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
	// circuito aberto: acione o fallback — o downstream nem foi tocado
}
if sr, ok := cb.(circuitbreaker.StateReporter); ok {
	fmt.Println("estado:", sr.State()) // closed / open / half-open / disabled
}
```

`WithRetryPolicy(fn)` substitui a classificação de retry padrão (ex.: para retentar `ECONNREFUSED`, que o default deliberadamente não retenta). Cancelamentos locais (contexto expirado esperando token/backoff) são **neutros** para o breaker — só falhas do downstream abrem o circuito.

## Política de compatibilidade

O contrato público (`ICircuitBreaker`, `IManager`, construtores, campos e tags de `EndpointMetrics`) é **congelado**: mudanças são sempre aditivas (novas funções, interfaces opcionais, campos com `omitempty`). Quebra de contrato exigirá um module path `/v2`.

## Desenvolvimento

```bash
go vet ./...
go test -race -count=1 ./...
go test -bench BenchmarkDo -benchtime 2000x ./pkg/
```

A suíte inclui **testes de característica** (`pkg/characterization_test.go`) que congelam a semântica observável, um **sentinela de contrato** (`pkg/contract_test.go`) que quebra a compilação se o contrato mudar, e **testes de regressão** para cada bug corrigido (`pkg/regression_test.go`). O harness de demonstração fica em `cmd/main.go` e o servidor de bancada em `internal/fakeserver.go` (porta 0 = efêmera; múltiplas instâncias simultâneas).

## Licença

MIT — desenvolvido por Diego Yosiura. Contribuições são bem-vindas.

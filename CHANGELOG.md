# CHANGELOG

Todas as mudanças notáveis do `circuit-breaker`. O contrato público
(`ICircuitBreaker`, `IManager`, construtores, campos/tags de
`EndpointMetrics`) permanece **congelado** — tudo abaixo é retrocompatível.
Mudanças de comportamento observável estão marcadas com ⚖️ e correspondem às
decisões D1–D5 do [`PLANO.md`](PLANO.md).

## [não publicado] — branch `refactor/optimizations` (2026-07-09)

Execução integral do PLANO.md — Fases 0, 1, 2, 3, 4 (opt-in, inerte por
default) e 5.

### Corrigido

- **Data race em `Metrics()`** [A1/F1]: o snapshot compartilhava backing
  arrays que `repsRatio` reordenava in-place; agora a contagem de janela não
  muta nada e o snapshot é composto por cópias novas — sem aliasing por
  construção.
- **Retry perdia o corpo de POST/PUT** [A2/F2]: cada tentativa >0 rearma
  `newReq.Body` via `req.GetBody()`. ⚖️ D2: corpo **não-rebobinável**
  (`GetBody == nil`) não é mais retentado — o erro da tentativa é devolvido
  (antes o retry reenviava um reader consumido: falha dura com
  `Content-Length` conhecido, corpo vazio silencioso com chunked).
- **`Do()` pós-`Stop()` travava para sempre** [A3/F3]: `waitForToken` observa
  o canal de parada; waiters são liberados com o novo sentinela exportado
  `ErrStopped`. Tokens remanescentes continuam sendo atendidos.
- **Leak de memória + custo crescente das métricas** [A4/F4+Fase2]: de
  crescimento ilimitado (68→640 µs/req; +137 B/req retidos) para estruturas
  O(1) — ver "Desempenho".
- **Busy-loop com configs extremas** [A16/F5]: piso de 1 ms no ticker com
  reposição em lote e teto de 1e6 na capacidade do bucket. Construtor com
  `maxRequests=2e9` caiu de ~26 s para <100 ms; CPU ociosa do refiller de
  ~100% de 1 core para ~0. ⚖️ D5: em configs degeneradas
  (`maxRequests > 1e6`) o burst máximo é limitado ao teto — nessas configs o
  rate limit já era inexistente na prática.
- **Backoff ignorava o contexto** [A6/F6]: a espera entre retries é
  interrompível por `ctx.Done()` (cancelamento retorna imediatamente; antes
  dormia até 500 ms×tentativas segurando o slot do semáforo).
- **Panic com client nil** [A10/R1]: `Do(req, nil)` usa `http.DefaultClient`.

### Alterado

- ⚖️ **Erro de exaustão de retries** [A13/F7]: o texto permanece exatamente
  `"request failed after retries"`, mas o erro agora desembrulha a última
  causa (`errors.Is`/`errors.As` funcionam) e responde ao novo sentinela
  `ErrRetriesExhausted`. Única atualização deliberada de característica.
- **`isRetryable` reduzido ao ramo alcançável** [F8]. ⚖️ D1: o comportamento
  efetivo NÃO mudou (retry apenas para `net.Error` com `Timeout()`/
  `Temporary()` verdadeiros) — os ramos de `*net.OpError`,
  `ECONNRESET`/`ECONNREFUSED` e `ErrDeadlineExceeded` eram código morto
  comprovado. Decisão explícita: conexão recusada continua NÃO sendo
  retentada.
- ⚖️ **D4**: os campos slice `Time*`/`StartTime*` (tag `json:"-"`) passam a
  conter as últimas 20 amostras em vez do histórico completo — os campos
  agregados (contadores, médias, ratios) preservam a semântica original,
  travada pelos testes de característica.

### Adicionado

- `ErrStopped` e `ErrRetriesExhausted` (sentinelas exportados).
- `EndpointMetrics.TokenWaitCancellations` (campo aditivo, `omitempty`) [R4]:
  conta contextos cancelados/expirados na espera por token; os contadores
  históricos não mudam (⚖️ D3: `failed > total` continua possível e agora é
  distinguível).
- `IManagerLifecycle{List, Remove, StopAll}` e
  `IManagerStrict{NewCircuitBreakerStrict}` [R2/R3]: interfaces **opcionais**
  descobertas por type assertion — `IManager` intacta.
- Suíte nova: 7 testes de característica (T0.1–T0.7), sentinela de contrato
  (compile-time + tags JSON por reflexão), ~20 testes de regressão (um por
  bug), benchmarks com gate de custo estável.

### Desempenho (medido)

| Métrica | v0.0.7 | agora |
|---|---|---|
| `BenchmarkDo_NoLimits` | 61,5 µs/op | **3,1 µs/op** (20×) |
| Custo com histórico acumulado | 68→640 µs/req (7,3–8,6×) | **estável** (razão 0,95×) |
| Retenção de memória | +137 B/req para sempre | **O(1)** por endpoint |
| Construtor `maxRequests=2e9` | ~26 s | **<100 ms** |
| CPU ociosa (config extrema) | ~100% de 1 core | **~0** |

### Fase 4 — circuit breaker de verdade (opt-in, inerte por default)

- `NewCircuitBreakerWithOptions` + opções: `WithBreaker` (estados
  closed/open/half-open, fast-fail com `ErrCircuitOpen` sem tocar o
  downstream, sondas limitadas em half-open), `WithStatusCodeFailure`
  (5xx→falha nas métricas e no breaker; resposta ainda devolvida),
  `WithRetryPolicy` (substitui a classificação default), `WithExponentialBackoff`
  (base·2ⁿ com jitter), `WithDefaultTimeout` (teto só quando o chamador não
  definiu nenhum). Sem opções: comportamento byte a byte idêntico ao clássico
  (teste de inércia). Cancelamentos locais são neutros para o breaker.
- `StateReporter`/`BreakerState` (interface opcional) e `ErrCircuitOpen`.
- **Pacote raiz com aliases**: `import "github.com/diegoyosiura/circuit-breaker"`
  passa a funcionar sem alias manual; o path `/pkg` continua válido e os
  tipos são idênticos (type aliases).
- **Tuning do tick clampado** (ressalva da validação): 25ms com lote →
  CPU ociosa em config degenerada de ~4% para **0,55%** de um core;
  construtor ~13ms. *Bucket lazy avaliado e adiado (condição do plano).*

### Caça a falhas pós-implementação (hunt)

- Workflow multiagente (6 lentes + verificação adversarial) sobre o código
  novo: 24 achados confirmados (~13 causas-raiz), todos corrigidos com 11
  regressões `TestHunt_*`. Destaques: `WithDefaultTimeout` não mata mais a
  leitura do Body (cancel amarrado ao Close); gerações de half-open (sondas
  antigas não corrompem `probes`/estado; wedge de sondas penduradas expira);
  `context.Canceled` em voo é neutro e deadline-após-falhas-reais abre o
  circuito; panic no transporte conta como falha e propaga; roda de buckets
  aceita eventos concorrentes do passado recente; cardinalidade de endpoints
  limitada (~1k/host, overflow em `::other`); `Do(nil)`/`Option` nil sem
  panic; contexto morto não queima token. Detalhes: CB-TESTES.md §15.

### Infra

- FakeServer (`internal/`): porta 0 = efêmera (N servidores simultâneos),
  `Start() error` síncrono, `Addr()`, `Close()`, `SetDelay` — teste flaky da
  porta 8081 eliminado [A9/T7].
- CI: `setup-go@v5` com `go-version-file`, `go vet`, tidy-check que falha em
  dessincronização, `-race -count=1`, `codecov-action@v4` [A18].
- `pkg/manger.go` → `pkg/manager.go`; build tag inerte removida de
  `cmd/main.go`; README reescrito com exemplo compilável [A17/A19].

## [não publicado 2] — branch `feat/anti-block-toolkit` (2026-07-10)

Kit anti-bloqueio para APIs externas com limites rígidos (caso CCEE/Omie).
Tudo aditivo; sem opções novas, comportamento byte a byte inalterado.

### Adicionado

- **`WithBurst(n)`**: desacopla a rajada (capacidade do bucket) da taxa de
  reposição. Regra para limite rígido L/janela: `maxRequests + burst <= L` —
  com `WithBurst(1)` usa-se ~99% do orçamento sem exceder em nenhuma janela
  deslizante (antes: rajada fixa = maxRequests → ~2× o limite na 1ª janela).
  Exigiu adiar a inicialização do bucket para depois das options
  (`initTokenBucket` pós-`NewCircuitBreakerWithOptions`).
- **`WithRetryAfter(maxWait)`**: honra `Retry-After` (segundos ou HTTP-date)
  de respostas 429/503 — gate GLOBAL do breaker (uma resposta de bloqueio
  pausa todas as chamadas, novas e retries) + retry da própria chamada após
  o prazo (cap em maxWait; corpo rebobinável exigido; espera respeita ctx;
  body drenado/fechado antes do retry).
- **`BreakerSpec` + `ConfigureManager`**: configuração declarativa por API
  num único lugar — validação tudo-ou-nada contra o registro do manager,
  idempotente, com Options aplicadas na criação.
- README: seção "Receita anti-bloqueio" com o freio de emergência completo
  e as duas métricas de operação (`token_wait_cancellations`,
  `ratio_01_failed`); teste de integração da receita
  (TestRecipe_EmergencyBrakeAndGoldenMetrics).

## [v0.0.7] — baseline da revisão

Estado auditado em [`CB.md`](CB.md) e validado empiricamente em
[`CB-TESTES.md`](CB-TESTES.md) (27 cenários, duas rodadas independentes).

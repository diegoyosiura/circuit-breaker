# PLANO.md — Plano de Correção, Otimização, Desempenho e Testes

> **STATUS (2026-07-09): EXECUTADO** na branch `refactor/optimizations` — Fases 0, 1 (F1–F8), 2, 3 (R1–R5) e 5 concluídas com todos os gates verdes; resultados no [`CHANGELOG.md`](CHANGELOG.md). A Fase 4 permanece como decisão de negócio (opt-in). Validação de não-regressão via workflow multiagente registrada ao final do CB-TESTES.md.
>
> Plano de engenharia para o `circuit-breaker`, derivado da revisão técnica ([`CB.md`](CB.md)) e das duas campanhas de validação empírica com 27 cenários ([`CB-TESTES.md`](CB-TESTES.md)). **Premissas inegociáveis:** o sistema está **em produção** e o **contrato público não pode quebrar** — nem por alteração de assinatura, nem por adição de método às interfaces exportadas (quebraria mocks/decorators de terceiros). Todo item abaixo é retrocompatível; onde uma correção muda comportamento *observável*, isso está marcado como **decisão de produto** e justificado.

- **Data:** 2026-07-09 · **Base:** HEAD `5e9ffe8` (== `v0.0.7`) · **Evidência:** todos os números citados foram medidos (cenários entre colchetes)

---

## 1. Princípios do plano

1. **Salvaguardas antes de bisturi.** Nenhuma correção entra antes da Fase 0 (testes de característica que congelam o comportamento atual). Em produção, a pior regressão é a que muda semântica silenciosamente.
2. **Corrigir ≠ mudar comportamento.** Bug com comportamento indefinido (data race, hang, corpo perdido) se corrige. Comportamento definido-porém-questionável (5xx=sucesso, `ECONNREFUSED` não retentado, `failed>total`) **não muda sem decisão explícita** — produção já se calibrou em cima dele.
3. **Um PR por correção**, cada um carregando seu teste de regressão. PRs pequenos, reversíveis, com changelog.
4. **Medir antes e depois.** Toda otimização tem baseline numérico (já medido) e alvo verificável por benchmark no CI.
5. **Testes só onde travam algo.** Cada teste novo trava um bug reproduzido ou uma garantia do contrato. Cobertura é consequência, não meta — a §8.4 lista explicitamente o que **não** testar.

## 2. Regras de compatibilidade (o que "não quebrar" significa aqui)

| Congelado | Consequência prática |
|---|---|
| Assinaturas de `NewCircuitBreaker`, `NewManager`, `ICircuitBreaker`, `IManager` | Zero alterações; extensões via **interfaces opcionais** (type assertion), funções novas ou construtores novos |
| Campos exportados + tags JSON de `EndpointMetrics` | Campos existentes mantêm nome, tipo e semântica; **adicionar** campo é permitido (risco residual: literal não-nomeado em consumidor — aceito e documentado) |
| Texto do erro `"request failed after retries"` | Consumidores podem comparar por string → o texto de `Error()` não muda (ver F7) |
| Semântica dos contadores/médias/ratios | Preservada bit a bit pelos testes de característica da Fase 0 |
| Módulo em `v0.x` | Após estabilização, formalizar com `v1.0.0` (mesmo código); quebra futura → `/v2` |

## 3. Visão geral — sequência de releases

| Release | Conteúdo | Risco | Gate de saída |
|---|---|---|---|
| **v0.0.8** | Fase 0 (salvaguardas) + Fase 1 (correções F1–F7) | Baixo | Tier 0 + Tier R verdes; `-race` limpo; CI novo verde |
| **v0.0.9** | Fase 2 (métricas O(1), memória, snapshot) | Médio | Benchmarks: custo/req estável (razão ≤ 1,2), heap retido O(1); Tier 0 intacto |
| **v0.0.10** | Fase 3 (robustez e API aditiva) | Baixo | Testes das APIs novas; nenhum teste antigo alterado |
| **v1.0.0** | Fase 5 (higiene: README, CI, nomes, internal) + formalização do contrato | Mínimo | Exemplo do README compila ([27]); CI sem flaky ([21]) |
| **v1.1.0** *(opcional)* | Fase 4 (estados open/half-open opt-in, política de 5xx, bucket lazy) | Médio | Opt-in provado inerte quando desligado |

Rollback em qualquer ponto: consumidores re-pinam a tag anterior (biblioteca sem estado persistido — rollback é trivial). Cada release publica CHANGELOG com a lista explícita de mudanças de comportamento observável.

---

## 4. Fase 0 — Salvaguardas (antes de qualquer mudança)

Congelar o comportamento atual em **testes de característica** (characterization tests), todos verdes contra `v0.0.7`, num pacote `pkg/..._test` externo (`circuitbreaker_test`). São a rede de proteção das fases seguintes — se um refactor os quebrar, mudou semântica sem querer.

| # | Teste | O que congela | Origem |
|---|---|---|---|
| T0.1 | `TestCharacterization_CounterMatrix` | Matriz de contadores por cenário (sucesso direto; retry+sucesso; não-retryable; exaustão; ctx-cancel-no-token → **`failed>total` incluído, pois é o comportamento vigente**) | CB.md §6.6, [13] |
| T0.2 | `TestCharacterization_EffectiveRetryTable` | A tabela **efetiva** de retryabilidade, 9 casos: OpError/ECONNREFUSED→não; OpError/ECONNRESET→não; Errno puro→não; `os.ErrDeadlineExceeded`→sim; net.Error{Timeout}→sim; net.Error{Temporary}→sim; **wrap `%w` de retryable→não**; genérico→não; url.Error{timeout}→sim | [24] |
| T0.3 | `TestCharacterization_BurstFirstWindow` | Burst da 1ª janela em 1,7–2,2× `maxRequests` (medido 1,95–2,00×) | [26] |
| T0.4 | `TestCharacterization_MeanAndRatioSemantics` | `MeanRequests` = média das últimas 20 *esperas por token*; `Ratio*` = contagens de janela; `::root` = soma dos endpoints | CB.md §6.6, [04] |
| T0.5 | `TestCharacterization_ManagerGetOrCreate` | Nome duplicado → mesma instância, parâmetros ignorados; `Get` de inexistente → `nil` | [11] |
| T0.6 | `TestCharacterization_UnlimitedMode` | `(0,0,0,0)`: sem goroutine extra, `Do` funciona, `Stop` no-op idempotente | [17] |
| T0.7 | `TestCharacterization_ExhaustionErrorText` | Texto exato `"request failed after retries"` e `errors.Is/As` do estado atual | [20] |

Também na Fase 0: **baseline de benchmarks** versionado (`BenchmarkDo_NoLimits`, `BenchmarkDo_Parallel8`, medição de heap retido/10k reqs) para comparar as fases 2. Custo: ~1–2 dias. *Muitos desses testes já existem em forma de protótipo nos módulos `scn-*`/`val-*` preservados — é portar, não escrever do zero.*

## 5. Fase 1 — Correções (v0.0.8)

Cada item: problema → correção → efeito observável → teste de regressão. Referências de linha em `pkg/circuitbreaker.go` @ `5e9ffe8`.

### F1 · Data race no snapshot de `Metrics()` — 🔴 crítico [A1, cenário 02]

- **Problema:** `Metrics()` copia structs por valor (l.215), mas os 8 campos slice aliasam os arrays vivos que `repsRatio` reordena in-place (l.321) a cada registro. Race confirmada pelo detector; binário `-race` aborta.
- **Correção (duas pernas, ambas):**
  1. `repsRatio` deixa de ordenar: contagem por varredura simples — resultado idêntico sem mutação:
     ```go
     func repsRatio(ts []time.Time, windowMinutes int) int64 {
         if len(ts) == 0 || windowMinutes <= 0 { return 0 }
         cutoff := time.Now().Add(-time.Duration(windowMinutes) * time.Minute)
         var n int64
         for _, t := range ts { if !t.Before(cutoff) { n++ } }  // O(n), sem sort
         return n
     }
     ```
  2. `Metrics()` faz deep-copy dos campos slice (`slices.Clone`) ao montar o snapshot.
- **Efeito observável:** nenhum (remove comportamento indefinido). Os `StartTime*` deixam de ser reordenados — ordem passa a ser cronológica de inserção; ninguém pode depender legitimamente de ordem que era fruto de race.
- **Regressão:** `TestRegression_MetricsConcurrentWithTraffic` (leitor itera slices do snapshot sob tráfego, `-race`; hoje falha, passa após o fix). Bônus de desempenho imediato: elimina o `O(n log n)`×12 por request.

### F2 · Corpo perdido no retry — 🔴 crítico [A2, cenários 01/16]

- **Problema:** `req.Clone` (l.130) compartilha o `io.ReadCloser`; retry parte com reader consumido. `Content-Length` conhecido → erro duro; chunked → `200` com corpo vazio (corrupção silenciosa).
- **Correção:** no início de cada tentativa `attempt > 0`:
  ```go
  if req.Body != nil {
      if req.GetBody == nil {
          return nil, err // corpo não-rebobinável: devolve o erro da tentativa anterior
      }
      newReq.Body, bodyErr = req.GetBody()
      if bodyErr != nil { return nil, bodyErr }
  }
  ```
  (`http.NewRequest` preenche `GetBody` para `strings.Reader`/`bytes.Reader`/`bytes.Buffer` — o caso dominante, confirmado no cenário 01: `GetBody != nil`.)
- **⚖️ Decisão de produto (D2):** corpo **não-rebobinável** deixa de ser retentado (retorna o erro original em vez de re-enviar corpo quebrado). Estritamente melhor que os dois desfechos atuais; ainda assim é mudança visível → changelog.
- **Regressão:** `TestRegression_RetryReplaysPostBody` (transport captura o corpo por tentativa; assevera `["PAYLOAD","PAYLOAD"]`) + `TestRegression_NoRetryWhenBodyNotRewindable`. GET permanece coberto por T0 (cenário 16).

### F3 · `Do()` pós-`Stop()` trava para sempre — 🟠 alto [A3, cenário 03]

- **Problema:** `waitForToken` (l.174-179) só seleciona `ctx.Done()` e `cb.tokens`; após `Stop()`, waiters e chamadas novas bloqueiam para sempre (goroutine + slot do semáforo presos).
- **Correção:**
  ```go
  select {
  case <-ctx.Done():        return ctx.Err()
  case <-cb.tokenStop:      return ErrStopped   // canal fechado desbloqueia todos os waiters
  case <-cb.tokens:         return nil
  }
  ```
  com `var ErrStopped = errors.New("circuit breaker stopped")` (sentinela **novo e exportado** — aditivo).
- **Efeito observável:** o caminho que hoje **trava** passa a retornar erro — só afeta código que já estava quebrado. Sutileza: quando `Stop()` já ocorreu mas há tokens sobrando no buffer, o `select` pode escolher qualquer case pronto; manter o comportamento "sobras ainda atendem" exige um `select` não-bloqueante em `cb.tokens` antes do `select` triplo — decidir e travar em teste (recomendado: sobras atendem, coerente com o observado hoje no cenário 03/afterstop).
- **Regressão:** `TestRegression_DoAfterStopReturnsError`, `TestRegression_StopUnblocksInflightWaiters`, `TestRegression_LeftoverTokensStillServed`.

### F4 · Métricas sem poda: leak + custo crescente — 🟠 alto [A4, cenários 04/05]

- **Problema:** slices `Time*`/`StartTime*` só recebem `append` (endpoint **e** `::root`); 5.000 reqs → `len==5000` nos dois níveis; +1,37 MB retidos/10k reqs; custo 68→640 µs/req (7,3–8,6×).
- **Correção mínima nesta fase (a estrutural fica para a Fase 2):** poda dentro de `updateMetrics` — reter apenas o necessário para a semântica atual: últimos **20** itens de `Time*` (é o que `avgLastNItens(…, 20)` usa) e `StartTime*` dentro da janela de **10 min** (maior janela de `repsRatio`), com teto de segurança absoluto (ex.: 100k entradas).
- **⚖️ Decisão de produto (D4):** os campos slice exportados (tag `json:"-"`) passam a conter só a janela útil. Consumidor que lia o histórico completo por acesso direto perde o restante — documentar; a alternativa (crescimento infinito) é o defeito.
- **Regressão:** `TestRegression_MetricsBounded` (após 10k reqs: `len(Time*) ≤ 20`, `StartTime*` só janela, contadores/médias/ratios idênticos aos de T0.4) + guarda de crescimento `TestRegression_PerRequestCostFlat` (razão bloco5/bloco1 ≤ **1,2**; hoje 7,3–8,6).

### F5 · Clamp de 1ns: busy-loop de CPU — 🟠 promovido por medição [A16, cenários 22/14]

- **Problema (medido):** `interval ≤ 0 → 1ns` (l.56-58): refiller consome **~100% de 1 core ocioso** (815× o controle); construtor gasta 13–27 s pré-enchendo bucket gigante; rate limit efetivamente anulado pelo pré-fill.
- **Correção:** piso prático no intervalo com reposição em lote:
  ```go
  const minInterval = time.Millisecond
  interval := (time.Duration(windowSeconds) * time.Second) / time.Duration(maxRequests)
  tokensPerTick := 1
  if interval < minInterval {
      tokensPerTick = int(minInterval / max(interval, 1))  // repõe em lote
      interval = minInterval
  }
  ```
  e **validação de configuração degenerada**: capacidade do bucket clampada (ex.: `min(maxRequests, 1_000_000)`) para eliminar o pré-fill de dezenas de segundos.
- **⚖️ Decisão de produto (D5):** para configs **absurdas** (`maxRequests > ~1e6·windowSeconds`), o burst máximo cai do valor nominal para o teto — comportamento atual nessas configs já é "sem rate limit + 1 core queimado", então o clamp só melhora. Configs sãs: taxa sustentada e burst **inalterados** (T0.3 protege).
- **Regressão:** `TestRegression_ExtremeRateNoBusyLoop` (construtor < 100 ms; CPU ociosa do processo < 5% de 1 core via `Getrusage` — hoje 100%).

### F6 · Backoff ignora contexto — 🟡 médio [A6, cenários 08/09]

- **Problema:** `time.Sleep(500ms)` (l.148) não observa `req.Context()`; cancelamento espera até 500 ms×tentativas (medido: cancel em 50 ms → retorno em 1,5 s), segurando o slot do semáforo.
- **Correção:**
  ```go
  select {
  case <-req.Context().Done(): return nil, req.Context().Err()
  case <-time.After(cb.backoff): // cb.backoff: campo interno, default 500ms
  }
  ```
  `backoff` vira **campo não exportado** (injetável em teste white-box — elimina os sleeps reais da suíte). Sem jitter/exponencial nesta fase (mudaria o perfil de carga em produção; ver Fase 4).
- **Efeito observável:** cancelamento retorna mais cedo — é o que o chamador já espera de um contexto.
- **Regressão:** `TestRegression_BackoffRespectsContext` (cancel a 50 ms → retorno < 150 ms).

### F7 · Exaustão de retries perde a causa — 🟡 médio [A13, cenário 20]

- **Problema:** `errors.New("request failed after retries")` (l.153) descarta o último erro; `errors.Is/As` não recuperam a causa (timeout? reset?).
- **Correção que preserva a string** (consumidores podem comparar `err.Error()`):
  ```go
  type retriesExhaustedError struct{ last error }
  func (e *retriesExhaustedError) Error() string { return "request failed after retries" } // texto idêntico
  func (e *retriesExhaustedError) Unwrap() error { return e.last }
  var ErrRetriesExhausted = &retriesExhaustedError{} // p/ errors.Is (aditivo)
  ```
- **Efeito observável:** `err.Error()` idêntico; `errors.Is/As` passam a *funcionar* onde antes retornavam `false` — ganho aditivo. T0.7 é atualizado deliberadamente neste PR (única mudança de característica, explícita).
- **Regressão:** `TestRegression_ExhaustionPreservesCause` (`errors.Is(err, ErrRetriesExhausted)` e `errors.As` recuperam a causa; texto exato preservado).

### F8 · Código morto em `isRetryable` — limpeza sem mudança de comportamento [cenários 07/24]

- **Problema:** tudo abaixo da l.188 é inalcançável (ramos `*net.OpError`, `ECONNRESET/ECONNREFUSED`, `os.ErrDeadlineExceeded`) — o 1º ramo (`net.Error`) decide sempre. O código **aparenta** retentar conexão recusada, mas não retenta.
- **⚖️ Decisão de produto (D1) — a mais importante do plano:** **manter o comportamento efetivo atual** (retry apenas para `Timeout()/Temporary()==true`) e **remover o código morto**. *Não* "ativar" o retry de `ECONNREFUSED`: (a) produção está calibrada no comportamento efetivo há toda a vida do componente; (b) retentar serviço caído é exatamente o que um circuit breaker não deve fazer. A intenção original do autor, se desejada, vira política configurável na Fase 4.
- **Nota:** a nuance do wrap (`fmt.Errorf("%w", retryable)` no transport **não** é retentado, pois `url.Error` usa type-assertion direta) fica **documentada** — "corrigir" com `errors.As` mudaria comportamento; adiar para a Fase 4 como política opt-in.
- **Regressão:** T0.2 (tabela efetiva) já trava tudo; a limpeza não pode alterá-la. `isRetryable` sai de 27,3% para ~100% de cobertura *por redução de código*, não por teste de vaidade.

## 6. Fase 2 — Desempenho e memória (v0.0.9)

A F4 estanca o leak; esta fase elimina a classe do problema. **Baselines medidos** → **alvos**:

| Métrica | Baseline (medido) | Alvo | Verificação |
|---|---|---|---|
| Custo por `Do()` (transport no-op) | 68 µs → 640 µs/req (cresce com histórico) | **< 5 µs/req, estável** (razão bloco5/bloco1 ≤ 1,1) | `TestRegression_PerRequestCostFlat` endurecido |
| Heap retido / 10k reqs | +1,37 MB | **O(1)** (< 50 KB independente de N) | teste com `runtime.ReadMemStats` pós-GC |
| Trabalho sob `metricsMu` por request | 12 passagens O(n) + (até F1) sorts | O(1) amortizado | benchmark paralelo 8 goroutines |
| Alocações por `Do()` | crescentes (appends) | ~constantes | `b.ReportAllocs` |

**Design — substituir slices crescentes por estruturas O(1)** (interno; campos exportados preservados como *views*):

1. **Médias (últimas 20):** ring buffer fixo `[20]float64` + soma corrente por família → `Mean*` em O(1).
2. **Janelas 1/5/10 min (`Ratio*`):** roda de buckets de tempo — 600 buckets de 1 s com avanço preguiçoso pelo relógio no momento do registro; contagem da janela = soma dos últimos 60/300/600 buckets (600 somas de `int64` ≈ centenas de ns, constante; otimizar para acumuladores deslizantes **só se** o benchmark mandar — simplicidade primeiro).
3. **Snapshot:** `Metrics()` monta `EndpointMetrics` novos a cada chamada; `Time*`/`StartTime*` preenchidos a partir do ring (≤ 20 itens; semântica pós-F4 mantida). Zero aliasing por construção.
4. **Lock:** manter o `metricsMu` global — com O(1) a seção crítica fica curtíssima. *Shard por host só se* o benchmark paralelo pós-mudança ainda mostrar contenção (>20% do tempo em lock) — não especular complexidade.
5. **`::root`** continua recebendo cada update (semântica preservada; custo agora O(1)×2).

Gate da fase: **todos os testes Tier 0 e Tier R passam sem alteração** (a não ser T0.4 se a *ordem* dos slices-view mudar — decidir e documentar) + alvos da tabela atingidos.

## 7. Fase 3 — Robustez e API aditiva (v0.0.10)

| # | Item | Correção (aditiva) | Evidência |
|---|---|---|---|
| R1 | `cl == nil` → panic dentro de `cl.Do` | Fallback para `http.DefaultClient` + doc de que o chamador deve prover `Timeout` (não impor timeout silencioso — mudaria chamadas longas legítimas) | [A10, 12] |
| R2 | Manager sem ciclo de vida (goroutines/tickers acumulam) | Interface opcional `IManagerLifecycle { List() []string; Remove(name string); StopAll() }` implementada por `*manager`, descoberta por type assertion; `Remove` chama `Stop()` antes do `delete` | [A7, 10] |
| R3 | Config divergente ignorada em silêncio | Interface opcional com `NewCircuitBreakerStrict(...) (ICircuitBreaker, error)`: guarda a config original e retorna erro em divergência | [A8, 11] |
| R4 | `failed > total` no cancelamento de token | **Não alterar contadores existentes** (dashboards calibrados). Adicionar campo novo `TokenWaitCancellations int64 \`json:"token_wait_cancellations,omitempty"\`` e documentar a invariante quebrada como característica da v0 | [A15, 13] |
| R5 | Semântica pós-`Stop` do manager e do breaker documentada | Godoc completo em `Stop`, `Do`, `Metrics` (hoje o doc promete "lifecycle management" inexistente) | [A7] |

## 8. Plano de testes consolidado

### 8.1 Inventário (o que entra, por quê)

Além dos Tier 0 (§4) e regressões F1–F8 (§5), fecham-se as lacunas de cobertura **que protegem funcionalidade central**:

| # | Teste | Trava | Origem |
|---|---|---|---|
| T1 | `TestSemaphore_PeakNeverExceedsMax` (gauge atômico, N=50, cap=5) + `TestSemaphore_CtxCancelWhileWaitingSlot` | O bulkhead em si (nunca fora exercido; regressão de vazamento de slot passaria verde) | [23], A11 |
| T2 | `TestManager_ConcurrentNewAndGet` (`-race`) | Instância única sob corrida | [15] |
| T3 | `TestStop_ConcurrentIdempotent` (`-race`, 32 goroutines) | `sync.Once` + `WaitGroup` | [18] |
| T4 | `TestTokenBucket_SustainedRate` (taxa média da 2ª janela em diante ≈ `maxRequests/window` ±20%) | O rate limiter em regime | [26] |
| T5 | `TestRootAggregate_SumsEndpoints` (2 endpoints → `::root` = soma) | Agregação por host | [04] |
| T6 | `TestGoroutines_NoLeakWithStop` (N breakers + `StopAll` → contagem retorna) | Ciclo de vida (com R2) | [10] |
| T7 | ✅ **APLICADO (2026-07-09)** — `internal`: porta efêmera (0) + `Start() error` síncrono + `Addr()`/`Close()`/`SetDelay`; `Listen` propaga erro; testes novos `MultipleServers` e `PortInUse`; 10 execuções verdes sob `-race` com a 8081 ocupada | CI confiável | [21], A9 |
| T8 | `BenchmarkDo_NoLimits` + `BenchmarkDo_Parallel8` no CI (job informativo, gate de razão apenas no repositório de perf) | Orçamento de desempenho | [05, 14] |

### 8.2 Estrutura

- **Tier 0 (característica)** — semântica congelada; roda sempre; nunca é editado casualmente (editar exige PR dedicado com justificativa, como no F7).
- **Tier R (regressão)** — um por bug corrigido; nasce **vermelho** contra o código antigo, verde após o fix.
- **Tier F (funcional)** — T1–T7.
- **Tier P (perf)** — T8; não bloqueia PRs comuns, bloqueia release.

Regras: `-count=1` sempre (cache mascarou variação na campanha); `-race` em tudo exceto testes de timing (T8, custo/req); **zero sleeps de sincronização** (canais/`httptest`/gauges — o backoff injetável do F6 elimina os 500 ms reais da suíte); cada teste < 2 s salvo Tier P.

### 8.3 CI (junto com T7)

`actions/setup-go@v5` com `go-version-file: go.mod` *(urgência elevada: o `go.mod` já foi atualizado para `go 1.26.5` enquanto o workflow instala `1.25` — hoje o CI depende do download automático de toolchain)*; passos: `go vet ./...` → `go mod tidy && git diff --exit-code go.mod go.sum` → `go test -race -count=1 ./...` → cobertura só de `./pkg/...`; remover a build tag inerte `!unittest` de `cmd/main.go` (manter só o filtro do coverpkg); `codecov-action@v4+`.

### 8.4 O que **não** testar (deliberado)

- **Ramos mortos de `isRetryable`** — remover (F8), não cobrir; cobrir código morto é cimentar o defeito.
- **Formatação/print do FakeServer e `cmd/main.go`** — harness de demonstração, não produto.
- **Metas de cobertura por linha** — nada de testes para "subir o número"; a cobertura de `pkg` sobe naturalmente de 87,3% para >95% como efeito colateral dos tiers acima.
- **Perfil fino do refiller (ticks exatos por segundo)** — flaky por natureza; T4/T0.3 (taxa e burst agregados, com tolerância) já travam o comportamento útil.
- **Combinações cartesianas de parâmetros do construtor** — só os equivalence classes já mapeados (limitado, ilimitado, degenerado).

## 9. Fase 4 — Evolução opcional (v1.1.0, decisão de negócio)

Somente após v1.0.0, tudo **opt-in e inerte por default** (comportamento default idêntico byte a byte, provado pelos Tier 0):

1. **Estados closed/open/half-open + fast-fail** — alimentados pelas métricas já coletadas; novo construtor/opções; estado via interface opcional `StateReporter` + `ErrCircuitOpen` exportado. Motivação medida: hoje a 11ª chamada após 10 falhas ainda atinge o transport [25].
2. **Política de erro configurável** — tratar 5xx como falha (hoje: sucesso [06]); classificação com `errors.As` (corrige a nuance do wrap [24]); retry de `ECONNREFUSED` se alguém realmente quiser (default: não).
3. **Backoff exponencial com jitter** — default permanece 500 ms fixo.
4. **Token bucket lazy** (contador atômico + refill calculado on-demand): elimina a goroutine por breaker e o ticker por completo; avaliar contra os testes T0.3/T4 — só entra se ficar indistinguível em semântica.

## 10. Riscos e mitigação

| Risco | Prob. | Mitigação |
|---|---|---|
| Consumidor dependia dos slices `Time*` completos (F4/D4) | Baixa | Campos são `json:"-"` (não serializam); changelog destacado; janela mantém os dados recentes |
| Consumidor compara string do erro de exaustão | Média | F7 preserva `Error()` byte a byte (tipo com `Unwrap`, texto idêntico) |
| Consumidor usa literal não-nomeado de `EndpointMetrics` (R4 adiciona campo) | Baixa | `go vet` acusa; documentar no changelog |
| Regressão de semântica no redesign O(1) da Fase 2 | Média | Tier 0 completo **antes**; F4 (poda simples) já entrega o estanque na v0.0.8, então a Fase 2 pode esperar quantos ciclos forem necessários |
| Mudança de timing do backoff ctx-aware (F6) altera perfil de carga | Baixa | Duração do backoff não muda (500 ms); só o cancelamento retorna antes |
| `Stop()` novo desbloqueando waiters muda shutdown de quem "dependia" do hang (F3) | Muito baixa | Hang nunca é dependência legítima; sentinela `ErrStopped` permite tratamento explícito |

## 11. Ordem de execução e esforço (1 dev sênior, PRs revisáveis)

| Ordem | Item | Esforço | Entrega |
|---|---|---|---|
| 1 | Fase 0 (T0.1–T0.7 + baselines) | 1–2 d | PR único de testes, verde contra v0.0.7 |
| 2 | F1 (race) → F3 (hang) → F2 (body) | 1,5–2 d | 3 PRs; os 2 críticos + o hang primeiro |
| 3 | F4 (poda) + F6 (backoff) + F5 (clamp) + F7 (erro) + F8 (dead code) | 2 d | 5 PRs pequenos → **tag v0.0.8** |
| 4 | T1–T7 + CI novo | 1,5 d | 2 PRs (pkg + internal/CI) |
| 5 | Fase 2 (métricas O(1)) | 2–4 d | 1–2 PRs com benchmarks antes/depois → **tag v0.0.9** |
| 6 | Fase 3 (R1–R5) | 2 d | 3 PRs → **tag v0.0.10** |
| 7 | Fase 5: README (exemplo compilável [27]), `manger.go`→`manager.go`, godoc, pacote raiz com aliases (opcional), CHANGELOG retroativo | 1–2 d | → **tag v1.0.0** |

**Total: ~11–15 dias úteis.** Caminho crítico de segurança: itens 1–3 (uma semana) eliminam os dois críticos e os dois altos com rede de proteção completa; tudo depois é qualidade incremental sobre um sistema já seguro.

## 12. Critérios de aceite globais (Definition of Done por release)

- `go build ./... && go vet ./...` limpos; `go test -race -count=1 ./...` verde e **sem flaky** (10 execuções consecutivas no CI).
- Tier 0 intacto (ou alterado apenas por PR dedicado com decisão de produto registrada).
- Benchmarks dentro do alvo da fase; números publicados no PR.
- CHANGELOG lista cada mudança de comportamento observável com o marcador ⚖️ das decisões D1–D5.
- Nenhuma assinatura pública alterada — verificado por `go test` de um módulo-sentinela que instancia todo o contrato congelado (compila = contrato intacto).

---

*Plano derivado de 27 cenários empíricos re-verificados em duas rodadas independentes (Opus e Fable) — ver [`CB-TESTES.md`](CB-TESTES.md). Os módulos de reprodução em scratchpad (`scn-*`, `val-*`) são o ponto de partida dos testes da Fase 0/Tier R.*

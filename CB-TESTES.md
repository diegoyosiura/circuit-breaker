# CB-TESTES.md — Campanha de Validação: 20 Cenários

> Relatório dos testes que validam empiricamente os achados de [`CB.md`](CB.md). Cada cenário foi **executado** (teste Go real, saída capturada), **validado de forma adversarial** por um segundo agente independente (releitura + re-execução), **sumarizado** por faixa de severidade e submetido a uma **auditoria de processo** final. Nenhum arquivo do repositório foi modificado — os testes rodam num módulo externo que importa o pacote público via `replace`.

## 1. Sumário executivo

Dos 20 cenários: **13 reproduziram o defeito hipotetizado**, **5 contrastes/casos de borda confirmaram o comportamento esperado**, e **2 não reproduziram** a hipótese na forma proposta — sendo que ambos revelaram nuances importantes (detalhadas abaixo). O validador adversarial concordou plenamente com **18** cenários e emitiu **2 vereditos PARCIAIS** (nenhuma refutação total).

| Resultado | Qtd | Cenários |
|---|---|---|
| 🔴 Bug **reproduzido** | 13 | 01, 02, 03, 04, 05, 06, 08, 09, 10, 11, 12, 13, 20 |
| 🟢 Contraste/edge **OK** | 5 | 15, 16, 17, 18, 19 |
| ⚪ **Não reproduzido** (com nuance) | 2 | 07, 14 |

*As contagens acima seguem o rótulo do **executor**; no veredito líquido da §4, o cenário 14 figura como "CONFIRMADO (com precisão)" — o desfecho central (rate limit anulado) é real.*

**Três refinamentos que a validação adversarial trouxe** — e que corrigem ou precisam o `CB.md`:

1. **Cenário 07 → corrige `CB.md` §6.5 e A5.** O "retry storm" **não** ocorre para `ECONNREFUSED`/`ECONNRESET`. Em `isRetryable`, o primeiro ramo testa `net.Error` e retorna `Temporary() || Timeout()`, ambos `false` para conexão recusada — então o `Do` **não retenta** e o downstream recebe **1** chamada. Os ramos `*net.OpError` (linhas 190–193) e `errors.Is(err, syscall.ECONNRESET/ECONNREFUSED)` (linhas 195–197) são **código morto** (o `net.Error` casa antes e curto-circuita). A amplificação 6× só vale para erros com `Timeout()/Temporary()==true`. Ou seja: a intenção declarada de retentar serviço caído está **silenciosamente anulada** — um bug de correção distinto, e a premissa de A5 sobre "conexão recusada" precisa ser corrigida.
2. **Cenário 01 → precisa A2.** O bug-raiz (retry não rebobina o corpo, ignora `req.GetBody`) é **real e confirmado**. Mas o "sucesso silencioso 200" é artefato de transport falso: com um `http.Transport` **real** e corpo de comprimento conhecido (`strings.Reader`/`bytes.Reader`/`Buffer`, o caso dominante), o retry **aborta ruidosamente** com `"http: ContentLength=11 with Body length 0"` → `(nil, erro)`. O sucesso silencioso com corpo vazio só ocorre para corpo **chunked** (comprimento desconhecido).
3. **Cenário 14 → precisa A16.** O rate limit **é** anulado (`waitForToken` nunca bloqueia com `maxRequests=2e9`), confirmando o desfecho central. Mas o mecanismo não é o "refill a 1ns": é o **bucket pré-preenchido** com 2e9 tokens (o construtor leva **~26s** só para enchê-lo). A vazão fica tetada em **~23–24k req/s** pelo custo O(n·log n) das métricas — idêntica a um breaker sem limiter. O *busy-loop* de CPU do ticker de 1ns foi **medido na rodada Fable** (cenário 22, §12): a goroutine do refiller consome **~100% de 1 core** com o breaker ocioso (815× o controle), cessando com `Stop()`.

**Confiança geral da auditoria de processo: alta.** Todos os bugs de severidade crítica e alta do `CB.md` que entraram em cenário foram reproduzidos com evidência empírica; as ressalvas concentram-se em precisão de rótulo (07, 14) e em fraquezas metodológicas dos executores que os validadores corrigiram (asserções só-log, artefatos de mock, falta de grupo de controle).

## 2. Metodologia

Pipeline multiagente determinístico (44 agentes, 0 erros):

```
20 cenários ─┬─▶ [Executor]  escreve go.mod+teste em scn-NN/, roda go test, captura saída real
             └─▶ [Validador] adversarial: relê o teste, procura falso-positivo, RE-EXECUTA (val-NN/)
                              ▼  (pipeline: cada validação começa assim que sua execução termina)
        3 × [Sumarizador]  consolida por faixa de severidade (críticos-altos / médios-baixos / contraste-edge)
                              ▼
        1 × [Revisor de processo]  audita metodologia, evidências, divergências, lacunas e confiança
```

- **Isolamento:** cada agente trabalhou em seu próprio diretório de scratch (`scn-NN/`, `val-NN/`) com um módulo Go independente e `replace` para o repositório. O código de produção foi apenas lido e importado, nunca alterado.
- **Adversarialidade:** o validador não recebeu instrução de concordar — foi instruído a **refutar**. Os 20 validadores re-executaram (`validator_reran=true` em todos), e vários adicionaram *grupos de controle* e *controles positivos do detector* que os executores haviam omitido.
- **`-race`** foi usado onde a hipótese envolvia concorrência (02, 08, 09, 15, 17, 18, 20; validadores também o usaram em 12 e 16). Dois contrastes incluem **controle positivo do detector**: um `x++` deliberadamente racy (15) e um `Stop()` sem `sync.Once` que gerou 31 panics de double-close (18) — provando que as passagens limpas são significativas, não tautológicas.

## 3. Ambiente

| | |
|---|---|
| Toolchain | `go1.26.0 linux/amd64` (repo pedia `go 1.25` durante as campanhas; atualizado para `go 1.26.5` em 2026-07-09 — módulos de repro revalidados no toolchain novo) |
| Máquina | 8 núcleos (cap de concorrência do workflow = 6) |
| Import | módulo externo com `replace github.com/diegoyosiura/circuit-breaker => <repo>`, alias `circuitbreaker ".../pkg"` |
| Alvo | HEAD `5e9ffe8` (== tag `v0.0.7`) |
| Custo | 44 agentes, ~1,43 M tokens de subagente, ~29 min de relógio |

## 4. Resultados — visão geral

Legenda de veredito líquido: **CONFIRMADO** = bug reproduzido e validado; **CONFIRMADO (com precisão)** = bug real mas o enunciado foi refinado; **OK** = contraste/edge comportou-se como esperado; **REFUTADO (revela outro bug)** = a hipótese específica caiu, mas apareceu um defeito relacionado.

| # | Sev | CB.md | Cenário | Exec | Valid. | Veredito líquido |
|---|-----|-------|---------|------|--------|------------------|
| 01 | 🔴 | A2 | Retry de POST reenvia corpo vazio | REPRODUZIDO | PARCIAL | **CONFIRMADO (com precisão)**: corpo perdido sempre; sucesso silencioso só em chunked |
| 02 | 🔴 | A1 | Data race no snapshot de `Metrics()` | REPRODUZIDO | CONFIRMA | **CONFIRMADO** (`-race` dispara em `repsRatio`) |
| 03 | 🟠 | A3 | `Do()` após `Stop()` bloqueia | REPRODUZIDO | CONFIRMA | **CONFIRMADO** (bloqueio permanente por análise de stack) |
| 04 | 🟠 | A4 | Métricas crescem sem poda (leak) | REPRODUZIDO | CONFIRMA | **CONFIRMADO** (len=N; +1,35 MB retidos / 10k reqs) |
| 05 | 🟠 | A4 | Custo por request O(n) sob lock | REPRODUZIDO | CONFIRMA | **CONFIRMADO** (72→608 µs/req, 8,4×; R²≈0,999) |
| 06 | 🟠 | A5 | HTTP 5xx contado como sucesso | REPRODUZIDO | CONFIRMA | **CONFIRMADO** (Success=1, Failed=0 em 500) |
| 07 | 🟠 | A5 | Retry storm em `ECONNREFUSED` | NAO_REPRODUZIDO | CONFIRMA | **REFUTADO (revela outro bug)**: ECONNREFUSED nunca retentado; linha 195 é código morto |
| 08 | 🟡 | A6 | Backoff ignora cancelamento de ctx | REPRODUZIDO | CONFIRMA | **CONFIRMADO** (cancel em 50ms, retorno só após sleeps) |
| 09 | 🟡 | A6 | Semáforo retido nos retries/sleeps | REPRODUZIDO | CONFIRMA | **CONFIRMADO** (2ª chamada espera A liberar) |
| 10 | 🟡 | A7 | Goroutine de ticker vaza sem `Stop` | REPRODUZIDO | CONFIRMA | **CONFIRMADO** (Δ≈200 goroutines) |
| 11 | 🟡 | A8 | Manager ignora params divergentes | REPRODUZIDO | CONFIRMA | **CONFIRMADO** (mesma instância, config A) |
| 12 | 🟡 | A10 | Client sem timeout trava o slot | REPRODUZIDO | CONFIRMA | **CONFIRMADO** (2ª chamada nunca entra) |
| 13 | 🔵 | A15 | Cancelamento infla `failed>total` | REPRODUZIDO | CONFIRMA | **CONFIRMADO** (invariante quebrada) |
| 14 | 🔵 | A16 | Clamp de 1ns anula rate limit | NAO_REPRODUZIDO | PARCIAL | **CONFIRMADO (com precisão)**: sem rate limit real, mas via pré-preenchimento, não refill; busy-loop medido depois (§12, cenário 22) |
| 15 | 🟢 | A7/ok | Manager/`Stop` concorrentes seguros | CONFIRMADO_OK | CONFIRMA | **OK** (sem race; controle positivo dispara) |
| 16 | 🟢 | A2/ok | GET re-tenta corretamente | CONFIRMADO_OK | CONFIRMA | **OK** (sucesso após 3 tentativas) |
| 17 | 🟢 | ilimitado | Modo sem limites (0,0) | CONFIRMADO_OK | CONFIRMA | **OK** (sem goroutine extra; `Stop` no-op) |
| 18 | 🟢 | idemp. | `Stop()` concorrente/duplo | CONFIRMADO_OK | CONFIRMA | **OK** (sem panic; controle: 31 panics sem `Once`) |
| 19 | 🟢 | A5/nr | Erro não-retryable retorna já | CONFIRMADO_OK | CONFIRMA | **OK** (1 chamada, sem sleep, erro preservado) |
| 20 | 🟡 | A13 | Exaustão perde a causa do erro | REPRODUZIDO | CONFIRMA | **CONFIRMADO** (`errors.Is`==false; RetryCount=2) |

---

## 5. Resultados detalhados — Críticos e Altos

Esta faixa reúne 7 cenários — 2 críticos (retry corrompendo corpo de POST; data race em `Metrics()`) e 5 altos (deadlock pós-`Stop()`, leak de métricas, custo superlinear por request, 5xx contado como sucesso e o suposto retry storm de ECONNREFUSED). Todos foram re-executados de forma independente pelo validador. Cinco (02–06) se sustentam integralmente; um (01) foi rebaixado para PARCIAL por generalização excessiva; um (07) teve a hipótese refutada e a refutação confirmada.

### Cenário 01 — Retry de POST reenvia corpo vazio e reporta sucesso

**Referência:** finding A2 (severidade: crítico).

**Hipótese:** Após um erro transitório na 1ª tentativa de um POST com corpo, o retry do circuit breaker reenvia o corpo VAZIO ao servidor e mesmo assim `Do()` retorna `(resp!=nil, err=nil)`, reportando sucesso.

**Método:** Módulo Go externo importando o pacote público via `replace` local. Um `http.RoundTripper` fake lê o corpo da requisição em toda tentativa (registrando o que o servidor veria), retorna na 1ª chamada um erro que implementa `net.Error` com `Temporary()==true` (forçando retry) e responde 200 nas seguintes. Um POST com `strings.NewReader("PAYLOAD-123")` é enviado por `cb.Do` com `maxRetries=2`, comparando-se os corpos vistos e o `(resp,err)` final.

**Comando:** `go test -run TestScn01_RetryPostBody -v -timeout 60s`

**Saída observada:**
```
=== RUN   TestScn01_RetryPostBody
    scn01_test.go:101: numero de chamadas ao transport (tentativas): 2
    scn01_test.go:102: corpos vistos pelo servidor por tentativa: []string{"PAYLOAD-123", ""}
    scn01_test.go:104: resposta final: StatusCode=200
    scn01_test.go:108: erro final: <nil>
    scn01_test.go:115: check bodies==[PAYLOAD-123, ""]: true
    scn01_test.go:116: check err==nil: true
    scn01_test.go:117: check resp!=nil && 200: true
    scn01_test.go:120: VERDICT=REPRODUZIDO: retry reenviou corpo VAZIO e Do() reportou sucesso (resp!=nil, err=nil)
--- PASS: TestScn01_RetryPostBody (0.50s)
PASS
ok  	scn01	0.503s
```
Métricas: 2 tentativas ao transport; corpos vistos = `["PAYLOAD-123", ""]`; payload original de 11 bytes vira 0 bytes no retry; resposta final `StatusCode=200`; `err=nil`; duração 0,50s (= backoff de 500ms entre tentativas).

**Veredito executor:** REPRODUZIDO. A causa-raiz está em `pkg/circuitbreaker.go`: no laço de retry o `Do` faz `newReq := req.Clone(req.Context())` e chama `cl.Do(newReq)`, mas `Clone` copia `Body` por REFERÊNCIA (cópia rasa do `io.ReadCloser`) e o código nunca usa `req.GetBody` para rebobinar o corpo. A 1ª tentativa consome o `strings.Reader` até EOF; na 2ª o mesmo reader (já em EOF) é reenviado e o servidor recebe corpo vazio. Como `cl.Do` só retorna `err!=nil` em falha de rede, `Do()` devolve `(resp=200, err=nil)`, mascarando a corrupção — perda silenciosa de dados.

**Veredito validador (com ressalvas):** PARCIAL. Re-executou o teste (idêntico, PASS) e escreveu 4 testes independentes em `val-01`. O BUG-RAIZ é real e confirmado: no laço de retry o CB faz `req.Clone(ctx)` e `cl.Do(newReq)` sem NUNCA usar `req.GetBody` — o clone compartilha o mesmo `io.Reader`, e após a 1ª tentativa a 2ª leitura retorna `""`, embora `req.GetBody` existisse e `ContentLength=11` (ou seja, o CB teria como corrigir trivialmente). Ressalva relevante: a parte "`Do()` retorna `(resp=200, err=nil)` — perda silenciosa" é FALSO POSITIVO da metodologia, porque o `fakeTransport` não valida `Content-Length` e sempre devolve 200. Com `http.Transport` real + `httptest.Server` real usando a MESMA requisição (`strings.NewReader`, `ContentLength=11` conhecido — caso dominante de POST/PUT), o retry NÃO tem sucesso silencioso: aborta com `"http: ContentLength=11 with Body length 0"` e `Do()` retorna `(nil, erro)`. O sucesso silencioso 200 com corpo vazio só se materializa para corpos de comprimento DESCONHECIDO (chunked), confirmado independentemente pelo validador (Test D: servidor real + chunked → recebeu `""` e `Do()` retornou `(200, nil)`). Por isso não REFUTA, mas rebaixa: a interpretação "qualquer glitch → POST/PUT reenviado sem corpo com sucesso reportado" é generalização excessiva.

**Conclusão:** O defeito de não rebobinar o corpo no retry (ignorando `req.GetBody`) é real e confirmado; porém a perda silenciosa com `200/err=nil` só ocorre para bodies chunked — para bodies de comprimento conhecido (o caso do próprio teste) o retry vira erro duro, não sucesso mascarado.

### Cenário 02 — Data race: snapshot de Metrics() vs sort in-place de repsRatio

**Referência:** finding A1 (severidade: crítico).

**Hipótese:** Um leitor iterando o slice `StartTimeRequests` obtido de um snapshot de `Metrics()`, concorrente com `Do()` em loop no mesmo host/endpoint, dispara DATA RACE porque `repsRatio()` ordena o array de timestamps in-place (`slices.SortFunc`) enquanto o leitor o percorre — o snapshot copia os structs por valor, mas os cabeçalhos de slice continuam apontando para o MESMO array vivo.

**Método:** Módulo externo (`scn02`) via `replace` local. Uma goroutine bombardeia `cb.Do()` contra um `httptest.Server` que responde 200 (endpoint fixo `/fixed`), sem semáforo/token/retry (`0,0,0,0`) para maximizar `recordAttempt→repsRatio→SortFunc`; outra goroutine chama `snap:=cb.Metrics()` em loop e itera `snap[host][ep].StartTimeRequests` fora do lock. Experimento de ~300ms sob `go test -race`.

**Comando:** `go test -race -run TestMetricsSnapshotSortRace -timeout 60s -v` (dir: `scratchpad/scn-02`; go1.26.0 linux/amd64)

**Saída observada:**
```
=== RUN   TestMetricsSnapshotSortRace
==================
WARNING: DATA RACE
Write at 0x00c0001aa090 by goroutine 11:
  slices.insertionSortCmpFunc[...]()
  slices.pdqsortCmpFunc[...]()
  slices.SortFunc[...]()
      /usr/local/go/src/slices/sort.go:32 +0xca
  github.com/diegoyosiura/circuit-breaker/pkg.repsRatio()
      /home/diego/projetos/sagace/helpers/circuit-breaker/pkg/circuitbreaker.go:321 +0x7c
  ...recordAttempt.func3() circuitbreaker.go:230
  ...updateMetrics() circuitbreaker.go:296
  ...Do() circuitbreaker.go:128
  scn02.TestMetricsSnapshotSortRace.func2() .../scn-02/race_test.go:49

Previous read at 0x00c0001aa090 by goroutine 12:
  scn02.TestMetricsSnapshotSortRace.func3() .../scn-02/race_test.go:72
==================
[dezenas de blocos WARNING: DATA RACE adicionais]
    testing.go:1712: race detected during execution of test
--- FAIL: TestMetricsSnapshotSortRace (0.30s)
FAIL
scn02	0.314s
(com GORACE=halt_on_error=1: "exit status 66")
```
Métricas: experimento de 300ms; race detectado quase de imediato; endereços em disputa `0x00c0001aa090` e `0x00c0001aa0f0` (elementos `time.Time` de `StartTimeRequests`); `exit status 1` no modo normal, `exit status 66` sob `GORACE=halt_on_error=1`.

**Veredito executor:** REPRODUZIDO. O `-race` apontou exatamente o caminho previsto: escrita em `slices.SortFunc` dentro de `circuitbreaker.go:321` (`repsRatio`), via `recordAttempt`(230)→`updateMetrics`→`Do`; leitura concorrente em `race_test.go:72`. O write lock (`metricsMu`) protege o `SortFunc`, mas não o leitor, pois `Metrics()` já devolveu o snapshot cujos slices apontam para os arrays vivos. Nota de honestidade: o `-race` moderno não imprime "Found N data race(s)"; imprime "race detected during execution of test" e encerra com `exit status 1` (66 com `halt_on_error=1`).

**Veredito validador:** CONFIRMA (sem ressalva que afete o veredito). Rerun idêntico ao relatado, com stacks batendo na hipótese. Variação distinta em `val-02` (sem `httptest.Server`, alvo em porta fechada exercitando `recordFailure`, leitor percorrendo `StartTimeFailedRequests` e o agregado `::root`, 3 leitores) reproduz a mesma DATA RACE, `exit status 1`, 59 blocos WARNING por execução, disparando tanto em `circuitbreaker.go:296` quanto em `:297` (`::root`). Mecanismo verificado no código: `Metrics()` (linha 215) faz `endpointCopy[endpoint] = *metrics` (cópia por valor, cabeçalhos de slice apontando ao mesmo array); `repsRatio` (321) faz `SortFunc` in-place sob `metricsMu.Lock()`, mas o leitor não segura lock — happens-before violado. Confirmou `exit status 1`/`66` e a nota de honestidade sobre a frase literal. Ressalva menor (sem afetar veredito): a corrida não se limita a `StartTimeRequests` — todo slice ordenado in-place (`StartTime{Successful,Failed,Retry}Requests`) e o `::root` são igualmente racy; o executor lê só dois, o que já basta.

**Conclusão:** Data race genuína e não-artefato do detector, com happens-before violado entre o `SortFunc` in-place de `repsRatio` (sob lock) e o leitor do snapshot (sem lock); confirmada de forma independente inclusive no caminho de falha e no agregado `::root`.

### Cenário 03 — Do() após Stop() bloqueia para sempre sem deadline

**Referência:** finding A3 (severidade: alto).

**Hipótese:** Após consumir o único token e chamar `Stop()`, um segundo `Do()` com `context.Background()` nunca retorna — bloqueio permanente, pois `waitForToken` depende de um token que jamais chegará.

**Método:** Módulo externo via `replace`. `NewCircuitBreaker("scn03",0,1,60,0)`: 1 token pré-carregado, intervalo de 60s, goroutine geradora ativa. O 1º `Do` consome o token (sucesso); chama `cb.Stop()` (encerra a geradora); dispara o 2º `Do` com `context.Background()` numa goroutine e observa com `select` entre `done` e `time.After(2s)`.

**Comando:** `cd .../scratchpad/scn-03 && go mod tidy && go test -v -run TestScn03_DoAfterStopBlocksForever -timeout 60s ./...`

**Saída observada:**
```
=== RUN   TestScn03_DoAfterStopBlocksForever
    scn03_test.go:54: 1o Do OK: status=200, dur=483.02µs
    scn03_test.go:59: cb.Stop() chamado
    scn03_test.go:92: RESULTADO=REPRODUZIDO: 2o Do NAO retornou em 2s (segue bloqueado em waitForToken)
--- PASS: TestScn03_DoAfterStopBlocksForever (2.00s)
PASS
ok  	scn03	2.005s
```
Métricas: 1º `Do` HTTP 200 em 483,02µs (token consumido); 2º `Do` sem retorno após 2,0s; duração total 2,005s. Config `maxConcurrent=0, maxRequests=1, windowSeconds=60 (tokenInterval=60s), maxRetries=0`.

**Veredito executor:** REPRODUZIDO. Após `cb.Stop()` fechar `tokenStop`, a geradora encerra e nenhum token novo é produzido. O 2º `Do` entra em `waitForToken`, cujo `select` observa apenas `ctx.Done()` e `cb.tokens`; com `context.Background()` (nunca cancela) e bucket vazio/não reabastecido, fica bloqueado indefinidamente. Deadlock de vida real: nem timeout nem o próprio `Stop` desbloqueiam. Causa-raiz: `waitForToken` não observa o canal de parada (`tokenStop`) nem impõe deadline próprio.

**Veredito validador:** CONFIRMA, com prova mais forte. Análise do código: com `maxConcurrent=0` o semáforo é `nil`, então o 2º `Do` cai direto em `waitForToken` (linhas 174-179); `Stop()` (158-167) fecha `tokenStop` e faz `Wait`, encerrando a geradora (80-97); o `select` observa somente `ctx.Done()` e `cb.tokens`, e como `Stop` fecha apenas `tokenStop` (não `tokens`), o receive bloqueia para sempre. Stack trace de runtime confirma a goroutine do 2º `Do` parada em `[select]` na linha 174 (chamada da linha 123), eliminando hipóteses alternativas (não está no semáforo nem em I/O). Ressalvas (não invalidam a conclusão): (1) o teste do executor sempre resulta em PASS — ambos os ramos do `select` usam só `t.Logf`, então o "PASS" não asserta nada e o veredito depende de ler a linha de log; (2) esperar 2s prova ≥2s sem retorno, não literalmente "para sempre". O validador corrigiu ambos em `val-03` (falha de verdade se o `Do` retornar; capturou o stack trace), o que somado à ausência de sender e ao canal nunca fechado estabelece a permanência.

**Conclusão:** Deadlock permanente confirmado por runtime: pós-`Stop()` não há sender para `cb.tokens` nem o canal é fechado, e `waitForToken` não observa `tokenStop` nem impõe deadline — a chamada nunca retorna.

### Cenário 04 — Métricas crescem sem poda (leak de memória)

**Referência:** finding A4 (severidade: alto).

**Hipótese:** Após N requests bem-sucedidas, `len(StartTimeRequests)==N` no endpoint e nada é podado; o agregado `::root` (por host) também acumula até N. Confirma-se leak de memória sem poda.

**Método:** Módulo externo (`scn04`) via `replace`. CB no caminho puro (`maxConcurrent=0, maxRequests=0` → sem semáforo e sem token bucket) executa 5000 `Do()` contra um `RoundTripper` no-op que sempre devolve 200 sem erro de rede (`recordAttempt`+`recordSuccess`, sem retries). Ao final lê `cb.Metrics()` e mede `len(...[host]["/x"].StartTimeRequests)` e `len(...[host]["::root"].StartTimeRequests)`.

**Comando:** `cd .../scratchpad/scn-04 && go test -run TestScn04_MetricsLeakSemPoda -v -timeout 60s`

**Saída observada:**
```
=== RUN   TestScn04_MetricsLeakSemPoda
    scn04_test.go:88: elapsed=775.890708ms  N=5000
    scn04_test.go:89: endpoint[/x] TotalRequests=5000 SuccessfulRequests=5000 len(StartTimeRequests)=5000 len(TimeRequests)=5000 len(StartTimeSuccessfulRequests)=5000
    scn04_test.go:91: ::root     TotalRequests=5000 SuccessfulRequests=5000 len(StartTimeRequests)=5000 len(TimeRequests)=5000 len(StartTimeSuccessfulRequests)=5000
    scn04_test.go:95: VERDICT=REPRODUZIDO: endpoint len=5000 == N e ::root len=5000 == N (nada foi podado; ::root tambem acumula)
--- PASS: TestScn04_MetricsLeakSemPoda (0.78s)
PASS
ok  	scn04	0.779s
```
Métricas: N=5000; endpoint `/x` e `::root` ambos com `TotalRequests=5000`, `SuccessfulRequests=5000`, `len(StartTimeRequests)=5000`, `len(TimeRequests)=5000`, `len(StartTimeSuccessfulRequests)=5000`; `elapsed=775,89ms`.

**Veredito executor:** REPRODUZIDO. Cada `Do()` bem-sucedida faz um `append` em `StartTimeRequests` (via `recordAttempt`) e um em `StartTimeSuccessfulRequests` (via `recordSuccess`), sem poda/janela deslizante — `avgLastNItens(...,20)` só serve ao cálculo, mas os slices subjacentes crescem indefinidamente. `updateMetrics` aplica o mesmo update no agregado `::root`, que também cresce a 5000. Confirma-se o leak, duplicado pelo agregado por host. Observação: custo O(N²·logN) porque `repsRatio` ordena in-place o slice inteiro a cada record.

**Veredito validador:** CONFIRMA. Verificado de três formas: (1) rerun idêntico (endpoint 5000, `::root` 5000, PASS, ~0,77s); (2) leitura do fonte + grep — os slices sofrem SOMENTE `append`, sem reslice/trim/cap/poda; `Metrics()` copia structs por valor mas os campos slice apontam aos arrays vivos; `updateMetrics` (296-297) atualiza endpoint e `::root`; (3) testes próprios em `val-04`: para N=100/1000/5000, `len==N` exato (crescimento linear sem cap); dois endpoints `/a`(300)+`/b`(700) dão `::root`=1000 = a SOMA (agregado duplica o volume); medição de heap: após `runtime.GC()`, `HeapAlloc` cresceu +1,35MB retido para 10000 requests, batendo com ~128 B/req. Ressalva menor: o executor mediu só o COMPRIMENTO dos slices (prova "sem poda"), não a memória real — lacuna fechada pelo teste de heap; o aparte O(N²·logN) foi comprovado empiricamente (o N=50000 do próprio validador estourou timeout em `repsRatio→slices.SortFunc`, deslize do validador, não do executor). Nenhum falso positivo no trabalho do executor.

**Conclusão:** Leak de memória não-limitado confirmado — os slices de métricas só sofrem `append` e nunca são podados, e o agregado `::root` duplica todo o volume por host; a retenção real de heap foi medida (+1,35MB para 10k requests).

### Cenário 05 — Custo por request cresce (O(n) sob lock global)

**Referência:** finding A4 (severidade: alto).

**Hipótese:** O tempo médio por `Do()` cresce de forma superlinear com o histórico acumulado: o custo por request não é constante, mas aumenta à medida que o slice de timestamps do endpoint (e do `::root`) cresce, porque cada `Do()` reordena esse slice inteiro sob o lock global.

**Método:** Módulo externo (replace), sem `-race`, single-thread. CB sem rate-limit (`maxRequests=0`) e sem semáforo (`maxConcurrent=0`), transport no-op (200/`err==nil`), executa 5 blocos de 2000 `Do()` no mesmo endpoint (`http://example.local/api/v1/resource`), de modo que o histórico só cresce. Mede-se o tempo de cada bloco e calcula-se µs/req; verdict REPRODUZIDO se o último bloco ≥ 2× o primeiro.

**Comando:** `cd .../scratchpad/scn-05 && go test -v -count=1 -run TestCustoPorRequestCresce -timeout 60s ./...`

**Saída observada:**
```
bloco 0: 2000 Do() em 145.225838ms => 72.61 us/req (historico apos bloco ~2000)
bloco 1: 2000 Do() em 420.486815ms => 210.24 us/req (historico apos bloco ~4000)
bloco 2: 2000 Do() em 643.166422ms => 321.58 us/req (historico apos bloco ~6000)
bloco 3: 2000 Do() em 929.156818ms => 464.58 us/req (historico apos bloco ~8000)
bloco 4: 2000 Do() em 1.215480837s => 607.74 us/req (historico apos bloco ~10000)
RESUMO us/req por bloco: [72.61, 210.24, 321.58, 464.58, 607.74]
primeiro=72.61 us/req  ultimo=607.74 us/req  razao_ultimo/primeiro=8.37x
endpoint TotalRequests=10000 SuccessfulRequests=10000 len(StartTimeRequests)=10000
::root TotalRequests=10000 SuccessfulRequests=10000
VERDICT=REPRODUZIDO: ultimo bloco >= 2x o primeiro (8.37x)
--- PASS: TestCustoPorRequestCresce (3.35s)
```
Métricas: Run 1 µs/req = `[72.61, 210.24, 321.58, 464.58, 607.74]`, razão 8,37×; tempos por bloco 145ms/420ms/643ms/929ms/1,215s; total 3,35s. Run 2 (confirmação): `[66.48, 191.66, 334.66, 453.23, 571.75]`, razão 8,60×. Ambas crescem monotonicamente e ultrapassam largamente o limiar de 2×.

**Veredito executor:** REPRODUZIDO. Custo por request marcadamente superlinear: de ~72,61 µs/req (histórico ~0-2000) a ~607,74 µs/req (~8000-10000), razão 8,37× (8,60× na 2ª execução). Crescimento monotônico e aproximadamente linear no nº de itens → custo total O(n²). Causa: cada `Do()` chama `recordAttempt`+`recordSuccess`; cada um invoca `repsRatio` 3 vezes (janelas 1/5/10 min) sobre o slice `StartTime*` do endpoint E do `::root`; `repsRatio` (linha ~321) faz `slices.SortFunc` IN-PLACE sobre o array inteiro a cada chamada, tudo sob `metricsMu`. O `::root` duplica o custo por host.

**Veredito validador:** CONFIRMA. Reproduziu (razão 7,33× aqui, vs 8,37×/8,60× reportado; crescimento monotônico) e confirmou o mecanismo no código: cada `Do` no sucesso roda `recordAttempt`+`recordSuccess`, cada `updateMetrics` roda no endpoint E no `::root`, cada record chama `repsRatio` 3×; `repsRatio` faz `SortFunc` in-place MAIS uma varredura O(n) que percorre TODO o slice (o cutoff está minutos no passado e o teste roda em ~3s) — 12 passagens Θ(n) por `Do()` sob `metricsMu`, custo/req Θ(n), total O(n²). Teste-controle em `val-05` para matar confundidores (deriva térmica, escalonamento, GC): regime ACUMULA sobe de 34,56 para 647,21 µs/req, estritamente monotônico, ajuste linear R²=0,9992; regime CONTROLE (CB novo por bloco, histórico ~0-1000) fica flat em ~34-37 µs/req — provando que a desaceleração vem do histórico acumulado. Ressalvas (não-fatais): faltava grupo de controle (suprido); teste single-thread prova o custo superlinear por request mas não exercita diretamente a contenção do lock global (inferida do código, só pioraria em produção); a atribuição ênfase-no-sort está essencialmente correta (a própria varredura O(n) já garante Θ(n)/req); o limiar de 2× é arbitrário, mas os 7-18× observados o ultrapassam robustamente. Nenhum falso positivo.

**Conclusão:** Custo por request superlinear confirmado com assinatura O(n)/req → O(n²) total (R²=0,9992 no ajuste linear) e um grupo de controle que fica flat, isolando o histórico acumulado como causa; a amplificação por contenção do lock global é inferida do código.

### Cenário 06 — HTTP 5xx é contabilizado como sucesso

**Referência:** finding A5 (severidade: alto).

**Hipótese:** Uma resposta HTTP 500 (com `err==nil` no transport) faz o circuit breaker incrementar `SuccessfulRequests` e NÃO incrementar `FailedRequests`, ou seja, trata 5xx como sucesso.

**Método:** Módulo externo (`scn06`) via `replace`. Um `http.RoundTripper` sempre devolve `&http.Response{StatusCode:500}, nil`. Executa um único `Do()` (`maxConcurrent=1, maxRequests=100, window=60s, maxRetries=0`) dentro de goroutine com guarda de timeout e inspeciona `Metrics()` para o endpoint e para `::root`.

**Comando:** `cd .../scratchpad/scn-06 && go test -run TestScn06_5xxContadoComoSucesso -v -timeout 60s ./...`

**Saída observada:**
```
=== RUN   TestScn06_5xxContadoComoSucesso
    scn06_test.go:84: HTTP status recebido pelo chamador = 500
    scn06_test.go:85: MEDIDO endpoint[example.test][/api/v1/thing]: TotalRequests=1 SuccessfulRequests=1 FailedRequests=0 RetryCount=0
    scn06_test.go:89: MEDIDO ::root[example.test]: TotalRequests=1 SuccessfulRequests=1 FailedRequests=0
    scn06_test.go:95: HIPOTESE CONFIRMADA: 5xx tratado como SUCESSO (Successful=1, Failed=0)
--- PASS: TestScn06_5xxContadoComoSucesso (0.00s)
PASS
ok  	scn06	0.003s
```
Métricas: endpoint `[example.test][/api/v1/thing]` `TotalRequests=1, SuccessfulRequests=1, FailedRequests=0, RetryCount=0`; `::root` idem; status devolvido ao chamador = 500 com `err==nil` vindo de `Do()`.

**Veredito executor:** REPRODUZIDO. O CB decide sucesso/falha exclusivamente pelo erro de `cl.Do` (`circuitbreaker.go` linha 134: `if err == nil { cb.recordSuccess(...); return resp, nil }`). Como um 5xx chega com `err==nil` no transporte, o `Do` chama `recordSuccess` e retorna a resposta 500 sem erro. Medido: `Successful==1` e `Failed==0` no endpoint e no `::root`. O código nunca inspeciona `resp.StatusCode`, então qualquer 4xx/5xx é contado como sucesso.

**Veredito validador:** CONFIRMA (ressalvas menores). Três frentes: (1) leitura do código (linhas 133-137) — nunca inspeciona `resp.StatusCode`; `recordSuccess` (236) é o único ponto que incrementa `SuccessfulRequests`, e `recordFailure` (249, único ponto de `FailedRequests`) só é chamado no ramo `err!=nil`, que não ocorre para 5xx; (2) contrato do `net/http` — `Client.Do` retorna `err==nil` para 4xx/5xx; (3) medição sem corrida (`Metrics()` lido após `done`), causalidade travada. Variante do validador com servidor real (`httptest.Server` devolvendo 500 via `WriteHeader`) reproduz idêntico, inclusive no `::root`, eliminando artefato de mock. Ressalvas menores: (a) o executor usa `t.Logf` no ramo de confirmação, mas o ramo negativo usa `t.Errorf`, então o teste falharia se a hipótese fosse falsa — asserção gateia corretamente; (b) dúvida de artefato de mock eliminada pelo servidor real; (c) o validador adicionou `TotalRequests==1` ao gate e passou.

**Conclusão:** Confirmado que 5xx com `err==nil` é contabilizado como sucesso e nunca como falha, pois o CB decide apenas pelo erro do transporte e jamais inspeciona `resp.StatusCode`; reproduzido também com servidor HTTP real.

### Cenário 07 — Retry storm: downstream caído recebe MaxRetries+1 tentativas

**Referência:** finding A5 (severidade: alto).

**Hipótese:** Com ECONNREFUSED (supostamente retryable) e `maxRetries=5`, o downstream caído receberia 6 tentativas por chamada (MaxRetries+1), configurando amplificação (retry storm) em vez de proteção, com erro final `"request failed after retries"`.

**Método:** Módulo Go externo (replace) importando o pacote público. Um `http.RoundTripper` que sempre falha com ECONNREFUSED e conta atomicamente as chamadas. Executa uma única `Do()` com `maxRetries=5` (`maxConcurrent=0, maxRequests=0`), medindo nº de chamadas e o erro final, para três construções do erro (OpError+SyscallError realista, OpError direto, Errno puro) mais um contraste com erro de timeout genuinamente retryable.

**Comando:** `cd .../scratchpad/scn-07 && go test -run TestRetryStormECONNREFUSED -v -count=1 -timeout 60s`

**Saída observada:**
```
variant=A_OpError_SyscallError_ECONNREFUSED(realistic) calls=1 finalErr="Get \"http://downstream.invalid/api\": dial tcp: connect: connection refused" failedAfterRetries=false
variant=B_OpError_ECONNREFUSED calls=1 finalErr="...: dial tcp: connection refused" failedAfterRetries=false
variant=C_bare_ECONNREFUSED calls=1 finalErr="...: connection refused" failedAfterRetries=false
ASSERT realistic: calls=1 wantCalls=6 finalErr=Get "...": dial tcp: connect: connection refused
HYPOTHESIS FAILED for realistic error: got 1 transport calls, expected 6
CONTRAST timeoutNetErr: calls=6 finalErr=request failed after retries failedAfterRetries=true
--- PASS: TestRetryStormECONNREFUSED (2.50s)
ok  	scn07	2.508s
```
Métricas: ECONNREFUSED (3 variantes) `calls=1` em todas — NÃO houve retry; erro final = `url.Error "...: connection refused"` (não `"request failed after retries"`). Esperado pela hipótese: 6 calls. Contraste (`net.Error` com `Timeout()==true`): `calls=6`, erro final `"request failed after retries"`, duração 2,50s (5 sleeps de 500ms). Total do teste 2,508s.

**Veredito executor:** NAO_REPRODUZIDO. ECONNREFUSED NÃO é tratado como retryable na prática, então o downstream recebe apenas 1 chamada e nenhuma amplificação ocorre. Em `isRetryable` (`circuitbreaker.go:185-188`) o PRIMEIRO ramo testa `net.Error` e retorna `netErr.Temporary() || netErr.Timeout()`; para ECONNREFUSED ambos são `false`, então `isRetryable` devolve `false` e o `Do` retorna de imediato. O ramo explícito `errors.Is(err, syscall.ECONNREFUSED)` na linha 195 é CÓDIGO MORTO — `syscall.Errno`, `*net.OpError` e `*url.Error` todos implementam `net.Error`, logo `errors.As` casa antes e faz curto-circuito. O contraste (`calls=6`, `"request failed after retries"`) comprova que o laço amplifica para MaxRetries+1, mas só para erros com `Temporary()/Timeout()==true`; ECONNREFUSED escapa. Observação colateral: existe um bug de correção diferente (dead code + retry de ECONNREFUSED não funciona), embora não seja o retry storm hipotetizado.

**Veredito validador:** CONFIRMA (a refutação se sustenta). Verificou re-execução e causa-raiz de forma independente. Em `circuitbreaker.go:184-197`, `isRetryable` testa PRIMEIRO `errors.As(err,&netErr net.Error)` e retorna `Temporary()||Timeout()`. `TestErrorSemantics` confirmou empiricamente que `*net.OpError`, `*net.OpError+os.SyscallError` e a própria `syscall.Errno` TODOS implementam `net.Error` e, para ECONNREFUSED, `Temporary()` e `Timeout()` são ambos `false` → `isRetryable` devolve `false`, o `Do` (141-142) retorna de imediato o `url.Error` e o downstream recebe 1 chamada. O ramo `errors.Is(err, syscall.ECONNREFUSED)` (195) é código morto. O contraste do executor (`net.Error` com `Timeout/Temporary=true` → `calls=6`, sentinel, 2,50s = 5 sleeps de 500ms) comprova que o laço amplifica quando o erro é de fato retryable. Concorda com a leitura secundária: há um bug real de correção (dead code + ECONNREFUSED nunca retentado apesar da intenção declarada), rotulado como observação colateral. Nits menores: (a) o executor só injetou o erro via mock — o validador supriu com `val-07/TestRealECONNREFUSED` e o caminho de produção também dá 1 chamada; (b) a checagem `failedAfterRetries` usa comparação de string exata com o sentinel real de `circuitbreaker.go:153`, válida; (c) a goroutine em `runOnce` está corretamente sincronizada por `close(done)`. O contraste com erro retryable (`calls=6`) prova que o harness CONSEGUE detectar amplificação, logo o resultado negativo não é artefato de teste quebrado.

**Conclusão:** O retry storm hipotetizado NÃO se reproduz — ECONNREFUSED não é retryable na prática (1 única chamada, sem sentinel), e a refutação é confirmada; fica registrado, porém, um bug de correção colateral (código morto na linha 195 e retry de ECONNREFUSED silenciosamente anulado apesar da intenção declarada).

---

## 6. Resultados detalhados — Médios e Baixos

### Cenário 08 — Backoff fixo de 500ms ignora cancelamento de contexto

**Referência:** Finding A6.

**Hipótese:** Cancelar o contexto durante o backoff (`time.Sleep` de 500ms entre tentativas) NÃO interrompe o sleep; `Do()` só retorna depois de completar ao menos um backoff de ~500ms, muito além dos ~50ms em que o cancel foi disparado — provando que o backoff fixo ignora o cancelamento do contexto.

**Método:** Módulo externo importando o pacote público via `replace` local. Um `http.Client` com `RoundTripper` que SEMPRE falha imediatamente com erro `net.Error` temporário/timeout (retentável), ignorando o contexto, força o loop de retry. CB criado com `maxConcurrent=10`, `maxRequests=1000`, `windowSeconds=1` (tokens sobrando) e `maxRetries=3`. O contexto é cancelado ~50ms após iniciar `Do()` e mede-se o tempo total até o retorno (watchdog de 40s via `time.After` contra deadlock), repetindo 6x para caracterizar variância.

**Comando:** `cd .../scratchpad/scn-08 && go mod tidy && go test -run TestScn08_BackoffIgnoresContextCancel -timeout 60s -v -count=1 ./...`

**Saída observada:**
```
Execução 1: "Do() retornou em 1.000836374s (cancel disparado em ~50ms)"
"erro retornado por Do(): context canceled"
"chamadas ao RoundTripper (cl.Do): 2"
"metrics /fail: TotalRequests=2 FailedRequests=3 RetryCount=2 SuccessfulRequests=0"
"VERDICT=REPRODUZIDO: Do() levou 1000 ms (>=~500ms) apesar do cancel em 50 ms"
Execução mínima: "Do() retornou em 500.568149ms"; "context canceled"; RetryCount=1
Execução máxima: "Do() retornou em 1.502514797s"; "request failed after retries"; RetryCount=3
Demais: 1.001098804s, 1.001157587s, 1.501786284s. --- PASS (1.00s) ok scn08.
```
Faixa observada de tempos de `Do()` (cancel sempre em ~50ms): 500ms, 1000ms, 1001ms, 1001ms, 1501ms, 1502ms — TODAS >= ~500ms (10x a 30x os 50ms do cancel). O total sempre cai em múltiplos de ~500ms.

**Veredito executor:** REPRODUZIDO.

**Veredito validador:** CONFIRMA. A leitura do código corrobora a causa raiz: em `pkg/circuitbreaker.go` a linha 148 `time.Sleep(500 * time.Millisecond)` é incondicional e não observa o contexto; os únicos pontos que observam `ctx` no `Do` são a aquisição do semáforo (linha 109) e `waitForToken` (linha 175), ambos ANTES do sleep. Re-executou o teste do executor 9x (1502, 500, 1501, 1502, 500, 1501, 1001, 1501, 1501 ms — todos múltiplos de ~500ms, mínimo 500ms). Bateria independente (val-08, com `-race`, sem data races): prova determinística desligando semáforo E token bucket (`maxConcurrent=0`, `maxRequests=0`) dá ~1502ms fixo nas 4 execuções com 4 chamadas ao RoundTripper e 3 backoffs completos; controle `maxRetries=0` retorna em 62µs; controle com erro não-retentável em 28µs. *Ressalvas (não invalidantes):* (a) o executor manteve semáforo e token bucket ligados, o que introduz a variância 500/1000/1500ms e deixa o mínimo de ~500ms como evidência de carga — prova válida, porém menos limpa que a variante determinística; (b) o limiar de asserção `>=450ms` é generoso mas sem risco de falso positivo (piso físico de 500ms); (c) a leitura de `rt.calls` após o canal `done` é segura por happens-before (verificado sob `-race`).

**Conclusão:** Confirmado que o `time.Sleep(500ms)` de backoff é totalmente surdo ao cancelamento: uma vez iniciado, dorme os 500ms inteiros, atrasando o retorno de `Do()` em até `maxRetries×500ms` (~1,5s aqui) mesmo com o contexto cancelado em ~50ms.

---

### Cenário 09 — Semáforo é retido durante os retries/sleeps

**Referência:** Finding A6.

**Hipótese:** Com `maxConcurrent=1`, enquanto a chamada A está no ciclo de retry+sleep (3x 500ms entre 4 tentativas), uma segunda chamada B disparada ~50ms depois fica bloqueada na entrada do semáforo por vários segundos, só progredindo depois que A libera o slot.

**Método:** Módulo externo importando o pacote público via `replace`. Um único circuit breaker (`maxConcurrent=1`, `maxRetries=3`, sem token bucket) e um `http.Transport` que SEMPRE falha com erro `net.Error` temporário (`isRetryable=true`), registrando o instante do primeiro `RoundTrip` de cada chamada (header `X-Call=A/B`). A entra primeiro; ~50ms depois B é disparada e mede-se quanto ela espera para chegar ao primeiro `RoundTrip` (passar do semáforo). Inclui contraste com `maxConcurrent=2`.

**Comando:** `cd .../scratchpad/scn-09 && go mod tidy && go test -timeout 60s -race -v ./...`

**Saída observada:**
```
=== RUN   TestSemaphoreHeldDuringRetries
    scn09_test.go:106: A: attempts(RoundTrips)=4 duracaoTotal=1.502624077s
    scn09_test.go:107: B: attempts(RoundTrips)=4 espera_para_entrar=1.452420916s
    scn09_test.go:108: gap(B_first - A_first)=1.502697527s
    scn09_test.go:112: VERDICT=REPRODUZIDO: B esperou 1.452420916s (>=1s) para entrar; slot retido durante retries/sleeps de A
--- PASS: TestSemaphoreHeldDuringRetries (3.01s)
=== RUN   TestSemaphoreControl_Concurrent2
    scn09_test.go:159: CONTROLE maxConcurrent=2: espera_para_entrar_B=118.918µs
    scn09_test.go:161: CONTROLE OK: com slot livre, B entra rapido (118.918µs)
--- PASS: TestSemaphoreControl_Concurrent2 (1.55s)
PASS
ok  	scn09	5.569s
```

**Veredito executor:** REPRODUZIDO.

**Veredito validador:** CONFIRMA. Reproduziu de forma independente e detectou que a primeira execução vinha do cache do `go test` (valores idênticos ao nanosegundo); com `-count=1` obteve números frescos e coerentes (A duração 1.502s; B espera 1.4519s; controle conc=2 em 283µs). Escreveu teste próprio (val-09) com asserções que FALHAM (`t.Fatalf`) e timeline completa: com `maxConcurrent=1` os RoundTrips de A ocorrem em 0s/501ms/1002ms/1503ms e TODOS os 4 de B estritamente depois (1503ms/2003ms/2504ms/3005ms); o primeiro de B (@1.5027s) coincide com o fim de A (@1.5025s). O código-fonte (linhas 105-113, `defer <-cb.sem` na 112, sleep na 148) confirma o mecanismo, com token bucket desativado isolando o semáforo como única causa. Sem corridas no `-race`. *Ressalva menor:* as asserções do teste do executor são não-falhas (só `t.Logf`), logo o `PASS` não prova nada por si só — o leitor precisa inspecionar o log; a lacuna foi coberta pelo val-09 com `t.Fatalf`.

**Conclusão:** Confirmado que o slot de concorrência fica retido por toda a janela de retries+sleeps de A (`cb.sem` adquirido no topo de `Do` e liberado só no `defer` ao final de todas as tentativas), serializando totalmente a chamada B por ~1,45s; com `maxConcurrent=2` o problema desaparece.

---

### Cenário 10 — Goroutine do token bucket vaza sem Stop()

**Referência:** Finding A7.

**Hipótese:** Criar N=200 circuit breakers com rate-limit (`maxRequests=10`, `windowSeconds=1`) via `NewCircuitBreaker(name_i,0,10,1,0)` SEM chamar `Stop()` deixa N goroutines de ticker vivas, fazendo `runtime.NumGoroutine()` crescer ~N. Como contraste, chamar `Stop()` em todos reduz a contagem de volta ao baseline.

**Método:** Módulo externo que importa o pacote público via `replace`. Mede baseline de `runtime.NumGoroutine` após GC e sleep, cria 200 breakers com rate-limit sem `Stop` e remede, depois chama `Stop` em todos e remede pela terceira vez, comparando os deltas.

**Comando:** `go test -v -run TestTokenBucketGoroutineLeak -timeout 60s ./...`

**Saída observada:**
```
=== RUN   TestTokenBucketGoroutineLeak
    leak_test.go:42: NumGoroutine antes           = 2
    leak_test.go:43: NumGoroutine apos criar 200    = 202
    leak_test.go:44: DELTA (vazamento)            = 200 (esperado ~200)
    leak_test.go:58: NumGoroutine apos Stop() em todos = 2
    leak_test.go:59: Goroutines recuperadas apos Stop  = 200
--- PASS: TestTokenBucketGoroutineLeak (0.80s)
PASS
ok  	scn10	0.808s
```

**Veredito executor:** REPRODUZIDO.

**Veredito validador:** CONFIRMA. Reproduziu o teste (fresh, `-count=1`) com saída idêntica (baseline=2, após 200=202, delta=+200, após `Stop()`=2). Duas validações independentes (val-10): (1) `TestLeakConfirmedViaStackDump` mede delta=+150 exato mesmo SEM o sleep de estabilização e, via `runtime.Stack`, comprova que as goroutines excedentes são exatamente `...pkg.(*circuitBreaker).startTokenBucket.func1` (`circuitbreaker.go:86`, o `select`), voltando a zero após `Stop()`; (2) `TestLeakSurvivesGCWithoutReferences` descarta todas as referências e roda 5x `runtime.GC()` — 100/100 goroutines de ticker sobrevivem, provando VAZAMENTO GENUÍNO e não coleta atrasada. O fonte confirma: `tokenStop` só é fechado por `Stop()` via `stopOnce` seguido de `tokenWaitGroup.Wait()`. *Ressalvas cosméticas (sem falso positivo):* (a) o sleep de 300ms após a criação é desnecessário; (b) `runtime.NumGoroutine` como proxy é levemente indireto (eliminado pelo teste por stack-frame); (c) as asserções usam tolerância frouxa (`N-20`), enquanto o delta real é exatamente N e a recuperação é total.

**Conclusão:** Confirmado com precisão exata: cada breaker com rate-limit ativo lança uma goroutine de ticker que só termina com `close(tokenStop)` feito por `Stop()`; instanciar breakers repetidamente (por request/tenant) sem `Stop()` vaza 1 goroutine por breaker indefinidamente.

---

### Cenário 11 — Manager ignora silenciosamente parâmetros divergentes

**Referência:** Finding A8.

**Hipótese:** Chamar `m.NewCircuitBreaker("svc", A...)` e depois `m.NewCircuitBreaker("svc", B...)` no mesmo Manager retorna a MESMA instância já criada com a config A; os parâmetros B são descartados silenciosamente (sem erro, log ou sinal), e o limite efetivo permanece o de A.

**Método:** Módulo externo (scn11) importando o pacote público via `replace`. Cria um Manager, registra `"svc"` com config A (`maxConcurrent=1, maxRequests=1, windowSeconds=60, maxRetries=0`) e depois com config B (`100,1000,1,5`). Compara identidade (igualdade de interface, `%p`, `reflect.Pointer`) de cb1, cb2 e `GetCircuitBreaker`. Depois demonstra o rate efetivo: dispara 2 requests num `httptest.Server` via cb2 — a 1ª consome o único token pré-carregado de A, a 2ª (ctx de 2s) fica sem token e estoura o deadline.

**Comando:** `cd .../scratchpad/scn-11 && go test -run TestManagerIgnoraParametrosDivergentes -v -timeout 60s ./...`

**Saída observada:**
```
=== RUN   TestManagerIgnoraParametrosDivergentes
[IDENTIDADE] cb1=0xd72a03b8180 cb2=0xd72a03b8180 cb3=0xd72a03b8180
[IDENTIDADE] cb1==cb2 (igualdade de interface) = true
[IDENTIDADE] mesmo ponteiro (%p) = true
[IDENTIDADE] mesmo ponteiro (reflect) = true
[RATE] req1: status=200 dur=500.767µs err=<nil>
[RATE] req2: dur=2.001111368s err=context deadline exceeded resp_nil=true
[RATE] req2 bloqueou ~2.001111368s ate o deadline (sem token) -> limite efetivo = config A
[VEREDITO] REPRODUZIDO: manager reusou a instancia A e descartou os parametros B silenciosamente
--- PASS: TestManagerIgnoraParametrosDivergentes (2.00s)
PASS
ok  	scn11	2.005s
```

**Veredito executor:** REPRODUZIDO.

**Veredito validador:** CONFIRMA. Verificou dois eixos: (1) identidade — `manger.go` retorna a instância existente pelo nome antes de avaliar qualquer parâmetro (`if cb, ok := m.cb[name]; ok { return cb }`, linhas 47-49) e a assinatura retorna apenas `ICircuitBreaker` (sem erro), logo o descarte de B é comprovadamente silencioso; (2) comportamento — a matemática do token bucket confirma (config A: cap 1, refil 60s; config B: cap 1000, intervalo 1ms). Teste independente com CONTROLES (val-11): config B fresca deixa req2 passar em 325µs, config A fresca bloqueia 2s, e o cb obtido do manager (registrado A, "sobre-escrito" por B) bloqueia 2s comportando-se como A. *Ressalva menor:* o teste original do executor não incluía um controle positivo provando que uma config B fresca deixaria req2 passar; a lacuna foi fechada no val-11, descartando o falso positivo teórico.

**Conclusão:** Confirmado nos dois eixos: a segunda chamada retorna a mesma instância (`0xd72a03b8180`) e os parâmetros B são 100% descartados sem erro/log/sinal — um segundo chamador que peça limites diferentes é servido silenciosamente com a configuração do primeiro.

---

### Cenário 12 — Client sem timeout + servidor que não responde trava o slot do semáforo

**Referência:** Finding A10.

**Hipótese:** Com `http.Client{Timeout:0}`, request usando `context.Background()` (sem deadline) e um servidor que aceita a conexão mas nunca responde, a goroutine da chamada A trava DENTRO de `cl.Do` segurando o slot do semáforo (só liberado no `defer`, ao retornar de `Do`). Com `maxConcurrent=1`, uma 2ª chamada B nunca consegue adquirir o slot.

**Método:** Módulo externo importando o pacote `pkg`. `httptest.NewServer` com handler que envia sinal (`serverHits`) e bloqueia em `<-block` (nunca responde). CB com `maxConcurrent=1`, sem rate-limit e sem retry; `http.Client{Timeout:0}`; requests com `context.Background()`. Dispara A (deve travar segurando o slot), aguarda o 1º hit, ~100ms depois dispara B e observa por 2s se um 2º hit chega ao servidor.

**Comando:** `cd .../scratchpad/scn-12 && go test -v -run TestScn12SlotTravado -timeout 60s ./...`

**Saída observada:**
```
=== RUN   TestScn12SlotTravado
    scn12_test.go:54: A alcançou o servidor: slot adquirido e preso dentro de cl.Do
    scn12_test.go:79: VERDICT=REPRODUZIDO: B NAO adquiriu o slot em 2.000361523s (bloqueado no semáforo; A segura o slot para sempre)
--- PASS: TestScn12SlotTravado (2.10s)
PASS
ok  	scn12	2.105s
```

**Veredito executor:** REPRODUZIDO.

**Veredito validador:** CONFIRMA. Ancorou a hipótese no código: A adquire o único slot (`circuitbreaker.go:108`), entra em `cl.Do` (linha 133) e trava porque o handler faz `<-block` e nunca escreve resposta; com `Timeout:0` e `context.Background()` não há deadline nem cancelamento, então o `defer` de liberação do slot (linha 112) jamais executa; B fica preso no mesmo `select` de aquisição, cuja única saída alternativa (`req.Context().Done()`, linha 109) nunca dispara. Re-execução independente confirmou os três pilares: B não alcança o servidor em conc=1, B ALCANÇA em conc=2 (isolando o semáforo como causa, não HTTP), e só A registrou tentativa. `-race` limpo. *Ressalvas menores:* (a) o teste do executor mede "B não alcança em 2s" mas não ISOLA a causa — compatível com bloqueio na camada HTTP; a lacuna foi suprida pelo controle `maxConcurrent=2` e checagem de métricas; (b) 2s é limite finito, não prova "para sempre" literal, mas a conclusão de bloqueio permanente é sólida por leitura de código.

**Conclusão:** Confirmado que sem timeout no client nem deadline no context um servidor que aceita a conexão e nunca responde congela permanentemente o slot de concorrência, paralisando todas as chamadas subsequentes com `maxConcurrent=1`.

---

### Cenário 13 — Cancelamento na espera de token infla FailedRequests sem TotalRequests (failed>total)

**Referência:** Finding A15.

**Hipótese:** Se o `ctx` é cancelado dentro de `waitForToken`, `recordFailure` é chamado sem `recordAttempt`, permitindo que `FailedRequests` seja maior que `TotalRequests` para o endpoint (invariante `failed<=total` violada).

**Método:** Módulo externo que importa o pacote público via `replace`. Cria `httptest` server 200 e `NewCircuitBreaker("scn13",0,1,3600,0)` (1 token, janela de 3600s, sem retries). A 1ª chamada `Do` consome o único token e sucede (Total=1). Em seguida faz 6 chamadas `Do` com context de timeout de 30ms que expiram esperando token (bucket vazio, sem recarga por 3600s), forçando `waitForToken -> err -> recordFailure` sem `recordAttempt`. Depois inspeciona `Metrics()` e compara `FailedRequests` com `TotalRequests`.

**Comando:** `go test -v -timeout 60s -run TestScn13_FailedMaiorQueTotal .`

**Saída observada:**
```
=== RUN   TestScn13_FailedMaiorQueTotal
    scn13_test.go:82: [agregado] host="127.0.0.1:39063" ep="::root" -> Total=1 Successful=1 Failed=6
    scn13_test.go:87: [endpoint] host="127.0.0.1:39063" ep="/probe" -> Total=1 Successful=1 Failed=6
    scn13_test.go:89: timedOut(err!=nil nas subsequentes)=6 de 6
    scn13_test.go:92: REPRODUZIDO: FailedRequests(6) > TotalRequests(1)
--- PASS: TestScn13_FailedMaiorQueTotal (0.18s)
PASS
ok  	scn13	0.185s
EXIT=0
```

**Veredito executor:** REPRODUZIDO.

**Veredito validador:** CONFIRMA (sem ressalvas). O loop de `Do` chama `waitForToken` (linha 123) ANTES de `recordAttempt` (linha 128); quando o `ctx` expira dentro de `waitForToken`, executa-se `recordFailure` (linha 124, `FailedRequests++` na linha 251) seguido de return imediato, e `recordAttempt` (único ponto que faz `TotalRequests++`, linha 225) nunca é alcançado. Confirmou empiricamente em duas bases (teste do executor e variação própria) que `Failed>Total` tanto no endpoint `/probe` quanto no agregado `::root` (via `updateMetrics`). Validação adicional prova a causa-raiz de forma inequívoca: o servidor foi tocado apenas 1 vez e todos os erros subsequentes são `context.DeadlineExceeded`, logo os `FailedRequests` inflados vêm 100% do caminho `waitForToken->recordFailure`, não de erros de rede de `cl.Do`. O validador registra que nenhum ponto de fragilidade compromete o veredito.

**Conclusão:** Confirmado que cada cancelamento na espera de token soma +1 em `FailedRequests` sem somar em `TotalRequests`, violando de forma determinística a invariante `FailedRequests<=TotalRequests` (medido 6>1), inconsistência que também se propaga ao agregado por host.

---

### Cenário 14 — Clamp de intervalo para 1ns anula o rate limiting (busy loop)

**Referência:** Finding A16.

**Hipótese:** Com `maxRequests=2e9` e `windowSeconds=1`, o intervalo (`1s/2e9=0,5ns`) trunca para 0 e é clampado para 1ns (`circuitbreaker.go:55-58`); o bucket seria reabastecido a uma taxa altíssima e, na prática, não haveria rate limit — esperando-se uma taxa efetiva absurdamente ALTA e SUSTENTADA em 50ms, limitada apenas pela resolução do runtime.

**Método:** Módulo externo importando o pacote público via `replace`. Caso A (cenário): `NewCircuitBreaker("scn14-clamp",0,2e9,1,0)`, transport no-op (200/`err==nil`), loop de `Do()` por ~50ms contando requisições atendidas e amostrando a taxa em sub-janelas de 5ms. Caso B (controle): limite de 10/1s para provar que o token bucket estrangula quando os parâmetros são razoáveis.

**Comando:** `cd scn-14 && go test -run 'TestA_ClampNullifiesRateLimit|TestB_SaneLimitDoesThrottle' -v -timeout 60s`

**Saída observada:**
```
[A] Construtor (0, 2e9, 1, 0): 26.840906054s para pré-encher bucket com 2e9 tokens
[A] Total Do() atendidos em 50.002148ms: 1183 requisicoes
[A] Taxa efetiva media: 23659 req/s (0.02 milhoes/s)
[A] Nominal configurada: 2e9 req/s (2.000.000.000/s)
[A] sub-janela 01 (~ 5ms): +356 req  -> 71166 req/s
[A] sub-janela 05 (~25ms): +89 req  -> 17866 req/s
[A] sub-janela 09 (~45ms): +64 req  -> 12761 req/s
--- PASS: TestA_ClampNullifiesRateLimit (26.89s)
(2a execucao) Total 1216 req em 50.077243ms -> 24282 req/s; sub-janela 01 70108 req/s -> sub-janela 09 13713 req/s
[B] limite SÃO (10/1s): atendidas 10 chamadas em 80.247644ms antes de bloquear (err=context deadline exceeded)
--- PASS: TestB_SaneLimitDoesThrottle (0.08s)
```

**Veredito executor:** NAO_REPRODUZIDO. A previsão quantitativa não se confirmou: a taxa efetiva (~24k req/s) é ~82.000x MENOR que a nominal de 2e9/s (não "absurdamente maior") e NÃO é sustentada — colapsa ~5x (de ~70k para ~13k req/s) dentro dos próprios 50ms. O gargalo real é o custo O(n·log n) por chamada em `Do()` (`repsRatio` faz `slices.SortFunc` na fatia crescente de timestamps, custo dobrado porque `updateMetrics` reaplica no agregado `::root`), não a resolução do runtime nem o token bucket. RESSALVA do próprio executor: o sub-fato "na prática sem rate limit" é real (`waitForToken` jamais bloqueou em ~1200 requests, enquanto o Caso B bloqueou na 11ª), mas isso decorre do bucket pré-preenchido com 2e9 tokens, não de um refill a 1ns exercido.

**Veredito validador:** PARCIAL. Reproduziu os fatos empíricos de forma quase idêntica e confirmou, de forma independente e mais forte, a atribuição de mecanismo: uma config SEM limiter algum (`maxRequests=0`) entrega exatamente a mesma vazão (~22-24k req/s) e o mesmo colapso que o cenário 2e9 — logo clamp/refiller/bucket são irrelevantes para a vazão, e a latência por chamada cresce monotonicamente (7,7µs->87µs). Concorda que a forma FORTE/quantitativa da hipótese (taxa absurdamente alta e sustentada) NÃO se reproduziu. *Ressalva que leva a PARCIAL:* a hipótese ORIGINAL afirma apenas "na prática sem rate limit", e isso É reproduzido (Caso A nunca bloqueia; Caso B bloqueia na 11ª), então o rótulo binário NAO_REPRODUZIDO subvaloriza a parte genuinamente confirmada. Aponta ainda que (1) o executor endureceu a hipótese acrescentando "SUSTENTADA" e "limitada só pela resolução do runtime", condições que o enunciado não fez; (2) o aspecto "busy loop" do título (ticker de 1ns queimando CPU na goroutine de refill) NUNCA foi medido — o teste afere só a vazão de `Do()`, não o consumo de CPU do refiller.

**Conclusão:** Divergência de nuance: o desfecho central "na prática sem rate limit" é real e reproduzido, mas por causa do pré-preenchimento do bucket com 2e9 tokens — o mecanismo alegado ("refill a taxa altíssima") é inobservável pela API pública e a magnitude quantitativa da hipótese foi refutada, com o teto de vazão imposto por outro defeito (custo O(n·log n) das métricas).

---

### Cenário 20 — Exaustão de MaxRetries perde a causa original do erro

**Referência:** Finding A13.

**Hipótese:** Quando todas as tentativas falham com um erro retryable, `Do()` retorna `errors.New("request failed after retries")` — a causa original NÃO é embrulhada, então `errors.Is/As` não a encontram — e as métricas registram `RetryCount==MaxRetries (2)` e `FailedRequests==3`.

**Método:** Módulo externo importando o `pkg` via `replace`. Um `RoundTripper` próprio (`alwaysFailTransport`) devolve SEMPRE o mesmo erro sentinela (`*sentinelTempError`) que implementa `net.Error` com `Timeout()==Temporary()==true`, garantindo `isRetryable()==true` e esgotamento do loop. `maxRetries=2`, `maxRequests=0` (sem token bucket). Verifica mensagem, `errors.Is/As/Unwrap` contra a sentinela e as métricas do endpoint.

**Comando:** `cd .../scratchpad/scn-20 && go test -run TestScn20_ExaustaoRetriesPerdeCausaOriginal -v -timeout 60s ./...`

**Saída observada:**
```
=== RUN   TestScn20_ExaustaoRetriesPerdeCausaOriginal
    scn20_test.go:94: elapsed=1.001134434s transport.calls=3
    scn20_test.go:95: err.Error()="request failed after retries" (bate com esperado=true)
    scn20_test.go:96: errors.Is(err, sentinela)=false  errors.As(err,*sentinela)=false  errors.Unwrap(err)=<nil>
    scn20_test.go:97: Metrics host=example.test presente=true endpoint=/foo presente=true
    scn20_test.go:98: EndpointMetrics /foo: TotalRequests=3 SuccessfulRequests=0 FailedRequests=3 RetryCount=2
    scn20_test.go:101: EndpointMetrics ::root: TotalRequests=3 FailedRequests=3 RetryCount=2
    scn20_test.go:136: VEREDITO LOCAL: REPRODUZIDO (mensagem generica + causa original perdida)
--- PASS: TestScn20_ExaustaoRetriesPerdeCausaOriginal (1.00s)
PASS
ok  	scn20	1.004s
```

**Veredito executor:** REPRODUZIDO.

**Veredito validador:** CONFIRMA. Confirmação em três frentes: (1) leitura do código — a linha 153 é literalmente `return nil, errors.New("request failed after retries")`, sem embrulho/Unwrap; o loop faz attempt 0..MaxRetries (3 tentativas) e `recordRetry` só ocorre se `attempt<MaxRetries`, resultando em `RetryCount=2`, espelhado no agregado `::root` via `updateMetrics`; (2) re-execução do teste do executor com saída idêntica; (3) validação independente (val-20, `-race -count=1`) reproduz o caminho de exaustão (`errors.Is/As(*tempErr)/As(net.Error)/As(*url.Error)/Unwrap` TODOS falham) E acrescenta um CONTROLE não-retryable: `transport.calls=1`, `Do` retorna o `*url.Error`, `errors.Is(errFatal)=true`, `errors.As(*url.Error)=true`, `Retry=0` — confirmando que a perda de causa é ESPECÍFICA do caminho de exaustão. Descarta falso positivo: se a sentinela não fosse retryable, `transport.calls` seria 1 e `RetryCount` 0, e as asserções falhariam. *Ressalvas menores (não-fatais):* o teste do executor não testava explicitamente `errors.As(err,&net.Error)` (coberto pela val-20, também `false`); leitura de `tr.calls`/`Metrics()` após o receive no canal `done` estabelece happens-before (verificado com `-race`).

**Conclusão:** Confirmado integralmente: o erro final genérico não faz `Unwrap`, tornando a causa original irrecuperável via `errors.Is/As` (impossível distinguir timeout/reset/refused programaticamente); as métricas batem com o loop (`Total=3, Failed=3, RetryCount=2`) e a perda é específica do caminho de exaustão de retries.

---

**Estatísticas da faixa "Médios e Baixos".** A faixa reúne 8 cenários — 6 de severidade média (08, 09, 10, 11, 12, 20) e 2 de severidade baixa (13, 14). O executor marcou 7 como REPRODUZIDO (08, 09, 10, 11, 12, 13, 20) e 1 como NAO_REPRODUZIDO (14). O validador re-executou todos os 8 e emitiu 7 vereditos CONFIRMA (08, 09, 10, 11, 12, 13, 20) e 1 PARCIAL (14), sem detectar data races em nenhum teste rodado com `-race`. Todos os cenários salvo o 13 vieram acompanhados de ressalvas metodológicas do validador, porém em 6 deles (08, 09, 10, 11, 12, 20) essas ressalvas são menores/cosméticas e não invalidam o veredito de reprodução; apenas o cenário 14 tem divergência substantiva (a hipótese na forma quantitativa forte foi refutada, mas o desfecho central "na prática sem rate limit" foi confirmado, e o gargalo real foi reatribuído ao custo O(n·log n) das métricas). Em resumo: 7 cenários confirmando 6 findings distintos de defeito (A6 ×2, A7, A8, A10, A15, A13) e 1 (A16) apenas parcialmente sustentado.

---

## 7. Resultados detalhados — Contrastes e Casos de Borda

Esta faixa reúne cenários de **contraste** (que validam o comportamento correto onde ele é esperado, dando credibilidade ao detector e aos cenários que acusam problemas) e **casos de borda** (configurações-limite e chamadas repetidas/concorrentes). Nos cinco cenários houve concordância plena entre executor e validador.

### Cenário 15 — CONTRASTE: Manager e Stop concorrentes são seguros (sem race)

**Referência:** finding A7/ok (severidade: contraste).

**Hipótese:** O acesso concorrente ao Manager (`NewCircuitBreaker`/`GetCircuitBreaker`) sobre poucos nomes, combinado com `Stop()` concorrente sobre o mesmo breaker, NÃO produz data race nem panic. É um cenário de contraste que valida que o detector `-race` não é ruído.

**Método:** Módulo externo `scn15` importando o pacote público via replace local. Um único teste com timeout interno de 45s (`time.After`) executa três fases: (A) 120 goroutines liberadas simultaneamente misturando `m.NewCircuitBreaker` e `m.GetCircuitBreaker` sobre 4 nomes, 200 iterações cada; (B) 32 goroutines chamando `Stop()` no mesmo breaker (com token bucket ativo); (C) 32 goroutines chamando `Stop()` em todos os breakers do Manager. Rodado com `go test -race`.

**Comando:** `cd .../scratchpad/scn-15 && go test -race -timeout 60s -v -count=1 ./...`

**Saída observada:**
```
=== RUN   TestManagerAndStopConcurrent
    scn15_test.go:122: instâncias distintas criadas via Manager: 4 (nomes: 4) | goroutines mgr=120 stop=32
--- PASS: TestManagerAndStopConcurrent (0.03s)
PASS
ok  	scn15	1.043s
EXIT=0

[6 execuções independentes com -race, 0 ocorrências de "DATA RACE"]:
=== run 1 === ok  	scn15	1.041s
=== run 2 === ok  	scn15	1.039s
=== run 3 === ok  	scn15	1.038s
=== run 4 === ok  	scn15	1.039s
=== run 5 === ok  	scn15	1.041s
```

**Veredito executor:** CONFIRMADO_OK. Em 6 execuções com `-race` (go1.26, 8 cores), 0 linhas "DATA RACE", 0 panic e 0 deadlock (exit 0, sem disparar o timeout de 45s). O Manager protege o map com `sync.RWMutex` (Lock em `NewCircuitBreaker`, RLock em `GetCircuitBreaker`), garantindo idempotência por nome — 120 goroutines concorrentes criaram apenas 4 instâncias distintas (uma por nome). `Stop()` usa `sync.Once` (`stopOnce`) para fechar `tokenStop` uma única vez, e `tokenWaitGroup.Wait()` sincroniza o encerramento da goroutine do token bucket.

**Veredito validador:** CONFIRMA (re-executado). A inspeção do código confirma a ausência de race no Manager e em `Stop()`. Re-executou o teste do executor 8x com `-race`, todas limpas (~1.04s), e escreveu uma variação mais agressiva em `val-15` que entrelaça create+get+stop SIMULTANEAMENTE (28.800 operações concorrentes, não em fases sequenciais): 5 execuções `-race` todas limpas. **Ressalvas (não-fatais):** (1) o teste do executor chama `t.Fatalf` de dentro de `runConcurrentScenario`, que roda em goroutine não-teste — anti-padrão do Go; uma falha de sanidade só apareceria como o timeout de 45s. (2) As três fases são estritamente sequenciais (nunca entrelaçam de fato create com Stop/Get concorrente), reduzindo o poder do teste — a variação do validador corrige e ainda assim não acha race. (3) O trecho de saída diz "6 execuções" mas lista só 5 (inconsistência menor). Crucialmente, o validador adicionou um CONTROLE POSITIVO (race deliberado em `x++`) que o `-race` ACUSOU (exit=1, FAIL), provando que o detector está armado neste ambiente — logo as passagens limpas são significativas.

**Conclusão:** Manager e `Stop()` concorrentes são genuinamente livres de race e panic. O contraste dá credibilidade ao detector `-race`, que passa limpo aqui e acusa problemas onde eles existem.

---

### Cenário 16 — CONTRASTE: GET sem corpo re-tenta corretamente

**Referência:** finding A2/ok (severidade: contraste).

**Hipótese:** Para um GET (sem body), o retry após erro temporário de rede funciona e retorna sucesso — o bug de corpo vazio (que afeta requests com body consumido no primeiro attempt) NÃO afeta requests sem corpo. Com `maxRetries=3` e um transport que falha 2x com erro temporário e depois responde 200, `Do()` deve retornar sucesso com exatamente 3 tentativas ao transport.

**Método:** Módulo externo `scn16` importando o pacote via replace local. Um `http.RoundTripper` (`flakyTransport`) falha as 2 primeiras chamadas com um erro que implementa `net.Error` (Timeout/Temporary=true, portanto retryable) e responde 200 OK na 3ª. Executa um GET sem body via `cb.Do` com `maxConcurrent=0`, sem rate-limit e `maxRetries=3`, numa goroutine com guarda `time.After(30s)`, validando sucesso, contagem de chamadas e métricas.

**Comando:** `cd .../scratchpad/scn-16 && go test -v -timeout 60s ./...`

**Saída observada:**
```
=== RUN   TestScn16_GETRetriesOK
    scn16_test.go:102: RESULTADO: err=<nil> StatusCode=200 body="ok" transportCalls=3 elapsed=1.001607318s
    scn16_test.go:104: METRICAS[example.test][/resource]: Total=3 Success=1 Failed=2 Retry=2
    scn16_test.go:121: CONFIRMADO_OK: GET sem corpo re-tentou e retornou sucesso apos 3 tentativas
--- PASS: TestScn16_GETRetriesOK (1.00s)
PASS
ok  	scn16	1.005s
```

**Veredito executor:** CONFIRMADO_OK. Após 2 falhas temporárias (attempts 0 e 1, com `recordRetry` + sleep de 500ms cada), o attempt 2 obtém sucesso e `Do()` retorna (resp 200, err=nil). Exatamente 3 tentativas ao transport. Métricas coerentes: Total=3, Success=1, Failed=2, Retry=2. O tempo (~1s) corresponde aos 2 sleeps de 500ms. Como o GET não tem body, o `req.Clone(ctx)` a cada attempt reproduz uma requisição íntegra — logo o retry entrega sucesso normalmente.

**Veredito validador:** CONFIRMA (re-executado). Releu `pkg/circuitbreaker.go`: com `failN=2` e `MaxRetries=3`, o loop sai pelo `return resp,nil` no attempt 2, exercitando o caminho retry-then-succeed (não o de exaustão). Re-executou o teste com saída idêntica e com `-race -count=1` (PASS, sem data race). Escreveu `val-16` com verificações mais fortes: `bodiesLen=[0 0 0]` (sem esvaziamento/corrupção, pois não há corpo), conferência do agregado `::root` (T=3 S=1 F=2 R=2), sanidade temporal (~2 sleeps de 500ms) e um POST com body nil como controle que também re-tenta e sucede (isola método de corpo). **Ressalva (menor, de escopo):** o teste prova apenas o lado positivo do contraste (GET sem corpo re-tenta e sucede); não exercita diretamente um request COM corpo para exibir o bug de esvaziamento — o que é adequado a um cenário A2/ok. Sem falso positivo, sem condição de corrida (contador atômico lido após o `Do` retornar; `-race` limpo), sem fragilidade de medição (métricas em campos escalares `int64`, não nas slices vivas afetadas por sort in-place).

**Conclusão:** O retry entrega sucesso normalmente para requests sem corpo — o bug de corpo vazio NÃO atinge GETs. O lado positivo do contraste está corretamente demonstrado.

---

### Cenário 17 — EDGE: modo ilimitado (maxConcurrent=0, maxRequests=0)

**Referência:** finding modo-ilimitado (severidade: edge).

**Hipótese:** No modo ilimitado (`maxConcurrent=0`, `maxRequests=0`), `Do()` funciona sem semáforo nem token bucket, nenhuma goroutine extra é criada na construção (não há ticker), e `Stop()` é um no-op seguro e idempotente (não entra em pânico nem trava, mesmo chamado duas vezes).

**Método:** Módulo externo (go.mod com replace) importando o pacote público. Cria `NewCircuitBreaker("scn17",0,0,0,0)`; mede `runtime.NumGoroutine()` antes e depois da criação (com GC + pausa para estabilizar); executa 5 `Do()` contra um `httptest.Server` que responde 200; inspeciona `Metrics()`; chama `Stop()` duas vezes (cada uma isolada com `recover()` e guard de `time.After`); e faz um `Do()` adicional após `Stop()`.

**Comando:** `cd .../scratchpad/scn-17 && go test -v -timeout 60s ./...` (e também `go test -race -v -timeout 60s ./...`)

**Saída observada:**
```
=== RUN   TestScn17ModoIlimitado
    scn17_test.go:43: NumGoroutine antes=3 depois-da-criacao=3 (sem aumento)
    scn17_test.go:86: Do() OK: 5/5 retornaram 200
    scn17_test.go:93: metrics host="127.0.0.1:36999" ep="::root" total=5 success=5 failed=0
    scn17_test.go:93: metrics host="127.0.0.1:36999" ep="/" total=5 success=5 failed=0
    scn17_test.go:99: TotalRequests somados (sem ::root)=5
    scn17_test.go:114: Stop() #1 retornou sem panic/deadlock
    scn17_test.go:132: Stop() #2 (duplo) retornou sem panic/deadlock — idempotente
    scn17_test.go:141: NumGoroutine final (pós-Stop)=3 (antes=3, pós-criacao=3)
    scn17_test.go:152: Do() após Stop() ainda funciona (200) — confirma Stop no-op
--- PASS: TestScn17ModoIlimitado (0.16s)
PASS
ok  	scn17	0.159s
--- (run com -race) ---
--- PASS: TestScn17ModoIlimitado (0.16s)
PASS
ok  	scn17	1.168s
```

**Veredito executor:** CONFIRMADO_OK. Com `maxConcurrent=0` o campo `sem` fica nil (bloco de aquisição de slot pulado) e com `maxRequests=0`/`windowSeconds=0` os canais `tokens`/`tokenStop` ficam nil, então `startTokenBucket()` nunca é invocado — por isso `NumGoroutine` não sobe (3→3). `Do()` opera normalmente (`waitForToken` retorna nil imediatamente com `tokens==nil`), as 5 chamadas retornam 200 e as métricas são registradas (endpoint `/` e agregado `::root`). `Stop()` entra no early-return `if cb.tokenStop == nil { return }`, sendo no-op verdadeiro e idempotente; um `Do()` após `Stop()` ainda retorna 200. Resultado idêntico e limpo sob `-race`.

**Veredito validador:** CONFIRMA (re-executado). Verificou o código-fonte (sem=nil, tokens/tokenStop=nil, `waitForToken` retorna nil imediatamente, `Stop()` early-return) e re-executou o teste sem cache (`-count=1`) e com `-race`, obtendo saída idêntica byte-a-byte. Escreveu `val-17` fortalecendo os pontos fracos: 40 requisições forçadas a coexistir em voo via handler-barrier (`inFlight=40`, 40/40 OK) — prova robusta de ausência de semáforo; e uma contraprova de goroutine mostrando que um CB COM limites sobe exatamente +1 (ticker) e volta ao baseline após `Stop()`, validando que a métrica `NumGoroutine` é sensível o bastante. Repositório intocado (HEAD 5e9ffe8). **Ressalvas (corrigidas na re-execução, sem alterar o veredito):** (1) o "teste de concorrência" do executor era ilusório — cada `Do()` era lançado em goroutine mas imediatamente aguardado num `select`, tornando as 5 chamadas efetivamente seriais (não prova ausência de semáforo, pois 5 chamadas seriais passariam com um semáforo de capacidade 1); suprido com o barrier de 40 requisições reais. (2) A medição `NumGoroutine` (3→3) com sleep de 50ms é, em tese, frágil; mas a contraprova (+1 do ticker) demonstra que a métrica de fato detecta um ticker.

**Conclusão:** O modo ilimitado dispensa semáforo e token bucket, não cria goroutines extras e torna `Stop()` um no-op idempotente. As conclusões do executor estavam corretas, apenas sub-potentes até a re-execução do validador.

---

### Cenário 18 — EDGE: Stop() concorrente/duplo é idempotente

**Referência:** finding stop-idempotente (severidade: edge).

**Hipótese:** Chamar `Stop()` múltiplas vezes, inclusive de forma concorrente (32 goroutines simultâneas), num circuit breaker com rate limit (`tokenStop != nil`) é idempotente: não entra em pânico ("close of closed channel"), não trava (deadlock) e não gera DATA RACE, graças ao `sync.Once` (`stopOnce`) que protege o `close(tokenStop)`.

**Método:** Módulo Go externo importando o pacote via replace local, executado com `go test -race`. Cria um breaker com rate limit; 32 goroutines sincronizadas por uma barreira (`sync.WaitGroup`) chamam `cb.Stop()` ao mesmo tempo. Um watchdog `time.After(10s)` detecta deadlock e chamadas seriais posteriores checam idempotência; um bloco de contraste repete com um breaker SEM rate limit (`tokenStop == nil`, early-return).

**Comando:** `cd .../scratchpad/scn-18 && go test -race -timeout 60s -v ./...` (e também `go test -race -timeout 60s -count=20 ./...`)

**Saída observada:**
```
=== RUN   TestStopIdempotenteConcorrente
    scn18_test.go:56: OK: 32 goroutines chamaram Stop() concorrentemente sem panic/deadlock
    scn18_test.go:76: OK: 32 goroutines chamaram Stop() (sem rate limit) sem panic/deadlock
--- PASS: TestStopIdempotenteConcorrente (0.00s)
PASS
ok  	scn18	1.012s

# Repetição sob -race (count=20):
ok  	scn18	1.017s
EXIT=0
```

**Veredito executor:** CONFIRMADO_OK. O race detector NÃO reportou DATA RACE, não houve panic de "close of closed channel" e o watchdog de 10s nunca disparou (todas as 32 chamadas retornaram), mesmo repetindo 20 vezes. Isso valida que o `sync.Once` (`stopOnce`) serializa o `close(tokenStop)` e que `tokenWaitGroup.Wait()` é seguro para múltiplas goroutines. O caso de contraste sem rate limit (`tokenStop == nil`) também é idempotente via early-return.

**Veredito validador:** CONFIRMA (re-executado). Em `pkg/circuitbreaker.go:158-167`, `Stop()` usa `sync.Once` para serializar o `close(tokenStop)` e `tokenWaitGroup.Wait()` é seguro para múltiplas goroutines; não há deadlock (a goroutine do token retorna no `<-tokenStop` do select, e o envio interno para `tokens` é não-bloqueante via `default`). Re-executou sob `-race` com `-count=50` (EXIT=0) e escreveu `val-18` mais agressivo (25 iterações x 64 goroutines com rate limit, path sem rate limit e Stop concorrente com bucket ativo), todas PASS. **Ponto decisivo contra falso positivo:** adicionou um CONTROLE NEGATIVO com um `Stop()` buggy SEM `sync.Once` submetido ao mesmo padrão de goroutines, que capturou 31 panics de double-close — provando que a metodologia (panic não recuperado derruba o processo + `-race`) realmente detectaria o bug se existisse; logo o PASS do código real é significativo, não tautológico. **Ressalvas menores:** (1) o teste do executor detecta panic apenas de forma implícita (panic não recuperado em goroutine derruba o processo) — semântica correta, mas implícita; o controle negativo tornou-a explícita. (2) O bloco de contraste sem rate limit exercita apenas o early-return trivial. (3) O bloco principal roda uma única iteração; reforçado com carga e repetições muito maiores.

**Conclusão:** `Stop()` é seguro para chamadas duplas/concorrentes — idempotente, sem panic, deadlock ou data race. O controle negativo do validador confirma que o detector acusaria o bug caso o `sync.Once` fosse removido.

---

### Cenário 19 — EDGE: erro NÃO-retryable retorna imediatamente sem retry

**Referência:** finding A5/nonretryable (severidade: edge).

**Hipótese:** Um erro não classificado como retryable (erro genérico, não-`net.Error`, não-`OpError`, não-ECONNRESET/REFUSED, não-`DeadlineExceeded`) faz `Do()` retornar de imediato, sem re-tentar nem dormir 500ms, e o erro original é preservado.

**Método:** Módulo externo `scn19` importando pkg via replace. Um `http.RoundTripper` conta chamadas com atomic e sempre retorna `errors.New("boom")`. CircuitBreaker com `maxRetries=5` e sem rate-limit/semáforo (0,0,0). `Do()` roda numa goroutine medindo tempo, com guarda `time.After(5s)`; ao final verifica número de chamadas, tempo decorrido, resp nil e preservação do erro via `errors.Is`.

**Comando:** `cd .../scratchpad/scn-19 && go test -run TestNonRetryableReturnsImmediately -v -timeout 60s`

**Saída observada:**
```
=== RUN   TestNonRetryableReturnsImmediately
    scn19_test.go:68: calls=1 elapsed=34.975µs err=Get "http://example.test/x": boom resp=<nil>
--- PASS: TestNonRetryableReturnsImmediately (0.00s)
PASS
ok  	scn19	0.003s
```

**Veredito executor:** CONFIRMADO_OK. Em `pkg/circuitbreaker.go`, após `cl.Do()` retornar erro, `recordFailure` é chamado e então `if !isRetryable(err) { return nil, err }` (linhas 141-143) sai imediatamente. Como o "boom" não satisfaz os casos de `isRetryable`, a função retorna já na 1ª tentativa (attempt=0), sem `recordRetry` nem `time.Sleep(500ms)`. Os ~35µs comprovam ausência de backoff e `calls=1` a ausência de re-tentativa. O erro original é preservado: o `http.Client` embrulha o "boom" num `*url.Error`, mas `errors.Is(err, boomErr)` resolve o unwrap e retorna true — contrastando com o caminho de exaustão, que devolveria `errors.New("request failed after retries")` sem vínculo com o erro original.

**Veredito validador:** CONFIRMA (re-executado). Confirmou três coisas: (1) o código executa `recordFailure` na attempt=0 e então `if !isRetryable(err) { return nil, err }`, saindo antes de `recordRetry`/`time.Sleep`; (2) re-rodou o teste com saída idêntica (calls=1, ~35µs, erro original preservado, sem "request failed after retries"); (3) escreveu um teste com CASO DE CONTROLE usando um `net.Error` com `Timeout()==true` (retryable), que produziu `calls=4` e `elapsed~1.5s` (3 sleeps de 500ms) e erro de exaustão — provando que o harness distingue retry de não-retry e que o caso não-retryable não é um passa-tudo. **Ressalva (imprecisão menor na interpretação, sem impacto no veredito):** o executor afirma que "boom não satisfaz `net.Error`", mas na prática o `http.Client` embrulha o `boomErr` num `*url.Error`, que IMPLEMENTA a interface `net.Error` (`errors.As(err,&net.Error)==true`). O que torna o erro não-retryable não é a ausência de `net.Error`, e sim que `url.Error.Timeout()` e `url.Error.Temporary()` retornam ambos false (delegam ao `boomErr` subjacente, que não tem esses métodos). O desfecho (`isRetryable==false`, retorno imediato) é idêntico. O teste é metodologicamente sólido: sem corrida (goroutine+channel com guarda `time.After`), limiar de 400ms bem separado dos 500ms de sleep, e contagem via atomic.

**Conclusão:** Erros não-retryable fazem `Do()` retornar na 1ª tentativa, sem backoff, preservando o erro original. A única correção é conceitual (o `*url.Error` de fato implementa `net.Error`, mas retorna Timeout/Temporary false), sem alterar o resultado observado.

---

**Estatísticas da faixa.** A faixa "Contrastes e Casos de Borda" reúne 5 cenários — 2 de contraste (15, 16) e 3 casos de borda (17, 18, 19). Todos os 5 receberam veredito **CONFIRMADO_OK** do executor e **CONFIRMA** do validador (5/5 de concordância, 0 revertidos). O validador re-executou de forma independente nos 5 cenários (`validator_reran=true` em 15, 16, 17, 18 e 19). Os 5 trazem ressalvas metodológicas não-fatais: fases sequenciais e `t.Fatalf` fora da goroutine de teste (15), escopo restrito ao lado positivo do contraste (16), teste de concorrência ilusório e medição de `NumGoroutine` frágil (17), detecção implícita de panic e bloco de contraste trivial (18) e imprecisão na interpretação sobre `net.Error` (19) — nenhuma reverteu o resultado. Em 3 cenários o validador ancorou a prova com controles decisivos que descartam falso positivo: controle positivo de race deliberado que o `-race` acusou (15), controle negativo de double-close sem `sync.Once` que capturou 31 panics (18) e caso de controle retryable com `calls=4`/~1.5s de backoff (19).

---

## 8. Auditoria do processo

Base da auditoria: leitura integral de `pkg/circuitbreaker.go` e de `CB.md` (A1–A20), mais **re-execução independente** dos três fatos mais load-bearing da campanha, num módulo externo com `replace` (go1.26 linux/amd64, `-race`). Reproduzi: (a) a data race A1 dispara em `circuitbreaker.go:321` pelos dois caminhos (endpoint `:296` e `::root` `:297`) — o detector está armado neste ambiente; (b) ECONNREFUSED via `*net.OpError` dá `Temporary()=false, Timeout()=false` → CB faz **1** chamada, enquanto um erro Timeout genuíno estoura para **6** chamadas em 2.50s; (c) no POST, `ContentLength=11`, `GetBody!=nil` e `clone.Body==req.Body`, com a 2ª leitura retornando 0 bytes. Ou seja, as âncoras de scn01, scn02 e scn07 se sustentam à minha verificação.

### 8.1 Metodologia — os testes provam mesmo a hipótese?

Na maioria dos cenários, **sim**, e com folga. Mas há um conjunto de fraquezas sistemáticas do EXECUTOR que os validadores tiveram de corrigir:

- **Asserções só-log (PASS não prova nada).** scn01, scn03, scn09 e parte do scn15 usam apenas `t.Logf` nos dois ramos do desfecho; o `--- PASS` não asserta a hipótese — o veredito depende de ler uma linha de log. Um CI que olhe só o exit code passaria mesmo com a hipótese falsa. Os validadores fecharam isso (val-03/val-09 com `t.Fatalf`, val-15 idem). É um padrão recorrente do executor que, sem o validador, geraria falsos "verde".
- **Artefato de transporte falso.** scn01 é o caso mais grave: o `fakeTransport` do executor não modela enforcement de Content-Length e devolve 200 para qualquer corpo, produzindo a conclusão "sucesso silencioso (200,nil)". Num `http.Transport` real, a requisição exata do teste (`strings.NewReader`, ContentLength conhecido) **aborta** com "ContentLength=11 with Body length 0" → `(nil, erro)`. O sucesso silencioso 200 só vale para corpo de comprimento **desconhecido** (chunked). O validador acertou ao rebaixar para PARCIAL. (scn06 tinha o mesmo risco de mock, mas o validador reconfirmou com `httptest.Server` real — mitigado.)
- **"Tempo finito ≠ para sempre".** scn03 e scn12 provam bloqueio ≥2s, não literalmente "para sempre". Suprido corretamente por análise de código + stack trace no `select` de `waitForToken` (val-03). Aceitável, mas o rótulo "para sempre" é, a rigor, inferência de código, não medição.
- **Ausência de grupo de controle no teste original.** scn05 (sem controle térmico/GC — val-05 adicionou CB-por-bloco e o controle fica flat, prova decisiva de que o custo vem do histórico), scn11 (sem config-B fresca como controle positivo — val-11 supriu), scn12 (sem isolamento maxConcurrent=2 — val-12 supriu). Padrão saudável: o validador quase sempre adicionou o controle que faltava.
- **Operacionalização enviesada (scn14).** O executor endureceu a hipótese A16 original ("na prática sem rate limit") acrescentando "SUSTENTADA" e "limitada só pela resolução do runtime", e então refutou essa forma reforçada, empurrando o rótulo para NAO_REPRODUZIDO. Isso mascara que o desfecho central (rate limit anulado: `waitForToken` nunca bloqueou no caso 2e9) É reproduzido. PARCIAL do validador é o rótulo mais fiel.

Onde a metodologia é forte: scn02, scn04, scn05, scn06, scn07, scn10, scn13, scn16, scn19, scn20 têm mecanismo ancorado em linha de código + medição limpa + (em vários) controle negativo. scn07 em particular é exemplar: o contraste "Timeout estoura para 6 / ECONNREFUSED fica em 1" prova que o harness CONSEGUE detectar amplificação, logo o negativo não é teste quebrado.

### 8.2 Qualidade das evidências

- **Saídas reais e números plausíveis.** As durações batem com a física do código (múltiplos de 500ms de backoff em scn08/09/16/20; ~2.5s = 5 sleeps em scn07-contraste; O(n²) monotônico com R²≈0.9992 em scn05). Nada aparenta ser inventado.
- **`-race` onde devido.** scn02, scn08, scn09, scn15, scn17, scn18, scn20 (e, nas re-execuções dos validadores, também 12 e 16). Confirmei que o detector está de fato armado (a race A1 dispara na minha repro independente).
- **Controles positivos do detector (excelente prática).** scn15 (race deliberado em `x++` que o `-race` acusou) e scn18 (Stop() sem `sync.Once` que gerou 31 panics de double-close) tornam as passagens limpas SIGNIFICATIVAS, não tautológicas. Isso eleva muito a qualidade probatória desses dois contrastes.
- **Vigilância com cache do `go test`.** O validador do scn09 detectou valores idênticos ao nanosegundo vindos do cache e re-rodou com `-count=1`. Ponto de atenção: **não há garantia de que todos os outros cenários usaram `-count=1`** — recomendo padronizar `-count=1` em toda a campanha para eliminar risco de cache mascarando variação.
- **Higiene de saída.** As saídas de validação de scn08 e scn10 vazaram tags de formatação (`</reasoning>`, `<parameter ...>`, `</invoke>`) para dentro do JSON. É cosmético e não afeta a substância, mas sinaliza descuido de parsing na coleta das evidências.

### 8.3 Divergências executor × validador

Só há **duas** divergências reais (validador ≠ CONFIRMA); nas outras 18 há acordo pleno (inclusive no NAO_REPRODUZIDO de scn07).

- **scn01 (REPRODUZIDO vs PARCIAL).** Resolução: manter REPRODUZIDO para o **bug-raiz** (retry não rebobina o corpo, ignorando `req.GetBody` que estava disponível — verifiquei), mas **desdobrar o desfecho**: para corpo de comprimento conhecido (`strings`/`bytes.Reader`/`Buffer` — o caso dominante e o que o próprio teste usa) o retry vira ERRO DURO `(nil, erro)`, não "sucesso silencioso 200"; a perda silenciosa com 200 só ocorre em corpo chunked. O veredito único do executor super-generaliza. Adotar a leitura do validador.
- **scn14 (NAO_REPRODUZIDO vs PARCIAL).** Resolução: adotar PARCIAL. O núcleo observável de A16 ("sem rate limit efetivo") é reproduzido; o MECANISMO alegado pelo executor ("reabastecido a taxa altíssima") e a MAGNITUDE ("bilhões/s sustentados") são refutados — a vazão é tetada em ~23k req/s pelo custo O(n log n) das métricas, idêntica a um CB SEM limiter (`maxRequests=0`). Rótulo binário mata a nuance.

Nas demais, o padrão é: validador re-executa, adiciona controle/positivo-controle e CONFIRMA. Divergência de substância, nenhuma.

### 8.4 Cenários com maior risco de falso positivo/negativo

- **scn01 — maior risco de falso POSITIVO** (a parte "sucesso silencioso 200"). Já capturado e rebaixado a PARCIAL. Correto.
- **scn07 — risco de falso NEGATIVO, descartado.** Se ECONNREFUSED fosse retryable, NAO_REPRODUZIDO esconderia um storm real. Verifiquei em go1.26: `OpError(ECONNREFUSED).Temporary()=false/Timeout()=false` → não retenta. Veredito correto. **PORÉM** isto expõe uma inconsistência que a campanha subvalorizou: **`CB.md` §6.5 (tabela) afirma que ECONNRESET/ECONNREFUSED são retryable (✅) e A5 constrói o "retry storm" sobre essa premissa** — o que é FALSO no Go atual, porque o ramo `errors.As(net.Error)` (linha 186-188) captura o OpError antes e retorna `Temporary()||Timeout()=false`, tornando o ramo `errors.Is(ECONNREFUSED)` (linha 195) **código morto**. Ou seja, o storm de A5 só é real para `net.Error` com Timeout/Temporary=true, **não** para "serviço caído / conexão recusada", que é justamente o sinal canônico de outage. Isto deveria ser elevado de "nit colateral" a **correção documentada de CB.md**, não ficar enterrado no `reasoning` do scn07.
- **scn14 — tensão FP/FN.** A refutação endurecida arrisca uma leitura de falso-negativo de A16 ("o rate limit funciona / não há problema"), quando o fato bruto ("sem rate limit no caso 2e9") é um positivo confirmado. Além disso, o **efeito real do clamp de 1ns — o busy-loop de CPU do refiller — NUNCA foi medido** por executor nem validador (ambos mediram só a vazão de `Do()`). Risco de falso-negativo sobre o próprio aspecto que dá nome ao cenário.
- **scn04/scn05 (O(n²)/leak) — FP baixíssimo.** Efeito enorme, monotônico, R²≈1, com controle. Sólido.
- **Cenários detector-dependentes (scn02/15/18) — FP baixo** dado os controles positivos + minha confirmação de que o `-race` dispara.

### 8.5 Nota de confiança

Resumo: **alta**, com ressalvas concentradas em (i) rótulo binário de scn14 e busy-loop não medido *(medido posteriormente na rodada Fable — §12, cenário 22)*; (ii) testes só-log do executor (mitigados pelos validadores); (iii) a correção de CB.md sobre ECONNREFUSED que a campanha achou mas não elevou. Nenhuma dessas ressalvas derruba um veredito de bug efetivamente reproduzido.

---

## 9. Erratas e ações sobre o `CB.md`

A campanha, ao ser adversarial, encontrou pontos onde o próprio `CB.md` precisava de correção ou precisão. **Status: todas aplicadas** (as duas primeiras na edição pós-campanha; a terceira após a rodada Fable — §12 — que mediu o busy-loop):

1. **Erratum §6.5 e A5 (retryabilidade de conexão recusada) — corrigir.** A tabela de `isRetryable` marca `ECONNRESET`/`ECONNREFUSED` como retryable (✅) e A5 constrói o "retry storm" sobre isso. O **cenário 07 prova que é falso no Go atual**: `errors.As(err, &netErr net.Error)` captura o `*net.OpError`/`syscall.Errno` antes e retorna `Temporary()||Timeout()==false`, tornando o ramo `errors.Is(err, syscall.ECONNREFUSED)` (linha 195) **código morto**. O storm de A5 é real apenas para `net.Error` com `Timeout()/Temporary()==true` — **não** para "serviço caído/conexão recusada", que é o sinal canônico de outage. Consequência dupla: (a) o retry storm por conexão recusada não acontece; (b) há um **bug de correção**: a intenção de retentar `ECONNREFUSED` está silenciosamente anulada. Ambos entraram no `CB.md`. **[APLICADA no CB.md §6.5 (erratum) e A5.]** *(Nota: o ramo `*net.OpError` (linhas 190–193) é igualmente morto — comprovação white-box no cenário 24 da §12.)*
2. **Precisão em A2 (modo de falha do retry de corpo).** O corpo é sempre perdido no retry (bug-raiz confirmado), mas o desfecho **bifurca**: corpo de comprimento **conhecido** → erro duro `(nil, "ContentLength=X with Body length 0")`; corpo **chunked** → sucesso silencioso `(200, nil)` com payload vazio. A correção (`GetBody`) resolve os dois. **[APLICADA no CB.md A2 e no §1 do sumário.]**
3. **Precisão em A16.** O rate limit é anulado via **pré-preenchimento** do bucket (`cap = maxRequests`), não pelo refill de 1ns; e o construtor gasta **~26s** pré-enchendo 2e9 tokens (efeito colateral relevante de parâmetros gigantes). **[APLICADA no CB.md A16.]** O *busy-loop* de CPU do ticker, não medido nesta campanha, foi **medido na rodada Fable (cenário 22, §12)**: ~100% de 1 core ocioso, 815× o controle.

## 10. Lacunas de cobertura (cenários 21+ sugeridos)

A auditoria identificou achados do `CB.md` sem cobertura direta nesta rodada:

- **A9 (FakeServer/teste flaky)** — não coberto (o pacote `internal/` não é importável de fora). Cenário 21: ocupar `127.0.0.1:8081`, subir o FakeServer e mostrar o bind falhando em silêncio + GET no processo errado (404). *Observação: a porta 8081 estava, de fato, ocupada nesta máquina durante a campanha — o cenário é reproduzível de imediato.*
- **A16 (busy-loop de CPU)** — medir o consumo de CPU/spin do refiller com `interval=1ns` (perfil ou proxy de contagem de ticks), o efeito que dá nome ao achado.
- **A11 (pico do semáforo ≤ `maxConcurrent`)** — os cenários 09/12 só exercitam `maxConcurrent=1`. Falta um gauge atômico de em-voo com `N=50, maxConcurrent=5` assertando pico == 5.
- **A12 (`isRetryable` table-driven)** — cobertura espalhada (07/16/19/20); falta um teste-tabela único cobrindo cada ramo, incluindo o **ramo morto** da linha 195 achado no cenário 07.
- **A5 (ausência de estado open/half-open e fast-fail)** — nunca demonstrada diretamente; falta um cenário mostrando que após M falhas consecutivas a (M+1)ª ainda tenta (nunca há fast-fail). O *burst* de "~2× `maxRequests` na 1ª janela" (§6.4) também não foi testado.
- **A17–A20 (README, CI, nomes, versionamento)** — fora do escopo de `go test`; exceto "o exemplo do README não compila", verificável por `go build` do snippet.

**Recomendação transversal da auditoria:** padronizar `-count=1` em toda campanha futura — vários cenários já o usaram (05, 07, 08, 15 e re-execuções dos validadores em 10, 16, 17, 18, 20), mas o uso não foi uniforme, e só o cenário 09 flagrou de fato um hit de cache.

## 11. Conclusão

A campanha confirma o núcleo do `CB.md`: os **dois defeitos críticos** (data race em `Metrics()` e corpo de POST perdido no retry) e os **defeitos altos** (deadlock pós-`Stop()`, leak/custo O(n) das métricas, 5xx como sucesso) são **reais e reproduzíveis**, com evidência empírica e re-verificação independente. Os contrastes e casos de borda confirmam que as partes corretas do componente (segurança concorrente do Manager e do `Stop`, retry de GET, modo ilimitado, idempotência do `Stop`, curto-circuito de erro não-retryable) funcionam como esperado — o que dá credibilidade ao conjunto (o detector e o harness sabem distinguir defeito de não-defeito).

O maior ganho da validação adversarial foi **corrigir o próprio `CB.md`** no ponto de `ECONNREFUSED` (código morto, storm inexistente para conexão recusada) e **precisar** os modos de falha de A2 e A16 — evitando que o time atue sobre um modelo mental errado do comportamento em produção. *(Complemento da rodada Fable, §12: o busy-loop de A16 foi medido — ~100% de 1 core — e os cenários 21–27 fecharam as lacunas listadas na §10.)*

Os módulos de reprodução ficam preservados nos diretórios de scratch `scn-01`…`scn-20` (executores) e `val-01`…`val-20` (validadores) e podem ser promovidos a testes de regressão do repositório (num pacote `_test` externo ou white-box, sem tocar o contrato) — recomendado especialmente para os cenários 01, 02, 03 e 20, que travam regressões dos bugs mais graves.

---

*Documento gerado a partir de execução real de testes Go, validação adversarial independente e auditoria de processo. Companion de [`CB.md`](CB.md).*

---

## 12. Rodada Fable (2026-07-09) — re-verificação integral, cenários novos 21–27 e auditoria documental

Rodada executada pelo modelo Fable 5 em três frentes: (a) re-execução dos 20 cenários da campanha original a partir dos módulos preservados no scratchpad (executores e validadores), sem qualquer modificação; (b) sete cenários novos (21–27) cobrindo findings ainda não testados empiricamente (A9, A16, A11, A12, A5, §6.4, A17), cada um com executor e validador adversarial independente; (c) auditoria de consistência interna e fidelidade ao código dos documentos CB.md e CB-TESTES.md.

### 12.1 Re-verificação dos 20 cenários originais

**Resultado: 20/20 CONFIRMA.** Todos os módulos preservados compilaram e rodaram sem nenhuma correção (nem `go mod tidy`); `-race` foi usado onde a campanha original indicava. As únicas variações observadas foram flutuações numéricas benignas (microtimings, portas efêmeras de `httptest`, razões de degradação dentro da faixa registrada) — nenhuma conclusão mudou.

| Cen. | Status | Nota da re-execução |
|---|---|---|
| 01 | CONFIRMA | Retry reenviou corpo VAZIO e Do() reportou 200 (mock); validador: com transport real e Content-Length o retry aborta duro ("http: ContentLength=11 with Body length 0"); com body chunked (sem GetBody) a perda é silenciosa mesmo em transport real. 4/4 PASS. |
| 02 | CONFIRMA | DATA RACE reproduzido em repsRatio/slices.SortFunc (circuitbreaker.go:321) sob `-race`, pelos caminhos recordAttempt e recordSuccess; FAIL esperado nos dois módulos (exit=1); binário pré-compilado ignorado (recompilado do fonte). |
| 03 | CONFIRMA | 2º Do pós-Stop segue bloqueado >2 s em waitForToken (chan receive, circuitbreaker.go:174); detalhe menor: frame `Do` ausente do stack (inlining), mas a prova exigida (waitForToken + chan receive) está presente. |
| 04 | CONFIRMA | len(StartTimeRequests)=5000==N no endpoint e no ::root, sem poda; validador: linearidade exata para N=100/1000/5000 e retenção pós-GC de +1,37 MB para N=10000. |
| 05 | CONFIRMA | Custo/req cresce 84,07→638,31 µs (7,59× vs ~8× registrado — flutuação, conclusão preservada); validador: acumulado 18,60× com R²=0,9967 vs controle flat 1,71×. Sem `-race`, como no original. |
| 06 | CONFIRMA | HTTP 500 contado como sucesso (Successful=1, Failed=0) no endpoint e no ::root, tanto com transport fake quanto com servidor HTTP real (porta efêmera 127.0.0.1:37833). |
| 07 | CONFIRMA | NAO_REPRODUZIDO mantido: ECONNREFUSED (3 variantes) gera exatamente 1 chamada sem retry; timeout genuíno gera 6 chamadas + "request failed after retries"; dial recusado real confirma a classificação. |
| 08 | CONFIRMA | Cancel a ~50 ms ignorado pelo backoff fixo de 500 ms: Do retornou em 1.501 ms com calls=3 (err=context canceled); validador determinístico (4 runs) + controles em µs; zero data race sob `-race`. |
| 09 | CONFIRMA | Com maxConcurrent=1, B esperou 1,452 s (slot retido durante retries/sleeps de A); serialização estrita (bFirst 46 µs após aLast); controle conc=2 intercala (B em 57 µs). Sem `-race`, como no veredito registrado. |
| 10 | CONFIRMA | Vazamento de goroutines do ticker: Δ=+200 exato após criar 200 CBs, retorno a 2 após Stop; validador: Δ=+150 via runtime.Stack (startTokenBucket.func1), 100/100 sobrevivem a 5× GC. |
| 11 | CONFIRMA | Manager ignora parâmetros divergentes: cb1==cb2==cb3 (mesmo ponteiro, %p e reflect); config B descartada (req2 bloqueada ~2 s sob os limites da config A; controles isolam a causa). |
| 12 | CONFIRMA | Client sem timeout + servidor mudo: B não adquire slot em 2 s (A retém o slot para sempre); validador: hits=1, e controle maxConcurrent=2 libera B (hits=2) — bloqueio é do semáforo. |
| 13 | CONFIRMA | FailedRequests(6) > TotalRequests(1): cancelamento de ctx em waitForToken chama recordFailure sem recordAttempt; violação também no ::root (validador: 5>1; diferença 6 vs 5 é só o nº de chamadas cancelantes). |
| 14 | CONFIRMA | Duas partes mantidas: vazão real ~23,6–24,0k req/s (não bilhões — gargalo é o custo do próprio Do, latência 7,9→163 µs/chamada) e rate limit ANULADO (clamp 1 ns + pré-fill 2e9, zero bloqueios); construtor 25,6–27,0 s; controle são (10/1s) bloqueia após 10. |
| 15 | CONFIRMA | Manager + Stop concorrentes sem race/panic sob `-race` (creates=gets=stops=9600); controle positivo `raceproof` dispara DATA RACE — detector comprovadamente armado. |
| 16 | CONFIRMA | GET sem corpo re-tentado com sucesso na 3ª tentativa (calls=3, ~1,0 s = 2×500 ms de backoff); métricas Total=3/Success=1/Failed=2/Retry=2 idênticas ao registro (endpoint e root). |
| 17 | CONFIRMA | Modo ilimitado: nenhuma goroutine extra, 5/5 Do OK, Stop() idempotente (2×, e 10× no validador) e Do pós-Stop funcional; idêntico com e sem `-race` (só a porta efêmera variou). |
| 18 | CONFIRMA | Stop concorrente/duplo sem panic/deadlock/race (32 e 64 goroutines, com `-race`); contraste sem sync.Once reproduziu exatamente 31 panics de double-close — mesmo número do registro. |
| 19 | CONFIRMA | Erro genérico não-retryable → exatamente 1 chamada, retorno em µs (guarda <400 ms), causa "boom" preservada via errors.Is; classificação confirmada (url.Error: Timeout=false/Temporary=false); controle retryable: 4 chamadas/1,5 s. |
| 20 | CONFIRMA | Exaustão de retries perde a causa original ("request failed after retries"; errors.Is/As=false, Unwrap=nil); RetryCount=2, Failed=3, elapsed ~1,0 s; controle não-retryable preserva a causa (calls=1). |

### 12.2 Cenários novos (21–27)

Sete cenários novos, todos com módulo externo próprio (go.mod com `replace` para o repo) e validador adversarial independente que re-executou o experimento e adicionou controles. Nenhum arquivo do repositório foi modificado. **Vereditos líquidos: 5 REPRODUZIDO (21, 22, 24, 25, 27), 2 CONFIRMADO_OK (23, 26); validação independente: 7/7 CONFIRMA.**

### Cenário 21 — [A9] FakeServer: bind silenciosamente falho + porta ocupada → teste consulta processo errado

**Hipótese.** Se a porta está ocupada, um `http.Server` cujo erro de ListenAndServe é descartado (padrão `_ = f.server.ListenAndServe()` em internal/fakeserver.go:141) não sobe e falha em silêncio; o GET do teste atinge o OUTRO processo que ocupa a porta — mecanismo da falha flaky "Expected status 200, got 404" em TestFakeServer_HandlesRequests (porta fixa 8081).

**Método.** (a) Mecanismo em módulo próprio: servidor A ocupa a porta respondendo 404 com header `X-Server:A`; servidor B (que responderia 200) tenta ListenAndServe na mesma porta com o erro desviado a um canal apenas como prova; asserções gate-adas com t.Fatalf (EADDRINUSE + GET→404/X-Server=A). (b) Direto no repo (read-only): `ss -ltnp` mostrou a 127.0.0.1:8081 JÁ ocupada por um nginx alheio; o teste real do repo foi executado com a porta ocupada. Validador: re-executou tudo e adicionou os controles que faltavam com `fakeserver.go` copiado byte a byte (sha256 idêntico `5d12d805...38b2`): Controle A (porta livre → 200) e Controle B (porta pré-ocupada, FakeServer REAL não sobe, GET recebe 404 do ocupante).

**Comando.**
```bash
ss -ltnp | grep ':8081'
cd /home/diego/projetos/sagace/helpers/circuit-breaker && go test -count=1 -run TestFakeServer_HandlesRequests ./internal/ -v -timeout 120s
cd .../scratchpad/scn-21 && go test -count=1 -v -timeout 300s ./...
```

**Saída real (excerto).**
```text
[ss] LISTEN 0 4096 127.0.0.1:8081 0.0.0.0:*
[curl -i localhost:8081/teste] HTTP/1.1 404 Not Found | Server: nginx

[teste REAL do repo, 8081 ocupada]
Servidor falso ouvindo em http://localhost:8080
    fakeserver_test.go:25: Expected status 200, got 404
--- FAIL: TestFakeServer_HandlesRequests (2.02s)   EXIT=1

[módulo do cenário]
mecanismo_test.go:64: bind de B falhou em silêncio (...): listen tcp 127.0.0.1:46409: bind: address already in use
mecanismo_test.go:80: GET /teste → 404, X-Server=A (...) — B nunca subiu e o GET atingiu o processo errado
--- PASS (2 testes)   ok  x  2.262s

[val-21, controles]
--- PASS: TestControleA_PortaLivre_FakeServerRespondeu200 (3.00s)
control_test.go:97: controle B: porta 40703 ocupada → FakeServer real não subiu (erro engolido) e o GET recebeu 404 do ocupante em 553.844µs
--- PASS: TestControleB (2.00s)   ok val21/internal 5.007s
```

**Vereditos.** Executor: **REPRODUZIDO**. Validador: **CONFIRMA** (re-executou; verificou a pré-condição de forma independente; supriu a falta de grupo de controle — Controle A prova que com porta livre o mesmo fluxo dá 200, Controle B exercita o `FakeServer` real).

**Conclusão.** Mecanismo do flaky completo e causal: erro de bind engolido pelo `_ =` → servidor inexistente → o sleep de 2 s do teste não detecta nada → o GET é atendido por quem quer que ocupe a 8081. Agravantes: porta fixa 8081 sem checagem de disponibilidade/readiness, e o log de Listen() é hardcoded "ouvindo em http://localhost:8080" — mente sobre a porta (o teste usa 8081). Nuance: nesta máquina (nginx permanente na 8081) a falha é determinística; o caráter "flaky" se manifesta entre máquinas/momentos com a porta livre vs ocupada. Correções óbvias (fora de escopo, contrato congelado): propagar o erro de ListenAndServe, usar listener `:0` e sinalizar readiness em vez de sleep.

### Cenário 22 — [A16] Busy-loop de CPU do refiller com interval=1ns

**Hipótese.** Com maxRequests > windowSeconds·1e9, o intervalo do token bucket clampa para 1 ns (pkg/circuitbreaker.go:55-57, divisão inteira → 0 → clamp) e a goroutine do refiller (`time.NewTicker(1ns)`) consome CPU significativa mesmo com o breaker ocioso.

**Método.** Módulo externo; CPU medida via `syscall.Getrusage(RUSAGE_SELF)` (Utime+Stime / wall × 100) em janela de 3 s. CONTROLE medido primeiro (10 req/1s, intervalo 100 ms; gate de ambiente: falha se >5%); depois BUSY: `NewCircuitBreaker(0, 2e9, 1, 0)` com breaker totalmente ocioso. Gates reais: t.Fatalf se busy <10% de um core ou <100× o controle. Validador: re-executou tal-qual e fechou a lacuna de atribuição em val-22 — limiar MÍNIMO do clamp (maxRequests=1e9+1) e medição pós-Stop() (que mata só o refiller, mantendo vivo o canal de 1e9+1).

**Comando.**
```bash
cd .../scratchpad/scn-22 && go test -count=1 -timeout 300s -v ./...
```

**Saída real (excerto).**
```text
busyloop_test.go:50: CONTROL (10 req / 1s, interval=100ms): idle CPU over 3s = 0.123% of one core
busyloop_test.go:73: BUSY breaker constructed in 26.836673338s (pre-fill of 2e9 tokens)
busyloop_test.go:81: BUSY (2e9 req / 1s, interval clamped to 1ns): idle CPU over 3s = 100.321% of one core
busyloop_test.go:84: busy/control CPU ratio: 815.1x
--- PASS: TestScenario22RefillerBusyLoop (33.24s)   ok scn22 33.243s

[rerun do validador: control 0.174% | busy 100.414% | ratio 577.5x | construção 26.88s]
[val-22, atribuição causal] maxRequests=1e9+1: construção 14.46s; goroutines 2→3;
BUSY idle CPU: 100.376% of one core | AFTER Stop(): 0.005% of one core (ratio 604x)
--- PASS (24.06s)   ok val22 24.068s
```

**Vereditos.** Executor: **REPRODUZIDO**. Validador: **CONFIRMA** (reprodução independente + atribuição causal: 100,4% → 0,005% após Stop() prova que a queima é da goroutine do refiller, não de GC nem do canal gigante).

**Conclusão.** Dois custos distintos confirmados: (1) ~1 core inteiro queimado permanentemente pela vida do breaker (invisível funcionalmente — bucket cheio só dropa tokens), 577–815× o controle; (2) construtor bloqueando 14–27 s no pré-fill do canal. Nota de escopo do validador: o busy-loop não é exclusivo do clamp — maxRequests=1e9 exato dá interval=1ns sem clamp; a correção precisa de piso de intervalo com refill em lotes, não apenas rejeitar o caso clampado.

### Cenário 23 — [A11] Pico do semáforo respeita maxConcurrent (gauge atômico, N=50, cap=5)

**Hipótese.** Com maxConcurrent=5 e 50 goroutines simultâneas, o pico de requisições em voo no transport nunca excede 5; e um contexto expirando/cancelado enquanto espera slot retorna ctx.Err() sem tocar o transport.

**Método.** Transport fake com gauge atômico (inFlight incrementado no RoundTrip, pico via loop CAS, contagem de entradas, bloqueio até `close(release)`). Teste 1: 50 goroutines, regime mantido 300 ms; asserções pico==5, entered==50, sucessos==50. Teste 2: semáforo cheio com 5 requisições presas no transport; 6ª chamada com ctx de 50 ms → errors.Is(DeadlineExceeded), entered congelado. `-count=1 -race`, 5 execuções. Validador: re-executou 4×, adicionou controle negativo (maxConcurrent=0 → pico=50, provando que o instrumento detecta violação), testou cancel() explícito (context.Canceled imediato) e estressou GOMAXPROCS {1,2,8} × `-count=3 -race` (9 execuções).

**Comando.**
```bash
cd .../scratchpad/scn-23 && go test -count=1 -race -timeout 60s -v ./...
```

**Saída real (excerto).**
```text
scn23_test.go:135: pico em voo=5 | entradas no transport=50 | sucessos=50/50
--- PASS: TestSemaphorePeakRespectsMaxConcurrent (0.31s)
scn23_test.go:213: extra retornou context deadline exceeded em 50ms sem tocar o transport (entradas=5)
--- PASS: TestCtxDeadlineWhileWaitingForSlot (0.05s)
ok scn23 1.370s   [5 execuções: 1.372s / 1.371s / 1.373s / 1.372s / 1.370s — todas PASS]

[val-23] CONTROLE sem semáforo: pico=50 — PASS | EXPERIMENTO: pico=5 — PASS
cancel explícito: Do retornou context canceled em 0s — PASS
GOMAXPROCS=1/2/8 com -count=3 -race: ok val23 4.343s/4.340s/4.344s (sem data race)
```

**Vereditos.** Executor: **CONFIRMADO_OK**. Validador: **CONFIRMA** (instrumento à prova de escalonamento: qualquer goroutine que furasse o semáforo ficaria presa no RoundTrip inflando o gauge).

**Conclusão.** Um dos caminhos CORRETOS do circuit breaker: em pkg/circuitbreaker.go:105-113, o canal com buffer maxConcurrent + `select { case cb.sem <- ...: case <-req.Context().Done(): ... }` limita a concorrência a exatamente 5 (pico nunca 6+, capacidade não subutilizada) e a espera de slot observa o contexto (deadline E cancel explícito retornam sem tocar o transport). Nota lateral: falha pré-slot não entra nas métricas (não é violação da hipótese). Ressalva conhecida de campanhas anteriores permanece: o slot é retido durante os sleeps de backoff — reduz throughput, mas não viola o limite testado aqui.

### Cenário 24 — [A12] Tabela efetiva de isRetryable via caixa-preta (inclui ramo morto)

**Hipótese.** A classificação EFETIVA de retryabilidade difere da aparente: todo erro entregue pelo http.Client chega embrulhado em `*url.Error`, que implementa net.Error e decide SEMPRE no 1º ramo de isRetryable; os ramos `*net.OpError` e `errors.Is(ECONNREFUSED/ECONNRESET)` são código morto.

**Método.** Caixa-preta: countingTransport que sempre retorna o erro-alvo; maxRetries=1 → 1 chamada = não-retryable, 2 = retryable. Tabela de 9 erros (OpError dial/ECONNREFUSED, OpError read/ECONNRESET, Errno puro, os.ErrDeadlineExceeded, net.Error custom Timeout/Temporary, fmt-wrap de timeout, errors.New, url.Error+timeout). Validador: re-executou fiel; adicionou controles de escala do proxy (maxRetries=0/1/2 → 1/2/3 chamadas, 33,8 µs/500,8 ms/1,0016 s), verificou o envelope REAL de cl.Do, e fez prova direta white-box em cópia byte-idêntica de circuitbreaker.go (sha256 `ae04fc6e...`) com perfil de cobertura (covermode=count).

**Comando.**
```bash
cd .../scratchpad/scn-24 && go mod tidy && go vet ./... && go test -count=1 -v -timeout 60s ./...
```

**Saída real (excerto).**
```text
retryable_test.go:77: syscall.Errno implementa net.Error: errors.As=true; ECONNREFUSED.Timeout()=false Temporary()=false
caso=1_OpError_dial_ECONNREFUSED chamadas=1 efetivo=NAO-RETRYABLE | pós-envelope: errors.As(net.Error)=true tipo=*url.Error
caso=2_OpError_read_ECONNRESET  chamadas=1 NAO-RETRYABLE | caso=3_Errno_ECONNREFUSED_puro chamadas=1 NAO-RETRYABLE
caso=4_os_ErrDeadlineExceeded chamadas=2 RETRYABLE (url.Error: Timeout()=true Temporary()=true)
caso=7_fmt_wrap_do_caso_5 chamadas=1 NAO-RETRYABLE  → FAIL da sub-previsão do método (previa 2)
casos 5/6/9: chamadas=2 RETRYABLE (0.50s cada) | caso 8: chamadas=1
FAIL scn24 2.006s (EXIT=1 — só o subcaso 7 diverge da previsão EXTRA do método)

[val-24, cobertura white-box] ramo 1 (186.29,188.3) count=9;
ramo 2 (191.28,193.3) count=0; ramo 3 (195.80,197.3) count=0; ramo 4 (199.44,201.3) count=0; return false count=0
```

**Vereditos.** Executor: **REPRODUZIDO** (hipótese central; a sub-previsão do caso 7 foi refutada empiricamente e o FAIL é dela). Validador: **CONFIRMA**, com evidência mais forte: cobertura mostra 0 execuções dos corpos dos ramos 2/3/4 em 9 invocações realistas.

**Conclusão.** Ramo morto CONFIRMADO: em todos os 9 casos a decisão ocorre no 1º ramo (o `*url.Error` do http.Client implementa net.Error); (1)(2)(3) não retentados, (4)(5)(6)(9) retentados. Achado colateral: até timeout genuíno vira não-retryable se houver wrapper opaco entre o url.Error e o net.Error (caso 7 — url.Error.Timeout/Temporary delegam por type assertion de UM nível). O ramo 4 (os.ErrDeadlineExceeded) também é morto no caminho do cliente. Nuance de enunciado: "inalcançável" vale para todo erro real do caminho http.Client; só erro sintético com Is() custom chamado diretamente alcança o ramo 3 — irrelevante para o contrato. Atenção metodológica registrada: a suíte do executor termina com EXIT=1 por asserir uma previsão errada do próprio método (caso 7) — um gate que leia só exit code interpretaria mal; a tabela pós-erratum do CB.md §6.5 bate 100% com os 9 resultados medidos.

### Cenário 25 — [A5] Ausência de fast-fail: após M falhas consecutivas, a M+1ª ainda tenta

**Hipótese.** Não existe estado open: mesmo após 10 falhas consecutivas, a 11ª chamada Do ainda atinge o transport — nenhum curto-circuito.

**Método.** Transport que SEMPRE falha com net.Error{Timeout:true} e conta chamadas via atomic; maxRetries=0 (1 tentativa por Do). Fase 1: 10 Do falhos (calls==10, FailedRequests==10). Fase 2: 11ª chamada — t.Fatalf se o contador NÃO incrementar (fast-fail = hipótese refutada). Validador: re-executou; VAL-A estendeu para 51 falhas com verificação de incremento exato por chamada (descarta threshold ≤50); VAL-B cobriu o loop de retry (maxRetries=2: 3 Do → 9/9 tentativas no transport); VAL-C adicionou o controle negativo que faltava (falha sem tocar o transport não incrementa o contador).

**Comando.**
```bash
cd .../scratchpad/scn-25 && go test -count=1 -v -timeout 30s ./...
```

**Saída real (excerto).**
```text
scn25_test.go:71: fase 1: 10 falhas consecutivas registradas (FailedRequests=10, transport calls=10)
scn25_test.go:102: fase 2: 11a chamada ATINGIU o transport (calls=11, latencia=16.036µs) — nenhum curto-circuito; ausencia de estado open CONFIRMADA
--- PASS (0.00s)   ok scn25 0.003s

[val-25] VAL-A: 51 chamadas falhas, TODAS atingiram o transport (calls=51) — PASS
VAL-B: 3 Do com maxRetries=2 => 9 tentativas no transport — PASS (3.00s)
VAL-C: falha sem tocar transport manteve o contador (err=context canceled) — PASS
```

**Vereditos.** Executor: **REPRODUZIDO**. Validador: **CONFIRMA** (três vias: re-execução, variações adversariais, corroboração estrutural — o struct em pkg/circuitbreaker.go:20-34 não tem campo de estado open/half-open/threshold; grep por open|half|state|threshold|trip no repo não encontra mecanismo).

**Conclusão.** Apesar do nome, o componente não implementa a máquina de estados closed/open/half-open (Nygard/Fowler): sem threshold de falhas, janela de observação ou cooldown, um backend em colapso continuará recebendo tráfego pleno (limitado só pelo rate limit). É um rate-limiter/retrier com métricas, não um circuit breaker no sentido clássico. Nota do validador: a latência de 16 µs citada é ambígua como evidência — só o contador atômico do transport prova a ausência de curto-circuito, e é nele que o veredito se apoia.

### Cenário 26 — [§6.4] Burst do token bucket: ~2× maxRequests na primeira janela

**Hipótese.** Bucket pré-enchido + reposição contínua permitem ~2× maxRequests na primeira janela (CB.md §6.4): com maxRequests=20 e windowSeconds=2, ≈20 (burst) + ≈20 (refill 1 token/100 ms em 2 s) ≈ 40 sucessos.

**Método.** RoundTripper no-op instantâneo; loop apertado por exatamente 2 s com ctx de 5 ms por chamada (bucket vazio → context.DeadlineExceeded em waitForToken). Gates: razão em [1,7x; 2,2x], sucessos > maxRequests, deadlineFails > 0, RoundTrips == sucessos. 4 execuções. Validador: re-executou e adicionou os controles que faltavam — 2 janelas consecutivas no mesmo breaker (descarta "refill a 2×": a 2ª janela daria ~2× também) e bucket drenado (20 chamadas imediatas consomem o estoque; janela seguinte só refill).

**Comando.**
```bash
cd .../scratchpad/scn-26 && go mod tidy && go test -count=1 -v -timeout 30s ./...
```

**Saída real (excerto).**
```text
burst_test.go:82: janela medida: 2.000172437s | sucessos (token obtido): 40 | falhas por deadline (bucket vazio): 380 | RoundTrips reais: 40
burst_test.go:84: maxRequests=20 | razao medida/maxRequests = 2.00x (esperado ~2x: 20 burst + ~20 refill)
--- PASS (2.00s)   [4 execuções: sucessos 40/39/39/40 → 2.00x/1.95x/1.95x/2.00x; média 1.975x]

[val-26] janela1=39 (1.95x), janela2=21 e 20 (1.05x/1.00x) — PASS
controle bucket drenado: janela pós-drenagem 19 sucessos (0.95x) em 2 runs — PASS
```

**Vereditos.** Executor: **CONFIRMADO_OK**. Validador: **CONFIRMA** (controles provam que o excedente da 1ª janela é exclusivamente o estoque pré-enchido: janelas seguintes convergem para ~1×).

**Conclusão.** CB.md §6.4 validado com precisão: pré-enchimento do canal `tokens` (linhas 62-64) + ticker de window/maxRequests iniciado imediatamente dão ~2× maxRequests na primeira janela (decomposição demonstrada: 20 burst + 19-20 refill; variação 39/40 é a borda do 20º tick). Implicação prática: o upstream pode receber o dobro do rate configurado no primeiro window após a criação (ou após ociosidade ≥ window, que reenche o bucket) — comportamento clássico de token bucket, não um bug, mas relevante ao dimensionar maxRequests contra limites rígidos. Fragilidade menor apontada pelo validador: o ctx de 5 ms expirar entre o token e o RoundTrip poderia causar flake (não ocorreu; `otherFails=0` em todas as execuções).

### Cenário 27 — [A17] Exemplo do README não compila

**Hipótese.** O exemplo de uso do README (`cb.Do(req)` com 1 argumento) falha em `go build` com "not enough arguments in call to cb.Do".

**Método.** Exemplo do README (bloco "Usage Example", linhas 60-89) copiado para módulo externo em duas variantes: (1) `example/` com o FakeServer comentado (internal/ inacessível) e `cb.Do(req)` exatamente como no README; (2) `example_verbatim/` mantendo o import de internal/. Scaffolding mínimo divulgado (o bloco do README começa em `func main()` sem package/imports). Teste gate-ado: falha se o build compilar OU se a mensagem esperada não aparecer. Validador: re-executou, conferiu README.md linha 77 (única chamada `.Do(`) e o contrato em pkg/ports_circuitbreaker.go:6, e adicionou o controle que faltava (val-27: mudando APENAS `cb.Do(req)` → `cb.Do(req, http.DefaultClient)`, vet+build passam — o scaffolding é benigno).

**Comando.**
```bash
cd .../scratchpad/scn-27 && go build -o /dev/null ./example; go build -o /dev/null ./example_verbatim; go test -count=1 -v -timeout 120s .
```

**Saída real (excerto).**
```text
# scn27/example
example/main.go:33:23: not enough arguments in call to cb.Do
	have (*http.Request)
	want (*http.Request, *http.Client)
exit=1

example_verbatim/main.go:12:2: use of internal package github.com/diegoyosiura/circuit-breaker/internal not allowed
exit=1

--- PASS: TestREADMEExampleDoesNotCompile_DoArity (0.04s)
--- PASS: TestREADMEExampleVerbatim_InternalImportInaccessible (0.01s)
ok scn27 0.053s

[val-27, controle] sed 's/cb.Do(req)/cb.Do(req, http.DefaultClient)/' → go vet && go build → exit=0 (compila)
```

**Vereditos.** Executor: **REPRODUZIDO**. Validador: **CONFIRMA** (a aridade de Do é o único erro de tipo na variante corrigível; go1.26.0 linux/amd64).

**Conclusão.** O exemplo publicado não compila por múltiplos motivos: (1) aridade — README usa `cb.Do(req)`, contrato congelado é `Do(*http.Request, *http.Client)`; (2) usa `internal.NewFakeServer`, inacessível de módulos externos (e esse erro de import é reportado ANTES da checagem de tipos, mascarando o de aridade para quem copia verbatim); (3) o bloco nem é um programa completo (sem package/imports). Latente: o usuário precisaria criar um `*http.Client` que o README nunca menciona. O README ainda afirma retry em "connection refused", refutado na campanha (cenário 07). Nenhum usuário consegue seguir o exemplo tal como publicado.

### 12.3 Auditoria dos documentos

#### 12.3.1 CB.md — 7 achados (0 erro, 2 imprecisos, 5 menores)

**Avaliação geral:** documento de alta fidelidade ao código — todas as ~35 referências arquivo:linha verificadas existem e dizem o que o texto afirma; tabela §6.5 e erratum ECONNREFUSED/ECONNRESET corretos; matriz de contadores §6.6 correta nas 5 linhas; fluxograma de Do() fiel; números re-verificados batem (cobertura 87,3%/27,3% reproduzida; tags v0.0.1–v0.0.7 com v0.0.7 == HEAD 5e9ffe8; go 1.25/1.26; build/vet limpos; pkg OK sob `-race`); âncoras e links para CB-TESTES.md resolvem.

- **[impreciso] §1 item 2 (l.22):** o sumário afirma sem qualificação que o retry "reenvia corpo vazio e reporta sucesso" ("corrupção silenciosa"). O próprio A2 (l.309) e o cenário 01 estabelecem que o sucesso silencioso só ocorre para corpo chunked; com ContentLength conhecido o transport real aborta ruidosamente ("http: ContentLength=N with Body length 0"). O repro citado usou mock que não valida Content-Length. *Correção:* adicionar ao item 2 a mesma ressalva do item 5.
- **[impreciso] §7, "Blocos com cobertura zero" (l.247):** as faixas "avgLastNItens/repsRatio guardas e janela (301–307, 317–339)" superdimensionam os blocos zero. Perfil real (go tool cover): em avgLastNItens só 301-303 e 305-307 são zero (304 é coberta); em repsRatio só 317-319, 327-328 e 338-339 são zero — SortFunc (321) e o laço da janela (332-337) SÃO cobertos, coerente com os 78,6% citados no mesmo parágrafo. *Correção:* listar as faixas exatas.
- **[menor] Preâmbulo (l.3):** "~1.100 SLOC" — contagem real dos .go: 927 linhas totais (782 não-vazias), incluindo testes; 15-20% abaixo do citado. *Correção:* "~930 linhas" ou "~800 SLOC".
- **[menor] §6.5 Erratum (l.202):** o ramo 195-197 é citado só como `errors.Is(ECONNREFUSED)`, mas a condição real é `errors.Is(ECONNRESET) || errors.Is(ECONNREFUSED)`. A afirmação de código morto vale para ambos; só a citação está parcial.
- **[menor] §6.5 tabela/erratum (código 199-201):** o ramo `errors.Is(err, os.ErrDeadlineExceeded)` é igualmente inalcançável (os.ErrDeadlineExceeded implementa net.Error com Timeout()==true → o 1º ramo decide antes); resultado efetivo (✅ retry) correto, mas 199-201 tem 0 execuções no perfil de cobertura — confirmado empiricamente pelo cenário 24 desta rodada. *Correção:* anotar que o retry também é decidido pelo 1º ramo.
- **[menor] §2 (l.43):** "68→556 µs/req lá; 68→529 µs/req aqui" — os números registrados no CB-TESTES.md (cenário 05) são 72,6→607,7 (8,4×) e 66,5→571,8 (8,6×), validador 7,33×. Ordem de grandeza e conclusão compatíveis, mas a origem de "68→556" não é verificável. *Correção:* citar a faixa observada (66-73 → 529-648 µs/req, 7,3-8,6×).
- **[menor] A17 (l.415):** "título vende 'fully-featured Circuit Breaker'" — a frase está na descrição de abertura do README (l.8), não no título ("Circuit Breaker and Test Harness (Go)"). Substância da crítica válida.

#### 12.3.2 CB-TESTES.md — 13 achados (1 erro, 4 imprecisos, 8 menores)

**Avaliação geral:** núcleo internamente muito consistente — vereditos da §4 batem um a um com os 20 blocos das §5–7; contagens do sumário (13/5/2 e 18 CONFIRMA/2 PARCIAL) conferem exatamente; as três "precisões" (07/01/14) são coerentes; referências de linha ao código verificadas (circuitbreaker.go 128/148/153/195/215/321, clamp 55–58, manger.go 47–49) corretas. Os problemas concentram-se na §8/§9 frente ao CB.md atual (estado das erratas) e em contradições internas pontuais; nenhum compromete os vereditos ou a evidência empírica.

- **[erro] §10 (l.807):** afirma que "só o cenário 09 documentou explicitamente ter invalidado o cache do go test", mas o próprio documento registra `-count=1` nos comandos dos cenários 05 (l.226), 07 (l.287), 08 (l.320) e 15 (l.583), além das re-execuções de validador nos cenários 10, 16, 17, 18 e 20. Contradição interna direta. *Correção:* "vários cenários já usaram -count=1 (...), mas o uso não foi uniforme — só o 09 flagrou um hit de cache; padronizar -count=1".
- **[impreciso] §5 (l.81):** "Seis se sustentam integralmente; um (01) rebaixado para PARCIAL; um (07) hipótese refutada" soma 6+1+1=8 para uma faixa de 7. *Correção:* "Cinco (02–06) se sustentam integralmente; um (01) rebaixado para PARCIAL; um (07) refutado, com a refutação confirmada pelo validador."
- **[impreciso] §9 errata 1 (l.792), §8.4 (l.777), §8.5 (l.784) vs CB.md atual:** a errata 1 descreve no presente que a tabela do CB.md "marca ECONNRESET/ECONNREFUSED como retryable (✅)" e pede correção — mas o CB.md HOJE já está corrigido (§6.5 marca ❌ com erratum completo na l.202, A5 com "⚠️ Correção validada" na l.347, sumário item 5 com a ressalva). §8.4/§8.5 descrevem um estado que não é mais o vigente; tensão com a §11 (l.813), que dá a correção como feita. *Correção:* marcar a errata 1 como "[APLICADA no CB.md §6.5/A5]" e reescrever §8.4/§8.5 no passado.
- **[impreciso] §9 errata 3 (l.794) e §11 (l.813) vs CB.md A16 (l.408–410):** a errata 3 (pré-preenchimento do bucket, ~26 s de construtor, busy-loop não medido) NÃO foi aplicada no A16 do CB.md, ao contrário do que a §11 sugere ("precisou os modos de falha de A2 e A16" — verdade só para A2). Das três erratas, 1 e 2 aplicadas, 3 não — e o documento não distingue. *Correção:* aplicar a errata 3 no A16 ou corrigir a §11. **Nota desta rodada:** o cenário 22 supre a lacuna empírica — o busy-loop agora FOI medido (100,3–100,4% de um core ocioso; atribuição causal via Stop()).
- **[impreciso] §9 errata 2 (l.793) vs CB.md A2 (l.309) e §1 item 2 (l.22):** a errata 2 já foi aplicada no detalhamento A2 (com nota sobre o mock), tornando o pedido redundante sem registro da aplicação; e a aplicação foi parcial — o item 2 do sumário do CB.md segue com a forma antiga sem nuance (mesmo achado da auditoria do CB.md acima). *Correção:* marcar "[APLICADA no A2; pendente no §1.2 do CB.md]".
- **[menor] §1 item 1 (l.17) e §9 errata 1 (l.792):** citam apenas o ramo `errors.Is(syscall.ECONNREFUSED)` (l.195) como código morto, mas o bloco do cenário 07 (l.302/304) e o erratum do CB.md estabelecem que o ramo `*net.OpError` (190–193) também é morto. Incompleto, não contraditório.
- **[menor] §1 item 3 (l.19) vs §8.3 (l.770):** "~24k req/s" vs "~23k req/s" para o mesmo fato (medições brutas: 23.659 e 24.282 req/s). *Correção:* harmonizar para "~23–24k req/s".
- **[menor] §1 tabela (l.13) vs §4 (l.69):** o cenário 14 é "⚪ Não reproduzido (com nuance)" no sumário, mas seu veredito líquido na §4 é "CONFIRMADO (com precisão)"; o critério (rótulo do executor) não é declarado. *Correção:* nota na tabela do sumário.
- **[menor] §6 (l.567):** "7 findings de defeito confirmados (A6 x2, A7, A8, A10, A15, A13)" — são 7 cenários e 6 findings distintos. *Correção:* "7 cenários confirmando 6 findings".
- **[menor] cenário 15 (l.594–599 e 604):** o bloco de evidência anuncia "[6 execuções...]" mas lista 5; a ressalva do validador reconhece, porém sem anotação inline no bloco. *Correção:* anotar no próprio bloco.
- **[menor] §8.5 (l.784):** "Nota de confiança. Ver campo dedicado." — referência a um "campo" inexistente; resquício do formato estruturado do pipeline que vazou para o documento. *Correção:* remover "Ver campo dedicado."
- **[menor] §2 (l.38) e §8.2 (l.760):** listam `-race` em {02, 08, 09, 15, 17, 18, 20}, mas os blocos dos cenários 12 (l.464) e 16 (l.633) também documentam execuções com `-race` pelos validadores. *Correção:* acrescentar a ressalva.
- **[menor] estrutura §6/§8:** (a) o bloco do cenário 20 aparece fora da ordem numérica (entre 14 e 15), por severidade, sem aviso; (b) as subseções da §8 numeradas "### 1."–"### 5." colidem visualmente com os capítulos "## 1."–"## 11.". Sem headings/links quebrados nem tags vazadas (as menções em l.763 e l.697 são citações intencionais em code span/fence). *Correção:* nota de ordenação na §6 e renumerar §8 como 8.1–8.5.

### 12.4 Balanço da rodada

- **Estabilidade:** 20/20 cenários re-verificados com veredito CONFIRMA e zero correções em módulos preservados — a campanha original é integralmente reprodutível nesta máquina/data (go1.26.0 linux/amd64).
- **Cenários novos:** 7/7 com validação independente CONFIRMA (todas com re-execução e controles adicionais). Defeitos/lacunas novos comprovados: erro de bind engolido no FakeServer + porta fixa 8081 (21, explica o teste flaky), busy-loop de ~1 core do refiller com interval=1ns + construtor de 14–27 s (22, fecha a lacuna "não medido" da errata 3), ramos mortos de isRetryable comprovados por cobertura white-box + wrapper opaco mata retry de timeout genuíno (24), ausência total de estado open/fast-fail (25) e exemplo do README que não compila (27). Comportamentos corretos confirmados: semáforo respeita maxConcurrent com espera sensível a contexto (23) e burst de ~2× maxRequests na primeira janela conforme documentado no §6.4 (26).
- **Documentos:** CB.md de alta fidelidade (2 imprecisões, 5 menores); CB-TESTES.md consistente no núcleo, com 1 erro factual (§10, -count=1), 4 imprecisões concentradas no estado das erratas frente ao CB.md vigente e 8 menores.

> **Nota desta edição:** todas as correções P0–P2 apontadas pela auditoria (§12.3) foram aplicadas a `CB.md` e a este documento nesta mesma edição; a §12.3 permanece como registro histórico do que a auditoria encontrou.

---

## 13. Validação de não-regressão da execução do plano (branch `refactor/optimizations`)

> Após a execução integral do [`PLANO.md`](PLANO.md) (Fases 0, 1, 2, 3 e 5 — 12 commits), um workflow multiagente de não-regressão verificou **24 itens** em 5 frentes: estabilidade da suíte (×5 com `-race`, zero flakes), bugs corrigidos (os repros antigos **falham em reproduzir** os defeitos — race, corpo perdido, hang pós-Stop, custo crescente, busy-loop, backoff surdo), comportamentos preservados (tabela efetiva de retry, 5xx=sucesso, `failed>total`, get-or-create silencioso, burst 2×, ausência de fast-fail — todos **idênticos**), contrato congelado (módulo-sentinela externo compila; interfaces com exatamente 2 e 3 métodos; extensões puramente aditivas) e desempenho (3,2 µs/op vs 61 µs do baseline).

### Veredito Final de Não-Regressão — branch `refactor/optimizations` (HEAD dd9c6ef)

**Data:** 2026-07-09 · **Juiz:** consolidação final da campanha de validação · **Itens avaliados:** 24/24

### Tabela consolidada

| Item | Expectativa (resumo) | Resultado observado | Veredito |
|---|---|---|---|
| `build-vet` | `go build` e `go vet` limpos | BUILD_EXIT=0, VET_EXIT=0 | SEM_REGRESSAO |
| `suite-x5` | 5x `go test -race` verdes, zero flakes | 5/5 ok, tempos estáveis (~1,0s/~7,0s), nenhum DATA RACE | SEM_REGRESSAO |
| `suite-norace` | Suite sem `-race` passa | `ok pkg 5.805s`, exit 0 | SEM_REGRESSAO |
| `bench` | `BenchmarkDo` ~3-5 µs/op (baseline antigo: 61 µs) | 3,24 µs serial, 4,62 µs paralelo, 11 allocs/op | SEM_REGRESSAO |
| `race-metrics` | Zero DATA RACE no snapshot de `Metrics()` | 0 ocorrências em 4 execuções por teste; antes abortava sob `-race` | SEM_REGRESSAO |
| `post-body` | Retry de POST reenvia corpo completo | Servidor viu `["PAYLOAD-123","PAYLOAD-123"]`; FAIL do repro antigo = bug sumiu | SEM_REGRESSAO |
| `hang-pos-stop` | `Do()` pós-Stop retorna `ErrStopped` imediato | Retorno em ~10µs com texto exato; tokens residuais e idempotência de Stop preservados; FAIL do val-03 = bug sumiu | SEM_REGRESSAO |
| `backoff-ctx` | Cancel a ~50ms interrompe backoff (<200ms) | `Do()` retornou em ~51ms com `context canceled`, 1 só chamada ao transport; FAIL do scn-08 = bug sumiu | SEM_REGRESSAO |
| `custo-flat` | Custo por bloco estável ~2-6 µs/req, razão ~1x | Blocos 3,4-5,8 µs/req, razão 0,59-0,6x; `len(StartTimeRequests)=20` após 10k reqs | SEM_REGRESSAO |
| `leak` | Slices do snapshot podados a ≤20 (mudança D4) | len=20 em endpoint e `::root`; contadores exatos (5000/5000); FAIL da asserção antiga = vazamento eliminado | SEM_REGRESSAO |
| `busy-loop` | Construtor <100ms e CPU ociosa ~0 (antes 26s / 100%) | Construtor 13-14ms; CPU ociosa 4,1-4,2%; ambos os gates do bug falharam (NOT REPRODUCED) | SEM_REGRESSAO |
| `tabela-retry` | Classificação retryable idêntica ao baseline (9 casos) | 9/9 idênticos; único FAIL é do palpite do teste (caso 7 wrap `%w`), medição = baseline | SEM_REGRESSAO |
| `econnrefused` | ECONNREFUSED → 1 chamada; timeout → 6 chamadas, texto exato do erro | 3 variantes com 1 chamada cada; timeout 6 chamadas; `"request failed after retries"` byte a byte | SEM_REGRESSAO |
| `5xx-sucesso` | HTTP 500 com err==nil conta como sucesso | Successful=1, Failed=0 no endpoint e `::root` | SEM_REGRESSAO |
| `failed-gt-total` | Cancel na espera de token: Failed>Total (D3) | Failed=6 > Total=1, sem bloqueio (0,18s) | SEM_REGRESSAO |
| `manager-silencioso` | Get-or-create silencioso reusa instância A | Identidade tripla confirmada; rate efetivo continua o de A (req2 bloqueou até deadline) | SEM_REGRESSAO |
| `sem-fastfail` | Sem estado open: 11ª chamada atinge o transport | calls=11, Failed=11; nenhum curto-circuito | SEM_REGRESSAO |
| `burst-2x` | Burst 1ª janela ~2x maxRequests [1,7-2,2x] | Razão exata 2,00x com 380 falhas por deadline (limite exercitado) | SEM_REGRESSAO |
| `get-retry` | GET re-tenta e sucede na 3ª; Total=3/Success=1/Failed=2/Retry=2 | Métricas idênticas, 200 na 3ª tentativa | SEM_REGRESSAO |
| `stop-idempotente` | 32 Stops concorrentes sem panic/deadlock/race | OK nos dois modos, `-race` limpo | SEM_REGRESSAO |
| `modo-ilimitado` | Sem goroutine extra, Do pós-Stop funciona, Stop no-op | 3→3→3 goroutines, 5/5 com 200, duplo Stop ok; coerente com F3 (ErrStopped só com rate limit) | SEM_REGRESSAO |
| `sentinela` | Contrato congelado exato prova por módulo externo | Assinaturas, interfaces (3+2 métodos) e 28 campos intactos; 29º campo é o aditivo | SEM_REGRESSAO |
| `aditivo` | Novidades puramente aditivas | ErrStopped, ErrRetriesExhausted, IManagerLifecycle, IManagerStrict, TokenWaitCancellations — nada removido/alterado | SEM_REGRESSAO |
| `json-tags` | `TestContract` do repo passa | PASS em 0,003s; tags JSON congeladas conferem | SEM_REGRESSAO |

### Destaques

1. **Semântica dos FAILs escrutinada item a item.** Todos os `--- FAIL` reportados (`post-body`/verify, `hang-pos-stop`/val-03, `backoff-ctx`/scn-08, `leak`/scn-04, `busy-loop`/scn-22) vêm de repros antigos escritos para *provar* o bug — a asserção dispara justamente quando o bug NÃO reproduz. A evidência bruta em cada um (corpo íntegro nas 2 tentativas, retorno em ~10µs, cancel em ~51ms, poda a 20 amostras com contadores exatos, construtor em 13ms) confirma que a falha decorre da ausência do bug, não de mudança em comportamento preservado. **Nenhum FAIL é regressão.**
2. **Caso 7 de `tabela-retry` (wrap `%w`):** o FAIL é do palpite embutido no teste (previa retry); o comportamento medido — 1 chamada, não-retryable — é *idêntico ao baseline registrado*. Classificação de retryability 100% preservada (9/9).
3. **Comportamentos preservados por decisão de projeto** — 5xx como sucesso, Failed>Total no cancel de token (D3), manager get-or-create silencioso, ausência de fast-fail, burst 2x na 1ª janela, tokens residuais consumíveis pós-Stop, texto exato `"request failed after retries"` — todos reproduzidos byte a byte.
4. **Contrato congelado provado por três vias independentes:** `var _` de assinatura, implementações mínimas satisfazendo `ICircuitBreaker`/`IManager` (prova de que nenhum método foi adicionado) e reflexão + `TestContract` do próprio repo. Novidades (F3/F7/Fase 3, `TokenWaitCancellations`) são estritamente aditivas.
5. **Ganhos confirmados sem custo comportamental:** ~19x em `BenchmarkDo` (61→3,2 µs/op), custo por request plano (razão 0,59x vs 7-8x antes), construtor ~2000x mais rápido, race do `Metrics()` eliminada.
6. **Ressalva não-bloqueante (tuning, não regressão):** no cenário extremo de 2e9 tokens/s, o breaker ocioso consome ~4% de um core (vs ~100% antes; gates do bug em >=10% e >=100x não dispararam). Se o alvo futuro for residual <1% em configurações extremas, tratar como item de tuning do refiller.

### Veredito final: **SEM_REGRESSAO** (confiança alta)

Justificativa: 24/24 itens verificados com sucesso contra o código novo, incluindo suíte 5x verde sob `-race`, contrato de API congelado provado por módulos externos compilando sem ajustes, e todos os comportamentos preservados por decisão reproduzidos exatamente. Todos os FAILs observados foram analisados individualmente e correspondem a repros de bugs corrigidos (bug ausente = resultado desejado) ou a palpite incorreto de teste com medição idêntica ao baseline. Nenhum item central ficou inconclusivo; nenhuma mudança não-deliberada de comportamento foi detectada. As únicas mudanças de comportamento observadas (poda D4 a 20 amostras, `ErrStopped` pós-Stop, backoff sensível a contexto, corpo reenviado no retry) são exatamente as correções deliberadas da campanha.

---

## 14. Matriz de 50 configurações — validação da Fase 4 e das estatísticas

> Após a implementação integral do plano (incl. Fase 4 opt-in, pacote raiz e tuning do tick), um workflow multiagente executou **50 configurações distintas** (10 executores × 5 configs, todas sob `-race`): clássicas (semáforo, bucket, retries, starvation), manager (get-or-create/strict/lifecycle/concorrente), Fase 4 (abre/fecha/reabre, 5xx, políticas, backoff exponencial, defaultTimeout, inércia), edge (configs degeneradas 1e6/2e9, dois hosts, corpo não-rebobinável) e import via pacote raiz. Cada config assevera as **estatísticas observadas contra o modelo semântico M1–M15** (contadores, `::root`=soma, ratios=contagens, means, `len≤20`, burst/taxa sustentada, fast-fail sem tocar transport/métricas). **Resultado: 50/50 OK, 0 erros, 0 inconclusivos, zero data races/panics/deadlocks.** Destaques: a config 10 bateu a previsão teórica exata do bucket (15 = 10 burst + 5 refills de 200 ms); a 48 tirou 30.911 snapshots concorrentes com tráfego sem uma única race; a 45 construiu o breaker degenerado (2e9) em 78 ms.

# Julgamento da matriz de 50 configurações — circuit-breaker

### Veredito consolidado: CONFORME

50/50 configurações passaram sob `-race` com evidência numérica concreta. Zero DATA RACE, zero panic, zero deadlock em ~30 execuções distribuídas pelos 10 batches (batches com `-count` de 1 a 5). Nenhum resultado ERRO ou INCONCLUSIVO foi reportado pelos executores.

### Tabela das 50 configurações

| id | Veredito | Observação-chave |
|----|----------|------------------|
| 01 | OK | M6/M7/M9: ::root=30 é soma exata de /a(10)+/b(20); ring de 20 amostras respeitado com 30 eventos |
| 02 | OK | 15/15 sucesso sequencial com maxConcurrent=1; mean_successful=0.000020s dentro de M8 |
| 03 | OK | M1: retry conta em total (total=4 para 3 chamadas, 1 retentada); 1 backoff de 500ms exato |
| 04 | OK | M13 integral: texto "request failed after retries" byte a byte, errors.Is/As na cadeia completa |
| 05 | OK | M10: erro genérico não retentado — 1 chamada apesar de maxRetries=5, retorno em 97µs |
| 06 | OK | GetBody rebobinou o corpo: transport viu "PAYLOAD-XYZ" completo nas 2 tentativas |
| 07 | OK | M2: sem WithStatusCodeFailure, 3×500 contam como success=3, failed=0 |
| 08 | OK | M3/M5: failed(4) > total(1) — 4 cancelamentos na espera por token sem tocar transport |
| 09 | OK | M15: pico em voo == 3 exato (semáforo 3, 30 goroutines), estável em 4 execuções |
| 10 | OK | M11 exato: successes=15 = burst 10 + 5 refills; failed==token_wait_cancellations==190 |
| 11 | OK | Janela 1: 39 sucessos (~burst+refill); janela 2: 21 ≈ taxa sustentada 20/s |
| 12 | OK | M14: sobras do bucket atendem pós-Stop; 3ª Do → ErrStopped; Stop() 2× sem panic (nota abaixo) |
| 13 | OK | Modo ilimitado (0,0,0,0): zero goroutines de ticker criadas; 50/50 ok; Stop() no-op |
| 14 | OK | (1,5,1,0) sequencial: nenhuma interferência de semáforo/bucket, 5/5 ok |
| 15 | OK | M10: ECONNREFUSED não retentado — 1 chamada, retorno em 73µs, sentinela via errors.Is |
| 16 | OK | M10 sutil: net.Error embrulhado em fmt.Errorf NÃO é retryable (type assertion direta em url.Error.Err) |
| 17 | OK | Timeout retryable: 2 chamadas (1+1 retry), backoff 500ms, M13 completo |
| 18 | OK | mean_successful=0.05s explicado: amostra inclui espera por token (2 reqs × 250ms / 10) |
| 19 | OK | R1: Client nil → http.DefaultClient sem panic, contra servidor httptest real |
| 20 | OK | M3/M5: deadline na espera por token → failed=1, cancels=1, total=3 (sem incremento) |
| 21 | OK | Manager: 2ª chamada com params diferentes devolve MESMA instância; prova comportamental (deadline 150ms) |
| 22 | OK | Strict: config divergente → erro descritivo + cb==nil; instância e registro originais intactos |
| 23 | OK | Lifecycle: List ordenado, Remove idempotente, Remove de inexistente não panica |
| 24 | OK | StopAll 2× idempotente (M14); registro preservado; sobras atendem, depois ErrStopped |
| 25 | OK | 50 goroutines simultâneas em NewCircuitBreaker: 1 única instância, zero race em 6 execuções |
| 26 | OK | M12: fast-fail ErrCircuitOpen antes de semáforo/token/métricas — contadores congelados |
| 27 | OK | open → half-open (após openFor) → closed em sonda com sucesso |
| 28 | OK | half-open → open em sonda falha, com rearme do openedAt |
| 29 | OK | threshold=1: uma única falha abre o circuito |
| 30 | OK | Sucesso zera consecFails: alternância falha/ok nunca abre com threshold=2 |
| 31 | OK | M2: WithStatusCodeFailure(500) → failed=3 mas resposta devolvida com err==nil, sem retry |
| 32 | OK | 404 com limiar 400 → failed=1, resposta entregue ao chamador |
| 33 | OK | 499 < limiar 500 → conta como sucesso; fronteira exata do limiar respeitada |
| 34 | OK | Status-failure alimenta o breaker: 2×500 abrem circuito; fast-fail não toca transport |
| 35 | OK | WithRetryPolicy custom retenta ECONNREFUSED (que o default não retenta); M13 completo |
| 36 | OK | WithRetryPolicy(false) suprime retry mesmo de net.Error Timeout; erro da tentativa devolvido direto |
| 37 | OK | Backoff exponencial sem jitter: esperas 10+20+40ms confirmadas nos StartTimeRequests |
| 38 | OK | Jitter dentro do intervalo teórico [15,30]ms; estável em 5 repetições |
| 39 | OK | WithDefaultTimeout(60ms) manda sem deadline do chamador; causa DeadlineExceeded via Unwrap (nota abaixo) |
| 40 | OK | Deadline do chamador (25ms) vence o default (60ms) — 25.4ms medidos |
| 41 | OK | Import raiz: tipos intercambiáveis com /pkg em compile-time e runtime |
| 42 | OK | Sentinela ErrCircuitOpen idêntico entre raiz e /pkg (errors.Is cruzado true) |
| 43 | OK | NewManager raiz: interfaces idênticas via alias; StopAll idempotente; ErrStopped igual nos dois caminhos |
| 44 | OK | Pré-fill de 1e6 tokens em 82ms (<1s, limite do clamp M11) |
| 45 | OK | maxRequests=2e9 clampado em 1e6; construtor em 78ms; 10/10 ok com deadline de 5ms |
| 46 | OK | ctx cancela o backoff de 500ms em ~50ms (select ctx.Done vs time.After); retry=1 agendado, M5=0 |
| 47 | OK | M12 inércia: sem WithBreaker, 5 falhas consecutivas não abrem circuito (StateDisabled) |
| 48 | OK | 30.911 snapshots concorrentes de Metrics() iterando slices: zero race — cópia sem aliasing; doCount=65 bate M11 exato |
| 49 | OK | M6 multi-host: ::root de cada host soma só os endpoints daquele host (sem vazamento entre hosts) |
| 50 | OK | M10/D2: corpo não-rebobinável (GetBody==nil) impede retry mesmo com erro retryable e maxRetries=4; retry_count=0 |

### Erros e divergências

**Nenhum erro de produto encontrado.** Duas divergências menores foram escrutinadas e descartadas como defeito:

1. **cfg12 — lacuna documental em M3, não defeito**: `Do` que retorna `ErrStopped` incrementa `failed_requests` (failed=1 sem incrementar total). Verifiquei `pkg/circuitbreaker.go:216-225`: `recordFailure` é chamado para qualquer erro de `waitForToken`, e apenas o contador de `token_wait_cancellations` exclui `ErrStopped` (linha 222). O comentário no código (linhas 217-220) declara essa semântica intencional (D3/R4). M3 enumera as fontes de failed sem citar ErrStopped — recomendação: atualizar o texto de M3 no modelo, sem mudança de código.
2. **cfg39 — comportamento coerente, não defeito**: com `maxRetries=0` e timeout via `WithDefaultTimeout`, o erro externo é `retriesExhaustedError` (porque `url.Error.Timeout()==true` classifica como retryable e o loop exaure em zero tentativas restantes), mas `errors.Is(err, context.DeadlineExceeded)==true` via Unwrap, exatamente o que a config exigia.

**Nota de cobertura**: a lista de batches de races omite a linha do batch3 (cfgs 11-15), mas cada um desses 5 resultados declara individualmente "PASS com -race" — a evidência existe inline e não compromete a conclusão.

### Conclusão sobre a conformidade das estatísticas

As estatísticas do circuit-breaker estão **conformes ao modelo semântico M1-M15** em todos os pontos exercitados:

- **Contadores (M1-M5)**: total conta tentativas (retries inclusos), cancelamentos em espera por token incrementam failed e token_wait_cancellations sem incrementar total (failed > total demonstrado em cfg08/cfg10/cfg20), cancelamento em backoff não conta em M5 (cfg46).
- **Agregação (M6)**: ::root é soma exata dos endpoints por host, sem vazamento entre hosts (cfg01, cfg49).
- **Ratios (M7)** e **médias (M8)**: iguais aos contadores em janela curta; a única média não-trivial (cfg18, 0.05s) foi explicada pelo início da amostra antes de waitForToken.
- **Rings (M9)**: todas as slices Time*/StartTime* limitadas a 20 amostras, mesmo com 30-65 eventos.
- **Retry/erros (M10, M13, D2)**: classificação de retryabilidade, texto congelado do sentinela e cadeias de Unwrap verificados byte a byte; corpo não-rebobinável bloqueia retry.
- **Bucket (M11)**: burst inicial + taxa de refill bateram nas previsões teóricas exatas (cfg10: 15; cfg48: 65); clamp de 1e6 funciona (cfg44/45).
- **Breaker (M12)**: máquina de estados completa (closed→open→half-open→closed/open), fast-fail sem tocar transport nem métricas, inércia sem WithBreaker.
- **Lifecycle (M14)** e **concorrência (M15, M9 sob leitura concorrente)**: Stop/StopAll idempotentes, manager thread-safe, snapshots de métricas sem aliasing — zero races em todas as execuções.

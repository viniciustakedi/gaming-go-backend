# Arquitetura

Este documento explica as decisões, as interpretações e as limitações da carteira de
apostas. A fonte formal das decisões é `.scratch/wallet-challenge/spec.md`; este arquivo
existe para o candidato defender cada uma delas linha a linha numa call de review, com
referências diretas ao código. O `README.md` cobre como rodar e chamar o serviço; este
documento cobre por que ele é assim.

## Dinheiro e o limite de `Money`

`internal/domain/money/money.go` guarda todo valor monetário como `int64` em unidades
mínimas (centavos) mais um código de moeda - nunca ponto flutuante. O valor zero do tipo
Go (`Money{}`, sem moeda) é inválido e toda operação o rejeita.

- **Moedas permitidas**: `isSupported` restringe a `BRL`, `USD` e `EUR` - uma allowlist
  fechada de moedas com duas casas decimais, não um allowlist genérico de ISO 4217. Os
  cenários principais do desafio usam `BRL`; `USD` e `EUR` existem só para os testes de
  incompatibilidade de moeda entre operação e referência.
- **Limite superior**: `MaximumAmount = "92233720368547758.07"`, isto é, `math.MaxInt64`
  centavos. `math.MinInt64` é rejeitado explicitamente na construção (`New`) como
  overflow, porque negar o menor `int64` também é overflow - não existe um `-MinInt64`
  representável.
- **Entrada externa**: `canonicalAmount = regexp.MustCompile(`^(0|[1-9]\d*)\.\d{2}$`)`
  aceita só `(0|[1-9]\d*)\.\d{2}`. Isso proíbe zero à esquerda (`01.00`), sinal, notação
  científica, `NaN`/`Infinity`, espaços e qualquer escala diferente de duas casas -
  cada valor válido tem exatamente uma representação textual, o que é o que permite o
  hash de idempotência usar a string recebida sem normalizar nada (ver "Idempotência"
  abaixo). `parseMinorUnits` detecta overflow dígito a dígito, comparando contra
  `(math.MaxInt64-digito)/10` antes de multiplicar, então nunca passa por `float64`.
- **Contrato JSON**: `MarshalJSON` emite `{"amount":"<decimal>","currency":"XXX"}`.
  `UnmarshalJSON` decodifica `amount` como `json.RawMessage` e exige que ele
  desserialize num `string` Go - um número JSON puro (`"amount":25.00`, sem aspas) falha
  nesse passo e vira `ErrInvalidAmount`, nunca um valor arredondado silenciosamente. O
  decoder usa `DisallowUnknownFields` e confere que não sobra conteúdo após o objeto.
- **Persistência**: toda tabela com valor monetário guarda `BIGINT` (unidades mínimas) e
  `CHAR(3)` (moeda) - o mesmo par, nunca um `NUMERIC`/`DECIMAL` que reintroduziria
  arredondamento fora do controle do domínio.
- Valores negativos só existem em cálculos internos - a `difference` da reconciliação, por
  exemplo - e serializam como `"-5.00"`; nenhuma operação de negócio aceita um `Money`
  negativo como entrada.

## Biblioteca de acesso a dados e delimitação da transação SQL

Não há um ORM. `internal/pg/querier.go` define `Querier`, uma interface mínima
(`Exec`/`Query`/`QueryRow`) que tanto `*pgxpool.Pool` quanto `pgx.Tx` satisfazem. Só os
repositórios de `internal/walletpg` recebem um `Querier` já vinculado pelo chamador - o
pool, para uma leitura sem transação; uma `pgx.Tx`, para uma escrita -, e por isso rodam
identicamente dentro de uma transação HTTP ou de uma transação SQS.
`internal/outboxpg` e `internal/referenceworkerpg` **não** seguem esse desenho: cada
`store` guarda o `*pgxpool.Pool` inteiro e abre sua própria transação quando precisa (ver
abaixo) - eles não são repositórios do caso de uso de carteira, e sim adaptadores de
fila/lease independentes que o caso de uso nunca vê.

- **Cinco lugares chamam `pool.Begin`/`BeginTx`, cada um por um motivo diferente - não
  existe um único lugar que abre transação.**
  - `internal/pg.UnitOfWork.Execute` (`internal/pg/querier.go:58`) é quem abre a
    transação do **caso de uso de carteira**: roda a função recebida contra o `pgx.Tx`, e
    só então decide. Erro → `tx.Rollback`. Sucesso → dispara o ponto de falha nomeado
    `before-commit` (ver "Injeção de falhas" no README) e chama `tx.Commit`; uma falha no
    próprio commit também tenta o rollback. `internal/walletpg`'s `unitOfWork.WithinTx`
    embrulha isso e converte o `Querier` transacional em `walletapp.Repositories` via
    `NewRepositories(q)`. É por aqui que passam `Process` (via `processWithRetry` →
    `runInNewTx`) e `ResumePending`, a entrada HTTP e o worker de referências pendentes.
  - `internal/consumer/consumer.go:268`'s `processOnce` abre sua própria transação
    porque o consumidor SQS precisa que o `INSERT ... ON CONFLICT DO NOTHING` da inbox e
    o efeito do caso de uso comitem juntos, e a inbox não é um repositório de
    `walletapp.Repositories` que o `UnitOfWork` HTTP possa incluir. Ele chama
    `NewRepositories(tx)` sobre essa mesma `pgx.Tx` e então `ExecuteInTx` diretamente -
    nunca passa pelo `UnitOfWork` -, então inbox, carteira, ledger e outbox ainda
    commitam ou revertem juntos, só que quem abre e fecha essa transação é o consumidor,
    não o `UnitOfWork`.
  - `internal/outboxpg/store.go:24`'s `Claim` e `internal/referenceworkerpg/store.go:20`'s
    `Claim` abrem, cada um, sua própria transação curta e independente só para
    reivindicar um lote com `FOR UPDATE SKIP LOCKED` antes de qualquer I/O de fila - nunca
    a transação do caso de uso, e comitam sozinhos antes do publisher enviar ao SQS ou do
    worker de referências processar. As demais operações desses dois `store`s
    (`MarkPublished`, `ScheduleRetry`, `Stats`, `Pending`) rodam direto contra o pool, sem
    transação nenhuma, por serem um `UPDATE`/`SELECT` de uma linha só.
  - `internal/walletpg/ledger_repository.go:61`'s `Reconcile` abre uma transação
    `REPEATABLE READ`/somente leitura para a auditoria de reconciliação: precisa ler o
    saldo da carteira e somar o ledger inteiro como um único snapshot consistente, sem
    que uma escrita concorrente mude o saldo entre as duas leituras. É a única transação
    deste projeto que nunca escreve.
- **A regra real do projeto: o caso de uso nunca abre transação, e recebe os
  repositórios já vinculados a uma.** `internal/walletapp/process_operation.go`'s
  `ExecuteInTx` só executa contra os repositórios que recebe (`Repositories`) - não abre,
  não comita e não sabe se está dentro de uma transação HTTP ou de uma transação de inbox
  SQS; quem decidiu isso foi o chamador, um dos cinco lugares acima.
- **Retry em nova transação, nunca na mesma.** Quando o `INSERT` da transação (passo 5 da
  concorrência) não afeta nenhuma linha - a colisão que `ON CONFLICT DO NOTHING` existe
  para prevenir -, `attempt` devolve `retry=true`, `ExecuteInTx` traduz isso para
  `ErrRetryInNewTransaction`, e é só `processWithRetry` (fora de qualquer transação
  aberta) que decide tentar de novo, numa transação nova, nunca reaproveitando uma que já
  pode estar abortada no lado do Postgres. Uma segunda colisão seguida não tenta uma
  terceira vez - vira `TEMPORARILY_UNAVAILABLE` (`503`), incrementando
  `ObserveConcurrencyConflict()` na primeira falha.

## Idempotência e algoritmo de hash

`internal/domain/operation/hash.go`'s `PayloadHash(request Request)` é a única função de
hash do sistema - HTTP e SQS chamam exatamente a mesma, sobre a mesma struct
(`canonicalPayload`), nunca duas implementações que podem divergir.

- **Campos**: `externalTransactionId`, `gameId`, `kind`, `money`, `playerId`,
  `providerId`, `roundId`, `walletId`, mais `referenceExternalTransactionId` só quando
  não nulo (`omitempty`). A chave de idempotência, o `messageId`, o `type`, o
  `occurredAt`, headers e qualquer outro metadado de transporte ficam de fora de
  propósito - dois canais diferentes entregando a mesma operação de negócio produzem o
  mesmo hash.
- **Canonicalização**: um `json.Encoder` Go com `SetEscapeHTML(false)`, cuja ordem de
  campos é a ordem declarada na struct `canonicalPayload` (mantida alfabética
  deliberadamente, para não depender da ordem de iteração de um `map`); o `\n` que o
  encoder acrescenta é removido antes de hashear. `Money` participa como
  `{"amount":"25.00","currency":"BRL"}`, a mesma string que a validação de `Money` já
  canonicalizou (sem zeros à esquerda, exatamente duas casas) - por isso não há nenhuma
  normalização adicional aqui: a string recebida já é a única representação possível.
  `SHA-256` sobre esses bytes, em hexadecimal.
- **Classificação** (`internal/domain/operation/idempotency.go`'s `ClassifyAttempt`,
  chamada de `lookupExisting` dentro da mesma transação que já tem o lock da carteira):
  mesma chave e mesmo hash → `Replay`; mesma chave e hash diferente →
  `AttemptConflict{ErrIdempotencyKeyReused}`; chave diferente mas
  `externalTransactionId` já registrado → `AttemptConflict{ErrExternalTransactionIDConflict}`;
  nenhum dos dois → `NewAttempt`. Isso acontece **depois** do lock de carteira (passo 2 da
  concorrência), então uma duplicata que chegou por uma instância concorrente e já
  commitou está garantidamente visível aqui - não existe janela onde duas duplicatas
  concorrentes passem as duas como `NewAttempt`.

## Locks e concorrência

Fluxo de uma operação externa (`internal/walletapp/process_operation.go`'s `attempt`),
dentro de uma única transação `READ COMMITTED`:

1. **Lock da carteira**: `internal/walletpg/wallet_repository.go`'s `FindForUpdate` -
   `SELECT id, player_id, currency, balance, version, created_at, updated_at FROM wallets
   WHERE id = $1 FOR UPDATE`. Carteira inexistente vira `WALLET_NOT_FOUND` antes de
   qualquer outra checagem.
2. Classificação de replay/conflito contra o que já está commitado (seção anterior).
3. Resolução da referência e regras de domínio (`operation.Evaluate`/`resolveDecision`).
4. **Backstop de unicidade**: `InsertNew` executa
   `insertWagerTransactionSQL || " ON CONFLICT DO NOTHING"`; `inserted := tag.RowsAffected() == 1`.
   Esse é o anteparo para duas requisições que, por engano do provedor, usaram
   `walletId`s diferentes no corpo para a mesma `(providerId, idempotencyKey)` ou
   `(providerId, externalTransactionId)` - elas não disputariam o mesmo lock de carteira
   do passo 1, então só a unicidade do banco as pega. Se nada foi inserido,
   `processWithRetry` recomeça numa transação nova (seção anterior) em vez de continuar
   numa transação cujo próximo `INSERT`/`UPDATE` já falharia.
5. **Update condicionado à versão**: `UPDATE wallets SET balance=$1, version=$2,
   updated_at=$3 WHERE id=$4 AND version=$5`. `RowsAffected()==0` vira
   `ErrConcurrencyConflict` - na prática inatingível depois do lock `FOR UPDATE` do passo
   1 sobre a mesma linha, mas mantido como defesa em profundidade caso um caminho futuro
   leia a versão fora do lock.
6. Lançamento no ledger e registros de outbox, tudo na mesma transação.

Nenhuma operação toca mais de uma carteira - a referência precisa ser da mesma carteira
que a operação -, então não há ordem de locks entre carteiras a coordenar, e carteiras
diferentes avançam em paralelo sem nunca disputar o mesmo lock. As garantias no banco
(`CHECK (balance >= 0)`, o `UPDATE` condicionado à versão, as unicidades) valem
independentemente do lock: mesmo um bug futuro que pulasse o `FOR UPDATE` não conseguiria
persistir um saldo negativo ou uma duplicata.

## Máquina de estados, falhas transitórias e permanentes

`internal/domain/wallet/wager_transaction.go` define os quatro estados possíveis de uma
transação persistida - `PENDING` só existe em memória, antes do primeiro `INSERT`:

```
PENDING ──► PROCESSED
   │  ├───► REJECTED
   │  ├───► FAILED
   └──────► PENDING_REFERENCE ──► PROCESSED | REJECTED | FAILED
```

`isAllowedTransition` codifica exatamente essas arestas; qualquer transição a partir de
`PROCESSED`, `REJECTED` ou `FAILED` (os três terminais) é rejeitada em memória por
`transition()`, que também exige `failureCode` não vazio em `REJECTED`/`FAILED` e vazio em
`PROCESSED`/`PENDING_REFERENCE`. A mesma regra é reforçada no banco por um trigger
independente do código Go: `wager_transactions_block_terminal_update`
(`migrations/0004_wager_transactions.up.sql`) recusa qualquer `UPDATE` cujo `OLD.status`
já seja terminal, mesmo que um bug futuro no Go tentasse.

- **Processamento síncrono.** Não existe commit intermediário de aceite: uma `BET`,
  `WIN` ou `LOSS` chega a `PROCESSED`/`REJECTED` na própria requisição. O único estado
  não terminal que chega a ser persistido é `PENDING_REFERENCE`, e só quando a operação é
  uma reversão ou um `WIN` referenciando algo que ainda não chegou.
- **Falha transitória** (erro de conexão, timeout, `lock_timeout`/`statement_timeout`
  do Postgres, cancelamento de contexto, serialização, indisponibilidade do SQS): nada é
  persistido, a transação sofre rollback, e quem chamou tenta de novo - HTTP devolve
  `503` com `Retry-After`, o SQS reentrega a mensagem, o worker de referências reagenda
  com backoff. `isTransient`/`isPermanent`
  (`internal/referenceworker/worker.go`) classificam por uma lista explícita de
  permanentes (`ErrInvalidPersistedTransaction`, `ErrCorruptedResultingBalance`) -
  **tudo o mais é transitório por padrão**, para que uma instabilidade de infraestrutura
  nunca vire `FAILED` por engano.
- **Falha permanente** só existe para um `PENDING_REFERENCE` que o worker não consegue
  concluir por um motivo que não é nem regra de negócio nem falha transitória - uma
  reidratação que viola uma invariante de domínio, por exemplo. Nesse caso, e só nesse
  caso, a transação vai para `FAILED` com `failureCode = PERMANENT_PROCESSING_FAILURE`,
  sem nenhum efeito financeiro e com log de erro e métrica - **sem nenhum evento de
  outbox**: a spec só define quatro tipos de evento
  (`WagerTransactionProcessed`/`Rejected`/`PendingReference`, `WalletBalanceChanged`), e
  nenhum deles descreve uma falha de infraestrutura permanente do próprio worker, então
  emitir um evento aqui seria inventar um quinto tipo não documentado. O uso de `FAILED`
  é deliberadamente restrito: nenhum caminho síncrono de HTTP ou SQS o alcança
  diretamente, só o worker de referências pendentes.

**Injeção de falhas só existe com a build tag `faultinject`.** Os cinco pontos nomeados
pela spec (`internal/faultinject.Trigger`, chamado antes/depois de cada commit ou envio
que os cenários de recuperação da suíte multi-instância precisam interromper) são um
no-op puro compilado *fora* do binário em qualquer build normal - `go build`/`go test`
sem `-tags faultinject` não lê `FAULT_INJECT_POINT` nem contém a lógica de
`syscall.Kill` nenhuma vez; o teste `internal/faultinject/faultinject_test.go` confirma
isso mesmo com a variável de ambiente definida. Só o binário compilado explicitamente com
`-tags faultinject` (o que a suíte `test/multiinstance` faz para os próprios cenários de
falha, nunca a imagem Docker de produção) pode matar o processo nesses pontos. Ver
README.md, "Injeção de falhas", para a tabela completa dos cinco pontos e onde cada um
fica no código.

## Referências pendentes

`internal/referenceworkerpg/store.go`'s `Claim` é uma transação curta e independente do
processamento em si:

```sql
WITH ready AS (
    SELECT id FROM wager_transactions
    WHERE status = 'PENDING_REFERENCE' AND next_attempt_at <= now()
    ORDER BY next_attempt_at, created_at
    FOR UPDATE SKIP LOCKED LIMIT $2
)
UPDATE wager_transactions AS t SET next_attempt_at = now() + $1::interval
FROM ready WHERE t.id = ready.id
RETURNING t.id, t.wallet_id, t.attempts
```

`SKIP LOCKED` deixa instâncias paralelas dividirem o lote sem esperar umas pelas outras -
cada uma pega linhas que a outra ainda não tocou. O mesmo `UPDATE` que reivindica o lote
já empurra `next_attempt_at` pela duração do lease (`REFERENCE_WORKER_LEASE`, 30s por
padrão): isso *é* o mecanismo de lease inteiro - não existe uma coluna `locked_by` ou
`locked_until` separada aqui. Se a instância que reivindicou cair antes de terminar, a
linha simplesmente volta a satisfazer `next_attempt_at <= now()` quando o lease expira, e
qualquer instância viva a reivindica de novo - uma queda nunca abandona um
`PENDING_REFERENCE` confirmado.

Processamento (`internal/walletapp/process_operation.go`'s `ResumePending`), na mesma
ordem de locks do fluxo síncrono - carteira primeiro, então a transação:

- Reidrata a transação pendente com `FOR UPDATE`, confirma que ainda está
  `PENDING_REFERENCE` e reavalia a decisão contra o estado atual da referência.
- **Resolvida e processável ou rejeitável** → conclui (ledger, saldo e outbox se for
  processar; `REJECTED` com o código correspondente se a referência já é definitivamente
  inválida) na mesma transação.
- **Ainda não existe, e nem os tentativas nem o TTL se esgotaram** →
  `ReschedulePending`: `UPDATE wager_transactions SET attempts = attempts+1,
  next_attempt_at = now() + $interval, updated_at = now() WHERE id = $1 AND status =
  'PENDING_REFERENCE'`, com o atraso calculado por
  `backoff.Exponential(attempts, RetryBase, RetryMax)` mais jitter
  (`internal/referenceworker/worker.go`).
- **Tentativas ou TTL esgotados**: rejeita com `REFERENCE_NOT_FOUND` se a referência nunca
  chegou, ou `REFERENCE_NOT_PROCESSED` se ela existe mas continua pendente/foi rejeitada -
  a mesma distinção que um caminho síncrono usa.

Configurável via `REFERENCE_WORKER_*` (`internal/config/config.go`): `PollInterval`
(250ms), `Lease` (30s), `BatchSize` (10), `RetryBase` (1s), `RetryMax` (1m),
`MaxAttempts` (10), `TTL` (24h), `ShutdownTimeout` (5s) - todos encurtados pelos testes
para não esperar minutos reais por um cenário de expiração. `pending_expires_at` (o TTL)
é calculado inteiramente no Postgres (`now() + $ttl::interval`, na própria transação de
`InsertPending`), nunca no relógio do processo Go - duas instâncias com o clock do sistema
operacional levemente desalinhado ainda concordam sobre quando uma pendência expira,
porque nenhuma delas faz essa conta localmente.

**Correlação de eventos emitidos pelo worker**: `correlationId` e `causationId` dos
eventos que o worker produz (`WagerTransactionProcessed`/`Rejected`/`WalletBalanceChanged`
resultantes de uma resolução tardia) são os dois o próprio id da transação pendente
(`writeOutboxEvents(ctx, repos, transaction, walletValue, entry, transaction.ID(),
transaction.ID(), now)` em `rejectPending`/`processPending`) - interpretação adotada
porque o `correlationId` da requisição HTTP ou da mensagem SQS original que criou a
pendência não é persistido por nenhum ticket. No consumidor SQS, por comparação,
`correlationId` e `causationId` são os dois o `messageId` da mensagem em processamento -
uma convenção diferente, também documentada, porque ali a origem (a mensagem) ainda está
disponível no momento do evento.

## Matriz de reversões

| Tipo | Movimento | Referência | Reversão que aceita |
| --- | --- | --- | --- |
| `BET` | Débito | Proibida | uma única `REFUND` **ou** `ROLLBACK`, nunca as duas |
| `WIN` | Crédito | Opcional (`BET` do mesmo round, se informada) | um único `ROLLBACK` |
| `LOSS` | Nenhum | Proibida | nenhuma |
| `REFUND` | Crédito | Obrigatória (`BET`) | um único `ROLLBACK` (volta a debitar) |
| `ROLLBACK` | Contrário ao original | Obrigatória (`BET`, `WIN` ou `REFUND`) | nenhuma |

A garantia não vive só na lógica do caso de uso - dois índices únicos parciais em
`migrations/0004_wager_transactions.up.sql` a impõem no próprio schema, contra qualquer
caminho de escrita, presente ou futuro:

- `wager_transactions_opening_per_wallet_idx`: `UNIQUE (wallet_id) WHERE kind = 'OPENING'
  AND status = 'PROCESSED'` - um crédito inicial por carteira, mesmo sob abertura
  concorrente.
- `wager_transactions_reversal_per_reference_idx`: `UNIQUE (reference_transaction_id)
  WHERE kind IN ('REFUND', 'ROLLBACK') AND status = 'PROCESSED'` - a *referência interna
  resolvida*, não o id externo, e só conta uma reversão que de fato **processou**; uma
  `REJECTED` (por exemplo, uma segunda tentativa que perdeu a corrida) não colide aqui,
  que é exatamente o "uma REJECTED passa" que a spec pede.

`ROLLBACK` de `REFUND` debita de novo e a aposta original continua valendo - ela não pode
ser reembolsada uma segunda vez, porque o índice acima já bloqueou o `REFUND` original
como a única reversão bem-sucedida daquela `BET`. `ROLLBACK` de `ROLLBACK` ou de `LOSS`
nunca chega a ser aceito: a tabela de regras acima rejeita a referência antes mesmo de
tentar resolvê-la (`REFERENCE_KIND_NOT_REVERSIBLE`).

**Limitação aceita e documentada pela spec**: um `WIN` que referencia uma `BET` já
revertida não é bloqueado. A referência de um `WIN` é sempre opcional e nunca participa do
índice de reversão acima, então isso é uma lacuna de auditoria conhecida, não um bug -
ver "Interpretações adotadas e trabalho não concluído" abaixo.

## Inbox e outbox, com contrato de eventos

**Inbox** (`migrations/0006_inbox_messages.up.sql`): `inbox_messages(id, consumer_name,
message_id, payload_hash, received_at, completed_at)`, com
`UNIQUE (consumer_name, message_id)` como a única garantia de deduplicação por mensagem.
O consumidor (`internal/consumer/consumer.go`'s `handle`/`processOnce`) processa uma
mensagem, dentro de uma única transação:

1. decodifica o envelope (`type = WagerTransactionRequested`) e valida `data`;
2. confere `data.providerId` contra a allowlist configurada de provedores - o canal é
   confiável quanto à origem, não quanto ao conteúdo (ver "Canal SQS confiável" abaixo);
3. calcula o hash e faz `INSERT INTO inbox_messages ... ON CONFLICT DO NOTHING`;
4. **0 linhas inseridas, mesmo hash já registrado** → duplicata: incrementa a métrica,
   apaga a mensagem, não toca no caso de uso;
5. **0 linhas inseridas, hash diferente para o mesmo `messageId`** → erro permanente
   (alguém reenviou o mesmo id com conteúdo diferente) → DLQ;
6. **linha inserida** → chama `ExecuteInTx` (a mesma função que o HTTP chama, ver
   "Biblioteca de acesso a dados" acima) dentro da *mesma* transação, marca a inbox como
   concluída, e só então dispara o ponto de falha nomeado `before-commit` e comita;
7. só depois do commit confirmado, `DeleteMessage` - o ponto de falha
   `after-commit-before-delete` existe exatamente para os testes de recuperação provarem
   que uma queda aqui não duplica nada: a redelivery encontra a inbox já completa e só
   apaga a mensagem.

**Cancelar o `ReceiveMessage` do lado do cliente não encerra o long-poll no MiniStack.**
`internal/consumer/consumer.go`'s laço principal deliberadamente *não* cancela a
requisição HTTP do `ReceiveMessage` em andamento quando o shutdown começa
(`receiveCtx` só é conferido *depois* que a chamada retorna, nunca usado como o contexto
da própria chamada) - alguns servidores compatíveis com SQS continuam o long-poll do lado
do servidor mesmo depois que o cliente abandona a conexão, e podem entregar mensagens
naquele momento que nunca mais seriam vistas se o cliente já tivesse descartado a
resposta. Por isso o consumidor deixa cada `ReceiveMessage` em andamento terminar sozinho
(até `pollWait + 1s`) e só então decide o que fazer com o lote, inclusive liberando de
volta mensagens recebidas depois que o cancelamento já começou. `internal/config.Load`
valida que `SQS_CONSUMER_SHUTDOWN_TIMEOUT` cobre `SQS_CONSUMER_POLL_WAIT` mais o dreno
pós-cancelamento fixo (`SQSConsumerPostCancelDrain`, 5s) exatamente por causa disso - o
orçamento de shutdown precisa sobrar tempo para essa última chamada em andamento
terminar, não só para cancelar e sair.

**Outbox** (`migrations/0007_outbox_events.up.sql`): `outbox_events(event_id, aggregate_type,
aggregate_id, event_type, event_version, payload JSONB, occurred_at, attempts,
next_attempt_at, locked_until, published_at, last_error)`, com um índice parcial em
`(next_attempt_at, occurred_at) WHERE published_at IS NULL` para o publisher nunca
escanear linhas já publicadas. Diferente da inbox e do worker de referências, o lease do
outbox usa uma coluna própria (`locked_until`), separada do agendamento de retry
(`next_attempt_at`) - uma escolha de desenho diferente da do worker de referências (ver
"Interpretações adotadas" abaixo, essa divergência é discutida ali). `internal/outboxpg`'s
`Claim` segue o mesmo padrão de `FOR UPDATE SKIP LOCKED` do worker de referências; `publish`
(`internal/outbox/publisher.go`) dispara o ponto de falha `after-outbox-claim-before-send`,
envia ao SQS, dispara `after-outbox-send-before-confirm`, e só então
`MarkPublished` (`UPDATE ... SET published_at = now() ... WHERE event_id = $1 AND
published_at IS NULL`). Um trigger (`outbox_events_block_immutable_columns`) impede
alterar `event_id`, `event_type` ou `payload` depois de gravados - o publisher pode
reagendar tentativas, nunca reescrever o que vai ser publicado.

**Nunca existe um caminho que publique antes do commit**: o registro de outbox é gravado
na mesma transação do estado, do saldo e do ledger (passo 6 da concorrência); o publisher
só lê linhas já commitadas, nunca participa da transação de negócio.

**Contrato de eventos**, com `eventId`, `eventType`, `aggregateId`, `correlationId`,
`causationId` opcional, `occurredAt` (UTC RFC 3339), `version` e `data` tipado -
construídos em `internal/domain/wallet/events.go`:

- `WagerTransactionProcessed` (agregado: transação) - toda conclusão com sucesso,
  inclusive `LOSS` e `OPENING`.
- `WagerTransactionRejected` (agregado: transação) - carrega `failureCode`.
- `WalletBalanceChanged` (agregado: carteira) - só emitido quando o lançamento de ledger
  existe (`entry != nil`); carrega `walletId`, `transactionId`, `direction`, `money`,
  `balanceBefore`, `balanceAfter` e `walletVersion`.
- `WagerTransactionPendingReference` (agregado: transação) - referência esperada e
  expiração.

**Deduplicação por `eventId` fora da janela FIFO**: a deduplicação nativa do SQS FIFO
cobre só 5 minutos. Uma republicação depois de um lease expirado (outra instância
reivindicando o trabalho de uma que caiu) pode acontecer bem depois disso, com o mesmo
`eventId` e o mesmo payload - o contrato documentado ao consumidor é que ele precisa
deduplicar por `eventId` por conta própria, indefinidamente, nunca confiar só na janela do
FIFO. A ordem dentro de um grupo (`MessageGroupId = walletId`) é garantida pela fila, mas
uma republicação tardia pode chegar fora de ordem relativa a eventos mais novos da mesma
carteira - por isso `WalletBalanceChanged` carrega `walletVersion`, para o consumidor
ordenar sua própria projeção de saldo em vez de confiar na ordem de chegada.

## Autenticação, autorização e canal SQS confiável

`internal/auth/verifier.go` valida assinatura via JWKS com cache
(`coreos/go-oidc` v3, `RemoteKeySet`), fixando `SupportedSigningAlgs` só em RS256.
`SkipExpiryCheck: true` desliga a checagem de `exp`/`nbf` embutida do go-oidc de propósito -
ela aplica 5 minutos fixos de tolerância a `nbf`, o que aceitaria um token com `nbf` até 5
minutos no futuro mesmo com `AUTH_CLOCK_SKEW` configurado bem menor. `validateTimes`
(o verificador próprio) aplica a tolerância *configurada* a `exp` (obrigatório), `nbf` e
`iat` (checados só quando presentes). A descoberta OIDC (`discoverWithRetry`) roda no
`OnStart`, com retry de 250ms limitado a `AUTH_DISCOVERY_TIMEOUT` (45s por padrão, porque o
boot do Keycloak - incluindo a importação do realm - costuma ser a dependência mais lenta
do processo).

**Ordem de checagem, sempre**: autenticação → autorização → validação → efeito.
`internal/httpapi/auth_middleware.go`'s `authenticate` envolve o mux inteiro e roda antes
de qualquer roteamento - um token ausente ou inválido nunca chega perto de saber se a rota
existe. `requireRole`/`requireAnyRole` rodam depois, por rota; quando os dois papéis são
aceitos, `wallet-admin` tem precedência sobre `provider` sempre que ambos aparecerem no
token, e um `provider` sem a claim `provider_id` é `403` (a identidade está incompleta,
não ausente - por isso não é `401`).

**Canal SQS confiável, e o limite disso explícito**: só o principal IAM de gateway tem
`sqs:SendMessage` na fila de entrada - o consumidor não verifica identidade
criptográfica por mensagem (`SenderId` no MiniStack traz a conta, não o usuário IAM real,
ver limitações abaixo). Isso significa que o consumidor **confia** que só o gateway
publica ali. O que ele não confia é no conteúdo: `data.providerId` é sempre conferido
contra a allowlist configurada de provedores (`SQS_CONSUMER_PROVIDER_ALLOWLIST`), e a
mesma validação de domínio que o HTTP aplica roda por cima - um provedor desconhecido ou
uma operação inválida vai para a DLQ exatamente como aconteceria com `401`/`422` no HTTP.
O limite de confiança é este: um comprometimento do publisher do gateway (não do
consumidor, nem de um provedor) poderia injetar operações em nome de qualquer provedor da
allowlist. Isso está fora do escopo do desafio (nenhum gateway real existe - os testes
publicam com a credencial de gateway), mas fica registrado aqui como a superfície que essa
decisão aceita.

## Uso do Fx e shutdown

`internal/app/app.go`'s `Modules` compõe, nesta ordem de leitura (a ordem real de start é
decidida pelo grafo de dependências do Fx, não por esta lista): `config`, `logging`,
`metrics`, `pg`, `queue`, `auth`, `wageringmetrics`, `walletpg`, `consumer`, `outboxpg`,
`outbox`, `referenceworkerpg`, `referenceworker`, `httpapi`. Cada módulo com um recurso de
ciclo de vida (Postgres, SQS, o verificador OIDC, o consumidor, o publisher, o worker de
referências, o servidor HTTP) registra seu próprio `RegisterLifecycle`; o Fx para os
componentes na ordem inversa de como eles subiram.

**No stop**, cada componente segue o mesmo padrão de duas fases (cancelar, esperar até um
prazo, só então liberar o que ainda estava em andamento) - ver `internal/consumer/consumer.go`'s
`stop` e `internal/referenceworker/worker.go`'s `stop` para os dois exemplos mais
elaborados:

1. readiness passa a falhar;
2. o servidor HTTP faz `Shutdown`, recusando conexões novas e drenando as em andamento;
3. consumidor e worker de referências cancelam o contexto de *recebimento* primeiro (sem
   interromper trabalho já em andamento), esperam até o próprio prazo configurado
   (`SQS_CONSUMER_SHUTDOWN_TIMEOUT`, `REFERENCE_WORKER_SHUTDOWN_TIMEOUT`), e só *depois*
   desse prazo esgotado cancelam o contexto de trabalho e liberam a visibilidade de
   mensagens SQS não concluídas;
4. só então os pools de Postgres e os clientes SQS fecham.

`FX_STOP_TIMEOUT` precisa ser maior que a soma de todos esses prazos internos
(`internal/config/config.go`'s `internalStopDeadline = HTTP.ShutdownTimeout +
SQS.Consumer.ShutdownTimeout + SQSConsumerPostCancelDrain(5s) +
ReferenceWorker.ShutdownTimeout`), ou o Fx poderia interromper um componente no meio do
seu próprio dreno gracioso - `config.Load` valida essa desigualdade e recusa iniciar se
ela não for satisfeita.

**Defeito encontrado e corrigido durante este ticket:** o valor de `FX_STOP_TIMEOUT`
aplicado ao `fx.App` (`internal/app/app.go`, via `config.StopTimeoutFromEnv()`) tinha um
*default* de 45s, diferente dos 60s que `config.Load` usava para validar a desigualdade
acima. Com toda a configuração em seus valores padrão, a soma dos prazos internos é 55s
(15+30+5+5): passava na validação contra 60s e excedia o teto real de 45s, então o Fx
podia encerrar o processo à força antes de o consumidor SQS esgotar o seu próprio
`SQS_CONSUMER_SHUTDOWN_TIMEOUT`. Os dois pontos passaram a ler a mesma constante
`defaultFxStopTimeout`, e `TestStopTimeoutFromEnv_MatchesLoad` falha se elas divergirem de
novo, com ou sem a variável definida.

## Catálogo de códigos HTTP e `failureCode`

Não existe uma única função de mapeamento - `/wallets*` e `/wagering*` traduzem
`*operation.Error` para status HTTP de formas ligeiramente diferentes, cada uma definida
perto das rotas que a usam (`internal/httpapi/errors.go`'s `statusForOperationError` para
`/wallets*`, `internal/httpapi/wagering.go`'s `statusForWageringError` para
`/wagering/transactions`), embora as duas partam da mesma
`internal/domain/operation/errors.go`'s `ClassificationFor`.

**`/wallets*`** (`statusForOperationError`): `WALLET_NOT_FOUND` → `404`; todo outro
`Correctable` → `400`; `Conflict` → `409` (`WALLET_ALREADY_EXISTS`); `Unavailable` →
`503`. `/wallets*` não produz códigos `Definitive`, porque abrir ou ler uma carteira não
tem rejeição de negócio equivalente a uma operação de apostas.

**`/wagering/transactions`** (`statusForWageringError`, espelhando a tabela "Contratos
HTTP" da spec - `"400 (formato) / 404 (WALLET_NOT_FOUND) / 422 (demais corrigíveis)"`):
só `INVALID_REQUEST` e `INVALID_MONEY` (problemas de formato) → `400`; `WALLET_NOT_FOUND`
e `TRANSACTION_NOT_FOUND` → `404`; **todo outro** `Correctable` (`UNSUPPORTED_CURRENCY`,
`INVALID_AMOUNT_FOR_KIND`, `KIND_NOT_ALLOWED`, `MISSING_IDEMPOTENCY_KEY`,
`REFERENCE_REQUIRED`, `REFERENCE_NOT_ALLOWED`, `WALLET_PLAYER_MISMATCH`,
`WALLET_CURRENCY_MISMATCH`) → `422`, mesmo destino que `Definitive`
(`INSUFFICIENT_FUNDS`, `REVERSAL_INSUFFICIENT_FUNDS`, `REFERENCE_NOT_FOUND`,
`REFERENCE_NOT_PROCESSED`, `REFERENCE_ALREADY_REVERSED`, `REFERENCE_KIND_NOT_REVERSIBLE`,
`REFERENCE_MISMATCH`, `REFERENCE_AMOUNT_MISMATCH`); `Conflict`
(`IDEMPOTENCY_KEY_REUSED`, `EXTERNAL_TRANSACTION_ID_CONFLICT`) → `409`; `Unavailable`
(`TEMPORARILY_UNAVAILABLE`) → `503` com `Retry-After`. Um `422` de entrada corrigível e um
`422` de rejeição persistida ainda se distinguem pelo formato do corpo, nunca só pelo
código: presença de `transactionId`+`status` (rejeição persistida, auditável, evento
emitido) contra presença de `error` (nunca persistida, pode ser reenviada com a mesma
chave depois de corrigida).

`PERMANENT_PROCESSING_FAILURE` (a classificação `PermanentFailure`) nunca é devolvido
por nenhuma das duas funções acima porque nenhum caminho HTTP síncrono o produz - só o
worker de referências pendentes o persiste (seção "Máquina de estados" acima); uma
consulta de transação (`GET /wagering/transactions/:id`) que leia uma linha `FAILED`
devolve `200` com `status: "FAILED"` no corpo, não um erro.

**Códigos que este ticket confirmou existirem no código e não estarem no catálogo
original da spec** (adicionados durante a implementação, com precedente e justificativa
registrados nos tickets que os criaram):

- `TRANSACTION_NOT_FOUND` (ticket 09) - `Correctable`, `404`. Usado pelas rotas de
  consulta de transação (`GET /wagering/transactions/:id` e
  `GET /providers/:providerId/wagering/transactions/:externalId`) tanto para um id
  inexistente quanto para um id que existe mas pertence a outro provedor ou é de origem
  interna - a mesma resposta nos dois casos, para nunca revelar que a linha existe.
  Segue o precedente de `WALLET_NOT_FOUND`.
- `REFERENCE_RESOLUTION_NOT_IMPLEMENTED` (ticket 08, `501`) - rotulado deliberadamente
  como fora do catálogo documentado: existiu só na janela entre o ticket 08 (que
  implementou `BET`/`WIN`/`LOSS`) e o ticket 10 (que implementou a resolução real de
  `REFUND`/`ROLLBACK`/`WIN` referenciado). Não deveria aparecer em nenhum caminho do
  código final - se aparecer, é sinal de uma regressão.
- `PERMANENT_PROCESSING_FAILURE` (ticket 11) - o código de `FAILED`, descrito acima na
  máquina de estados; a spec já previa o nome, mas não o listava na tabela de
  "Catálogo de erros e códigos" original.

## Limitações do MiniStack, verificadas na tag `1.5.12`

Verificado em 14/09/2026 contra `ministackorg/ministack:1.5.12` com `AUTH=true`, com AWS
CLI containerizado e leitura direta do código da imagem (32 de 33 checagens manuais
passaram; a exceção era defeito do próprio script de verificação, refeito à parte). O
primeiro ticket de infraestrutura (`test/integration/iam_test.go`) versiona essas mesmas
checagens como teste de integração.

- **Não confere secret nem assinatura SigV4.** Uma access key conhecida com secret errado
  ainda é aceita e ainda fica presa à política do usuário dono da key - o MiniStack não
  reproduz esse aspecto de uma AWS real.
- **Chaves root ignoram toda política.** A chave literal `test` e qualquer chave de 12
  dígitos numéricos (o formato de `AWS_ACCESS_KEY_ID` do próprio container) são tratadas
  como root e pulam todo o modelo de IAM - por isso o serviço do MiniStack no Compose não
  define essa variável, e nem a aplicação nem os testes usam essas chaves.
- **Requisição sem assinatura também é root.** Descoberto durante a verificação (não
  documentado pelo projeto MiniStack): uma chamada sem cabeçalho `Authorization` é tratada
  como a mesma conta root acima, ignorando toda política. Isso reforça a mesma regra
  operacional - nunca fazer uma chamada não assinada contra o MiniStack em produção ou em
  teste.
- **Queue policies são armazenadas, mas não aplicadas.** O modelo de permissão que
  realmente funciona é só o de políticas de identidade (anexadas a um usuário IAM) - uma
  política de fila (`SetQueueAttributes` com `Policy`) pode ser gravada sem erro e
  simplesmente não tem efeito nenhum.
- **`SenderId` traz a conta, não o usuário IAM** (`000000000000` sempre). Mapear qual
  provedor publicou uma mensagem pelo `SenderId` não é viável no MiniStack - é por isso
  que a identidade do provedor tem que vir de dentro do payload (`data.providerId`),
  nunca do metadado de transporte, ver "Canal SQS confiável" acima.
- **O que funciona como esperado**: FIFO com um único in-flight por
  `MessageGroupId`, `MessageDeduplicationId` deduplicando de verdade,
  `ChangeMessageVisibility`, e o redrive para a DLQ depois de `maxReceiveCount` - toda a
  mecânica que este projeto realmente depende de estar correta.

## Interpretações adotadas e trabalho não concluído

Interpretações que a spec pede para registrar aqui, com a razão de cada uma:

- **Entradas corrigíveis vs. rejeições definitivas.** A distinção não é "erro de
  validação vs. erro de negócio" - é "depende só do conteúdo da requisição e de dados
  imutáveis" (corrigível, nunca persistido, pode reenviar com a mesma chave) contra
  "depende de estado mutável ou da resolução de uma referência" (persistido como
  `REJECTED`, auditável, emite evento). `WALLET_NOT_FOUND` é corrigível mesmo dependendo
  de uma consulta ao banco, porque o dado que falta (a carteira) não muda com o tempo do
  ponto de vista do provedor - ou ele mandou o id certo, ou não.
- **Uso restrito de `FAILED`.** Só o worker de referências pendentes pode produzir
  `FAILED`, e só para um motivo que não é regra de negócio nem infraestrutura transitória.
  Isso significa que `FAILED` deveria ser raro o suficiente para virar alerta operacional
  sempre que aparecer - não é um estado esperado do fluxo normal, mesmo sob falhas de
  rede ou concorrência pesada.
- **`WIN` com referência segue o fluxo de pendência.** Um `WIN` referenciando uma `BET`
  que ainda não existe vai para `PENDING_REFERENCE`, exatamente como `REFUND`/`ROLLBACK` -
  a spec exigiu esse ajuste explicitamente (decisão 5) para não perder a informação por
  causa de entrega fora de ordem.
- **Matriz de reversões**: documentada e garantida por índice, seção acima.
- **Canal SQS confiável**: documentado, seção acima.
- **Deduplicação por `eventId` fora da janela FIFO**: documentada, seção "Inbox e
  outbox" acima.
- **Limitações do MiniStack**: documentadas, seção acima.
- **Limite numérico de `Money`**: documentado, seção "Dinheiro" acima.

Trabalho não concluído ou limitações aceitas, descobertas durante a implementação e
registradas aqui em vez de absorvidas silenciosamente:

- **Um `WIN` que referencia uma `BET` já revertida não é bloqueado** (fora de escopo
  explícito da spec) - ver "Matriz de reversões" acima.
- **Lease do worker de referências vs. lease do outbox usam desenhos diferentes.** O
  worker de referências (ticket 11) reaproveita a própria coluna de agendamento
  (`next_attempt_at`) como lease - reivindicar e adiar são a mesma escrita. O outbox
  (ticket 12, escrito depois, em revisão cruzada) separou lease (`locked_until`) de
  backoff (`next_attempt_at`) em duas colunas. As duas funcionam corretamente e são
  testadas, mas um leitor comparando os dois vai notar a inconsistência de desenho - ela
  não foi unificada porque nenhum ticket específico revisitou o worker de referências
  depois que o padrão do outbox se estabilizou.
- **`internal/outbox/publisher_integration_test.go` tem hosts/portas hardcoded**
  (`localhost:4566`, `localhost:8081`, `localhost:5432` como *fallback* quando a variável
  de ambiente correspondente está ausente) em vez de sempre derivar do ambiente como o
  resto da suíte de integração faz - descoberto nos tickets 10, 12 e 15 e nunca corrigido.
  Inofensivo com as portas padrão (é o que os comandos documentados no `README.md` usam),
  mas quebra se alguém remapear `MINISTACK_PORT`/`KEYCLOAK_PORT`/`POSTGRES_PORT` sem
  também exportar `SQS_ENDPOINT_URL`/`DATABASE_URL` explicitamente - o que este próprio
  ticket precisou fazer para verificar tudo numa máquina com as portas padrão do Postgres
  e do Keycloak já ocupadas por outro serviço (ver `README.md`, "Testes").
- **A suíte multi-instância pressupõe um banco que a suíte de integração ainda não usou.**
  `test/multiinstance`'s `drainBacklog` espera os indicadores `outbox_pending_events` e
  `pending_reference_active` chegarem a zero antes de cada cenário, para começar de uma
  base limpa. Isso funciona dentro da própria suíte multi-instância (ela já é desenhada
  para tolerar o que uma execução anterior *dela mesma* deixou para trás - ver o
  comentário de `drainBacklog`). O que não é coberto: rodar `test/integration` e
  `test/multiinstance` em sequência contra o **mesmo** Postgres sem reiniciá-lo entre as
  duas. Alguns cenários de `test/integration` (os que testam expiração de referência
  pendente, por exemplo) deliberadamente deixam para trás uma `PENDING_REFERENCE` cuja
  referência nunca vai chegar, criada sob as configurações curtas *daquele* teste - mas o
  `attempts`/TTL restantes daquela linha são então avaliados pela suíte multi-instância
  sob as configurações de *produção* (`REFERENCE_WORKER_MAX_ATTEMPTS=10`,
  `RetryMax=1m`), que podem levar minutos para esgotar, bem além do prazo de 20s que
  `drainBacklog` espera. Confirmado experimentalmente neste ticket: a suíte
  multi-instância inteira passa sem nenhuma falha quando roda contra um Postgres que
  `test/integration` ainda não tocou (`docker compose down -v && docker compose up
  --build -d` entre as duas suítes), e falha de forma determinística em vários cenários
  quando roda logo depois de `test/integration` no mesmo banco. O `README.md` documenta
  a ordem correta; nenhum código de produção precisou mudar.
- **Uma instância `app` do Compose rodando ao mesmo tempo que qualquer suíte de
  integração ou multi-instância interfere nos resultados.** Ambas as suítes assumem que
  são a única coisa consumindo `wager-transactions.fifo` e reivindicando
  `PENDING_REFERENCE`/outbox pendente contra a infraestrutura compartilhada - uma
  instância `app` viva (por exemplo, deixada no ar depois de testar as chamadas do
  README manualmente) disputa mensagens e linhas com o harness de teste e produz falhas
  intermitentes que não são bugs de produto. Confirmado experimentalmente neste ticket:
  os mesmos testes de `test/integration` que falhavam de forma incerta com `app` no ar
  passaram de forma consistente (`-count=5`, duas rodadas completas da suíte) assim que
  `docker compose stop app` rodou antes. O `README.md` documenta isso explicitamente na
  seção de testes.

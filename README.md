# jungle-gaming-wallet

Carteira de apostas distribuída em Go. Ver `ARCHITECTURE.md` para as decisões
de desenho, as interpretações adotadas e as limitações conhecidas. Este README
ensina a subir tudo, chamar cada rota autenticada e rodar cada suíte de teste a
partir de um clone limpo.

## Pré-requisitos

- Docker e Docker Compose v2 (`docker compose`, não `docker-compose`).
- Go 1.26.5, só para rodar `go test`/`go vet`/`gofmt` fora de container.

## Subir tudo

```sh
docker compose up --build
```

Isso sobe, nesta ordem: `postgres` (com healthcheck, publicado só em
`127.0.0.1`), `ministack` (com `AUTH=true`), `keycloak` (também publicado só
em `127.0.0.1`, sem administrador - realm `wallet` importado automaticamente
do arquivo `deploy/keycloak/realm-wallet.json` - ver "Autenticação e
autorização" abaixo), `provisioning` (one-shot: cria
as filas, o redrive para a DLQ, os quatro usuários IAM de menor privilégio e
os fixtures de teste de IAM - ver "Fixtures de teste de IAM" abaixo),
`postgres-provisioning` (one-shot: cria o papel `wallet_app`, se ainda não
existir, e define sua senha - ver "Credenciais do Postgres" abaixo),
`migrate` (one-shot: aplica as migrations - só depois que o papel já
existe) e por fim `app`, que só inicia depois que Postgres está saudável, o
container do Keycloak já foi iniciado (não necessariamente pronto - ver
abaixo) e os one-shots de IAM/Postgres/migrations terminaram com sucesso.

`provisioning` e `postgres-provisioning` são idempotentes: rodar `docker
compose up --build` de novo contra o mesmo MiniStack/Postgres reaproveita
filas, usuários, chaves de acesso e a senha de `wallet_app` já existentes,
em vez de falhar ou rotacionar credenciais. `docker compose stop && docker
compose up --build` funciona sem precisar derrubar nenhum volume.

Verificação:

```sh
curl http://localhost:8080/health/live
curl http://localhost:8080/health/ready
curl http://localhost:8080/metrics
```

`SIGTERM` (`docker compose stop app`, ou `Ctrl+C` no modo attached) encerra
na ordem: readiness cai para `503`, o servidor HTTP drena requisições em
andamento, e só depois o pool do Postgres fecha.

## Eventos de carteira

Eventos de integração são gravados na outbox na mesma transação que a
carteira, o ledger e a transação; o publisher só os envia depois do commit.
Ele publica o snapshot JSON persistido em `wallet-events.fifo`, com
`MessageGroupId = walletId` e `MessageDeduplicationId = eventId`. O
consumidor deve rotear por `eventType`, desserializar `data` no tipo concreto,
deduplicar por `eventId` mesmo fora da janela de cinco minutos do FIFO e usar
`walletVersion` para ordenar projeções de saldo: uma republicação após lease
expirado pode chegar depois de um evento mais novo.

## Entrada SQS de operações

O gateway interno publica `WagerTransactionRequested` em
`wager-transactions.fifo`. `data.idempotencyKey` é a mesma chave usada no
HTTP e fica fora do hash canônico; `messageId` é a identidade durável da
inbox. O produtor define `MessageGroupId = walletId` e
`MessageDeduplicationId = messageId`.

O consumidor confirma o `DeleteMessage` somente depois do commit conjunto da
inbox, operação, ledger e outbox. `SQS_CONSUMER_CONCURRENCY`,
`SQS_CONSUMER_VISIBILITY_TIMEOUT`, `SQS_CONSUMER_PROCESSING_TIMEOUT`,
`SQS_CONSUMER_RETRY_BASE`, `SQS_CONSUMER_RETRY_MAX`,
`WAGER_TRANSACTIONS_MAX_RECEIVE_COUNT` e `SQS_CONSUMER_SHUTDOWN_TIMEOUT`
ajustam respectivamente paralelismo, ownership, prazo, backoff, redrive e
drain; o visibility timeout precisa ser maior que o prazo de processamento,
e o prazo de shutdown precisa cobrir o long poll mais o drain pós-cancelamento.
`WAGER_TRANSACTIONS_MAX_RECEIVE_COUNT` é lida exclusivamente pelo
`deploy/ministack/provision.sh` para a `RedrivePolicy` da fila, não pelo
processo Go.

## Referências fora de ordem

`REFUND`, `ROLLBACK` e `WIN` com referência que chegam antes da operação
referenciada recebem `202` e são persistidos como `PENDING_REFERENCE`. O
worker configurado por `REFERENCE_WORKER_*` reclama lotes com `FOR UPDATE
SKIP LOCKED` e um lease em `next_attempt_at`; assim instâncias paralelas não
trabalham a mesma linha e uma queda só torna o registro elegível novamente.
Ele usa backoff exponencial com jitter e encerra por TTL ou tentativas com o
evento de rejeição auditável.
`REFERENCE_WORKER_SHUTDOWN_TIMEOUT` limita o dreno no stop; `FX_STOP_TIMEOUT`
é validado acima da soma desse orçamento e dos demais componentes.

## Variáveis de ambiente

Ver `.env.example` - cobre tanto o que `docker compose` lê para montar o
Compose (portas, nomes de fila, credenciais locais do Postgres) quanto os
valores por trás de cada default do próprio binário Go
(`internal/config.Load`), para quem for rodar `go run
./cmd/wallet-service serve` fora do Compose. `docker compose` lê um `.env`
na raiz automaticamente; copie o exemplo se quiser mudar portas ou nomes de
fila locais - por exemplo, se `5432`, `4566`, `8080`, `8081`, `8091`-`8093`
já estiverem em uso por outro processo na máquina, mude só
`POSTGRES_PORT`/`MINISTACK_PORT`/`HTTP_PORT*`/`KEYCLOAK_PORT` no `.env` e
rode `docker compose up --build` normalmente - todas as chamadas deste
README continuam funcionando trocando `8080`/`8081` pelas portas escolhidas.
A única pegadinha é para quem for rodar `go test -tags integration` ou
`-tags multiinstance` **do host** contra portas remapeadas: esses testes
resolvem a maioria dos endereços a partir dos arquivos de credenciais e de
`KEYCLOAK_PORT` (com o padrão `8081` embutido quando a variável não está no
ambiente do processo `go test`, não só no `.env`), mas `DATABASE_URL` e
`SQS_ENDPOINT_URL` são lidos diretamente do ambiente do host sem consultar o
`.env` - exporte os três antes de rodar `go test`, por exemplo:

```sh
export DATABASE_URL="postgres://wallet:wallet@localhost:${POSTGRES_PORT:-5432}/wallet?sslmode=disable"
export SQS_ENDPOINT_URL="http://localhost:${MINISTACK_PORT:-4566}"
export KEYCLOAK_PORT="${KEYCLOAK_PORT:-8081}"
```

As chaves de acesso do SQS não vão no `.env` - são geradas pelo
`provisioning` a cada `docker compose up` e escritas em dois arquivos sob
`deploy/ministack/.runtime/` (git-ignored), separados por menor privilégio:

- `app-credentials.env` - só `SQS_CONSUMER_*` e `SQS_PUBLISHER_*`, as duas
  credenciais que a aplicação de fato usa. É o único arquivo montado no
  container `app` (um mount de arquivo específico, não do diretório
  inteiro - ver `docker-compose.yml`); o entrypoint da aplicação
  (`deploy/docker/entrypoint.sh`) só sabe ler esse arquivo, nunca as chaves
  de gateway, events-reader ou dos fixtures de teste.
- `test-credentials.env` - gateway, events-reader e os fixtures de teste de
  IAM (`deny-probe`, `redrive-tester`), mais os nomes das filas descartáveis
  que os testes usam. Nunca é montado em nenhum container da aplicação; só
  `test/integration` o lê, direto do host.

Ambos os arquivos, e o diretório que os contém, são criados com permissões
restritas (`0700`/`0600`) pelo `provisioning`, para que outros usuários
locais da máquina não consigam ler chaves reais de acesso ao MiniStack.
`app-credentials.env` é adicionalmente associado ao UID/GID do usuário sem
privilégio que o container `app` usa (ver Dockerfile e
`APP_RUNTIME_UID`/`APP_RUNTIME_GID` em `docker-compose.yml`), para que
continue legível por ele apesar do modo `0600`.

### Credenciais do Postgres

O papel `wallet_app` e sua senha são objetos do cluster Postgres - existem
fora de qualquer banco específico e sobrevivem a um `migrate down`/`up` - e
por isso pertencem inteiramente ao provisionamento, nunca às migrations. O
serviço one-shot `postgres-provisioning` (`deploy/postgres/provision.sh`)
roda antes de `migrate`, contra o papel dono (`OWNER_DATABASE_URL`), e:

- cria o papel `wallet_app` com `LOGIN`, se ele ainda não existir (sem
  senha nesse momento);
- gera 32 bytes aleatórios de `/dev/urandom`, em base64 URL-safe, só na
  primeira vez;
- roda `ALTER ROLE wallet_app WITH PASSWORD '...'`;
- escreve `WALLET_APP_PASSWORD` em `deploy/postgres/.runtime/credentials.env`
  (diretório `0700`, arquivo `0600`, criados com `umask`, git-ignorado e
  fora da imagem via `.dockerignore` - mesmo padrão de
  `deploy/ministack/.runtime`).

Nenhum SQL versionado, em migration ou em qualquer outro lugar, contém uma
senha funcional ou cria o papel. `migrations/0002_app_role.up.sql` só
concede `USAGE` no schema `public` a `wallet_app`, e assume que o papel já
existe - se não existir, a migration falha com uma mensagem clara ("papel
wallet_app ausente: rode o provisionamento do Postgres antes das
migrations") em vez de criar o papel silenciosamente. O `down` correspondente
só revoga esse mesmo `USAGE`, nunca apaga o papel. Como resultado, um ciclo
`migrate down` seguido de `migrate up` pelo subcomando público nunca afeta o
papel `wallet_app` nem sua senha - só o provisionamento cria ou altera
esse objeto.

Idempotente como `provisioning`: se o arquivo de credenciais já existe, a
senha é lida dele e reaplicada com o mesmo `ALTER ROLE`, em vez de gerar
uma nova - por isso `docker compose stop && docker compose up --build`
mantém a mesma senha. `test/integration` lê esse arquivo para conectar como
`wallet_app` (ver `walletAppPassword` em `test/integration/helpers_test.go`).

O processo `serve` também lê esse arquivo, mas nunca recebe nem conecta com
a credencial do papel dono: ele não lê `DATABASE_URL` de jeito nenhum, só
`DATABASE_HOST`, `DATABASE_PORT`, `DATABASE_NAME` e `DATABASE_SSLMODE` -
nenhum segredo. `internal/config.Load` monta o DSN a partir só desses
quatro (e falha se `DATABASE_HOST` contiver `@`, sinal de userinfo
embutido); `internal/pg.New`/`AppDSN` adiciona por cima as credenciais de
`wallet_app`, lidas de `DATABASE_APP_CREDENTIALS_FILE` (padrão
`deploy/postgres/.runtime/credentials.env`; no Compose, o serviço `app`
monta esse arquivo em `/shared/postgres-credentials.env` e aponta a
variável para lá - ver `docker-compose.yml`). `docker compose exec app env`
nunca mostra a senha do dono, porque ela nunca chega a esse container.
`DATABASE_URL`, com a credencial do dono, continua existindo só para
`migrate` e `postgres-provisioning`.

## Contratos HTTP implementados

- `POST /wallets` - abre a carteira de um jogador numa moeda. Corpo:
  `{"playerId": "<uuid>", "initialBalance": {"amount": "100.00", "currency": "BRL"}}`.
  `201` com `id`, `playerId`, `balance` e `version`; `409
  WALLET_ALREADY_EXISTS` para o mesmo par jogador/moeda; `400` com um
  código estável (`INVALID_REQUEST`, `INVALID_MONEY`,
  `UNSUPPORTED_CURRENCY`) para entrada inválida, sem persistir nada; `503`
  com `Retry-After` numa falha transitória do banco, também sem persistir
  nada.
- `GET /wallets/{walletId}` - `200` com `id`, `playerId`, `balance` e
  `version`, ou `404` se a carteira não existe.
- `GET /wallets/{walletId}/ledger?cursor=&limit=` - pagina os lançamentos do
  ledger em ordem estável (pela sequência interna, nunca por timestamp).
  `limit` padrão 50, máximo 200. `200` com `entries` e `nextCursor` (nulo no
  fim); cursor inválido devolve `400`.
- `POST /wallets/{walletId}/reconciliation` - reconstrói o saldo a partir do
  ledger, numa transação `REPEATABLE READ READ ONLY` (nunca altera o saldo
  armazenado). `200` com `storedBalance`, `calculatedBalance`, `difference`,
  `consistent` e `checkedEntries`; uma divergência ainda devolve `200`, mas
  gera log `warn` e incrementa uma métrica - ver `ARCHITECTURE.md`.

`/wallets*` acima exige o papel `wallet-admin`. As rotas de apostas abaixo
exigem o papel `provider` (com `providerId` do corpo/rota batendo com o
`provider_id` do token) ou `wallet-admin` (vê qualquer provedor); ver
"Autenticação e autorização" abaixo.

- `POST /wagering/transactions` - processa `BET`, `WIN`, `LOSS`, `REFUND` ou
  `ROLLBACK`. Corpo: `{"providerId", "externalTransactionId", "playerId",
  "walletId", "roundId", "gameId", "kind", "money": {"amount", "currency"},
  "referenceExternalTransactionId"?}`, com o header
  `Idempotency-Key: <chave>`. `200` (`PROCESSED`) com `transactionId`,
  `status`, `balance` e `idempotentReplay`; `202` (`PENDING_REFERENCE`,
  sem `balance`) quando a referência ainda não chegou; `422` com
  `failureCode` para uma rejeição de negócio persistida (mesmo corpo, mas
  sem `error`) ou para entrada corrigível que não seja `INVALID_REQUEST`/
  `INVALID_MONEY` (aí sim com `error`); `400` para essas duas, `404` para
  `WALLET_NOT_FOUND`/`TRANSACTION_NOT_FOUND`, `409` para
  `IDEMPOTENCY_KEY_REUSED`/`EXTERNAL_TRANSACTION_ID_CONFLICT`, `503` com
  `Retry-After` numa indisponibilidade transitória - ver `ARCHITECTURE.md`,
  "Catálogo de códigos HTTP e `failureCode`" para a tabela completa e por
  que ela difere da de `/wallets*`.
- `GET /wagering/transactions/{transactionId}` - registro completo (ids,
  tipo, `money`, referências, `status`, `failureCode`, saldo resultante,
  tentativas, próximo envio e timestamps). `provider` só vê as próprias
  transações; qualquer outra coisa (provedor errado, id inexistente, linha
  de origem interna) devolve o mesmo `404 TRANSACTION_NOT_FOUND`, para nunca
  revelar que a linha existe.
- `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}` -
  o mesmo registro, indexado pelo id externo do provedor. `providerId` da
  rota diferente do token devolve `403` (aqui a rota já denuncia o
  provedor, então não faz sentido escondê-lo atrás de um `404`).

`/health/*` e `/metrics` continuam públicos.

### Exemplos de chamadas autenticadas

Com o stack no ar (`docker compose up --build`) e um shell no host:

```sh
# wallet-admin: abre a carteira e lê o saldo
ADMIN_TOKEN=$(curl -s -X POST "http://localhost:${KEYCLOAK_PORT:-8081}/realms/wallet/protocol/openid-connect/token" \
  -d grant_type=client_credentials -d client_id=wallet-service -d client_secret=wallet-service-secret \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')

PLAYER_ID=$(python3 -c 'import uuid;print(uuid.uuid4())')
WALLET=$(curl -s -X POST "http://localhost:${HTTP_PORT:-8080}/wallets" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d "{\"playerId\":\"$PLAYER_ID\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}")
echo "$WALLET"
WALLET_ID=$(echo "$WALLET" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

curl -s "http://localhost:${HTTP_PORT:-8080}/wallets/$WALLET_ID/ledger" -H "Authorization: Bearer $ADMIN_TOKEN"
curl -s -X POST "http://localhost:${HTTP_PORT:-8080}/wallets/$WALLET_ID/reconciliation" -H "Authorization: Bearer $ADMIN_TOKEN"

# provider: aposta contra a carteira acima
PROVIDER_TOKEN=$(curl -s -X POST "http://localhost:${KEYCLOAK_PORT:-8081}/realms/wallet/protocol/openid-connect/token" \
  -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-secret \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')

EXTERNAL_ID=$(python3 -c 'import uuid;print(uuid.uuid4())')
BET=$(curl -s -X POST "http://localhost:${HTTP_PORT:-8080}/wagering/transactions" \
  -H "Authorization: Bearer $PROVIDER_TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: bet-$EXTERNAL_ID" \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"$EXTERNAL_ID\",\"playerId\":\"$PLAYER_ID\",\"walletId\":\"$WALLET_ID\",\"roundId\":\"r1\",\"gameId\":\"g1\",\"kind\":\"BET\",\"money\":{\"amount\":\"20.00\",\"currency\":\"BRL\"}}")
echo "$BET"
TRANSACTION_ID=$(echo "$BET" | python3 -c 'import sys,json;print(json.load(sys.stdin)["transactionId"])')

curl -s "http://localhost:${HTTP_PORT:-8080}/wagering/transactions/$TRANSACTION_ID" -H "Authorization: Bearer $PROVIDER_TOKEN"
curl -s "http://localhost:${HTTP_PORT:-8080}/providers/provider-a/wagering/transactions/$EXTERNAL_ID" -H "Authorization: Bearer $PROVIDER_TOKEN"
```

Saída real de uma execução (`docker compose up --build` a partir de um clone
limpo, seguida exatamente destes comandos):

```
$ curl -s -X POST http://localhost:8080/wallets ...
{"id":"01a0a783-8355-76d0-b7cc-32d575c3f53b","playerId":"543d083e-4bae-4347-997b-663778197668","balance":{"amount":"100.00","currency":"BRL"},"version":1}

$ curl -s http://localhost:8080/wallets/$WALLET_ID/ledger ...
{"entries":[{"sequenceNumber":1,"transactionId":"01a0a783-8355-76d4-bb9d-c7809a2a0d1f","direction":"CREDIT","money":{"amount":"100.00","currency":"BRL"},"balanceBefore":{"amount":"0.00","currency":"BRL"},"balanceAfter":{"amount":"100.00","currency":"BRL"},"occurredAt":"2026-09-16T00:00:09.045446Z"}],"nextCursor":null}

$ curl -s -X POST http://localhost:8080/wallets/$WALLET_ID/reconciliation ...
{"walletId":"01a0a783-8355-76d0-b7cc-32d575c3f53b","storedBalance":{"amount":"100.00","currency":"BRL"},"calculatedBalance":{"amount":"100.00","currency":"BRL"},"difference":{"amount":"0.00","currency":"BRL"},"consistent":true,"checkedEntries":1}

$ curl -s -X POST http://localhost:8080/wagering/transactions ... (BET 20.00)
{"transactionId":"01a0a783-ba87-7602-9115-51d1532a466e","status":"PROCESSED","balance":{"amount":"80.00","currency":"BRL"},"idempotentReplay":false}

$ curl -s http://localhost:8080/wagering/transactions/$TRANSACTION_ID ...
{"transactionId":"01a0a783-ba87-7602-9115-51d1532a466e","externalTransactionId":"d0740891-94ac-411f-8824-075f19b4fa04","providerId":"provider-a","playerId":"543d083e-4bae-4347-997b-663778197668","walletId":"01a0a783-8355-76d0-b7cc-32d575c3f53b","roundId":"r1","gameId":"g1","kind":"BET","origin":"EXTERNAL","money":{"amount":"20.00","currency":"BRL"},"referenceExternalTransactionId":null,"referenceTransactionId":null,"status":"PROCESSED","failureCode":null,"resultingBalance":{"amount":"80.00","currency":"BRL"},"attempts":0,"nextAttemptAt":null,"pendingExpiresAt":null,"createdAt":"2026-09-16T00:00:23.174213Z","updatedAt":"2026-09-16T00:00:23.174213Z"}
```

### Fixtures de teste de IAM

`test/integration/iam_test.go` prova dois comportamentos de IAM que
exigiriam a chave root do MiniStack para montar em tempo de teste - anexar
uma política de Deny explícito, e criar/observar filas descartáveis de
redrive. Em vez disso, `provisioning` cria esses fixtures com antecedência,
como o único lugar autorizado a usar a chave root:

- **`deny-probe`**: um usuário com uma política Allow e uma política Deny
  explícita, as duas sobre `sqs:SendMessage` na mesma fila descartável
  (`iam-test-deny-probe.fifo`). O teste só observa que o Deny já anexado
  vence o Allow - nunca muta IAM.
- **`redrive-tester`**: um usuário com permissão de enviar e receber num par
  de filas FIFO descartáveis (`iam-test-redrive-in.fifo` e
  `iam-test-redrive-dlq.fifo` por padrão), com `maxReceiveCount` baixo, para
  que o teste de redrive não precise esperar o `maxReceiveCount` bem maior
  da fila de produção. Tem também `sqs:DeleteMessage`, mas restrito à ARN da
  fila DLQ descartável - o suficiente para remover a mensagem que o próprio
  teste redirecionou para lá, sem nunca poder apagar da fila de entrada.
- **`dlq-reader`**: leitor da DLQ de operações, com apenas
  `ReceiveMessage` e `DeleteMessage` na ARN de
  `wager-transactions-dlq.fifo`. Ele permite que o seam 3a correlacione e
  remova uma falha permanente explícita sem ampliar o papel do consumidor.
- **`consumer-fixture`**: consumidor do par descartável de redrive, com
  receive/delete/change-visibility na entrada e send na DLQ. O teste o usa
  contra um Postgres inalcançável; a aplicação nunca recebe essas chaves.

Nenhum desses usuários, filas ou chaves participa do Compose da aplicação;
eles existem só para o `test/integration` rodar sem tocar na chave root.

## Autenticação e autorização

Toda rota de negócio (hoje `/wallets*`; o desenho acomoda `/wagering*` e
`/providers*` sem mudança estrutural) exige um token OAuth 2.0 emitido por um
Keycloak real via `client_credentials`. A autenticação roda antes do próprio
roteamento (`internal/httpapi/server.go`'s `authenticate`, que envolve o mux
inteiro): qualquer caminho ou método de negócio sem token válido devolve
`401`, mesmo um método que nenhuma rota mapeia (por exemplo `PUT /wallets`) -
nunca o `405` público que o mux devolveria por conta própria. Com token
válido, um método não mapeado ainda devolve `405`, e o papel errado devolve
`403`. `/health/*` e `/metrics` continuam públicos.

**Keycloak não tem administrador.** Nem `KC_BOOTSTRAP_ADMIN_*` nem o legado
`KEYCLOAK_ADMIN`/`KEYCLOAK_ADMIN_PASSWORD` são definidos - o container sobe
sem nenhuma conta no realm `master`. Nada neste repositório precisa da API
admin: todo client, papel, mapper e claim do realm `wallet` vem declarado
estaticamente em `deploy/keycloak/realm-wallet.json` e é criado pelo próprio
`--import-realm`, incluindo o `access.token.lifespan` de 2s de
`provider-a-short-lived` (um atributo estático do client, não uma chamada à
API admin). A porta do Keycloak (`KEYCLOAK_PORT`, 8081 por padrão) é
publicada só em `127.0.0.1`, pelo mesmo motivo do Postgres acima: um Console
Admin alcançável, mesmo sem credencial nenhuma configurada, é superfície de
ataque que este projeto não precisa expor.

O realm `wallet` é importado automaticamente no boot do Keycloak, a partir
de `deploy/keycloak/realm-wallet.json` (`start-dev --import-realm`) - sem
provisionamento em separado, ao contrário do MiniStack e do Postgres acima.
Ele define:

- `provider-a` e `provider-b` - papel `provider`, claim fixo `provider_id`
  (`provider-a`/`provider-b` respectivamente) por um protocol mapper
  hardcoded.
- `wallet-service` - papel `wallet-admin`, o único que passa em `/wallets*`.
- `no-roles-client` - nenhum papel, para os testes de `403`.
- `provider-a-short-lived` - igual a `provider-a`, mas com
  `access.token.lifespan` de 2 segundos, para o teste de token expirado.
- Um client scope `wallet-api-audience`, com um audience mapper que coloca
  `wallet-api` no `aud` de todo token - a audiência que `internal/config`
  valida por padrão (`AUTH_AUDIENCE`).
- Um client scope `wallet-realm-roles`, com o mapper padrão de papéis de
  realm (`realm_access.roles`) - definido explicitamente porque um realm
  importado do zero via `--import-realm` não herda os client scopes
  embutidos (`roles`, `profile`, `web-origins`, `acr`) que um realm criado
  pelo Admin Console ganharia automaticamente.

Nenhum desses `client_secret` é um segredo real - são fixtures de um realm
de desenvolvimento reimportado a cada `docker compose up`, documentados em
`.env.example` (`AUTH_TEST_*`).

### Validação

`internal/auth` (`internal/auth/verifier.go`) descobre a configuração OIDC
do Keycloak (`issuer`, `jwks_uri`) no `OnStart` da aplicação, com retry e
backoff fixo limitado a `AUTH_DISCOVERY_TIMEOUT` (45s por padrão - o boot do
Keycloak, incluindo a importação do realm, costuma ser o mais lento entre
todas as dependências deste processo). O verificador resultante
(`coreos/go-oidc` v3) confere assinatura RS256 via JWKS com cache
(`RemoteKeySet`), `issuer` e `aud`. Expiração (`exp`), validade futura
(`nbf`) e emissão futura (`iat`) são conferidas pelo próprio
`internal/auth`, não pelo go-oidc: a checagem embutida do go-oidc aplica 5
minutos fixos de tolerância a `nbf` (para interoperar com provedores fora do
spec), o que aceitaria um token com `nbf` até 5 minutos no futuro mesmo com
`AUTH_CLOCK_SKEW` configurado bem menor - por isso o verificador desliga essa
checagem (`SkipExpiryCheck: true`) e aplica a própria, com um relógio
injetável para teste, contra a tolerância configurada (`AUTH_CLOCK_SKEW`, 5s
por padrão). Um token sem `exp` é sempre rejeitado; `nbf` e `iat` só são
checados quando presentes.

Duas variáveis distintas controlam onde a descoberta acontece e o que o
token precisa provar:

- `AUTH_ISSUER_URL` é o `iss` exato que todo token aceito precisa carregar -
  o endereço público do Keycloak (o mesmo que `KC_HOSTNAME`, abaixo, fixa),
  idêntico para qualquer chamador, dentro ou fora do Compose.
- `AUTH_DISCOVERY_URL` é só de onde este processo busca a configuração
  OIDC/JWKS - nunca comparado contra o `iss` de um token. No Compose, o
  container `app` não alcança o endereço público do Keycloak, só
  `http://keycloak:8080` pela rede interna; no host (`go test`,
  `test/integration`), as duas variáveis coincidem, e `AUTH_DISCOVERY_URL`
  pode ficar de fora (tem `AUTH_ISSUER_URL` como padrão).

A separação usa o mecanismo do próprio go-oidc para descobrir num host
diferente do issuer (`oidc.InsecureIssuerURLContext`) sem afrouxar a
validação do `iss` do token - ver o comentário de `discoverWithRetry` em
`internal/auth/verifier.go`.

Os papéis e o `provider_id` do token viram um `auth.Identity` no contexto da
requisição (`internal/httpapi/auth_middleware.go`'s `authenticate`), antes
de qualquer roteamento acontecer - ordem autenticação → autorização →
validação → efeito. Um provedor sem `provider_id` no token é tratado como
`403`, não `401`: o token é genuíno, só a identidade que ele carrega está
incompleta.

### Fluxo para chamar a API

```sh
TOKEN=$(curl -s -X POST http://localhost:${KEYCLOAK_PORT:-8081}/realms/wallet/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=wallet-service \
  -d client_secret=wallet-service-secret \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')

curl -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"playerId":"<uuid>","initialBalance":{"amount":"10.00","currency":"BRL"}}' \
  http://localhost:${HTTP_PORT:-8080}/wallets
```

Isso funciona sem nenhum passo extra: `KC_HOSTNAME` fixa o `iss` de todo
token em `http://localhost:${KEYCLOAK_PORT:-8081}/realms/wallet`, o mesmo
endereço que `AUTH_ISSUER_URL` do container `app` valida contra - o mesmo
token buscado do host acima é o que a chamada a `app` (rodando no Compose)
aceita, direto, `201`. `KC_HOSTNAME_BACKCHANNEL_DYNAMIC` é o que permite essa
mesma consistência sem impedir o container `app` de alcançar Keycloak pelo
nome do serviço (`AUTH_DISCOVERY_URL=http://keycloak:8080/realms/wallet`,
ver "Validação" acima) - ver `docker-compose.yml`'s serviço `keycloak`.

Logs (`log/slog`) nunca registram o header `Authorization` nem o token em
si - `internal/httpapi`'s auth middleware só registra o resultado
(autenticado/negado), nunca a credencial.

## Migrations

O binário tem um subcomando dedicado:

```sh
go run ./cmd/wallet-service migrate up
go run ./cmd/wallet-service migrate down
```

usa `DATABASE_URL` do ambiente. No Compose, o serviço `migrate` já roda
`up` automaticamente antes da aplicação, depois que `postgres-provisioning`
já criou o papel `wallet_app` (ver "Credenciais do Postgres" acima). Um
`migrate down` seguido de `migrate up` nunca apaga nem recria esse papel ou
sua senha.

## Testes

```sh
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
```

Esses quatro comandos são o seam 1 - domínio e unidades, sem nenhuma
infraestrutura - e passam sempre, em qualquer ordem, mesmo sem Docker.

### `staticcheck`

```sh
go install honnef.co/go/tools/cmd/staticcheck@latest
staticcheck -tags "integration multiinstance faultinject" ./...
```

A primeira vez instala o binário em `$(go env GOPATH)/bin` (adicione ao
`PATH` se ainda não estiver); as próximas só rodam a checagem. As três
build tags juntas cobrem todo o código do repositório, incluindo os testes
de integração, multi-instância e os pontos de injeção de falhas - sem elas,
`staticcheck` nunca compila (e portanto nunca analisa) esses arquivos.
Sem apontamentos é o estado esperado; qualquer um novo é para corrigir, não
suprimir.

### Infraestrutura compartilhada entre suítes: cuidados de quem for verificar tudo de uma vez

`test/integration` e `test/multiinstance` apontam para o **mesmo** Postgres,
MiniStack e Keycloak do Compose - não sobem uma cópia isolada por suíte.
Isso é deliberado (é o que prova "várias instâncias reais, mesma
infraestrutura"), mas tem duas consequências para quem for rodar as duas
suítes em sequência, ou testar as chamadas manuais do "Fluxo para chamar a
API" acima antes de rodar os testes:

- **Pare o serviço `app` antes de rodar qualquer suíte com a tag
  `integration` ou `multiinstance`.** Uma instância `app` viva do Compose
  também tem seu próprio consumidor SQS, publisher de outbox e worker de
  referências rodando contra a mesma infraestrutura - ela disputa mensagens
  de `wager-transactions.fifo` e linhas `PENDING_REFERENCE` com o harness de
  teste, produzindo falhas intermitentes de métrica/timeout que não são bugs
  de produto. `docker compose stop app` resolve; `docker compose start app`
  (ou outro `up --build`) devolve depois.
- **Rode `test/multiinstance` antes de `test/integration`, ou reinicie o
  Postgres entre as duas.** Alguns cenários de `test/integration` (expiração
  de referência pendente, por exemplo) deixam propositalmente uma
  `PENDING_REFERENCE` cuja referência nunca chega, criada sob configurações
  curtas daquele teste - mas o worker de referências da suíte
  multi-instância a reavalia sob configurações de *produção*
  (`REFERENCE_WORKER_MAX_ATTEMPTS`/`RetryMax` bem maiores), o que pode levar
  minutos para esgotar. O sintoma é `drainBacklog`'s espera pelo indicador
  `pending_reference_active` chegando a zero estourando o prazo de 20s -
  ver `ARCHITECTURE.md`, "Trabalho não concluído". `docker compose down -v
  && docker compose up --build -d` entre as duas suítes (ou simplesmente
  multi-instância primeiro) evita o problema por completo; nenhuma das duas
  suítes precisa disso quando roda sozinha.

Testes de integração (tag `integration`) precisam da infraestrutura no ar.
`docker compose up -d` sozinho não espera os one-shots (`provisioning`,
`postgres-provisioning`, `migrate`) terminarem - ele volta assim que os
containers começam a rodar, e ler os arquivos de credenciais nesse instante
pode pegar um arquivo de uma subida anterior ou ainda incompleto.
`scripts/wait-for-integration.sh` resolve isso: sobe `postgres`, `ministack`
e `keycloak` com `docker compose up -d --wait` (aguardando os dois primeiros
saudáveis - `keycloak` não tem healthcheck, ver "Autenticação e
autorização"), roda `keycloak-wait` (one-shot que faz polling do endpoint de
descoberta OIDC até responder `200`, provando que o realm já foi importado)
e roda cada one-shot restante com `docker compose run --rm` em sequência - o
que bloqueia até cada um terminar e aborta o script (`set -euo pipefail`) no
primeiro que sair com código diferente de zero. O script não imprime nada em
stdout e não lê nem exporta nenhuma credencial - é só uma sequência de
comandos do Compose:

```sh
scripts/wait-for-integration.sh && go test -race -tags integration -count=1 ./...
```

Como o `&&` só roda `go test` se o script terminar com sucesso, um one-shot
que falhar aborta o fluxo inteiro com o código de saída do Compose, sem
rodar a suíte com infraestrutura incompleta ou credenciais de uma execução
anterior.

`test/integration` e `internal/app` leem `deploy/ministack/.runtime/app-credentials.env`,
`deploy/ministack/.runtime/test-credentials.env` e
`deploy/postgres/.runtime/credentials.env` diretamente (path default, ou
`APP_CREDENTIALS_FILE`/`TEST_CREDENTIALS_FILE`/`POSTGRES_CREDENTIALS_FILE`
para apontar para outro lugar) - nenhuma variável `SQS_*` ou de senha precisa
estar no ambiente, só os arquivos precisam existir, o que
`scripts/wait-for-integration.sh` já garante ao esperar `provisioning` e
`postgres-provisioning` terminarem antes de retornar. Se um arquivo não
existir, o teste falha pedindo para rodar `scripts/wait-for-integration.sh`.

- `internal/app`: composição Fx completa via `fxtest` - start/stop
  liberando recursos, e start falhando com config inválida.
- `internal/httpapi`: um start cujo `OnStart` de uma dependência anterior
  falha nunca chega a abrir o listener HTTP, e a porta continua livre para
  uma nova tentativa - reproduzido com um `fxtest.Lifecycle` isolado, sem
  precisar de Postgres nem MiniStack reais.
- `internal/auth`: o verificador OIDC contra um provedor OIDC falso local
  (`httptest`, sem Keycloak real) - assinatura válida extraindo papéis e
  `provider_id`, token sem papéis, assinatura adulterada rejeitada,
  audiência errada rejeitada, `exp`/`nbf` respeitando `AUTH_CLOCK_SKEW` com
  valores escritos à mão e um relógio injetado (tanto aceitando quanto
  rejeitando conforme a tolerância, nos dois lados - vencido e futuro),
  token sem `exp` rejeitado, e a descoberta OIDC expirando o prazo de start
  contra um issuer inalcançável.
- `internal/walletapp`: os casos de uso de abertura e leitura de carteira,
  com repositórios e unidade de trabalho falsos - abertura com saldo
  positivo grava transação, ledger e os dois eventos de outbox; saldo zero
  não grava nada além da carteira; conflito e falha de escrita não
  reconhecida são classificados corretamente.
- `test/integration`: as políticas IAM contra o MiniStack real - acesso por
  ação e por ARN (incluindo o ciclo completo de `DeleteMessage` do
  events-reader), chave desconhecida rejeitada, Deny explícito vencendo
  Allow (fixture `deny-probe`), e redrive para a DLQ (fixture
  `redrive-tester`) - ver "Fixtures de teste de IAM" acima. Também o
  contrato HTTP de `POST /wallets` e `GET /wallets/{walletId}` (seam 3a),
  via a aplicação Fx completa subida com `fxtest` numa porta livre
  (`appHarness`, em `apphttp_test.go`) - saldo positivo e zero, conflito,
  aberturas concorrentes, entrada inválida, leitura e falha transitória do
  banco por `lock_timeout`, com asserções no banco para ledger e outbox. E a
  autenticação/autorização contra um Keycloak real (`auth_test.go`, tokens
  obtidos por client via `keycloak_test.go`) - token ausente, assinatura
  inválida e token expirado devolvendo `401`; client sem papel e o papel
  `provider` em `/wallets` devolvendo `403` (em POST e em GET); `wallet-admin`
  passando; nenhum desses casos negados cria carteira.

## Injeção de falhas

`internal/faultinject.Trigger(point string)` é o único ponto de entrada do
mecanismo. Compilado sem a build tag `faultinject` (todo `go build`/`go test`
normal, incluindo os comandos acima), é um no-op puro - nenhuma variável de
ambiente é lida, nenhum ponto existe no binário produzido. Compilado com
`-tags faultinject`, ele compara `point` com a variável de ambiente
`FAULT_INJECT_POINT` e, se baterem, encerra o processo com `SIGKILL` contra
si mesmo - abrupto, incapturável, sem rollback nem handler de `SIGTERM`, para
que os testes de recuperação provem uma queda real, não um desligamento
gracioso. `internal/faultinject/faultinject_test.go` confirma o no-op mesmo
com `FAULT_INJECT_POINT` definida, no build sem a tag.

O ticket 15 entregou só o mecanismo - nenhuma chamada a `Trigger` existia
ainda em `internal/walletapp`, `internal/outbox` ou no consumidor SQS. O
ticket 16 liga os cinco pontos nomeados pela spec ("Injeção de falhas e
ambiente"), cada um logo antes ou depois da operação que o nome descreve:

| Ponto (`FAULT_INJECT_POINT`) | Onde | O que ele guarda |
| --- | --- | --- |
| `before-commit` | `internal/pg.UnitOfWork.Execute` (todo caso de uso HTTP - abertura, `Process`, `ResumePending`, `FailPending`, `ReschedulePendingAfterFailure`) e `internal/consumer.processOnce` (as duas transações SQS: nova operação e duplicata reconhecida) | Nada foi persistido ainda - a próxima linha é o `Commit`. |
| `after-commit-before-delete` | `internal/consumer.handle`, logo após `RecordOutcome`/a métrica de duplicata | A transação SQS (inbox + efeito na carteira) já comitou; só falta `DeleteMessage`. |
| `after-pending-reference-commit` | `internal/walletapp.Process` (HTTP) e `internal/consumer.handle` (SQS), só quando o resultado é `PENDING_REFERENCE` novo, nunca um replay | O `PENDING_REFERENCE` já comitou; falta responder ao chamador. |
| `after-outbox-claim-before-send` | `internal/outbox.Publisher.publish`, antes de `Queue.Send` | O registro já foi reivindicado (lease empurrado); nada foi enviado ao SQS ainda. |
| `after-outbox-send-before-confirm` | `internal/outbox.Publisher.publish`, depois de `Queue.Send` | O evento já chegou ao SQS; falta só `MarkPublished`. |

Cada ponto é um no-op fora da tag `faultinject` (a mesma verificação do
ticket 15, agora também exercida por esses cinco call sites via
`go vet ./...`/`go test -race ./...` sem a tag). Com a tag, `Trigger`
bloqueia (`select {}`) depois de mandar o `SIGKILL`: o próprio `syscall.Kill`
só enfileira o sinal, não interrompe a goroutine na mesma instrução - sem o
bloqueio, uma chamada de rede já em andamento (como o `Commit` ou o `Send`
que o ponto pretende impedir) podia terminar antes do processo
efetivamente morrer.

## Múltiplas instâncias

### Perfil do Compose

O perfil `multi` sobe três instâncias do serviço, cada uma na sua própria
porta, contra o mesmo Postgres, MiniStack e Keycloak - nunca uma cópia
isolada por instância, para que disputem de fato as mesmas carteiras:

```sh
docker compose --profile multi up -d --build app-1 app-2 app-3
```

Nomear os três serviços explicitamente limita o `up` a eles e às suas
próprias dependências (Postgres, MiniStack, Keycloak, migrations e os
one-shots de provisionamento) - o serviço `app` de instância única não sobe
junto. Portas padrão: `HTTP_PORT_1` (8091), `HTTP_PORT_2` (8092),
`HTTP_PORT_3` (8093) - substituíveis do mesmo jeito que `HTTP_PORT` (ver
`.env.example`). Verificação:

```sh
curl http://localhost:8091/health/ready
curl http://localhost:8092/health/ready
curl http://localhost:8093/health/ready
```

`docker compose --profile multi stop app-1 app-2 app-3` derruba as três sem
mexer no restante da infraestrutura.

### Harness de teste (seam 3b)

`test/multiinstance` (build tag `multiinstance`, separada de `integration`
porque é bem mais lenta: compila o binário com `-race -tags faultinject` e
sobe três processos reais do sistema operacional por teste, em vez de um
`fxtest` em processo) prova o mesmo resultado financeiro com três processos
independentes - cada um com suas próprias conexões e sua própria memória -
disputando as mesmas carteiras sobre a infraestrutura real. O banco só é
lido para asserções (`countLedgerEntries`, `netLedgerBalance`), nunca para
conduzir o cenário.

Precisa da mesma infraestrutura de `test/integration` no ar (Postgres,
MiniStack, Keycloak - não do Compose `app`/`app-1..3` em si, o harness sobe
os processos ele mesmo):

```sh
scripts/wait-for-integration.sh
go test -race -tags multiinstance -timeout 10m -count=1 ./test/multiinstance/...
```

Cobre: 80.00 + 80.00 sobre uma carteira de 100.00, cada `BET` numa instância
diferente, resultando em uma `PROCESSED`, uma `INSUFFICIENT_FUNDS`, saldo
`"20.00"` e um único débito; a mesma aposta enviada 50 vezes distribuída
entre as três instâncias, com um único débito; carteiras distintas
processadas em paralelo em instâncias diferentes; e as três instâncias
mortas com `SIGKILL` e reiniciadas, com o reenvio da mesma operação
devolvendo o resultado original (`idempotentReplay: true`) e o saldo
conferido contra o ledger. Cada teste encerra suas próprias instâncias com
`SIGTERM` ao final (ou, no cenário de reinício, já as matou) e falha se o
log combinado (stdout/stderr) de qualquer uma contiver um relatório do race
detector (`WARNING: DATA RACE`).

### Cenários de recuperação (ticket 16)

`test/multiinstance/faultinject_test.go`, mesma build tag `multiinstance` e
mesma infraestrutura acima. Cada cenário compila o mesmo binário
`-race -tags faultinject`, sobe uma instância "vítima" com
`FAULT_INJECT_POINT` num dos cinco pontos da tabela acima, espera-a morrer
de verdade (`waitExit`, nunca um `SIGTERM` externo) e só então sobe uma
instância "resgate" sem esse ponto para concluir o trabalho interrompido -
nunca uma queda simulada em memória, sempre um processo real morto no meio
de uma operação real contra o mesmo Postgres, MiniStack e Keycloak que as
instâncias saudáveis usam. Todo cenário fecha conferindo o saldo armazenado
contra créditos menos débitos do ledger (leitura direta do Postgres) e falha
se o log de qualquer instância contiver `WARNING: DATA RACE`.

```sh
scripts/wait-for-integration.sh
go test -race -tags multiinstance -timeout 10m -count=1 -run 'TestMultiInstance_ConsumerDies|TestMultiInstance_OutboxPublisher|TestMultiInstance_TwoPublishersDispute|TestMultiInstance_DiesAfterPendingReference|TestMultiInstance_PendingReferenceExpires|TestMultiInstance_RefundBeforeBet|TestMultiInstance_CrossingHTTPAndSQS' ./test/multiinstance/...
```

Cada cenário também roda isolado, com o mesmo `-run` trocado pelo nome do
teste:

- `TestMultiInstance_ConsumerDiesAfterCommitBeforeDelete_RedeliveredAndAcknowledgedOnce` -
  ponto `after-commit-before-delete`. A vítima debita a carteira e morre
  antes do `DeleteMessage`; a reentrega, após o `SQS_CONSUMER_VISIBILITY_TIMEOUT`
  curto expirar, é reconhecida pela inbox por outra instância, que apaga a
  mensagem sem debitar de novo - `sqs_consumer_duplicate_messages_total` da
  instância de resgate confirma a reentrega.
- `TestMultiInstance_ConsumerDiesBeforeCommit_RedeliveryProcessesOnce` -
  ponto `before-commit`. A vítima morre antes de comitar a transação SQS;
  nem a linha da inbox nem o efeito na carteira sobrevivem (uma linha não
  comitada nunca é visível a outra conexão), e a reentrega processa a
  operação uma única vez.
- `TestMultiInstance_OutboxPublisherDiesAfterSendBeforeConfirm_AnotherInstanceRepublishesSameEventID` -
  ponto `after-outbox-send-before-confirm`, com `OUTBOX_LEASE` curto. A
  vítima já enviou o evento ao SQS quando morre, sem confirmar
  `publishedAt`; outra instância reivindica o lease expirado e republica com
  o mesmo `eventId` - nenhum evento confirmado se perde, e o leitor
  (`events-reader`) dedupica por `eventId`.
- `TestMultiInstance_OutboxPublisherDiesAfterClaimBeforeSend_LeaseExpiresAndAnotherInstancePublishes` -
  ponto `after-outbox-claim-before-send`. A vítima morre logo após reivindicar
  o lote, sem nunca chamar `Send`; o lease expira e outra instância publica.
- `TestMultiInstance_DiesAfterPendingReferenceCommit_AnotherInstanceWorkerCompletes` -
  ponto `after-pending-reference-commit`. Um `REFUND` sem a `BET`
  referenciada ainda comita como `PENDING_REFERENCE` e a vítima morre antes
  de responder; a `BET` chega por outra instância, e o worker de referências
  pendentes dessa mesma instância conclui a operação.
- `TestMultiInstance_RefundBeforeBet_SurvivesAllInstancesRestarted` - sem
  ponto nomeado, mata as três instâncias saudáveis com `SIGKILL`
  (`restartTrio`, o mesmo "queda indiferenciada de todas ao mesmo tempo" do
  ticket 15) com o `REFUND` ainda `PENDING_REFERENCE`. A pendência sobrevive
  à perda da memória das três, e é resolvida assim que a `BET` chega ao
  trio reiniciado.
- `TestMultiInstance_CrossingHTTPAndSQS_ConsumerDiesMidway_SameOperationSettlesOnce` -
  ponto `after-commit-before-delete`, cruzando canais: a mesma operação
  processada por SQS numa vítima morta antes do delete é replay por HTTP
  numa segunda instância (`idempotentReplay: true`, mesma `transactionId`)
  enquanto a mensagem original ainda está invisível, e só depois reentregue
  por SQS a uma terceira - um único débito ao final, qualquer que seja o
  canal consultado.

Os helpers de SQS de teste (credenciais `gateway`/`events-reader`,
nome/URL das filas, envelope `WagerTransactionRequested`) vivem em
`test/testclient/sqs.go`, compartilhados pelos dois harnesses:
`test/integration/iam_test.go` e `test/integration/sqs_consumer_test.go`, de
um lado, e `test/multiinstance/sqs_test.go`, do outro, apenas delegam para
eles - ver os comentários dos arquivos e "Escopo descoberto" no ticket para
o histórico da extração.

## Idempotência e segundo `docker compose up`

`docker compose stop && docker compose up --build` reaproveita o que já
existe no MiniStack (filas, usuários, chaves) e no Postgres (a senha de
`wallet_app`) em vez de falhar ou rotacionar. A suíte de integração continua
passando depois de uma segunda subida, com as mesmas credenciais ou com
credenciais reaproveitadas - nunca rotacionadas sem necessidade.

## Processo de engenharia: spec-driven development

O desafio foi entregue em três dias (14 a 16 de setembro de 2026) com
desenvolvimento assistido por IA sob coordenação humana, dentro de um fluxo
**SDD - spec-driven development**: a especificação é o artefato de origem, o
código é derivado dela, e nada é implementado antes de existir uma decisão
escrita sobre o que deve acontecer e como isso será provado. Esta seção
descreve esse fluxo porque ele explica a forma do repositório: por que os
tickets estão na ordem em que estão, por que certos testes existem e por que
determinadas decisões foram tomadas do jeito que foram.

### 1. Interrogar o enunciado antes de escrever qualquer linha

Um enunciado de 466 linhas parece completo até você tentar implementá-lo. A
primeira etapa foi um interrogatório dirigido do texto, buscando toda
ambiguidade que viraria retrabalho depois: o que exatamente é `WIN` com
referência; o que acontece quando uma reversão chega antes da operação
referenciada; qual é o limite de `Money` e o que fazer no overflow; quais
falhas são transitórias e quais são permanentes; quem pode ler o quê; o que
significa "at-least-once" para cada canal de entrada.

Cada ambiguidade virou uma **decisão explícita**, com a alternativa
descartada e o motivo. Nove dessas decisões estruturam o sistema inteiro e
estão registradas no `ARCHITECTURE.md`, junto das interpretações adotadas
onde o enunciado deixava espaço.

### 2. Especificação como fonte única da verdade

As decisões foram consolidadas numa especificação de desenho cobrindo
dinheiro e precisão, máquina de estados, idempotência persistente, política
de locks e ordem de aquisição, referências pendentes, matriz de reversões,
inbox e outbox com o contrato de eventos, autenticação e autorização,
observabilidade e - a parte que mais economiza tempo depois - as **decisões
de teste**: quais são os seams públicos do sistema, o que cada seam prova e
o que deliberadamente não é testado ali.

Definir os seams antes de escrever testes evita o padrão mais comum de
suíte inútil: testes acoplados a implementação, que quebram quando o código
muda e passam quando o comportamento quebra. Aqui existem quatro seams -
domínio puro, aplicação com dependências em memória, contrato externo com
Postgres, Keycloak e SQS reais, e o harness de três instâncias com injeção
de falhas.

### 3. Tickets como fatias verticais, não como camadas

A spec virou 17 tickets em ordem topológica de dependência, cada um com
critérios de aceite verificáveis, as seções da spec que precisava atender e
a lista explícita do que provaria ao terminar. Os tickets são fatias
verticais: "BET, WIN e LOSS por HTTP com idempotência persistente" atravessa
domínio, aplicação, persistência e transporte de uma vez, em vez de entregar
uma camada inteira sem nada funcionando ponta a ponta.

As arestas de bloqueio entre tickets formam um grafo, não uma fila: o
esqueleto executável e o schema do banco destravam várias frentes em
paralelo, enquanto o worker de referências pendentes só pode começar depois
das reversões e do consumidor SQS.

### 4. Execução isolada, um agente por ticket

Cada ticket foi implementado por um agente em contexto próprio, na sua
worktree e branch, com acesso ao ticket, à spec e ao repositório - nunca ao
raciocínio de outro agente. Duas frentes, em famílias de modelo diferentes:
Claude Sonnet 5 no esqueleto, banco, HTTP, autenticação, reversões,
leituras, harness multi-instância, recuperação de falhas e documentação;
Codex GPT-5.6 em `Money`, domínio, regras e hash de idempotência, outbox,
consumidor SQS, ledger e referências pendentes.

O isolamento é deliberado: contextos independentes não compartilham os
mesmos pontos cegos, e a divergência entre eles aparece na revisão em vez de
virar um erro consistente no sistema todo.

### 5. Revisão adversarial, sempre por outro modelo

Nenhum agente revisou o próprio trabalho, nem o de um colega da mesma
família de modelo. Tickets fundacionais ou de maior risco passaram por um
painel de quatro lentes independentes - aderência à spec, padrões do
repositório, corretude e segurança - e os demais por uma passada combinada.
Cada revisor classificava os achados em Critical, Major, Minor ou
Suggestion, com arquivo, linha e o caminho concreto até a falha.

Critical e Major **bloqueavam o merge**. O ticket voltava para correção e
depois para uma nova revisão, que precisava confirmar item a item que o
achado tinha sido resolvido, sem regressão e sem escopo novo. Foram 59
relatórios e 127 achados ao longo da execução.

Alguns exemplos do que esse gate pegou, todos corrigidos antes do merge:
uma senha de banco versionada numa migration; um Keycloak com administrador
exposto; a credencial do dono do banco no ambiente da aplicação; uma
estrutura de operação preparada que podia ser forjada fora do caso de uso;
um lease de outbox calculado com o relógio do processo em vez do relógio do
Postgres; um `stop()` que retornava antes de os workers terminarem; e
cenários de injeção de falha que passariam mesmo sem a garantia que
alegavam provar.

### 6. Verificação como um avaliador, não como o autor

A última etapa não confia em nenhuma das anteriores. A entrega foi rodada a
partir de um clone limpo, seguindo os comandos deste README na ordem em que
eles aparecem: `gofmt`, `go vet` nas quatro combinações de build tags,
`staticcheck`, `go test`, `go test -race`, `docker compose up --build`, a
suíte de integração, a suíte multi-instância com os sete cenários de morte
de processo e, por fim, os exemplos de chamada autenticada colados daqui.

Essa passada encontrou três defeitos que nenhuma suíte via isoladamente:
testes de configuração que liam o ambiente do shell e quebravam logo depois
de rodar a suíte de integração como o próprio README ensina; uma dependência
de ordem entre as suítes, causada por uma linha de fixture que o worker de
referências nunca conseguiria resolver; e um cenário de recuperação cujo
prazo era menor que o lease padrão que ele mesmo precisava esperar. Os três
foram corrigidos antes da entrega - e nenhum deles teria aparecido rodando
cada suíte isoladamente, que é como o autor normalmente roda.

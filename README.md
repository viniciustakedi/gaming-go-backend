# jungle-gaming-wallet

Carteira de apostas distribuída em Go. Ver `.scratch/wallet-challenge/spec.md`
para o desenho completo; este README cobre só o que este ticket (esqueleto
executável) entrega: composição Fx, health checks, filas e IAM no MiniStack,
e o subcomando de migrations.

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

## Variáveis de ambiente

Ver `.env.example`. `docker compose` lê um `.env` na raiz automaticamente;
copie o exemplo se quiser mudar portas ou nomes de fila locais. As chaves de
acesso do SQS não vão no `.env` - são geradas pelo `provisioning` a cada
`docker compose up` e escritas em dois arquivos sob
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

Toda chamada exige `Authorization: Bearer <token>` com o papel
`wallet-admin`; ver "Autenticação e autorização" abaixo. `/health/*` e
`/metrics` continuam públicos.

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
go test -race ./...
```

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

Este ticket entrega só o mecanismo - nenhuma chamada a `Trigger` existe
ainda em `internal/wagering`, `internal/outbox` ou no consumidor SQS. O
ticket 16 adiciona os pontos nomeados do desenho (antes do commit, depois do
commit e antes do `DeleteMessage`, depois do commit do `PENDING_REFERENCE`,
depois do envio da outbox e antes da confirmação, depois do claim da outbox
e antes do envio - spec, "Injeção de falhas e ambiente") e os testes que
efetivamente definem `FAULT_INJECT_POINT` num processo do harness abaixo.

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

## Idempotência e segundo `docker compose up`

`docker compose stop && docker compose up --build` reaproveita o que já
existe no MiniStack (filas, usuários, chaves) e no Postgres (a senha de
`wallet_app`) em vez de falhar ou rotacionar. A suíte de integração continua
passando depois de uma segunda subida, com as mesmas credenciais ou com
credenciais reaproveitadas - nunca rotacionadas sem necessidade.

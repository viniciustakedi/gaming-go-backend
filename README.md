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
`127.0.0.1`), `ministack` (com `AUTH=true`), `provisioning` (one-shot: cria
as filas, o redrive para a DLQ, os quatro usuários IAM de menor privilégio e
os fixtures de teste de IAM - ver "Fixtures de teste de IAM" abaixo),
`postgres-provisioning` (one-shot: cria o papel `wallet_app`, se ainda não
existir, e define sua senha - ver "Credenciais do Postgres" abaixo),
`migrate` (one-shot: aplica as migrations - só depois que o papel já
existe) e por fim `app`, que só inicia depois que Postgres está saudável e
os one-shots de IAM/Postgres/migrations terminaram com sucesso.

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

Sem autenticação por enquanto (ticket 07 protege `/wallets*` com o papel
`wallet-admin`) - não expor esses endpoints fora do ambiente local.

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

Nenhum desses usuários, filas ou chaves participa do Compose da aplicação;
eles existem só para o `test/integration` rodar sem tocar na chave root.

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
`scripts/wait-for-integration.sh` resolve isso: sobe `postgres` e `ministack`
com `docker compose up -d --wait` (aguardando os dois saudáveis) e roda cada
one-shot com `docker compose run --rm` em sequência - o que bloqueia até cada
um terminar e aborta o script (`set -euo pipefail`) no primeiro que sair com
código diferente de zero. O script não imprime nada em stdout e não lê nem
exporta nenhuma credencial - é só uma sequência de comandos do Compose:

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
  banco por `lock_timeout`, com asserções no banco para ledger e outbox.

## Idempotência e segundo `docker compose up`

`docker compose stop && docker compose up --build` reaproveita o que já
existe no MiniStack (filas, usuários, chaves) e no Postgres (a senha de
`wallet_app`) em vez de falhar ou rotacionar. A suíte de integração continua
passando depois de uma segunda subida, com as mesmas credenciais ou com
credenciais reaproveitadas - nunca rotacionadas sem necessidade.

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

Isso sobe, nesta ordem: `postgres` (com healthcheck), `ministack` (com
`AUTH=true`), `provisioning` (one-shot: cria as filas, o redrive para a DLQ,
os quatro usuários IAM de menor privilégio e os fixtures de teste de IAM -
ver "Fixtures de teste de IAM" abaixo), `migrate` (one-shot: aplica as
migrations) e por fim `app`, que só inicia depois que Postgres está
saudável e os dois one-shots terminaram com sucesso.

`provisioning` é idempotente: rodar `docker compose up --build` de novo
contra o mesmo MiniStack reaproveita filas, usuários e chaves de acesso já
existentes em vez de falhar. `docker compose stop && docker compose up
--build` funciona sem precisar derrubar o volume do MiniStack.

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
`up` automaticamente antes da aplicação.

## Testes

```sh
gofmt -l .
go vet ./...
go test -race ./...
```

Testes de integração (tag `integration`) precisam da infraestrutura no ar:

```sh
docker compose up -d postgres ministack provisioning migrate

export DATABASE_URL="postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable"
export SQS_ENDPOINT_URL="http://localhost:4566"
export $(grep -v '^#' deploy/ministack/.runtime/app-credentials.env | xargs)

go test -race -tags integration ./...
```

`test/integration` lê `deploy/ministack/.runtime/test-credentials.env`
diretamente (path default, ou `TEST_CREDENTIALS_FILE` para apontar para
outro lugar) - não precisa exportar essas variáveis, só o arquivo precisa
existir, o que o passo `docker compose up ... provisioning` acima já
garante.

- `internal/app`: composição Fx completa via `fxtest` - start/stop
  liberando recursos, e start falhando com config inválida.
- `internal/httpapi`: um start cujo `OnStart` de uma dependência anterior
  falha nunca chega a abrir o listener HTTP, e a porta continua livre para
  uma nova tentativa - reproduzido com um `fxtest.Lifecycle` isolado, sem
  precisar de Postgres nem MiniStack reais.
- `test/integration`: as políticas IAM contra o MiniStack real - acesso por
  ação e por ARN (incluindo o ciclo completo de `DeleteMessage` do
  events-reader), chave desconhecida rejeitada, Deny explícito vencendo
  Allow (fixture `deny-probe`), e redrive para a DLQ (fixture
  `redrive-tester`) - ver "Fixtures de teste de IAM" acima.

## Idempotência e segundo `docker compose up`

`docker compose stop && docker compose up --build` reaproveita o que já
existe no MiniStack (filas, usuários, chaves) em vez de falhar. A suíte de
integração continua passando depois de uma segunda subida, com as mesmas
credenciais ou com credenciais reaproveitadas - nunca rotacionadas sem
necessidade.

#!/usr/bin/env bash
# Sobe a infraestrutura de integração e espera cada one-shot terminar com
# sucesso, abortando com código != 0 no primeiro que falhar
# (`set -euo pipefail`, e `docker compose run --rm` bloqueia até o container
# terminar e propaga o exit code dele). Não imprime nada em stdout e não lê
# nem exporta nenhuma credencial: os testes de integração leem os arquivos
# de runtime (deploy/ministack/.runtime/, deploy/postgres/.runtime/)
# diretamente, do jeito que os testes de IAM já fazem (ver README.md). Uso:
#
#   scripts/wait-for-integration.sh && go test -race -tags integration -count=1 ./...
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> subindo postgres, ministack e keycloak (aguardando saudáveis)" >&2
docker compose up -d --wait postgres ministack keycloak 1>&2

echo "==> aguardando o realm 'wallet' importado no keycloak" >&2
docker compose run --rm keycloak-wait 1>&2

echo "==> provisionando o papel wallet_app no postgres" >&2
docker compose run --rm postgres-provisioning 1>&2

echo "==> provisionando filas e usuários IAM no ministack" >&2
docker compose run --rm provisioning 1>&2

echo "==> aplicando migrations" >&2
docker compose run --rm migrate 1>&2

echo "==> infraestrutura pronta" >&2

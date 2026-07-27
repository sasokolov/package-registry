# Деплой в Kubernetes

Helm-чарт в `deploy/helm/registry`. Смоук-проверка чарта в одноразовом
kind-кластере: `deploy/helm/smoke.sh` (нужны `kind`, `kubectl`, `helm`,
`docker`).

## Установка

Секреты создаются отдельно — в values их класть нельзя (инварианты 8 и 12:
конфигурация декларативна, секреты не логируются и не хранятся в ConfigMap).
В конфиге они присутствуют как `${VAR}`; подстановку делает сам registry при
загрузке, поэтому файл остаётся ConfigMap'ом, hot-reload работает, а значения
с `&`, `|` и `\` не ломаются. Незаданная переменная — ошибка старта, а не
пустая строка.

```bash
kubectl create namespace registry

# Доступ к S3-совместимому хранилищу
kubectl -n registry create secret generic registry-s3 \
  --from-literal=access_key=... --from-literal=secret_key=...

# DSN PostgreSQL
kubectl -n registry create secret generic registry-postgres \
  --from-literal=dsn='postgres://registry:...@postgres:5432/registry'

helm install registry deploy/helm/registry -n registry \
  --set site.name=eu-1 \
  --set site.externalURL=https://registry.example.com \
  --set storage.s3.endpoint=minio.storage.svc:9000 \
  --set storage.s3.bucket=registry \
  --set ingress.enabled=true --set ingress.host=registry.example.com
```

Что разворачивается: Deployment (по умолчанию 2 реплики, rolling update
без просадки), HPA, PodDisruptionBudget, Service, опционально Ingress и
ServiceMonitor. Конфиг лежит в ConfigMap и **перечитывается на лету**
(SIGHUP/интервал), поэтому смена набора фидов не требует рестарта подов —
namespace ConfigMap обновляется kubelet'ом, процесс подхватывает.

Хранилище: для нескольких реплик — S3 (`storage.type: s3`). Файловый
бэкенд с несколькими репликами требует RWX-тома; чарт такой PVC умеет
создать, но S3 — поддерживаемый путь (инвариант 3).

БД: `database.existingSecret: ""` отключает PostgreSQL целиком. Тогда
чтение работает, а publish и статические токены — нет (инвариант 7).

## Токены

```bash
kubectl -n registry exec deploy/registry-registry -- \
  registry token create -name ci-bot -config /etc/registry/config.yaml
```

Секрет печатается один раз, в БД хранится только его хэш.

## Подключение GitLab CI

Кросс-сайтовая и «беспарольная» идентичность — OIDC id_tokens GitLab
(`auth.oidcIssuers` в values); статический токен подходит для простых
случаев. В сниппетах ниже `$REGISTRY_TOKEN` — либо статический токен, либо
`$CI_JOB_TOKEN`-подобный id_token из `id_tokens:`.

### npm / `.npmrc`

```yaml
job:
  id_tokens:
    REGISTRY_TOKEN:
      aud: package-registry
  before_script:
    - |
      cat > .npmrc <<EOF
      registry=https://registry.example.com/npm/npmjs/
      //registry.example.com/npm/npmjs/:_authToken=${REGISTRY_TOKEN}
      EOF
  script:
    - npm ci
```

### Maven / `settings.xml`

Maven-клиенты умеют только HTTP Basic — регистри принимает токен в поле
пароля (username игнорируется).

```yaml
job:
  id_tokens:
    REGISTRY_TOKEN: {aud: package-registry}
  before_script:
    - |
      cat > settings.xml <<EOF
      <settings>
        <servers>
          <server>
            <id>registry</id>
            <username>ci</username>
            <password>${REGISTRY_TOKEN}</password>
          </server>
        </servers>
        <mirrors>
          <mirror>
            <id>registry</id>
            <mirrorOf>*</mirrorOf>
            <url>https://registry.example.com/maven/maven-central</url>
          </mirror>
        </mirrors>
      </settings>
      EOF
  script:
    - mvn -s settings.xml verify
```

Публикация: `distributionManagement.repository.url` →
`https://registry.example.com/maven/<hosted-feed>`, тот же `<server>` с
`id`, совпадающим с id репозитория.

### Composer / `auth.json`

Composer-фиду обязателен `site.externalURL` (относительные URL он не
резолвит).

```yaml
job:
  before_script:
    - composer config repositories.registry composer https://registry.example.com/composer/packagist
    - composer config --global --auth http-basic.registry.example.com ci "${REGISTRY_TOKEN}"
  script:
    - composer install
```

### Terraform / `.terraformrc`

Terraform требует HTTPS для service discovery: registry должен быть за
TLS-ингрессом.

```yaml
job:
  before_script:
    - |
      cat > ~/.terraformrc <<EOF
      credentials "registry.example.com" {
        token = "${REGISTRY_TOKEN}"
      }
      EOF
  script:
    - terraform init
```

Источник модуля: `source = "registry.example.com/<ns>/<name>/<provider>"`.

### NuGet / `NuGet.config`

```yaml
job:
  before_script:
    - |
      cat > NuGet.config <<EOF
      <?xml version="1.0" encoding="utf-8"?>
      <configuration>
        <packageSources>
          <clear />
          <add key="registry" value="https://registry.example.com/nuget/nugetorg/v3/index.json" protocolVersion="3" />
        </packageSources>
        <packageSourceCredentials>
          <registry>
            <add key="Username" value="ci" />
            <add key="ClearTextPassword" value="${REGISTRY_TOKEN}" />
          </registry>
        </packageSourceCredentials>
      </configuration>
      EOF
  script:
    - dotnet restore
```

## Гео-репликация

Включается секцией `replication` в values (см. docs/geo-replication.md).
Внутренний API репликации слушает отдельный порт и закрывается
NetworkPolicy: чарт **не отрендерится**, если при `replication.enabled`
не заданы `peerCIDRs` или `peerNamespaceSelector` — открытый наружу
listener репликации означал бы право писать в этот сайт.

```bash
kubectl -n registry create secret generic registry-replication \
  --from-file=ca.crt --from-file=tls.crt --from-file=tls.key

helm upgrade registry deploy/helm/registry -n registry \
  --set site.name=eu-1 \
  --set replication.enabled=true \
  --set 'replication.peerCIDRs={10.20.0.0/16}' \
  --set-json 'replication.peers=[{"name":"us-1","url":"https://registry-us.example.com:8443","public_url":"https://registry-us.example.com","pull_interval":"10s"}]'
```

Набор пиров перечитывается на лету; смена адреса listener'а или
auth-материала требует рестарта (процесс пишет об этом в лог).

Каждый сайт должен иметь **собственные** PostgreSQL и S3: реплицируются
факты через журнал, а не базы данных.

## Наблюдаемость

`serviceMonitor.enabled=true` подключает Prometheus Operator. Ключевые
метрики: `registry_requests_total{feed,source}` (RPS и доля попаданий в
кэш), `registry_upstream_request_duration_seconds`,
`registry_upstream_breaker_state`, `registry_site_info`; для гео —
`registry_repl_lag`, `registry_repl_durable_lag` (RPO),
`registry_repl_publish_conflicts_total`, `registry_repl_feed_digest`.

Готовые артефакты: `deploy/observability/dashboard.json` (Grafana) и
`deploy/observability/alerts.yaml` (Prometheus rules). Дежурные сценарии —
`docs/runbooks.md`.

Нагрузочный baseline — `docs/perf.md` (`make load-test`).

## Обслуживание

```bash
# Сборка мусора: сначала всегда dry-run
kubectl -n registry exec deploy/registry-registry -- \
  registry gc -config /etc/registry/config.yaml
kubectl -n registry exec deploy/registry-registry -- \
  registry gc -config /etc/registry/config.yaml -delete -min-age 168h
```

GC удаляет блобы, на которые не ссылается ни один манифест, держит
advisory-lock на весь прогон и никогда не трогает блобы младше `-min-age`.

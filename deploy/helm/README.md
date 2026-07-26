# Деплой в Kubernetes

Helm-чарт в `deploy/helm/registry`. Смоук-проверка чарта в одноразовом
kind-кластере: `deploy/helm/smoke.sh` (нужны `kind`, `kubectl`, `helm`,
`docker`).

## Установка

Секреты создаются отдельно — в values их класть нельзя (инварианты 8 и 12:
конфигурация декларативна, секреты не логируются и не хранятся в ConfigMap).

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
  registry token create -name ci-bot -config /tmp/config.yaml
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

## Наблюдаемость

`serviceMonitor.enabled=true` подключает Prometheus Operator. Ключевые
метрики: `registry_requests_total{feed,source}` (RPS и доля попаданий в
кэш), `registry_upstream_request_duration_seconds`,
`registry_upstream_breaker_state`, `registry_site_info`.

Нагрузочный baseline — `docs/perf.md` (`make load-test`).

## Обслуживание

```bash
# Сборка мусора: сначала всегда dry-run
kubectl -n registry exec deploy/registry-registry -- \
  registry gc -config /tmp/config.yaml
kubectl -n registry exec deploy/registry-registry -- \
  registry gc -config /tmp/config.yaml -delete -min-age 168h
```

GC удаляет блобы, на которые не ссылается ни один манифест, держит
advisory-lock на весь прогон и никогда не трогает блобы младше `-min-age`.

# PLAN.md — фазы реализации

Правила работы с планом — в `PROMPT.md` и `CLAUDE.md`. Фазы выполняются строго
по порядку. Acceptance-критерии фазы — обязательные условия перехода дальше.

---

## Фаза 0 — скелет и тестовая обвязка

Цель: пустой, но собирающийся проект + инфраструктура, в которой можно честно
проверять всё последующее.

- [x] `go mod init` (спросить у пользователя module path), структура каталогов
      из CLAUDE.md, `Makefile` со всеми целями (пустые цели допустимы только
      в этой фазе).
- [x] `cmd/registry`: запуск HTTP-сервера, `/healthz`, `/readyz`, `/metrics`,
      slog JSON-логирование, graceful shutdown.
- [x] `core/config`: YAML-схема v0 (`server`, `storage`, `feeds: []`),
      загрузка + валидация + тесты на невалидные конфиги.
- [x] `conformance/`: docker compose с сервисами `minio`, `postgres`,
      `registry` (собирается из исходников multi-stage Dockerfile),
      `fake-upstream` (nginx/caddy со статикой из `conformance/fixtures/`).
- [x] Скрипт `conformance/run.sh`: поднять compose, дождаться readiness,
      выполнить сценарии из `conformance/scenarios/*.sh`, свести отчёт,
      погасить compose. Ненулевой exit при любом провале.
- [x] Первый сценарий-смоук: `curl /healthz` изнутри compose-сети.
- [x] `golangci-lint` конфиг; CI-заготовка `.gitlab-ci.yml` со стадиями
      lint → test → conformance (docker-in-docker или shell-runner — оставить
      комментарий с двумя вариантами).

**Acceptance:** `make build lint test conformance` зелёные локально; в репо нет
ни одной директории вне структуры из CLAUDE.md.

---

## Фаза 1 — ядро

Цель: рабочий generic-пайплайн без единого формат-модуля.

- [x] `core/config`: hot-reload (SIGHUP + периодическая перечитка), атомарная
      подмена снапшота конфига; невалидный новый конфиг отклоняется с логом,
      процесс продолжает работать со старым (инвариант 8). Отложено из Фазы 0.
- [x] `core/api`: все интерфейсы и типы из CLAUDE.md (`BlobStore`, `Presigner`,
      `FormatModule`, `Hoster`, `Policy`, `Intent`, `PackageCoordinate`,
      `Identity`, `Decision`, sentinel-ошибки). Пакет без внешних зависимостей.
- [x] `modules/storage/fs`: реализация BlobStore поверх директории; атомарная
      запись через tmp+rename; тесты.
- [ ] `modules/storage/s3`: minio-go; Put через multipart для больших блобов;
      реализация `Presigner`; тесты против MinIO из compose
      (build-tag `integration`).
- [ ] `core/state`: pgx-пул, goose-миграции (таблицы: `tokens`, `audit`,
      `publish_sessions`, `quarantine`), helper для advisory locks; тесты
      против Postgres из compose.
- [ ] `core/auth`: статические токены (создание через CLI-субкоманду
      `registry token create`, хэш в БД, кэш в памяти с TTL); OIDC: валидация
      JWT по JWKS издателя из конфига, маппинг claims → `Identity`
      (`project_path`, `ref`, `sub`); тесты с локально сгенерированным JWKS.
- [ ] `core/policy`: движок цепочки (первый Deny побеждает; пустая цепочка =
      Allow), регистрация политик как модулей; политика `allowlist` (glob по
      координатам из YAML) как референс; тесты.
- [ ] `core/pipeline` (read-path): cache lookup в BlobStore → miss →
      singleflight → advisory lock → fetch upstream (retry с джиттером,
      circuit breaker, per-upstream rate limit) → верификация чексуммы (если
      Intent её несёт) → Put в сторадж → отдача. Мутабельные метаданные: TTL
      из Intent + stale-while-revalidate + отдача stale при недоступном
      апстриме. Заголовок `X-Registry-Source` во всех ветках. Юнит-тесты с
      fake-BlobStore и fake-upstream (httptest): hit, miss, конкурентный miss
      (ровно один запрос к апстриму), упавший апстрим + свежий кэш, упавший
      апстрим + stale, битая чексумма.
- [ ] `core/server`: маршрут `/{format}/{feed}/...`, резолв фида из конфига,
      middleware-цепочка auth → policy → pipeline; anonymous-доступ
      конфигурируется per-feed.
- [ ] Prometheus-метрики: RPS по фидам, cache hit ratio, латентность апстримов,
      состояние circuit breaker'ов.

**Acceptance:** conformance-сценарий «эхо-формат» (тестовый FormatModule в
`conformance/`): скачивание файла через registry с fake-upstream → повторное
скачивание при выключенном fake-upstream отдаёт из кэша с
`X-Registry-Source: cache`.

---

## Фаза 2 — Maven и Terraform, proxy

- [ ] `modules/format/maven`: Parse путей `/{group}/{artifact}/{version}/{file}`
      и `maven-metadata.xml`; чексуммы `.sha1/.sha256` — отдавать из
      сохранённых при инжесте значений, не проксировать отдельными запросами;
      `maven-metadata.xml` — мутабельный (SWR), артефакты релизных версий —
      иммутабельные. SNAPSHOT-версии: вынести отдельной задачей в Фазу 5,
      в этой фазе SNAPSHOT-запросы → 404 с понятным телом.
- [ ] `modules/format/terraform`: service discovery (`/.well-known/terraform.json`),
      протокол Module Registry v1 (`versions`, `download` с `X-Terraform-Get`);
      download указывает на blob-эндпоинт registry, не на апстрим.
- [ ] Fixtures fake-upstream: минимальный Maven-репозиторий (2 библиотеки с
      настоящими jar/pom/чексуммами) и Terraform-registry (1 модуль, 2 версии).
- [ ] Conformance-сценарии (клиенты — официальные docker-образы
      `maven:3-eclipse-temurin-21`, `hashicorp/terraform`):
      - [ ] `mvn dependency:resolve` референс-проекта через registry
            (`settings.xml` с mirror) — успех, все артефакты в S3(MinIO).
      - [ ] Повторный resolve при выключенном fake-upstream — успех из кэша.
      - [ ] `terraform init` референс-модуля через registry — успех.
      - [ ] Запрос запрещённой allowlist-политикой координаты → 403, аудит-лог
            содержит запись с Identity и координатой.
      - [ ] Скачивание с анонимом при `anonymous: false` → 401; с валидным
            статическим токеном → 200.

- [ ] `make conformance-live`: наполнить цель — прогон сценариев фазы против
      реальных апстримов (Maven Central, registry.terraform.io), ручной
      запуск. Отложено из Фазы 0 (там цель — заглушка с exit 2).

**Acceptance:** все сценарии фазы зелёные в `make conformance`; golden-тесты
RewriteMetadata покрывают оба формата.

---

## Фаза 3 — hosting (Maven, Terraform) + политики v1

- [ ] Maven `Hoster`: PUT артефактов, immutability (повторный PUT той же
      релизной координаты → 409), пересборка `maven-metadata.xml` хостового
      фида при publish.
- [ ] Terraform `Hoster`: загрузка модуля (API: PUT архива + версия),
      генерация ответов `versions`/`download` из хранимого.
- [ ] Publish-права: политика/permission `publish` per-feed per-identity;
      OIDC-identity GitLab (claim `project_path`) как субъект прав.
- [ ] `policies/osv`: батч-запросы к OSV.dev API, кэш вердиктов в Postgres
      c TTL, режим `enforce|warn`, fail-open конфигурируемо (по умолчанию
      warn+fail-open, чтобы не ронять билды при недоступном OSV).
- [ ] `policies/license`: извлечение лицензии из метаданных (pom `<licenses>`,
      npm `license` — задел на Фазу 4), deny-список SPDX-идентификаторов.
- [ ] `policies/quarantine`: версия младше N часов (из метаданных апстрима) →
      Deny с отдельным кодом причины.
- [ ] Conformance: `mvn deploy` в хостовый фид от identity с правом → успех;
      без права → 403; повторный deploy той же версии → 409; скачивание
      пакета с запрещённой лицензией из fixtures → 403.

**Acceptance:** сценарии зелёные; для каждой политики есть unit-тесты Allow/
Deny/недоступности внешней зависимости.

---

## Фаза 4 — npm и Composer, proxy

- [ ] `modules/format/npm`: package root (`/{pkg}`, `/@{scope}%2f{pkg}`) —
      RewriteMetadata переписывает `dist.tarball` на registry, сохраняет
      `dist.integrity/shasum` для верификации; tarball — иммутабельный блоб;
      abbreviated metadata (`Accept: application/vnd.npm.install-v1+json`) —
      поддержать; `dist-tags` — мутабельные (SWR). Auth: `Bearer` из `.npmrc`.
- [ ] `modules/format/composer`: Composer v2 (`/p2/{vendor}/{pkg}.json`,
      включая `~dev`), переписывание `dist.url`, `metadata-url` в корневом
      `packages.json`.
- [ ] Fixtures: мини-срез npm (3 пакета, один scoped, с настоящими tarball и
      integrity) и Packagist-совместимый (2 пакета).
- [ ] Conformance (образы `node:22`, `composer:2`):
      - [ ] `npm ci` референс-проекта (lockfile с integrity) через registry —
            успех; повторно без fake-upstream — успех из кэша.
      - [ ] `npm ci` при подмененном tarball в fixtures (битая чексумма) —
            registry отдаёт ошибку, артефакт не закэширован.
      - [ ] `composer install` референс-проекта — успех; повторно офлайн от
            апстрима — успех.
      - [ ] Scoped-пакет ставится корректно.
- [ ] Golden-тесты RewriteMetadata: минимум 6 реальных package root'ов npm
      разной степени уродства (старые пакеты с нестандартными полями).

**Acceptance:** сценарии зелёные; cache hit ratio по повторному `npm ci`
в метриках > 0.95.

---

## Фаза 5 — NuGet (read), npm publish, Maven SNAPSHOT

- [ ] `modules/format/nuget`: Service Index (`/v3/index.json`) с ресурсами
      registry; RegistrationsBaseUrl, PackageBaseAddress (flat container),
      SearchQueryService — минимум для `dotnet restore`; всё через generic-
      пайплайн.
- [ ] npm `Hoster`: `PUT /{pkg}` (JSON с base64-attachment), immutability
      версий, `dist-tags` update, `npm publish`/`npm unpublish` (unpublish —
      только скрытие из индекса, блоб не удаляется).
- [ ] Maven SNAPSHOT: мутабельные `maven-metadata.xml` уровня версии,
      timestamp-версии артефактов, retention-настройка «хранить последние N».
- [ ] Conformance: `dotnet restore` референс-проекта (образ
      `mcr.microsoft.com/dotnet/sdk`) — успех и офлайн-повтор; `npm publish` от
      identity с правом → успех, повторный publish той же версии → 409;
      `mvn deploy` SNAPSHOT дважды → второй становится актуальным.

**Acceptance:** сценарии зелёные. Отдельно зафиксировать в `docs/decisions.md`
решение по Docker/OCI: рекомендация — Harbor рядом, свой OCI-модуль только если
пользователь явно попросит (обсудить с пользователем в конце фазы).

---

## Фаза 6 — HA-hardening и деплой

- [ ] Presigned redirects: если сторадж — `Presigner` и формат помечен
      redirect-safe (maven, npm tarball, terraform, nuget flat container) —
      отдавать 302 на presigned URL; конфиг-флаг per-feed; conformance-сценарий
      `npm ci` через redirect-режим.
- [ ] Chaos-сценарии в conformance (отдельная цель `make conformance-chaos`):
      - [ ] kill -9 одной из двух реплик registry посреди `npm ci` → установка
            завершается успешно (retry клиента/балансировщика).
      - [ ] Остановка Postgres → read-path с токеном из кэша работает,
            publish отдаёт 503 с внятным телом.
      - [ ] Остановка fake-upstream → stale-метаданные, билды проходят.
- [ ] Нагрузочный тест k6: профиль «CI-шторм» (200 конкурентных `npm ci`-подобных
      последовательностей), зафиксировать baseline p99 в `docs/perf.md`.
- [ ] `deploy/helm`: chart с Deployment (2+ реплики), HPA, PDB,
      ServiceMonitor, values для S3/Postgres/issuer'ов; README с примером
      установки и подключением GitLab CI (сниппеты `.npmrc`, `settings.xml`,
      `auth.json`, `.terraformrc`, NuGet.config через `CI_JOB_JWT`/id_token).
- [ ] GC: команда `registry gc` — удаление блобов без ссылок из манифестов,
      dry-run по умолчанию; advisory lock на весь прогон.

**Acceptance:** chaos- и нагрузочные сценарии зелёные; helm-чарт ставится в
kind-кластер скриптом `deploy/helm/smoke.sh` и проходит смоук.

---

## Вне плана (только по явному запросу пользователя)

Web UI; поиск по пакетам сверх протокольного минимума; репликация между
инстансами; OCI-модуль; PyPI/Cargo/Helm-модули (архитектура их допускает —
добавляются как обычные FormatModule).

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
- [x] `modules/storage/s3`: minio-go; Put через multipart для больших блобов;
      реализация `Presigner`; тесты против MinIO из compose
      (build-tag `integration`).
- [x] `core/state`: pgx-пул, goose-миграции (таблицы: `tokens`, `audit`,
      `publish_sessions`, `quarantine`), helper для advisory locks; тесты
      против Postgres из compose.
- [x] `core/auth`: статические токены (создание через CLI-субкоманду
      `registry token create`, хэш в БД, кэш в памяти с TTL); OIDC: валидация
      JWT по JWKS издателя из конфига, маппинг claims → `Identity`
      (`project_path`, `ref`, `sub`); тесты с локально сгенерированным JWKS.
- [x] `core/policy`: движок цепочки (первый Deny побеждает; пустая цепочка =
      Allow), регистрация политик как модулей; политика `allowlist` (glob по
      координатам из YAML) как референс; тесты.
- [x] `core/pipeline` (read-path): cache lookup в BlobStore → miss →
      singleflight → advisory lock → fetch upstream (retry с джиттером,
      circuit breaker, per-upstream rate limit) → верификация чексуммы (если
      Intent её несёт) → Put в сторадж → отдача. Мутабельные метаданные: TTL
      из Intent + stale-while-revalidate + отдача stale при недоступном
      апстриме. Заголовок `X-Registry-Source` во всех ветках. Юнит-тесты с
      fake-BlobStore и fake-upstream (httptest): hit, miss, конкурентный miss
      (ровно один запрос к апстриму), упавший апстрим + свежий кэш, упавший
      апстрим + stale, битая чексумма.
- [x] `core/server`: маршрут `/{format}/{feed}/...`, резолв фида из конфига,
      middleware-цепочка auth → policy → pipeline; anonymous-доступ
      конфигурируется per-feed.
- [x] Prometheus-метрики: RPS по фидам, cache hit ratio, латентность апстримов,
      состояние circuit breaker'ов.

**Acceptance:** conformance-сценарий «эхо-формат» (тестовый FormatModule в
`conformance/`): скачивание файла через registry с fake-upstream → повторное
скачивание при выключенном fake-upstream отдаёт из кэша с
`X-Registry-Source: cache`.

---

## Фаза 2 — Maven и Terraform, proxy

- [x] `modules/format/maven`: Parse путей `/{group}/{artifact}/{version}/{file}`
      и `maven-metadata.xml`; чексуммы `.sha1/.sha256` — отдавать из
      сохранённых при инжесте значений, не проксировать отдельными запросами;
      `maven-metadata.xml` — мутабельный (SWR), артефакты релизных версий —
      иммутабельные. SNAPSHOT-версии: вынести отдельной задачей в Фазу 5,
      в этой фазе SNAPSHOT-запросы → 404 с понятным телом.
- [x] `modules/format/terraform`: service discovery (`/.well-known/terraform.json`),
      протокол Module Registry v1 (`versions`, `download` с `X-Terraform-Get`);
      download указывает на archive-маршрут самого registry
      (`.../{version}/archive.tar.gz`), не на апстрим. Отдельный
      blob-эндпоинт для этого не нужен и не заводится: в гео-ADR peer-fetch
      живёт на internal listener'е (Фаза 7).
- [x] Fixtures fake-upstream: минимальный Maven-репозиторий (2 библиотеки с
      настоящими jar/pom/чексуммами) и Terraform-registry (1 модуль, 2 версии).
- [x] Conformance-сценарии (клиенты — официальные docker-образы
      `maven:3-eclipse-temurin-21`, `hashicorp/terraform`):
      - [x] `mvn dependency:resolve` референс-проекта через registry
            (`settings.xml` с mirror) — успех, все артефакты в S3(MinIO).
      - [x] Повторный resolve при выключенном fake-upstream — успех из кэша.
      - [x] `terraform init` референс-модуля через registry — успех.
      - [x] Запрос запрещённой allowlist-политикой координаты → 403, аудит-лог
            содержит запись с Identity и координатой.
      - [x] Скачивание с анонимом при `anonymous: false` → 401; с валидным
            статическим токеном → 200 (Bearer и HTTP Basic; `mvn` с
            `<server>`-кредами из settings.xml).
      - [x] Битая чексума в fixtures → 502, ничего не закэшировано.

- [x] `make conformance-live`: наполнить цель — прогон сценариев фазы против
      реальных апстримов (Maven Central, registry.terraform.io), ручной
      запуск. Отложено из Фазы 0 (там цель — заглушка с exit 2).
- [x] Задел гео (см. docs/geo-replication.md): зарезервировать в YAML-схеме
      ключи `site: {name, external_url}` и `replication: {}` (strict-парсер
      иначе уронит поды старого образа при rolling upgrade); `site` — в root
      slog, audit и info-метрику. Задокументировать лексикографический
      порядок `List` в контракте BlobStore; правило «meta/ и прокси-кэш —
      сайт-локальные производные, bucket-репликация поверх них запрещена».
      Публичный blob-эндпоинт по хэшу НЕ заводится (обходил бы цепочку
      политик): peer-fetch появится в Фазе 7 на отдельном internal
      listener'е с mTLS, как требует ADR.

**Acceptance:** все сценарии фазы зелёные в `make conformance`; golden-тесты
RewriteMetadata покрывают оба формата.

---

## Фаза 3 — hosting (Maven, Terraform) + политики v1

- [x] Maven `Hoster`: PUT артефактов, immutability (повторный PUT той же
      релизной координаты → 409), пересборка `maven-metadata.xml` хостового
      фида при publish.
- [x] Terraform `Hoster`: загрузка модуля (API: PUT архива + версия),
      генерация ответов `versions`/`download` из хранимого.
- [x] Publish-права: политика/permission `publish` per-feed per-identity;
      OIDC-identity GitLab (claim `project_path`) как субъект прав.
- [x] `policies/osv`: батч-запросы к OSV.dev API, кэш вердиктов в Postgres
      c TTL, режим `enforce|warn`, fail-open конфигурируемо (по умолчанию
      warn+fail-open, чтобы не ронять билды при недоступном OSV).
- [x] `policies/license`: извлечение лицензии из метаданных (pom `<licenses>`,
      npm `license` — задел на Фазу 4), deny-список SPDX-идентификаторов.
- [x] `policies/quarantine`: версия младше N часов (из метаданных апстрима) →
      Deny с отдельным кодом причины.
- [x] Conformance: `mvn deploy` в хостовый фид от identity с правом → успех;
      без права → 403; повторный deploy той же версии → 409; скачивание
      пакета с запрещённой лицензией из fixtures → 403.
- [x] Задел гео: hosted-коммит только через ядро — модуль стейджит блобы
      через `deps.Blobs()`, затем зовёт `CoreServices.Publish(feed, path,
      coord, sha256, size)` — единственная точка записи hosted-манифестов
      (сюда позже втыкается журнал; модули никогда не пишут `manifests/*`
      сами). Локальный инвариант 4 = PG unique (feed, coordinate) +
      manifest-exists → 409. Provenance в манифесте publish: origin, site,
      published_by. `Reindex` — детерминированная чистая функция множества
      манифестов; golden-тест «reindex дважды → байт-идентичный индекс».
      Токены: revoke = UPDATE revoked_at (не DELETE), добавить updated_at.
      Карантин ключуется (feed, coordinate) и проверяется в read-path
      (TTL-кэш, деградация при падении PG).

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
- [ ] Задел гео: npm dist-tags — отдельные именованные указатели в
      PG-состоянии (не внутри index-документа), Reindex их читает.

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
      dry-run по умолчанию; advisory lock на весь прогон. Задел гео:
      mark-and-sweep с минимальным возрастом блоба и грейсом ≥ журнального
      горизонта; GC выключен во время бутстрапа/ресинка сайта.

**Acceptance:** chaos- и нагрузочные сценарии зелёные; helm-чарт ставится в
kind-кластер скриптом `deploy/helm/smoke.sh` и проходит смоук.

---

## Фаза 7 — гео-репликация (мастер-мастер, журнальная федерация)

Дизайн: docs/geo-replication.md (принят по явному запросу пользователя).
Позиция фазы — после Фазы 5 (все Hoster'ы существуют, журнальный контракт
замораживается один раз); заделы разложены по Фазам 2–6. Открытые вопросы
из ADR решаются с пользователем до старта фазы.

- [ ] Событийная модель v1 в `core/repl`: `manifest_put`, `blob_available`,
      `token_revoke`, `quarantine_set`, `quarantine_release`,
      `conflict_resolve`; `schema_version` в каждом событии;
      tombstone-типы зарезервированы; неизвестный тип → park + алерт.
      Property-тест: случайные перестановки/дубли событий → байт-идентичное
      состояние на всех «сайтах».
- [ ] Миграция replication: `repl_journal` (UNIQUE(origin_site, origin_seq)),
      `repl_cursors`, peer_acks + durability watermark, `publish_conflicts`,
      dead-letter, parked, `hlc_state` + `repl_hlc_next()/repl_hlc_recv()`,
      site UUID. Seq выделяется внутри `repl_hlc_next()` под замком строки
      hlc_state → порядок коммитов == порядок seq (конкурентный тест:
      писатели молотят, читатель пагинирует — потерь ноль).
- [ ] JournalWriter (transactional outbox) в `CoreServices.Publish`;
      projection-outbox для S3-проекции манифестов + continuous repair +
      метрика дивергенции проекции.
- [ ] Applier: идемпотентный порядконезависимый merge; правило K1
      (канон = min sha256, карантин координаты, publish_conflicts, audit,
      метрика, алерт); token_revoke sticky по хэшу; события «из будущего»
      (> max_clock_skew) паркуются; ошибка переноса блоба → park + advance
      курсора (без head-of-line blocking).
- [ ] Внутренний API `/internal/replication/v1/{journal, blobs/sha256/*,
      manifest, snapshot, status, nudge}` на ОТДЕЛЬНОМ listener'е
      (обязателен при enabled); mTLS (prod) / bearer из token_file (dev);
      пиннинг (site, UUID) при handshake; origin-pinning событий
      (mesh-only, relay в v1 нет); peer-креды в логах — 8 символов хэша.
- [ ] Puller per (peer, origin): выборы через pg_try_advisory_lock с lease;
      advance курсора в одной транзакции с apply; 410 за горизонтом →
      авто snapshot-ресинк; GC журнала по min(ack peers, watermark) с
      потолком journal_retention; приёмник nudge.
- [ ] Блоб-перенос по digest: стриминг с проверкой sha256 всего тела,
      Range-докачка, multi-peer fallback; режимы eager (блоб до манифеста,
      watermark = RPO) / lazy per feed.
- [ ] `core/config`: `site{}`, `replication{}` (peers, auth file-refs,
      internal_listen, retention, skew, blob_fetch), per-feed
      `publish_policy` (дефолт `forward:<home>`), `replication_mode`,
      `peer_fallback`; `s3.*_file`-варианты кредов; валидация + hot-reload
      набора peer'ов.
- [ ] Publish-форвардинг: на не-home сайте локальная аутентификация +
      реверс-прокси на home с on-behalf-of identity; home недоступен →
      503 + Retry-After + указатель; НИКАКИХ 307 наружу. Конформанс с
      настоящими клиентами: `mvn deploy` и `npm publish` через не-home сайт.
- [ ] Peer-fallback в `core/pipeline` (per-feed): miss манифеста ИЛИ блоба →
      sha-пинованный fetch у peer'ов; `api.SourcePeer` + `X-Registry-Site`;
      negative cache с TTL.
- [ ] CLI: `registry repl status | backfill | resync | retry-dead-letter |
      resolve --feed --path --keep <sha256>` (журналируемое, аудируемое).
- [ ] Бутстрап нового сайта: snapshot → авто-backfill → Reindex всех
      hosted-фидов; до сходимости фиды живут через peer-fallback.
- [ ] Наблюдаемость: lag per (peer, origin), durability watermark,
      конфликты, dead-letter/parked, digest множества манифестов per feed
      (алерт на расхождение дольше окна лага); NetworkPolicy для
      internal listener в helm; Grafana dashboard + alert rules.
- [ ] `conformance/geo`: цель `make conformance-geo` — два полных стека
      (2× minio, 2× postgres, 2× registry), партиция через iptables:
      - [ ] publish@A → install@B до сходимости (source: peer, чексумма
            сверена) и после (cache); индекс на B пересобран Reindex'ом.
      - [ ] Партиция → конкурентный конфликтующий publish (policy: local) →
            heal → идентичный канон (min sha256) на обоих, координата в
            карантине, GET → 409 + X-Registry-Conflict, конфликт в
            publish_conflicts/audit/метриках → `repl resolve` → выдача
            восстановлена, оба блоба целы.
      - [ ] Партиция → чтение живо на обоих, publish на forward-фид со
            стороны не-home → 503 с указателем; heal → курсоры догоняют,
            digest'ы сходятся, алерты гаснут; повторный publish → 409.
      - [ ] token revoke@A → отказ на B в пределах lag + token_cache_ttl;
            create@A НЕ появляется на B; OIDC работает на обоих без
            репликации.
      - [ ] Бутстрап пустого третьего сайта → snapshot + backfill →
            hosted-фид отдаётся; GC журнала за курсором → 410 →
            авто-ресинк; «манифест есть/блоба нет» → peer-fallback спасает.
- [ ] Runbook'и: разбор конфликта, долгий ресинк, вывод/возврат peer'а,
      ротация peer-кредов, требование NTP; формула окна отзыва токена.

**Acceptance:** `make conformance-geo` зелёный целиком; property-тест
merge-конвергенции зелёный; ни один гео-сценарий не наблюдает молчаливой
дивергенции (digest-алерты) или подмены байтов.

---

## Вне плана (только по явному запросу пользователя)

Web UI; поиск по пакетам сверх протокольного минимума; OCI-модуль;
PyPI/Cargo/Helm-модули (архитектура их допускает — добавляются как обычные
FormatModule). Гео-репликация перенесена в план (Фаза 7) по запросу
пользователя 2026-07-26.

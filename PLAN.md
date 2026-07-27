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

- [x] `modules/format/npm`: package root (`/{pkg}`, `/@{scope}%2f{pkg}`) —
      RewriteMetadata переписывает `dist.tarball` на registry, сохраняет
      `dist.integrity/shasum` для верификации; tarball — иммутабельный блоб;
      abbreviated metadata (`Accept: application/vnd.npm.install-v1+json`) —
      поддержать; `dist-tags` — мутабельные (SWR). Auth: `Bearer` из `.npmrc`.
- [x] `modules/format/composer`: Composer v2 (`/p2/{vendor}/{pkg}.json`,
      включая `~dev`), переписывание `dist.url`, `metadata-url` в корневом
      `packages.json`.
- [x] Fixtures: мини-срез npm (3 пакета, один scoped, с настоящими tarball и
      integrity) и Packagist-совместимый (2 пакета).
- [x] Conformance (образы `node:22`, `composer:2`):
      - [x] `npm ci` референс-проекта (lockfile с integrity) через registry —
            успех; повторно без fake-upstream — успех из кэша.
      - [x] `npm ci` при подмененном tarball в fixtures (битая чексумма) —
            registry отдаёт ошибку, артефакт не закэширован.
      - [x] `composer install` референс-проекта — успех; повторно офлайн от
            апстрима — успех.
      - [x] Scoped-пакет ставится корректно.
- [x] Golden-тесты RewriteMetadata: минимум 6 реальных package root'ов npm
      разной степени уродства (старые пакеты с нестандартными полями).

**Acceptance:** сценарии зелёные; cache hit ratio по повторному `npm ci`
в метриках > 0.95.

---

## Фаза 5 — NuGet (read), npm publish, Maven SNAPSHOT

- [x] `modules/format/nuget`: Service Index (`/v3/index.json`) с ресурсами
      registry; RegistrationsBaseUrl, PackageBaseAddress (flat container),
      SearchQueryService — минимум для `dotnet restore`; всё через generic-
      пайплайн.
- [x] npm `Hoster`: `PUT /{pkg}` (JSON с base64-attachment), immutability
      версий, `dist-tags` update, `npm publish`/`npm unpublish` (unpublish —
      только скрытие из индекса, блоб не удаляется).
- [x] Maven SNAPSHOT: мутабельные `maven-metadata.xml` уровня версии,
      timestamp-версии артефактов, retention-настройка «хранить последние N».
- [x] Conformance: `dotnet restore` референс-проекта (образ
      `mcr.microsoft.com/dotnet/sdk`) — успех и офлайн-повтор; `npm publish` от
      identity с правом → успех, повторный publish той же версии → 409;
      `mvn deploy` SNAPSHOT дважды → второй становится актуальным.
- [x] Задел гео: npm dist-tags — отдельные именованные указатели в
      PG-состоянии (не внутри index-документа), Reindex их читает.

**Acceptance:** сценарии зелёные. Отдельно зафиксировать в `docs/decisions.md`
решение по Docker/OCI: рекомендация — Harbor рядом, свой OCI-модуль только если
пользователь явно попросит (обсудить с пользователем в конце фазы).

---

## Фаза 6 — HA-hardening и деплой

- [x] Presigned redirects: если сторадж — `Presigner` и формат помечен
      redirect-safe (maven, npm tarball, terraform, nuget flat container) —
      отдавать 302 на presigned URL; конфиг-флаг per-feed; conformance-сценарий
      `npm ci` через redirect-режим.
- [x] Chaos-сценарии в conformance (отдельная цель `make conformance-chaos`):
      - [x] kill -9 одной из двух реплик registry посреди `npm ci` → установка
            завершается успешно (retry клиента/балансировщика).
      - [x] Остановка Postgres → read-path с токеном из кэша работает,
            publish отдаёт 503 с внятным телом.
      - [x] Остановка fake-upstream → stale-метаданные, билды проходят.
- [x] Нагрузочный тест k6: профиль «CI-шторм» (200 конкурентных `npm ci`-подобных
      последовательностей), зафиксировать baseline p99 в `docs/perf.md`.
- [x] `deploy/helm`: chart с Deployment (2+ реплики), HPA, PDB,
      ServiceMonitor, values для S3/Postgres/issuer'ов; README с примером
      установки и подключением GitLab CI (сниппеты `.npmrc`, `settings.xml`,
      `auth.json`, `.terraformrc`, NuGet.config через `CI_JOB_JWT`/id_token).
- [x] GC: команда `registry gc` — удаление блобов без ссылок из манифестов,
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

- [x] Событийная модель v1 в `core/repl`: `manifest_put`, `blob_available`,
      `token_revoke`, `quarantine_set`, `quarantine_release`,
      `conflict_resolve`; `schema_version` в каждом событии;
      tombstone-типы зарезервированы; неизвестный тип → park + алерт.
      Property-тест: случайные перестановки/дубли событий → байт-идентичное
      состояние на всех «сайтах».
- [x] Миграция replication: `repl_journal` (UNIQUE(origin_site, origin_seq)),
      `repl_cursors`, peer_acks + durability watermark, `publish_conflicts`,
      dead-letter, parked, `hlc_state` + `repl_hlc_next()/repl_hlc_recv()`,
      site UUID. Seq выделяется внутри `repl_hlc_next()` под замком строки
      hlc_state → порядок коммитов == порядок seq (конкурентный тест:
      писатели молотят, читатель пагинирует — потерь ноль).
- [x] JournalWriter (transactional outbox) в `CoreServices.Publish`;
      projection-outbox для S3-проекции манифестов + continuous repair +
      метрика дивергенции проекции.
- [x] Applier: идемпотентный порядконезависимый merge; правило K1
      (канон = min sha256, карантин координаты, publish_conflicts, audit,
      метрика, алерт); token_revoke sticky по хэшу; события «из будущего»
      (> max_clock_skew) паркуются; ошибка переноса блоба → park + advance
      курсора (без head-of-line blocking).
- [x] Внутренний API `/internal/replication/v1/{journal, blobs/sha256/*,
      manifest, snapshot, status, nudge}` на ОТДЕЛЬНОМ listener'е
      (обязателен при enabled); mTLS (prod) / bearer из token_file (dev);
      пиннинг (site, UUID) при handshake; origin-pinning событий
      (mesh-only, relay в v1 нет); peer-креды в логах — 8 символов хэша.
- [x] Puller per (peer, origin): выборы через pg_try_advisory_lock с lease;
      advance курсора в одной транзакции с apply; 410 за горизонтом →
      авто snapshot-ресинк; GC журнала по min(ack peers, watermark) с
      потолком journal_retention; приёмник nudge.
- [x] Блоб-перенос по digest: стриминг с проверкой sha256 всего тела,
      Range-докачка, multi-peer fallback; режимы eager (блоб до манифеста,
      watermark = RPO) / lazy per feed.
- [x] `core/config`: `site{}`, `replication{}` (peers, auth file-refs,
      internal_listen, retention, skew, blob_fetch), per-feed
      `publish_policy` (дефолт `forward:<home>`), `replication_mode`,
      `peer_fallback`; `s3.*_file`-варианты кредов; валидация + hot-reload
      набора peer'ов.
- [x] Publish-форвардинг: на не-home сайте локальная аутентификация +
      реверс-прокси на home с on-behalf-of identity; home недоступен →
      503 + Retry-After + указатель; НИКАКИХ 307 наружу. Конформанс с
      настоящими клиентами: `mvn deploy` и `npm publish` через не-home сайт.
- [x] Peer-fallback в `core/pipeline` (per-feed): miss манифеста ИЛИ блоба →
      sha-пинованный fetch у peer'ов; `api.SourcePeer` + `X-Registry-Site`;
      negative cache с TTL.
- [x] CLI: `registry repl status | backfill | resync | retry-dead-letter |
      resolve --feed --path --keep <sha256>` (журналируемое, аудируемое).
- [x] Бутстрап нового сайта: snapshot → авто-backfill → Reindex всех
      hosted-фидов; до сходимости фиды живут через peer-fallback.
- [x] Наблюдаемость: lag per (peer, origin), durability watermark,
      конфликты, dead-letter/parked, digest множества манифестов per feed
      (алерт на расхождение дольше окна лага); NetworkPolicy для
      internal listener в helm; Grafana dashboard + alert rules.
- [x] `conformance/geo`: цель `make conformance-geo` — два полных стека
      (2× minio, 2× postgres, 2× registry), партиция через iptables:
      - [x] publish@A → install@B до сходимости (source: peer, чексумма
            сверена) и после (cache); индекс на B пересобран Reindex'ом.
      - [x] Партиция → конкурентный конфликтующий publish (policy: local) →
            heal → идентичный канон (min sha256) на обоих, координата в
            карантине, GET → 409 + X-Registry-Conflict, конфликт в
            publish_conflicts/audit/метриках → `repl resolve` → выдача
            восстановлена, оба блоба целы.
      - [x] Партиция → чтение живо на обоих, publish на forward-фид со
            стороны не-home → 503 с указателем; heal → курсоры догоняют,
            digest'ы сходятся, алерты гаснут; повторный publish → 409.
      - [x] token revoke@A → отказ на B в пределах lag + token_cache_ttl;
            create@A НЕ появляется на B; OIDC работает на обоих без
            репликации.
      - [x] Бутстрап пустого третьего сайта → snapshot + backfill →
            hosted-фид отдаётся; GC журнала за курсором → 410 →
            авто-ресинк; «манифест есть/блоба нет» → peer-fallback спасает.
- [x] Runbook'и: разбор конфликта, долгий ресинк, вывод/возврат peer'а,
      ротация peer-кредов, требование NTP; формула окна отзыва токена.

**Acceptance:** `make conformance-geo` зелёный целиком; property-тест
merge-конвергенции зелёный; ни один гео-сценарий не наблюдает молчаливой
дивергенции (digest-алерты) или подмены байтов.

---

## Фаза 8 — admin API и конфигурация как ресурс

Предпосылка: пользователь запросил Terraform-провайдер для фидов,
коннекторов и прав (2026-07-27) и выбрал модель «admin API поверх YAML в
блоб-хранилище». Инвариант 8 переформулирован соответственно: источник
правды остаётся одним декларативным YAML-документом вне БД, но у него
появляется API-путь записи.

- [x] `core/config`: источник конфигурации абстрагирован (`file` | `store`),
      версия документа = sha256, атомарная замена целиком, сидирование
      store из файла при первом старте, валидация ДО записи (невалидный
      документ не сохраняется никогда).
- [x] Оптимистическая блокировка: `GET` отдаёт `ETag`, `PUT` требует
      `If-Match`; конкурентная запись → 409 с текущей версией. Запись —
      под межрепличным advisory-локом (инвариант 9).
- [x] Права администратора: секция `admins` с теми же identity-паттернами,
      что у `publishers`; каждая мутация в аудит-лог с identity и diff'ом
      затронутых секций.
- [x] Admin API `/api/v1/config`: документ целиком и по-ресурсно
      (`/config/feeds/{name}`, `/config/peers/{name}`, `/config/oidc/{issuer}`,
      `/config/admins`), read-modify-write под локом.
- [x] Read API для UI и провайдера: `/api/v1/{status,whoami,feeds,
      feeds/{f}/packages,...,replication,conflicts,quarantine,tokens}`;
      секреты не отдаются никогда (инвариант 12).
- [x] Операторские мутации через API: выпуск/отзыв токена, карантин и его
      снятие, разрешение конфликта — те же кодовые пути, что у CLI.
- [x] Conformance: правка фида через API видна на ВСЕХ репликах без
      рестарта; невалидный документ отклонён и не сохранён; конкурентный
      PUT → 409; неадминистратор → 403; всё в аудите.

**Acceptance:** конфигурацией можно управлять по API, инвариант 8 в новой
формулировке не нарушен, `make conformance` и `make conformance-chaos`
зелёные.

## Фаза 9 — Web UI (SPA внутри бинаря)

- [x] `ui/`: React + TypeScript + Vite, сборка в `ui/dist`, встраивание
      через `go:embed`; `make build` собирает UI и бинарь, Dockerfile —
      multi-stage, CI — отдельная джоба. `ui/dist` не коммитится: в дереве
      лежит плейсхолдер, чтобы `go build ./...` работал без Node, а бинарь
      без консоли честно отвечает 503 на `/ui/` вместо тишины.
- [x] Отдача на `/` с SPA-фолбэком для deep links; ассеты с
      контент-хэшем и иммутабельным кэшем; `/api` и `/ui` зарезервированы
      от имён форматов и фидов. Промах в `assets/` — 404, а не документ:
      иначе рассинхрон сборки превращается в синтаксическую ошибку.
- [x] Экраны: обзор сайта, фиды, пакеты фида с поиском, детали версии,
      репликация и гео, конфликты, карантин, токены, редактор конфигурации.
- [x] Аутентификация в браузере: одно поле принимает и статический токен, и
      OIDC id_token — реестр различает их по форме, консоль ходит тем же
      `Authorization`-заголовком, что и npm/maven. Отдельного
      redirect-флоу (authorization code + PKCE) нет: он потребовал бы
      регистрации консоли как OIDC-клиента и нового callback-эндпоинта.
      Токен живёт в sessionStorage; localStorage — только по явному
      согласию. `whoami` решает, какие действия показывать.
- [x] Conformance: SPA отдаётся, deep link не 404, ассеты кэшируются и
      ревалидируются по ETag, промах в `assets/` — 404, API отвечает, фиды
      не потеряли ни одного пути (`91_console.sh`); правила отдачи покрыты
      unit-тестами в `ui/embed_test.go`.

**Acceptance:** `make conformance` включает UI-сценарии; бинарь
самодостаточен (никаких внешних ассетов).

## Фаза 10 — Terraform-провайдер

- [x] `terraform-provider-registry/` — отдельный Go-модуль,
      terraform-plugin-framework, версионирование независимое.
- [x] Ресурсы: `registry_feed` (включая upstream-коннектор, политики,
      publishers, redirect, publish_policy, replication_mode,
      peer_fallback), `registry_admin_binding`, `registry_oidc_issuer`,
      `registry_replication_peer`, `registry_token` (secret отдаётся один
      раз и попадает в state — задокументировано, импорт невозможен по той
      же причине), `registry_quarantine`.
- [x] Data sources: `registry_site`, `registry_feed`, `registry_feeds`,
      `registry_replication_status`.
- [x] Импорт существующей конфигурации, drift detection, plan-time
      валидация схемы фида на стороне провайдера (строгое подмножество:
      имена, enum'ы, длительности, JSON политик; всё, что зависит от
      остального документа, решает реестр).
- [x] Acceptance-тесты против настоящего registry в Docker
      (`make terraform-test`), примеры и сгенерированная документация
      (`make terraform-docs`).

Для этого в admin API добавлены пер-ресурсные эндпоинты OIDC-issuer'ов и
одиночных admin-биндингов, а типы конфигурации получили JSON-теги и
человекочитаемую сериализацию `Duration` — без этого запись фида через API
молча теряла бы `publish_policy`, `replication_mode` и остальные поля со
snake_case-именами.

**Acceptance:** `terraform apply` разворачивает набор фидов, коннекторов и
прав с нуля; повторный `plan` пуст; ручная правка через API детектируется
как drift. Проверяется дважды: acceptance-тестами (провайдер в процессе) и
`conformance/terraform/e2e.sh` — настоящий terraform CLI против настоящего
бинаря провайдера, apply → пустой plan → правка через API → drift → apply →
destroy, с проверкой, что фиды реально отдавали пакеты и выпущенный токен
реально публиковал.

---

## Фаза 11 — Группы фидов (Nexus-style repository groups)

Один URL, за которым и локально опубликованные, и проксированные пакеты.
Клиент настраивается один раз и не знает, откуда приехал артефакт.

- [x] `members: [a, b]` в описании фида делает его группой: только чтение,
      без `upstream`, `hosted` и `publishers`. Валидация: члены существуют,
      того же формата, без циклов, вложенность ограничена (5 уровней).
- [x] Артефакты — первый попавший член в порядке списка. Мутабельные
      индексы (maven-metadata.xml, npm packument, nuget flat/registration
      index, composer p2) — СКЛЕИВАЮТСЯ: иначе hosted-член спрячет версии
      из апстрима, и группа тихо потеряет пакеты.
- [x] Склейка — знание формата, значит capability модуля `GroupMerger`
      (инвариант 1). Формат без неё групп не поддерживает: конфигурация
      отклоняется с внятной ошибкой, а не молча отдаёт половину версий.
      Реализовано для maven, npm, nuget, composer; terraform-модуль групп
      пока не поддерживает и говорит об этом при валидации.
- [x] Права членов сохраняются: член, который вызывающему не виден,
      пропускается, а не раскрывается через группу. Политики члена
      применяются при обращении к нему; карантин члена скрывает координату
      из группы (все члены в карантине → 409, а не 404).
- [x] Publish в группу — 405 с указанием hosted-члена, куда публиковать.
- [x] Заголовки: `X-Registry-Member` (кто ответил) и `X-Registry-Merged`
      (чьи документы склеены); склеенный документ — `X-Registry-Source:
      local`, потому что его собрал этот сайт. Склеенное не кэшируется:
      копия каждого члена уже кэширована своим пайплайном.
- [x] Группы в UI (состав, порядок) и в Terraform-провайдере (`members`).
- [x] Conformance: `mvn` и `npm` тянут из группы `[hosted, proxy]` и
      локальный, и апстримный пакет; версии обоих видны в одном индексе;
      publish в группу отклонён.

**Acceptance:** группа `[group-hosted, central]` отдаёт
`mvn dependency:resolve` и локальную, и центральную зависимость, причём
диапазон `[1.0.0,)` разрешается в версию, которая есть ТОЛЬКО локально —
то есть клиент реально прочитал склеенный индекс; `npm install` из группы
ставит и апстримный, и локально опубликованный пакет
(`conformance/scenarios/92_groups.sh`).

---

## Фаза 12 — Хостинг NuGet

- [x] `dotnet nuget push`: `PUT /api/v2/package` (multipart), разбор
      `.nuspec` из zip'а, блоб + манифест по тем же путям, по которым
      кэшируется прокси — иначе группа `[hosted, proxy]` не смогла бы
      задать обоим членам один и тот же вопрос.
- [x] Факты из nuspec (лицензия, зависимости, published) сохраняются
      отдельной координатой, а не перечитываются из nupkg при каждом
      реиндексе; гео-пир, получивший только манифесты, пересобирает
      документы локально (инвариант 15).
- [x] Reindex генерирует flat-container `index.json` (версии в порядке
      версий, а не строк) и registration index с dependencyGroups —
      именно из него `dotnet restore` разрешает граф.
- [x] Push-эндпоинт объявляется в service index ТОЛЬКО у hosted-фида:
      иначе `dotnet nuget push` уедет в прокси и получит 405 без
      объяснений. Для этого `api.Feed` узнал про `Hosted`.
- [x] `X-NuGet-ApiKey` принимается как обычный bearer: имя заголовка
      объявляет модуль (`api.CredentialHeader`), ядро о нём не знает
      (инвариант 1). Явный `Authorization` всегда выигрывает.
- [x] `published` берётся из архива, а не из часов: два сайта, собирая
      один registration, обязаны получить одинаковые байты.
- [x] Hosted NuGet требует `site.external_url` — registration обязан
      нести абсолютные URL, и push отклоняется с этим текстом, а не
      публикует пакет, который никто не разрешит.
- [x] Conformance: `dotnet pack` → `dotnet nuget push` → `dotnet restore`
      из hosted-фида; повторная публикация версии — 409; публикация без
      права — отказ; группа `[nuget-hosted, nugetorg]` показывает и
      локальную 9.9.9, и апстримную 1.2.3 под одним id, и `dotnet
      restore` берёт любую из них.

**Acceptance:** `conformance/scenarios/93_nuget_publish.sh` — настоящий
`dotnet` публикует и восстанавливает пакет, а группа над hosted и proxy
отдаёт обе версии одного id.

---

## Фаза 13 — Хостинг Composer, terraform-группы, поиск NuGet

- [x] **Composer, хостинг.** У Composer нет команды публикации: Packagist
      читает пакеты из VCS, Satis рендерит статический репозиторий. Поэтому
      конвенция задана здесь и выбрана самая нескучная из возможных: архив
      кладётся PUT'ом туда, откуда будет отдаваться —
      `PUT /packages/{vendor}/{name}/{version}.zip`. Манифест — composer.json
      внутри архива; p2-документы и корневой manifest выводятся из него.
- [x] Имя в архиве и имя в пути обязаны совпадать: разойтись им — значит
      опубликовать пакет под именем, которого никто не ждал.
- [x] **Terraform-группы.** Источник модуля называет ХОСТ, а service
      discovery называет ровно один реестр на хост — поэтому группа это не
      удобство, а единственный способ отдавать свои модули и проксируемый
      реестр с одного адреса. `ValidateFeeds` теперь допускает несколько
      terraform-фидов ровно при одной группе, и discovery указывает на неё.
- [x] Попутно найдено и починено: hosted terraform был сломан на чтении.
      Publish клал архив по пути загрузки, а GET резолвился в путь
      indirection'а — архив хранился там, куда никто не приходит. Теперь
      публикуется по тому пути, в который резолвится запрос, и это
      закреплено тестом.
- [x] **Поиск NuGet.** `v3/query` у hosted-фида отвечает из опубликованных
      фактов (те же, из которых собирается registration, поэтому поиск не
      может разойтись с restore). Ранжирование простое и честное: точный
      id, префикс, вхождение.
- [x] Попутно: query-строка вообще не доезжала до апстрима — прокси спрашивал
      `<upstream>/v3/query` без параметров и кэшировал один ответ на все
      запросы. `Intent.RemoteQuery` теперь едет наверх и участвует в ключе
      кэша.
- [x] Поиск в группе склеивается, а не берётся у первого члена: hosted-член
      отвечает всегда, и first-hit означал бы, что результаты апстрима не
      видны никогда. Потерять ранжирование лучше, чем потерять результаты.
- [x] Conformance: `composer install` из hosted-фида и из группы;
      `terraform init` через discovery достаёт и локальную, и апстримную
      версию модуля; поиск NuGet находит запушенный пакет, пустой ответ на
      несуществующий термин, и два разных запроса дают два разных ответа.

**Acceptance:** `94_composer_publish.sh`, `95_terraform_group.sh` и
поисковая часть `93_nuget_publish.sh` — все три с настоящими клиентами.

---

## Вне плана (только по явному запросу пользователя)

Поиск по пакетам сверх протокольного минимума; OCI-модуль;
PyPI/Cargo/Helm-модули (архитектура их допускает — добавляются как обычные
FormatModule). Гео-репликация перенесена в план (Фаза 7) по запросу
пользователя 2026-07-26; Web UI и Terraform-провайдер — в Фазы 8–10 по
запросу 2026-07-27.

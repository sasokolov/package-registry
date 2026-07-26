# Журнал решений

Одна строка на решение. Крупные (меняющие интерфейсы из CLAUDE.md) — только
после согласования с пользователем, здесь фиксируется факт.

- 2026-07-26: module path — `github.com/sasokolov/package-registry` (реальный репозиторий пользователя, вместо плейсхолдера из CLAUDE.md).
- 2026-07-26: golangci-lint ставится пиновано через `go install` в `./bin` (без `curl | sh`), версия — переменная в Makefile.
- 2026-07-26: YAML-парсинг конфига — `gopkg.in/yaml.v3` со strict-режимом (`KnownFields`), неизвестные поля = ошибка.
- 2026-07-26: роутер — `chi/v5` с первой фазы; метрики — собственный `prometheus.Registry` (без глобального DefaultRegisterer).
- 2026-07-26: fake-upstream в conformance — nginx:alpine (не caddy): статики достаточно, образ меньше.
- 2026-07-26: клиент для conformance-сценариев — сервис `client` (curlimages/curl) в compose с profile `tools`, запуск через `docker compose run`.
- 2026-07-26: runtime-образ registry в conformance — alpine (не distroless): нужен wget для healthcheck.
- 2026-07-26: конфиг для `make dev` лежит в `conformance/dev.yaml` (отдельной dev-директории в структуре CLAUDE.md нет).
- 2026-07-26: OIDC/JWKS — `lestrrat-go/jwx/v2` (первый из двух вариантов CLAUDE.md): готовый auto-refresh JWKS-кэш.
- 2026-07-26: стораджи регистрируются в реестре (`api.RegisterStorage`), типизированный конфиг конвертируется в options-map через `StorageConfig.Options()`; выбор бэкенда — по имени, без switch в сборке.
- 2026-07-26: пустой `database.dsn` = БД отключена (никакого неявного fallback на PG*-env, чтобы не подключаться «куда-то» молча).
- 2026-07-26: SWR реализован как синхронный refresh с fallback на stale при недоступном апстриме (не асинхронный revalidate) — покрывает инвариант 6, проще в отладке; асинхронность можно добавить позже без смены контрактов.
- 2026-07-26: маршрутизация Bearer-кредов: префикс `reg_` → статический токен, две точки → JWT; иное → 401.
- 2026-07-26: echo-модуль для conformance живёт в `conformance/echomodule` и линкуется в бинарь только под build-tag `conformance` (в проде его нет).
- 2026-07-26: гео-репликация — журнальная федерация (app-level outbox + pull-mesh), НЕ стораджевая репликация и НЕ PG multi-master; ADR в docs/geo-replication.md, реализация — Фаза 7.
- 2026-07-26: конфликт конкурентных публикаций — правило K1 (канон = min sha256 + карантин + 409 + ручной resolve), не LWW по часам; дефолт записи — write-affinity (forward на home_site), active-active publish — opt-in per feed.
- 2026-07-26: audit сайт-локален; token create не реплицируется (репликация не создаёт полномочий), revoke реплицируется по хэшу.
- 2026-07-26: кросс-репличный advisory lock деградирует в lock-less ingest при недоступном Postgres (инвариант 7); безопасно из-за идемпотентных контент-адресуемых записей.
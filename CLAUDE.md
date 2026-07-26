# CLAUDE.md — модульный package registry

## Что это

Self-hosted package registry: проксирование (pull-through cache) и хостинг
пакетов для нескольких пакетных менеджеров через единое ядро. Ядро владеет
generic-пайплайном `auth → policy → cache → upstream → store → serve`; модули
владеют только трансляцией протокола конкретного пакетного менеджера. Стораджи
(FS, S3) — тоже модули. Деплой-цель: Kubernetes, N stateless-реплик, блобы в
S3-совместимом хранилище, динамическое состояние в PostgreSQL.

Язык: Go (актуальный stable). Модуль: `github.com/sasokolov/package-registry`.

## Архитектурные инварианты (уровень ADR — нарушать нельзя)

1. **Ядро не знает форматов.** Ни один файл вне `modules/format/*` не содержит
   логики, специфичной для npm/maven/etc. Признак нарушения — `switch` по имени
   формата в ядре.
2. **Модули не знают инфраструктуры.** Формат-модуль не импортирует S3-клиент,
   pgx, HTTP-клиент апстрима. Всё нужное приходит через интерфейсы `CoreServices`.
3. **Реплики stateless.** Никакого состояния в памяти процесса, потеря которого
   ломает корректность. Локальные кэши — только производные, с TTL.
4. **Иммутабельность релизов.** Опубликованная версия пакета никогда не
   перезаписывается. Publish поверх существующей координаты → 409.
5. **Чексуммы обязательны.** При инжесте из апстрима артефакт верифицируется по
   чексумме из метаданных (где протокол её даёт); чексумма сохраняется рядом с
   блобом. Несовпадение → артефакт не сохраняется и не отдаётся.
6. **Stale-while-revalidate.** Мутабельные метаданные при недоступном апстриме
   отдаются протухшими (с заголовком `X-Registry-Source: stale`), read-path не
   падает из-за падения апстрима.
7. **Read-path переживает падение Postgres.** БД нужна для publish, выдачи
   токенов, аудита; скачивание при закэшированной аутентификации продолжает
   работать (деградация, не отказ).
8. **Конфигурация декларативна.** Фиды, коннекторы, permissions — YAML-файлы,
   перечитываемые по SIGHUP/интервалу. В БД конфигурация не хранится.
9. **Никакого Redis.** Межрепличная координация — PostgreSQL advisory locks.
   Внутрипроцессная дедупликация — singleflight.
10. **Контент-адресуемые блобы.** Блоб хранится по `blobs/sha256/<hash>`,
    координаты пакета — указатели (манифесты) на блобы. Дедупликация и GC
    опираются на это.
11. **Каждый ответ помечен источником.** Заголовок `X-Registry-Source:
    cache|upstream|stale|local`.
12. **Секреты не логируются.** Токены в логах — только первые 8 символов хэша.

## Ключевые интерфейсы (стабильные контракты)

Определены в `core/api` (см. Фазу 1 плана). Менять сигнатуры — только по
согласованию с пользователем. Ориентиры:

```go
type BlobStore interface {
    Get(ctx context.Context, key string) (io.ReadCloser, BlobInfo, error)
    Put(ctx context.Context, key string, r io.Reader, opts PutOpts) error
    Stat(ctx context.Context, key string) (BlobInfo, error)
    Delete(ctx context.Context, key string) error
    List(ctx context.Context, prefix string) (Iter[BlobInfo], error)
}
type Presigner interface { // optional capability, через type assertion
    PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

type FormatModule interface {
    Name() string
    Routes() []Route
    Parse(r *http.Request) (Intent, error)
    RewriteMetadata(feed Feed, upstreamBody []byte) ([]byte, error)
}
type Hoster interface { // optional capability модуля
    HandlePublish(ctx context.Context, feed Feed, r *http.Request, deps CoreServices) error
    Reindex(ctx context.Context, feed Feed, deps CoreServices) error
}

type Policy interface {
    OnResolve(ctx context.Context, id Identity, c PackageCoordinate) Decision
    OnServe(ctx context.Context, id Identity, a Artifact) Decision
    OnPublish(ctx context.Context, id Identity, a Artifact) Decision
}
```

`Intent{Kind, Coord, CacheTTL}` — каноническое намерение запроса; весь generic-
пайплайн работает только с ним. Регистрация модулей — компайл-тайм, через
`init()` + central registry (стиль Caddy). Интерфейсы проектируются так, будто
модуль может жить в другом процессе: только сериализуемые типы, никаких
разделяемых глобалов.

## Технологии

- HTTP: stdlib `net/http` + `chi` для роутинга. Без тяжёлых фреймворков.
- S3: `minio-go` (покрывает AWS S3, MinIO, Ceph RGW).
- Postgres: `pgx/v5`, миграции — `goose`, embedded в бинарь.
- Auth: статические токены (хэш в БД) + OIDC-валидация GitLab CI id_tokens по
  JWKS (`lestrrat-go/jwx` или `golang-jwt` + свой JWKS-кэш). Issuer'ы — в YAML.
- Observability: `log/slog` (JSON), `prometheus/client_golang`, `/healthz`,
  `/readyz`, аудит-лог отдельным slog-логгером.
- Тесты: stdlib `testing`, golden-файлы для RewriteMetadata, `docker compose`
  для conformance. Линт: `golangci-lint`.

## Структура репозитория

```
cmd/registry/          — main, сборка модулей через импорты
core/api/              — интерфейсы и канонические типы (без зависимостей)
core/server/           — HTTP, роутинг фидов, middleware
core/pipeline/         — generic read-path: cache, singleflight, upstream, SWR
core/auth/             — token store, OIDC/JWKS, Identity
core/policy/           — движок цепочки политик
core/config/           — YAML-схема фидов, загрузка, валидация, hot-reload
core/state/            — pgx, миграции, advisory locks
modules/storage/fs/
modules/storage/s3/
modules/format/maven/
modules/format/terraform/
modules/format/npm/
modules/format/composer/
modules/format/nuget/
policies/allowlist/  policies/osv/  policies/license/  policies/quarantine/
conformance/           — docker compose, fake-upstream fixtures, сценарии
deploy/helm/
docs/decisions.md      — журнал мелких решений (одна строка на решение)
```

## Конвенции кода

- Ошибки: `fmt.Errorf("...: %w", err)`, sentinel-ошибки в `core/api/errors.go`
  (`ErrNotFound`, `ErrForbidden`, `ErrChecksumMismatch`, `ErrImmutable`...).
- Контексты везде; никаких `context.Background()` глубже `main` и тестов.
- Никаких глобальных переменных состояния; DI через конструкторы.
- Табличные тесты; для трансляции протоколов — golden-файлы в
  `modules/format/<x>/testdata/`.
- Комментарии и идентификаторы — английский; docs/ и PLAN — русский.

## Команды

```
make build          # go build ./...
make test           # unit-тесты
make lint           # golangci-lint run
make conformance    # docker compose: герметичный прогон с fake-upstream
make conformance-live  # то же против реальных апстримов (ручной запуск)
make dev            # локальный запуск: compose с minio+postgres, registry на хосте
```

## Definition of Done (для каждой задачи)

1. `make lint test` — зелёные.
2. Затронутый путь покрыт тестом (unit или conformance).
3. Инварианты выше не нарушены.
4. Чекбокс в PLAN.md отмечен, коммит сделан.
5. Нет молчаливых заглушек: паникующих stub'ов, пустых реализаций без задачи в
   плане.

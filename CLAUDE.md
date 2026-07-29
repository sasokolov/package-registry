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
   перезаписывается. Publish поверх существующей координаты → 409. В
   федерации по умолчанию write-affinity: у hosted-фида есть `home_site`,
   publish на чужом сайте аутентифицируется локально и проксируется на home
   (home недоступен → 503, не 307 и не тихая очередь). В opt-in режиме
   active-active кросс-сайтовый конфликт (одна координата, разные sha256)
   разрешается правилом K1 (см. docs/geo-replication.md): детерминированное
   каноническое состояние (наименьший sha256), координата в карантине с 409
   до явного `repl resolve`. Байты релиза никогда не подменяются молча.
5. **Чексуммы обязательны.** При инжесте из апстрима артефакт верифицируется по
   чексумме из метаданных (где протокол её даёт); чексумма сохраняется рядом с
   блобом. Несовпадение → артефакт не сохраняется и не отдаётся. Межсайтовый
   перенос блоба самоверифицируем: ключ и есть sha256, принимающий сайт
   хэширует поток и отбрасывает несовпадение.
6. **Stale-while-revalidate.** Мутабельные метаданные при недоступном апстриме
   отдаются протухшими (с заголовком `X-Registry-Source: stale`), read-path не
   падает из-за падения апстрима.
7. **Read-path переживает падение Postgres и потерю любого peer-сайта.** БД
   нужна для publish, выдачи токенов, аудита; скачивание при закэшированной
   аутентификации продолжает работать (деградация, не отказ). Недоступный
   peer для read-path — это недоступный upstream (stale/fallback/404 под
   circuit breaker'ом), никогда не каскад 5xx.
8. **Конфигурация декларативна и живёт вне БД.** Источник правды — один
   YAML-документ: файл на диске (`config.source: file`, дефолт) либо объект
   в блоб-хранилище (`config.source: store`), который все реплики
   перечитывают по интервалу/SIGHUP. Мутации — только через admin API,
   который валидирует документ целиком, пишет его атомарно под межрепличным
   локом и с оптимистической блокировкой по версии (sha256 документа);
   частичных правок «на месте» не существует. В БД конфигурация
   по-прежнему не хранится, и её недоступность не мешает читать: реплика
   держит последний валидный снимок в памяти.
9. **Никакого Redis.** Межрепличная координация — PostgreSQL advisory locks.
   Внутрипроцессная дедупликация — singleflight.
10. **Контент-адресуемые блобы.** Блоб хранится по `blobs/sha256/<hash>`,
    координаты пакета — указатели (манифесты) на блобы. Дедупликация и GC
    опираются на это.
11. **Каждый ответ помечен источником.** Заголовок `X-Registry-Source:
    cache|upstream|stale|local|peer`; в федерации дополнительно
    `X-Registry-Site: <site>`; конфликтная координата — 409 с
    `X-Registry-Conflict`.
12. **Секреты не логируются.** Токены в логах — только первые 8 символов хэша.
13. **Через границу сайта — только журнал.** Единственный канал межсайтовой
    репликации — append-only журнал событий: transactional outbox в PG
    сайта, pull по курсорам через отдельный internal listener, идемпотентный
    порядконезависимый apply; порядок seq совпадает с порядком коммитов.
    Стораджевая репликация (CRR, MinIO site replication) как механизм
    корректности запрещена; допустима только как необязательный ускоритель
    переноса блобов.
14. **Репликация не создаёт полномочий.** События федерации могут только
    отзывать доступ (token_revoke, карантин) и добавлять контент,
    верифицируемый по хэшу. Кросс-сайтовая идентичность — OIDC; статические
    токены сайт-локальны, отзыв реплицируется по хэшу. Applier принимает
    события только от аутентифицированного peer'а с совпадающим origin;
    internal API никогда не живёт на публичном listener'е.
15. **Производные данные не пересекают WAN.** Прокси-кэш (артефакты, meta/)
    каждый сайт наполняет от своего апстрима; индексы hosted-фидов
    пересобираются локально через `Hoster.Reindex` из сошедшегося множества
    манифестов. Реплицируются только факты: манифесты, блобы по sha256,
    отзывы, карантин.
16. **Дивергенция детектируется во всех режимах.** Каждый сайт экспортирует
    lag, durability watermark (RPO), конфликты, dead-letter и digest
    множества манифестов per feed; расхождение digest'ов дольше окна лага —
    алерт, а не тишина.

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
- Authz: модель Vault — именованные политики путей с capabilities и явными
  `deny`, привязки к claim'ам аутентификации; по умолчанию не разрешено ничего,
  решает самое специфичное правило, `deny` бьёт grant'ы на своём уровне.
  Старые поля `anonymous`/`publishers`/`admins` компилируются в те же политики —
  движок один. Подробности: `docs/access-control.md`.
- Observability: `log/slog` (JSON), `prometheus/client_golang`, `/healthz`,
  `/readyz`, аудит-лог отдельным slog-логгером. Метрики использования — по
  фидам и группам, никогда по пакетам: координат неограниченно много.
- Тесты: stdlib `testing`, golden-файлы для RewriteMetadata, `docker compose`
  для conformance. Линт: `golangci-lint`.

## Структура репозитория

```
cmd/registry/          — main, сборка модулей через импорты
core/api/              — интерфейсы и канонические типы (без зависимостей)
core/server/           — HTTP, роутинг фидов, middleware
core/pipeline/         — generic read-path: cache, singleflight, upstream, SWR
core/auth/             — token store, OIDC/JWKS, Identity
core/access/           — RBAC: политики путей, capabilities, привязки, explain
core/policy/           — движок цепочки политик
core/config/           — YAML-схема фидов, загрузка, валидация, hot-reload
core/state/            — pgx, миграции, advisory locks
core/repl/             — гео-федерация: журнал, applier, puller (Фаза 7)
core/usage/            — инвентарь фидов и счётчики трафика, метрики
modules/storage/fs/
modules/storage/s3/
modules/format/maven/
modules/format/terraform/
modules/format/npm/
modules/format/composer/
modules/format/nuget/
modules/format/helm/
modules/format/oci/
modules/internal/semver/  — общий semver-компаратор (npm и helm; maven — свой)
policies/allowlist/  policies/osv/  policies/license/  policies/quarantine/
ui/                    — консоль: React+TS+Vite, встраивается go:embed
terraform-provider-registry/ — Terraform-провайдер (отдельный Go-модуль)
conformance/           — docker compose, fake-upstream fixtures, сценарии
deploy/helm/
docs/decisions.md      — журнал мелких решений (одна строка на решение)
docs/geo-replication.md — ADR гео-репликации (журнальная федерация, правило K1)
docs/access-control.md — модель доступа: пути, capabilities, политики, привязки
docs/usage.md          — что лежит в фидах и как это используется
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
make build          # сборка консоли (make ui) + go build ./...
make ui             # только консоль: npm ci && vite build -> ui/dist
make test           # unit-тесты
make lint           # golangci-lint run
make conformance    # docker compose: герметичный прогон с fake-upstream
make conformance-chaos # два реплики + инъекция отказов
make conformance-geo   # два гео-сайта: репликация, конфликты, партиция
make conformance-live  # то же против реальных апстримов (ручной запуск)
make terraform-build   # провайдер: build/vet/lint/unit (отдельный модуль)
make terraform-test    # acceptance-тесты провайдера против registry в Docker
make terraform-docs    # регенерация docs/ провайдера из схем и examples/
make test-integration  # тесты, которым нужны настоящие minio и postgres
make load-test         # k6 против поднятого стенда, baseline в docs/perf.md
make dev            # локальный запуск: compose с minio+postgres, registry на хосте
make dev-ha         # тот же стенд в деплой-форме: две реплики за балансировщиком
make dev-down / dev-ha-down  # погасить соответствующий стенд
make smoke          # живой смоук по всем форматам против поднятого стенда
```

## Definition of Done (для каждой задачи)

1. `make lint test` — зелёные.
2. Затронутый путь покрыт тестом (unit или conformance).
3. Инварианты выше не нарушены.
4. Чекбокс в PLAN.md отмечен, коммит сделан.
5. Нет молчаливых заглушек: паникующих stub'ов, пустых реализаций без задачи в
   плане.

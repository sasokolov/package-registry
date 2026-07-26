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
- 2026-07-26: conformance-гейтвей — nginx с self-signed сертификатом registry.local (terraform требует HTTPS для registry-протокола); тестовый приватный ключ закоммичен сознательно, это фикстура.
- 2026-07-26: maven-клиент conformance — образ с запечёнными при docker build плагинами (dependency-plugin по полным координатам) и удалёнными `_remote.repositories`; герметичность уровня «pull базовых образов».
- 2026-07-26: X-Terraform-Get отдаётся абсолютным URL из `site.external_url` — terraform отвергает голые относительные пути («cannot detect a supported module source type»).
- 2026-07-26: terraform-модуль проксирует только HTTP(S)-архивы; VCS-источники (`git::…`, весь публичный registry.terraform.io) → внятный 502: git-операции вне скоупа pull-through HTTP-кэша.
- 2026-07-26: публичного blob-эндпоинта по хэшу нет — он обходил бы цепочку политик и аудит; terraform download указывает на archive-маршрут фида, peer-fetch федерации приедет в Фазе 7 на internal listener'е (ADR).
- 2026-07-26: X-Terraform-Get по умолчанию — `./archive.tar.gz` (относительная форма, резолвится клиентом от download-URL и не требует конфигурации); абсолютный URL из `site.external_url` — если он задан. Голая `archive.tar.gz` terraform'ом отвергается.
- 2026-07-26: аутентификация принимает HTTP Basic (пароль = токен/id_token, username игнорируется) — `mvn`/Gradle с `<server>`-кредами не умеют Bearer; 401 анонсирует обе схемы.
- 2026-07-26: indirect-локации из апстрима фетчатся с SSRF-guard'ом: чужой хост обязан резолвиться в публичный адрес (loopback/RFC1918/link-local/CGNAT/ULA запрещены), редиректы проверяются пóхопно; хост самого апстрима фида доверенный.
- 2026-07-26: go-getter-параметр `?checksum=algo:hex` из X-Terraform-Get становится ожидаемой чексуммой (инвариант 5), `//subdir` — отказ; URL в логах пишется без query (инвариант 12).
- 2026-07-26: формат-модуль может ограничивать набор своих фидов (`api.FeedSetValidator`): terraform запрещает >1 фида на сайт, т.к. service discovery хостовый.
- 2026-07-26: maven-модуль отдаёт .sha256/.sha512 sidecar'ы даже когда апстрим их не публикует — все дайджесты считаются при инжесте и хранятся в манифесте.
- 2026-07-26: кросс-репличный advisory lock деградирует в lock-less ingest при недоступном Postgres (инвариант 7); безопасно из-за идемпотентных контент-адресуемых записей.
- 2026-07-26: hosted-манифесты пишутся ТОЛЬКО через `CoreServices.Publish`; иммутабельность держит unique-констрейнт PG (не advisory lock), S3-манифест — проекция для read-path без БД.
- 2026-07-26: publish-права — не политика, а per-feed список identity-паттернов в YAML (`token:`, `project:` с glob'ами): это авторизация, а не контентная политика.
- 2026-07-26: политики получают зависимости конструктором (`api.PolicyServices`: общий кэш вердиктов + логгер), без глобалов; таблица вердиктов generic — ядро не знает специфики политик.
- 2026-07-26: канонические ключи метаданных (`license`, `published_at`, `ecosystem`) поставляет формат-модуль через `api.MetadataSource`; политики форматов не знают (инвариант 1).
- 2026-07-26: `Reindex` — детерминированная функция множества манифестов (lastUpdated берётся из данных, не из часов); golden-тест «дважды → байт-идентично» защищает гео-инвариант 15.
- 2026-07-26: клиентский `maven-metadata.xml` при deploy принимается и игнорируется — индекс пересобирает Reindex.
- 2026-07-26: conformance-проект для deploy собирается воспроизводимо (`project.build.outputTimestamp`), иначе одинаковый передеплой давал бы разные байты и ложный 409.
- 2026-07-26: npm использует ДЕКОДИРОВАННЫЕ пути (`@scope/pkg`, не `%2f`): так URL к апстриму строится без двойного экранирования, а npm-регистри принимают обе формы.
- 2026-07-26: чексумма tarball'а берётся из `dist.integrity`/`shasum` package root'а через `api.MetadataSource` (новый канонический ключ `checksum`) — инвариант 5 работает и там, где чексумма лежит внутри JSON-метаданных.
- 2026-07-26: Composer dist-URL (произвольный хост, обычно GitHub) кодируется base64url прямо в путь registry (`Intent.RemoteURL`), фетчится с тем же SSRF-guard'ом; относительная база composer'ом не поддерживается, поэтому composer-фид требует `site.external_url` (проверяется валидацией).

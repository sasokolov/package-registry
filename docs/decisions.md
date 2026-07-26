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

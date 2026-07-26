#!/usr/bin/env bash
# k6 load test: brings up the two-replica stack, warms the cache with real
# clients, runs the "CI storm" profile and records the baseline in
# docs/perf.md.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
export CONFORMANCE_DIR="$SCRIPT_DIR"
export COMPOSE_PROJECT="${COMPOSE_PROJECT:-registry-load}"
export COMPOSE_OVERLAY="$SCRIPT_DIR/compose.chaos.yml"

# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"

KEEP_STACK="${KEEP_STACK:-0}"
VUS="${VUS:-200}"
DURATION="${DURATION:-30s}"

cleanup() {
  if [[ "$KEEP_STACK" == "1" ]]; then
    echo "==> KEEP_STACK=1: stack left running"
    return
  fi
  compose down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> starting the two-replica stack"
compose up -d --build --wait --wait-timeout 300
compose --profile tools build >/dev/null

echo "==> warming the cache"
compose run --rm -T npm-client sh -c '
  set -e
  cp -r /src /tmp/w && cd /tmp/w
  npm config set registry http://lb/npm/npmjs/
  npm ci --no-audit --no-fund >/dev/null
' >/dev/null
for path in \
  /maven/central/com/example/liba/1.0.0/liba-1.0.0.jar \
  /maven/central/com/example/liba/1.0.0/liba-1.0.0.pom \
  /maven/central/com/example/liba/1.0.0/liba-1.0.0.jar.sha1 \
  /maven/central/com/example/libb/1.0.0/libb-1.0.0.jar; do
  client_curl -fsS -o /dev/null "http://lb$path"
done

echo "==> running the CI-storm profile (VUS=$VUS, DURATION=$DURATION)"
outdir="$(mktemp -d)"
chmod 777 "$outdir"
compose run --rm -T \
  -v "$SCRIPT_DIR/load:/scripts:ro" \
  -v "$outdir:/out" \
  -e VUS="$VUS" -e DURATION="$DURATION" -e REGISTRY_BASE=http://lb \
  --entrypoint k6 k6 run /scripts/ci-storm.js
summary="$outdir/summary.json"
if [[ ! -s "$summary" ]]; then
  echo "k6 did not write a summary" >&2
  exit 1
fi

echo "==> recording the baseline in docs/perf.md"
python3 - "$summary" "$REPO_ROOT/docs/perf.md" "$VUS" "$DURATION" <<'PY'
import json, sys, pathlib, datetime

data = json.loads(pathlib.Path(sys.argv[1]).read_text())
out = pathlib.Path(sys.argv[2])
vus, duration = sys.argv[3], sys.argv[4]

metrics = data.get('metrics', {})

def g(name, key, default='n/a'):
    m = metrics.get(name, {})
    values = m.get('values', m)
    v = values.get(key)
    if v is None and key == 'value':
        # Rate metrics report the share under "rate".
        v = values.get('rate')
    if not isinstance(v, (int, float)):
        return default
    if key in ('value', 'rate') and m.get('type') == 'rate':
        return f"{v * 100:.2f}%"
    if key == 'count':
        return f"{int(v)}"
    return f"{v:.2f}"

now = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%d')
body = f"""# Нагрузочный baseline

Профиль «CI-шторм»: {vus} конкурентных VU, {duration}, каждый VU повторяет
последовательность запросов одного CI-джоба (метаданные npm + tarball'ы +
maven jar/pom/sha1) против прогретого кэша. Стек — две реплики registry за
nginx-балансировщиком, блобы в MinIO, состояние в PostgreSQL.

Запуск: `make load-test` (переменные `VUS`, `DURATION`).

## Baseline от {now}

| Метрика | Значение |
|---|---|
| Запросов | {g('http_reqs', 'count')} |
| RPS | {g('http_reqs', 'rate')} |
| Доля ошибок | {g('http_req_failed', 'value')} |
| Доля ответов из кэша | {g('registry_cache_hits', 'value')} |
| Латентность p50, мс | {g('http_req_duration', 'med')} |
| Латентность p95, мс | {g('http_req_duration', 'p(95)')} |
| Латентность p99, мс | {g('http_req_duration', 'p(99)')} |
| Латентность max, мс | {g('http_req_duration', 'max')} |

Пороги k6 (падение прогона при нарушении): доля ошибок < 1%, доля ответов
из кэша > 95%.

Цифры сняты на рабочей станции разработчика в docker compose — это
относительный ориентир для отслеживания регрессий, а не обещание для
продакшена.
"""
out.write_text(body)
print(f"wrote {out}")
PY

echo "==> load test finished"

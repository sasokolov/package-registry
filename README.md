# package-registry

Self-hosted package registry: pull-through caching and hosting for several
package managers behind one core. The core owns the generic pipeline
(`auth → policy → cache → upstream → store → serve`); format modules only
translate protocols. Storage backends are modules too.

Deployment target: Kubernetes, N stateless replicas, blobs in
S3-compatible storage, dynamic state in PostgreSQL. Sites can be federated
across regions in an active-active mesh.

## Supported formats

| Format | Proxy | Hosting |
|---|---|---|
| Maven | yes | yes, including timestamped SNAPSHOTs |
| npm | yes | yes, including dist-tags |
| Terraform modules | yes | yes |
| NuGet (v3) | yes | — |
| Composer | yes | — |

Every response is labelled with `X-Registry-Source`
(`cache`, `upstream`, `stale`, `local`, `peer`) and `X-Registry-Site`.

## Quick start

```bash
make dev        # MinIO + PostgreSQL in compose, registry on the host
make test       # unit tests
make lint       # golangci-lint
```

Point a client at a feed:

```bash
npm config set registry http://localhost:8080/npm/npmjs/
mvn -Dmaven.repo.remote=http://localhost:8080/maven/central verify
```

## Testing

Everything below runs against real clients and real infrastructure in
Docker — no mocks of the protocols being implemented.

```bash
make conformance        # 21 scenarios: mvn, npm, dotnet, composer, terraform
make conformance-chaos  #  3 scenarios: replica kill, PostgreSQL down, upstream down
make conformance-geo    #  8 scenarios: replication, conflicts, partition, bootstrap
make conformance-live   #     the same protocols against real upstreams (manual)
make load-test          #     k6 "CI storm"; writes docs/perf.md
make test-integration   #     tests that need real PostgreSQL and MinIO
```

## Operating it

- `deploy/helm/` — chart, plus `smoke.sh` which installs it into a
  throwaway kind cluster and verifies it end to end.
- `deploy/observability/` — Grafana dashboard and Prometheus alert rules.
- `docs/runbooks.md` — what to do when something is wrong.
- `docs/perf.md` — load-test baseline.

CLI beyond serving:

```bash
registry token create -name ci-bot     # secret printed once, hash stored
registry token revoke -name ci-bot     # propagates to every site
registry gc                            # dry run; -delete to collect
registry repl status | peers | conflicts | resolve | resync | backfill
```

## Geo replication

Sites converge by exchanging an append-only journal of facts over an
authenticated internal API on its own listener — not by replicating
databases or object stores. Design and rationale: `docs/geo-replication.md`.

Two properties are worth knowing before operating a mesh:

- **Concurrent publishes of one coordinate never swap bytes silently.**
  If two sites publish different content at the same coordinate, the
  canonical state is the lexicographically smallest sha256 — derived from
  content, so every site agrees without coordination — the coordinate is
  quarantined, both sides are recorded, and an operator resolves it with
  `registry repl resolve`.
- **Replication can only remove authority.** There is no event that
  creates a token or grants a permission; revocations and quarantines
  propagate, grants do not.

## Documentation

- `CLAUDE.md` — architecture brief and the invariants the code is held to.
- `PLAN.md` — the phased plan this was built against.
- `docs/decisions.md` — one line per decision, in order.
- `docs/geo-replication.md` — the federation ADR.
- `docs/runbooks.md` — on-call procedures.

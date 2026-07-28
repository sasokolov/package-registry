# package-registry

Self-hosted package registry: pull-through caching and hosting for several
package managers behind one core. The core owns the generic pipeline
(`auth → policy → cache → upstream → store → serve`); format modules only
translate protocols. Storage backends are modules too.

Deployment target: Kubernetes, N stateless replicas, blobs in
S3-compatible storage, dynamic state in PostgreSQL. Sites can be federated
across regions in an active-active mesh.

## Supported formats

| Format | Proxy | Hosting | Search |
|---|---|---|---|
| Maven | yes | yes, including timestamped SNAPSHOTs | — |
| npm | yes | yes, including dist-tags | yes |
| Terraform modules | yes | yes | — |
| NuGet (v3) | yes | yes | yes |
| Composer | yes | yes | yes |

Every response is labelled with `X-Registry-Source`
(`cache`, `upstream`, `stale`, `local`, `peer`) and `X-Registry-Site`.

## Groups

Several feeds of one format can be served through a single endpoint, the way
Nexus does it: a client points at the group and gets hosted packages and
proxied ones from the same URL.

```yaml
feeds:
  - name: npm-public
    format: npm
    group: true
    members: [npm-hosted, npmjs]
```

Members are asked in order. For an artifact the first hit answers; for
metadata the format module merges the answers, so a package published locally
and one cached from upstream appear in the same listing. A group cannot widen
access: every member is checked against the caller's own rights on that
member's path, so putting a private feed in a public group grants nothing.

## Access control

Named policies of path capabilities, bound to what authentication
established about the caller — the model HashiCorp Vault uses. Nothing is
permitted until a policy says so, the most specific matching rule decides, and
an explicit `deny` beats every grant at that specificity.

```yaml
access_policies:
  - name: team-acme
    rules:
      - path: "feed/releases/maven:com.acme:*"
        capabilities: [read, list, publish]
      - path: "feed/releases/maven:com.acme.internal:*"
        capabilities: [deny]

bindings:
  - name: acme-ci
    policies: [team-acme]
    match: {kind: oidc, project_path: "acme/*", ref: main}
```

`GET /api/v1/access/explain` answers what would be decided and which rule
decided it — a refusal nobody can account for is one people route around.
Details: `docs/access-control.md`.

## Console and Terraform

The web console is built into the binary and served at `/ui/`: feeds and their
packages, replication, conflicts, quarantine, tokens, access, and the
configuration document itself. It has no separate login — it presents the same
credential every other client does, and the sign-in form is built from what
the site says it accepts.

Everything the console can change, Terraform can too:
`terraform-provider-registry/` manages feeds, connectors, OIDC issuers,
replication peers, access policies and bindings as code.

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
make conformance        # 29 scenarios: mvn, npm, dotnet, composer, terraform,
                        #     groups, console, access policies and the access API
make conformance-chaos  #  5 scenarios: replica kill, PostgreSQL, upstream and S3
                        #     outages, configuration reaching every replica
make conformance-geo    # 12 scenarios: replication, conflicts, partition, bootstrap,
                        #     site loss, quarantine, mutable coordinates, parked events
make conformance-live   #     the same protocols against real upstreams (manual)
make terraform-test     #     provider acceptance tests against a registry in Docker
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
registry config check                  # parse and validate without starting
registry token create -name ci-bot     # secret printed once, hash stored
registry token revoke -name ci-bot     # propagates to every site
registry gc                            # dry run; -delete to collect
registry repl status | peers | conflicts | resolve | resync | backfill
registry repl quarantine | release     # take a package down mesh-wide
registry repl retry-parked | trust-reset
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
- `docs/access-control.md` — paths, capabilities, policies and bindings.
- `docs/runbooks.md` — on-call procedures.
- `terraform-provider-registry/README.md` — configuration as code.

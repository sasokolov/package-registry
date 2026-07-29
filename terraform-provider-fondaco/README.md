# terraform-provider-registry

Terraform provider for the self-hosted package registry: feeds and their
upstream connectors, who may administer and publish, trusted OIDC issuers,
replication peers, static tokens and quarantine.

Its own Go module, versioned independently of the registry.

## What it actually does

The registry's configuration is **one declarative YAML document** that lives
outside its database and is validated as a whole before it is stored. The
provider does not model that document — it names one part of it per resource
and lets the registry decide whether the result is valid. That is why:

- **A rejected change changes nothing.** The registry validates the whole
  document before writing it, so an invalid feed does not half-apply.
- **Parallel applies do not clobber each other.** Every per-resource endpoint
  is a read-modify-write of the whole document inside a cross-replica advisory
  lock, so two resources touching two different feeds are safe. The provider
  therefore does *not* send `If-Match`: a version check would make resources
  collide on a document they each only partly own.
- **Plan-time validation is a strict subset.** Names, enums, durations and
  JSON shape are checked here so a typo fails next to the line that caused it.
  Anything that depends on the rest of the document — a `forward:<site>` that
  must name a real peer, a format module that must be compiled in — is left to
  the registry, which is the only thing that can see it.
- **Drift is real drift.** Optional attributes are only written back into
  state when the registry actually holds a value, and the registry's
  normalisations (`20m` → `20m0s`) are recognised as equal, so a plan that
  reports a change means something changed.

## Usage

```hcl
terraform {
  required_providers {
    registry = { source = "registry.local/fondaco-dev/fondaco" }
  }
}

provider "registry" {}   # FONDACO_ENDPOINT and FONDACO_TOKEN

resource "fondaco_feed" "central" {
  name      = "central"
  format    = "maven"
  upstream  = "https://repo1.maven.org/maven2"
  anonymous = true
}
```

The credential must match one of the site's `admins` identity patterns. It is
the same credential every other client uses — a static registry token or an
OIDC id_token from a configured issuer — so revoking it revokes Terraform's
access along with everything else's.

## Resources and data sources

| Resource | Manages |
| --- | --- |
| `fondaco_feed` | A feed: upstream connector, hosting, publishers, redirect, policy chain, federation behaviour |
| `registry_admin_binding` | One identity pattern allowed to administer the site |
| `registry_oidc_issuer` | One trusted OIDC issuer |
| `registry_replication_peer` | One geo-replication partner |
| `fondaco_token` | A static token — the secret is returned once and lands in state |
| `fondaco_quarantine` | A blocked coordinate; destroying the resource releases it |
| `fondaco_access_policy` | A named set of path capabilities |
| `fondaco_binding` | Which identities a set of policies applies to |

| Data source | Reads |
| --- | --- |
| `registry_site` | Site name, configuration version and source, database state |
| `fondaco_feed` | One feed as configured, managed here or not |
| `fondaco_feeds` | Every configured feed |
| `registry_replication_status` | Applied and durable watermarks per stream, parked events |
| `fondaco_access_explain` | What the registry would decide for an identity, path and capability |

Full reference, generated from the schemas: [`docs/`](docs/).

## Access as code

A `fondaco_access_policy` says what may be done on which paths; a
`fondaco_binding` says whose identity it applies to. Nothing is permitted
until a policy says so, the most specific matching rule decides, and a `deny`
beats every other capability at that specificity — so a narrow rule can be a
deliberate exception to a broad one, in either direction. The model and the
path namespaces are described in [`docs/access-control.md`](../docs/access-control.md).

The part worth building a habit around is `fondaco_access_explain`. It asks
the same engine that answers real requests what it would decide, which makes
an access change testable in the same plan that makes it:

```hcl
data "fondaco_access_explain" "ci_internal" {
  path         = "feed/releases/maven:com.acme.internal:secret@1.0.0"
  capability   = "publish"
  kind         = "oidc"
  project_path = "acme/widget"
}

check "nothing_widened" {
  assert {
    condition     = !data.fondaco_access_explain.ci_internal.allowed
    error_message = "CI gained publish on the internal namespace"
  }
}
```

Assert both directions. That a grant works is usually noticed within the hour;
that a deny quietly stopped applying is not noticed at all.

## The token caveat

`fondaco_token` writes a working credential into Terraform state, because the
registry stores only a hash and the secret exists exactly once — in the
response that issues it. Use an encrypted state backend, or pipe the value
into a secret manager and never read it back. For the same reason the resource
cannot be imported: an existing token's secret is not recoverable from
anywhere.

## Development

```
make terraform-build   # build, vet, lint, unit tests
make terraform-test    # acceptance tests against a real registry in Docker
make terraform-docs    # regenerate docs/ from the schemas and examples
```

To try it by hand, point Terraform at a local build with a dev override in
`~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides {
    "registry.local/fondaco-dev/fondaco" = "/path/to/bin"
  }
  direct {}
}
```

The acceptance tests run the provider in-process against the conformance
stack's registry — the same binary, the same admin API, the same validation.
There is no mock, because a mock would validate nothing while passing every
test.

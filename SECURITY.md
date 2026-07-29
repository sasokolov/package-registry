# Security

## Reporting a vulnerability

Do not open a public issue. Use GitHub's **private vulnerability reporting**
on this repository — the "Report a vulnerability" button under the Security
tab — which opens a private advisory only the maintainers can see.

What helps: a version or commit, the configuration that exposes it, and what
an attacker gets. A proof of concept is welcome but not required; a clear
description of the mechanism is worth more than an exploit.

You will get an acknowledgement within a few days and an assessment within
two weeks. If a report turns out to be a vulnerability, you will be credited
in the advisory unless you ask not to be.

## What counts

This is infrastructure that stands between a build and the code it runs, so
the interesting failures are the quiet ones. In particular:

- Serving bytes that do not match the digest they were requested by, or
  accepting an artefact whose checksum does not verify (invariant 5).
- Overwriting a published release, or making one resolve to different bytes
  than it did yesterday (invariant 4).
- Any path that lets one identity read or publish what its access rules do
  not allow, including through a group, a peer site, or a forwarded publish
  (invariants 11 and 14).
- A token, a signing key or an upstream credential reaching a log, a metric,
  an error body or a replicated event (invariant 12).
- Anything that makes a replica serve content another site never published.

Denial of service through sheer volume against a stand you control is not
interesting; a request that costs the registry unbounded memory or disk is.

## Supported versions

Until there is a tagged release, the supported version is the tip of
`master`. Once releases exist, this section will say which are supported and
for how long.

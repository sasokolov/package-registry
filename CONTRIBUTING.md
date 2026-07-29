# Contributing

## The licence, and why there is no CLA

This project is Apache-2.0, and it is meant to stay that way permanently.

Your contribution is licensed to everyone under the same Apache-2.0 as the
rest of the project — inbound equals outbound, which is what section 5 of the
licence says by default. You keep your copyright. You are not asked to assign
it, and there is no contributor licence agreement to sign.

That is a deliberate choice rather than an omission. Every project that
changed its licence out from under its users could do it because a CLA had
collected the rights in one place: one owner who could decide, later, that the
terms should be different. Here the copyright stays spread across everyone who
wrote a line of it, which means nobody — including the maintainers — can
relicense the project without asking all of them. The promise that it stays
open is worth what it costs to break it, and this makes it expensive.

Instead of a CLA, sign off your commits with the Developer Certificate of
Origin.

## Signing off

```
git commit -s
```

That adds one line:

```
Signed-off-by: Your Name <your.email@example.com>
```

By adding it you state that you wrote the change, or that you have the right
to submit it under this licence — the full text is in [DCO](DCO). It is a
statement about provenance, not a transfer of anything. Use your real name and
a working address; a pseudonym you use consistently and can be reached at is
fine.

If you forget, `git commit --amend -s` on the last commit, or
`git rebase --signoff origin/HEAD` on a branch.

## Working here

Read [CLAUDE.md](CLAUDE.md) first. It is short, and the sixteen architectural
invariants at the top are the ones that make this a registry rather than a
pile of protocol adapters. A change that violates one is not a change that
needs discussing — it is a change that needs a different design.

Two of them come up in almost every pull request:

- **The core knows no formats.** Nothing outside `modules/format/*` may
  contain logic specific to npm, maven, docker or anything else. A `switch` on
  a format name in the core is the tell.
- **Modules know no infrastructure.** A format module never imports an S3
  client, pgx, or an HTTP client for an upstream. Everything it needs arrives
  through the interfaces in `core/api`.

Before opening a pull request:

```
make lint test        # both must be clean
make conformance      # the hermetic suite, if you touched a serving path
```

Every change to a serving or publishing path needs a test. Unit tests for
logic, golden files for protocol translation (`modules/format/<x>/testdata/`),
a conformance scenario for anything a real client would notice. The
conformance suite drives real clients — `mvn`, `npm`, `dotnet`, `composer`,
`terraform`, `helm`, `crane` — against a real stack on purpose: a protocol
that only passes against a mock is a protocol that has not been tested.

Small decisions go in `docs/decisions.md`, one line each, with the reason.
Not what you did — the diff says that — but why the alternative was worse.

Comments and identifiers are in English. `docs/` and `PLAN.md` are in Russian.

## Commits

Explain the problem, not the patch. A message that says what was wrong, what
was silently wrong about it, and why this fix rather than the obvious one is
worth more than a list of the files you touched.

## Security

Do not open a public issue for a vulnerability — see [SECURITY.md](SECURITY.md)
for where it goes instead.

## What is likely to be accepted

New format modules, if they respect the invariants. Storage backends. Bug
fixes with a test that fails without them. Documentation that corrects
something wrong.

Less likely: a feature that puts state in the process (invariant 3), anything
that makes a published version mutable (invariant 4), and any dependency that
is not permissively licensed — the licence audit in
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md) is meant to keep being
boring.

#!/usr/bin/env python3
"""Generate THIRD-PARTY-NOTICES.md from what is actually linked.

Attribution is an obligation about what you DISTRIBUTE, so the input is the
set of modules the linker pulls into each binary (`go list -deps`) and the
production dependency tree of the console that is embedded in one of them —
not go.mod, which also lists things that are only ever imported by tests.

Two details this gets right and a naive scan does not:

  * A licence file that mentions several licences is read in order. What the
    file leads with covers the module; anything after a "Files: <glob>"
    stanza covers that subtree only. Matching "Apache License" anywhere in
    the text would attribute github.com/klauspost/compress — which is
    BSD-3-Clause with an Apache-2.0 subtree nobody here links — to Apache-2.0.

  * NOTICE files of dependencies are reproduced verbatim, because Apache-2.0
    section 4(d) is about carrying THEIR notices, not about having one of
    your own.

Usage: python3 scripts/third-party-notices.py [--check]
"""

import os
import re
import subprocess
import sys
from collections import Counter, defaultdict

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "THIRD-PARTY-NOTICES.md")
PROVIDER = "terraform-provider-fondaco"
PROVIDER_OUT = os.path.join(ROOT, PROVIDER, "THIRD-PARTY-NOTICES.md")

LICENCE_FILES = ["LICENSE", "LICENCE", "LICENSE.txt", "LICENSE.md", "COPYING",
                 "LICENSE-MIT", "LICENSE.BSD", "LICENSE.APACHE2"]
NOTICE_FILES = ["NOTICE", "NOTICE.txt", "NOTICE.md"]

# Ordered: the first match wins within a family, so BSD-3 is never also
# reported as BSD-2 (its text contains BSD-2's).
MARKERS = [
    ("Apache-2.0", lambda t: "Apache License" in t and "Version 2.0" in t),
    ("MPL-2.0", lambda t: "Mozilla Public License" in t),
    ("MIT", lambda t: "Permission is hereby granted, free of charge" in t),
    ("BSD-3-Clause", lambda t: "Redistribution and use in source and binary forms" in t
     and ("may be used to endorse" in t or "contributors may be used" in t)),
    ("BSD-2-Clause", lambda t: "Redistribution and use in source and binary forms" in t),
    ("ISC", lambda t: "ISC License" in t or "Permission to use, copy, modify, and/or distribute" in t),
]

EXCLUSIVE = [{"BSD-3-Clause", "BSD-2-Clause"}]


def linked_modules(packages, cwd=ROOT):
    """The modules the linker pulls into the given packages."""
    result = subprocess.run(
        ["go", "list", "-deps",
         "-f", "{{if .Module}}{{.Module.Path}} {{.Module.Version}} {{.Module.Dir}}{{end}}"] + packages,
        cwd=cwd, capture_output=True, text=True, check=True)
    modules = {}
    for line in result.stdout.splitlines():
        parts = line.split(" ", 2)
        if len(parts) == 3 and parts[0] and not parts[0].startswith("github.com/fondaco-dev"):
            modules[parts[0]] = (parts[1], parts[2])
    return modules


def licence_text(directory):
    for name in LICENCE_FILES:
        path = os.path.join(directory, name)
        if os.path.exists(path):
            return open(path, errors="replace").read().strip()
    return None


def detect(directory):
    """Return (spdx, subtree_licences, notice_text) for one module."""
    for name in LICENCE_FILES:
        path = os.path.join(directory, name)
        if not os.path.exists(path):
            continue
        text = open(path, errors="replace").read()
        # "covered by two different licenses" (the yaml modules) means both
        # apply to the module, not to separate subtrees.
        if "different licenses" in text[:300]:
            found = [spdx for spdx, test in MARKERS if test(text)]
            subtree = []
        else:
            head = re.split(r"\n(?:Files:|####)", text, maxsplit=1)[0]
            found = [spdx for spdx, test in MARKERS if test(head)]
            rest = text[len(head):]
            subtree = [spdx for spdx, test in MARKERS if test(rest) and spdx not in found]
        for group in EXCLUSIVE:
            if len(group & set(found)) > 1:
                found = [f for f in found if f == next(m for m, _ in MARKERS if m in group)]
        return (" AND ".join(found) or f"see {name}"), subtree, notice_of(directory)
    return "UNKNOWN", [], None


def notice_of(directory):
    for name in NOTICE_FILES:
        path = os.path.join(directory, name)
        if os.path.exists(path):
            body = open(path, errors="replace").read().strip()
            if body:
                return body
    return None


def ui_modules():
    """Production dependencies of the console, which ships inside the binary."""
    import json
    seen = {}

    def walk(name):
        path = os.path.join(ROOT, "ui/node_modules", name, "package.json")
        if name in seen or not os.path.exists(path):
            return
        meta = json.load(open(path))
        licence = meta.get("license") or "UNKNOWN"
        if not isinstance(licence, str):
            licence = str(licence)
        seen[name] = (meta.get("version", "?"), licence)
        for dep in (meta.get("dependencies") or {}):
            walk(dep)

    root = json.load(open(os.path.join(ROOT, "ui/package.json")))
    for dep in (root.get("dependencies") or {}):
        walk(dep)
    return seen


def render():
    registry = linked_modules(["./cmd/fondaco"])
    provider = linked_modules(["."], os.path.join(ROOT, PROVIDER))
    console = ui_modules()

    rows = defaultdict(list)
    notices = {}
    texts = defaultdict(list)   # licence text -> modules under it
    for label, modules in (("registry", registry), ("provider", provider)):
        for path, (version, directory) in sorted(modules.items()):
            spdx, subtree, notice = detect(directory)
            note = ""
            if subtree:
                note = f" (also {', '.join(subtree)} for subtrees this does not link)"
            rows[label].append((path, version, spdx + note))
            if notice:
                notices[f"{path} {version}"] = notice
            body = licence_text(directory)
            if body:
                texts[body].append(f"{path} {version}")
    for name, (version, spdx) in sorted(console.items()):
        rows["console"].append((name, version, spdx))

    counts = Counter(spdx.split(" (")[0] for group in rows.values() for _, _, spdx in group)
    mpl = sorted({p for p, _, s in rows["provider"] if "MPL" in s})

    out = ["""# Third-party notices

This software bundles the work of others. Below is what each binary links,
under which licence, with the attribution notices those projects ship.

It is generated from what is actually linked — `go list -deps` on each binary
and the production dependency tree of the console — rather than from the
declared dependency list: a module that is declared but never linked is not
distributed, and a module that is linked must be attributed whether or not
anyone remembered to declare it. Regenerate with `make notices`.
"""]

    sections = [
        ("registry", "The registry binary (`cmd/fondaco`)",
         "Linked into the registry binary. The web console is embedded in the same binary."),
        ("console", "The web console (`ui/`, embedded in the registry binary)",
         "Shipped inside the binary as built assets."),
        ("provider", f"The Terraform provider (`{PROVIDER}/`)",
         "A separate Go module, a separate binary, released separately."),
    ]
    for key, title, note in sections:
        out.append(f"\n## {title}\n\n{note}\n")
        out.append("| Module | Version | Licence |")
        out.append("|---|---|---|")
        for name, version, spdx in rows[key]:
            out.append(f"| `{name}` | {version} | {spdx} |")

    if mpl:
        out.append(f"""
## Source availability for the MPL-2.0 modules

The Terraform provider links {len(mpl)} modules under the Mozilla Public
Licence 2.0. Section 3.2(a) of that licence obliges anyone distributing them
in executable form to tell recipients how to obtain their source, whether or
not they were modified — and this project does not modify them.

Their source is the upstream repository at the version recorded in
`{PROVIDER}/go.mod`, and a copy of every version this build used is in the Go
module cache and in the checksum database:

""")
        for path in mpl:
            version = next(v for p, v, _ in rows["provider"] if p == path)
            out.append(f"- `{path}` {version} — https://{path.split('/v')[0]} "
                       f"(`go mod download {path}@{version}`)")

    if notices:
        out.append("""
## Attribution notices carried from bundled works

Apache-2.0 section 4(d) is about carrying the notices of what you bundle.
These are reproduced verbatim from the NOTICE files of the modules above.
""")
        for name in sorted(notices):
            out.append(f"\n### {name}\n\n```\n{notices[name]}\n```")

    out.append("\n## Summary\n")
    for spdx, count in sorted(counts.items(), key=lambda kv: (-kv[1], kv[0])):
        out.append(f"- {spdx}: {count}")

    out.append("""
## Licence texts

Reproduced in full, because that is what the licences ask for: MIT and the BSD
family both require their text and their copyright line to travel with a
binary, and a link to a repository does not reach anyone who received one.
Identical texts are given once, with the modules they cover.
""")
    for body in sorted(texts, key=lambda b: (-len(texts[b]), texts[b][0])):
        modules = ", ".join(f"`{m}`" for m in sorted(texts[body]))
        out.append(f"\n### {modules}\n\n```\n{body}\n```")
    return "\n".join(out) + "\n"


def render_provider():
    """The provider ships on its own, so its attribution has to as well.

    Someone who installs the provider from the Terraform registry never sees
    this repository; a notices file that lives only at the root would leave
    them with a licence and no attribution for what the binary contains.
    """
    modules = linked_modules(["."], os.path.join(ROOT, PROVIDER))
    rows, notices, mpl = [], {}, []
    texts = defaultdict(list)
    for path, (version, directory) in sorted(modules.items()):
        spdx, subtree, notice = detect(directory)
        if subtree:
            spdx += f" (also {', '.join(subtree)} for subtrees this does not link)"
        rows.append((path, version, spdx))
        if notice:
            notices[f"{path} {version}"] = notice
        if "MPL" in spdx:
            mpl.append((path, version))
        body = licence_text(directory)
        if body:
            texts[body].append(f"{path} {version}")

    out = ["""# Third-party notices — Terraform provider

This provider is a separate Go module and a separate binary from the registry
it configures. What it links, and under which licence:
""", "| Module | Version | Licence |", "|---|---|---|"]
    for name, version, spdx in rows:
        out.append(f"| `{name}` | {version} | {spdx} |")

    if mpl:
        out.append("""
## Source availability for the MPL-2.0 modules

Mozilla Public Licence 2.0 section 3.2(a) obliges anyone distributing these in
executable form to tell recipients how to obtain their source, modified or
not — this provider does not modify them. The source is the upstream
repository at the version in `go.mod`:
""")
        for path, version in mpl:
            out.append(f"- `{path}` {version} — https://{path.split('/v')[0]} "
                       f"(`go mod download {path}@{version}`)")

    if notices:
        out.append("""
## Attribution notices carried from bundled works
""")
        for name in sorted(notices):
            out.append(f"\n### {name}\n\n```\n{notices[name]}\n```")

    out.append("""
## Licence texts

Reproduced in full: MIT and the BSD family require their text and their
copyright line to travel with a binary, and a link to a repository does not
reach whoever received one. Identical texts are given once.
""")
    for body in sorted(texts, key=lambda b: (-len(texts[b]), texts[b][0])):
        modules = ", ".join(f"`{m}`" for m in sorted(texts[body]))
        out.append(f"\n### {modules}\n\n```\n{body}\n```")
    return "\n".join(out) + "\n"


NOTICE_START = "<!-- attribution notices: generated, do not edit below -->"
NOTICE_END = "<!-- end attribution notices -->"


def render_notice(existing, notices):
    """Put the bundled attribution notices inside NOTICE itself.

    Apache-2.0 section 4(d) is a requirement about the NOTICE file that
    travels with the distribution, and a separate document is one more thing
    that can fail to travel.
    """
    block = [NOTICE_START, "",
             "This product includes software developed by the projects below;",
             "these are their own attribution notices, reproduced verbatim.", ""]
    for name in sorted(notices):
        block.append(f"--- {name} ---")
        block.append(notices[name])
        block.append("")
    block.append(NOTICE_END)
    generated = "\n".join(block)

    if NOTICE_START in existing:
        head = existing.split(NOTICE_START)[0]
        tail = existing.split(NOTICE_END)[1] if NOTICE_END in existing else "\n"
        return head + generated + tail
    return existing.rstrip("\n") + "\n\n" + generated + "\n"


def bundled_notices():
    out = {}
    for packages, cwd in ((["./cmd/fondaco"], ROOT), (["."], os.path.join(ROOT, PROVIDER))):
        for path, (version, directory) in linked_modules(packages, cwd).items():
            notice = notice_of(directory)
            if notice:
                out[f"{path} {version}"] = notice
    return out


def main():
    notice_path = os.path.join(ROOT, "NOTICE")
    files = {OUT: render(), PROVIDER_OUT: render_provider(),
             notice_path: render_notice(open(notice_path).read(), bundled_notices())}
    if "--check" in sys.argv:
        stale = [p for p, body in files.items()
                 if (open(p).read() if os.path.exists(p) else "") != body]
        if stale:
            print("out of date, run `make notices`: " + ", ".join(
                os.path.relpath(p, ROOT) for p in stale), file=sys.stderr)
            return 1
        print("third-party notices are up to date")
        return 0
    for path, body in files.items():
        open(path, "w").write(body)
        print(f"wrote {os.path.relpath(path, ROOT)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

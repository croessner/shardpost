# Working agreements for this repository

## Documentation language

All documentation in this repository is written in English. This covers
`README.md`, `ARCHITECTURE.md`, `ROADMAP.md`, everything under `docs/`
(including reports, review notes and archived snapshots), the manual
pages under `docs/man/`, comments inside documentation code blocks, and
Mermaid diagram labels.

German text that turns up in documentation is translated and the German
original is replaced, not kept alongside. The exceptions are literal
data: example mailbox names, example subjects and quoted log lines stay
as they are, because they are what the system actually contains.

Code comments, commit messages, API descriptions and log messages are
English as well.

Dates in documentation use ISO form (`2026-09-20`).

## Licence

The repository is under GPL-3.0-or-later; `COPYRIGHT.md` states that
this holds for every file, so a new file needs no licence header. What
does need care: code copied in from elsewhere keeps its own licence and
belongs under `vendor/`, `third_party/` or — if it has to live among our
own packages, as `internal/inflate` does — with its licence file and its
original header intact, listed in `COPYRIGHT.md`. A new dependency with
a copyleft licence is a decision for the owner, not a detail of the
change that needs it.

## Manual pages

`docs/man/man1/` holds one manual page per command group of
`shardpostctl`, in roff. A page states what the command does, which role
it needs, every option, the exit codes and a section of practical
examples. When a command, flag or exit code changes, its manual page
changes in the same commit.

Check the pages with `make man` (it renders every page and fails on a
formatting error) or read one with `man ./docs/man/man1/<page>.1`.

## Before a commit

- `make lint` reports 0 findings.
- `make man` passes when a manual page changed.
- `make check-generate` is clean when `api/admin/openapi.yaml` changed.
- `make test-docker` is green (it runs the cgo packages too; `cephstore`
  does not build on macOS).
- Documentation that describes changed behaviour is updated, including
  the manual page.

## graphify

This project has a knowledge graph at graphify-out/ with god nodes, community structure, and cross-file relationships.

Rules:
- When graphify-out/graph.json exists, run `graphify query "<question>"` before your first grep, search or source-file read for a codebase question, and put this rule in every subagent prompt that explores code. Read raw files only for lines the graph or an earlier read already located. Use `graphify path "<A>" "<B>"` for relationships and `graphify explain "<concept>"` for focused concepts. These return a scoped subgraph, usually much smaller than GRAPH_REPORT.md or raw grep output.
- If graphify-out/wiki/index.md exists, use it for broad navigation instead of raw source browsing.
- Read graphify-out/GRAPH_REPORT.md only for broad architecture review or when query/path/explain do not surface enough context.
- After modifying code, run `graphify update .` to keep the graph current (AST-only, no API cost).
- A committed graphify-out/graph.json is rewritten whole on every build, so a large diff to it in a pull request is expected. Do not flag it as churn or suggest skipping the graph rebuild or its hooks.

## Keeping the knowledge graph current

- `graphify-out/` is local build output and is not committed. The git
  hooks (`graphify hook install`: post-commit, post-checkout, post-merge,
  post-rewrite) re-extract changed code after each commit without an LLM.
- The hooks do not see documentation. After changing anything under
  `docs/`, a top-level `*.md` or `website/src/`, run `/graphify . --update`
  in the same session so the semantic part of the graph follows.
- `.graphifyignore` lists what stays out of the graph (`vendor/`,
  `third_party/`, rendered diagrams, binary assets). A new top-level
  directory of foreign or generated content belongs there.
- A fresh clone builds the graph once with `/graphify .`.

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

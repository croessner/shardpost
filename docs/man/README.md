# Manual pages

Manual pages for `shardpostctl`, the administrative client. They are the
reference for day-to-day operation: what each command does exactly, which
role it needs, what it returns, and a section of examples taken from real
support work.

One page per subject, in the style of `doveadm`:

| Page | Covers |
| --- | --- |
| [shardpostctl(1)](man1/shardpostctl.1) | Overview, connection flags, roles, exit codes |
| [shardpostctl-server(1)](man1/shardpostctl-server.1) | `version`, `server status` |
| [shardpostctl-account(1)](man1/shardpostctl-account.1) | `account show`, `quota show`, `shared list` |
| [shardpostctl-mailbox(1)](man1/shardpostctl-mailbox.1) | `mailbox list/show/create/rename/subscribe/unsubscribe` |
| [shardpostctl-acl(1)](man1/shardpostctl-acl.1) | `acl list/explain/set/delete` |
| [shardpostctl-message(1)](man1/shardpostctl-message.1) | `messages`, `search`, `message show/export`, `flags`, `copy`, `move` |
| [shardpostctl-sieve(1)](man1/shardpostctl-sieve.1) | `sieve list/show/put/delete/activate/deactivate/check` |
| [shardpostctl-job(1)](man1/shardpostctl-job.1) | `plan`, `apply`, `check`, `job list/show/wait/cancel` |
| [shardpostctl-learning(1)](man1/shardpostctl-learning.1) | `learning list/show/retry` |
| [shardpostctl-export(1)](man1/shardpostctl-export.1) | `export`, `archive verify`, `restore` |
| [shardpostctl-deliver(1)](man1/shardpostctl-deliver.1) | `deliver` |
| [shardpostctl-migrate(1)](man1/shardpostctl-migrate.1) | `migrate` |
| [shardpostctl-trace(1)](man1/shardpostctl-trace.1) | `trace on/status/off` |
| [shardpostctl-auth(1)](man1/shardpostctl-auth.1) | `auth flush` |

One page covers commands of the server binary that operators run as
jobs of their own:

| Page | Covers |
| --- | --- |
| [shardpost-scrub(1)](man1/shardpost-scrub.1) | `shardpost scrub`, `scrub --batch`, `scrub --sweep` |

## Reading them

All pages are rendered by `make man`, which fails on a formatting error.
To read one without installing anything:

```sh
man ./docs/man/man1/shardpostctl.1
man ./docs/man/man1/shardpostctl-job.1
```

Or put the directory on the manual path for the current shell, which
makes cross-references and `apropos`-style lookup work:

```sh
export MANPATH="$PWD/docs/man:$MANPATH"
man shardpostctl
man shardpostctl-export
```

To install them system-wide, copy `man1/*.1` into a directory on the
manual path, for example `/usr/local/share/man/man1`.

## Scope

The pages describe the client and the operations it offers. Two other
documents describe the layers underneath, and they stay authoritative for
those:

- [admin-api.md](../admin-api.md) — the HTTP API itself, its semantics
  and its error codes.
- [configuration.md](../configuration.md) — the server settings the pages
  refer to, such as `jobs.settle_seconds` or `admin.search_scan_limit`.

When a command changes, its manual page changes with it in the same
commit.

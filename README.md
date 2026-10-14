# ShardPost — Mail Server with Direct RADOS Storage

This project implements a mail server in Go that can be run in a distributed
fashion: IMAP, POP3 and JMAP for access, LMTP for delivery, ManageSieve for
script management, plus Sieve and IMAPSieve for processing. Metadata, messages and
durable learning jobs live directly in Ceph RADOS. In the operational profile,
Nauthilus handles authentication and identity resolution.

The website [shardpost.de](https://shardpost.de/) introduces the project and
carries the [manual](https://shardpost.de/docs/), rendered from `docs/` by
[`website/`](website/README.md).

**ShardPost** has been the project and product name since version 0.12.0; since
0.13.0 the namespace, deployment, RADOS pools, Director shard and external
identities are named `shardpost` as well. The Go module and the server binary
are called `shardpost`, the load generator `shardpost-load`. The
[name migration](docs/decisions/project-name.md) describes the procedure and the
deliberately stable metric families.

## Getting Started

| You want to … | Document |
|---|---|
| understand the system as a whole | [Architecture with system and component sketches](ARCHITECTURE.md) |
| see which protocols, extensions and RFCs are supported | [Protocols, features and RFCs](docs/protocols.md) |
| change and test the code | [Developer guide](docs/development/README.md) |
| trace interfaces and flows | [UML, states and sequences](docs/architecture/runtime.md) |
| understand storage, concurrency and error handling | [RADOS data model and consistency](docs/architecture/storage.md) |
| configure | [Configuration](docs/configuration.md) |
| extend Sieve or learning programs | [Sieve, IMAPSieve and external programs](docs/imapsieve.md) |
| quota rules, warnings, over flag | [Quota](docs/quota.md) |
| Admin API, shardpostctl, local delivery | [Admin API and LDA](docs/admin-api.md) |
| roll out or investigate an incident | [Operations and releases](docs/operations/README.md) |
| understand logs, metrics and traces | [Observability](docs/observability.md) |
| assess limits and test coverage | [Quality status](docs/development/quality.md) |

The [documentation index](docs/README.md) contains the complete reading order,
decisions, historical reviews and release evidence.

## System Overview

![System context](docs/diagrams/context.svg)

The server is the mailbox and processing layer. An upstream MTA handles internet
SMTP and queuing; outgoing Sieve actions use a configured SMTP relay. Director,
Nauthilus, Ceph/Rook, cert-manager, Rspamd and monitoring are separate
components with their own operational contracts.

Several server replicas share the RADOS pools. Sessions, parsers and caches are
process-local; durable mail state lives in RADOS. Switching pods requires a new
client connection. The architecture makes multiple replicas possible, but on its
own it does not establish any particular maximum number of users.

## Developing Locally

Prerequisites: Go as specified in [go.mod](go.mod) (currently 1.27.1), a
populated vendor tree and, for the complete server, Linux with matching librados
development libraries. The Docker build provides this environment.

A quick check that does not depend on Ceph:

```bash
go test -race ./internal/config ./internal/logging ./internal/imapproto ./internal/pop3proto ./internal/sieve
```

The complete server, including LMTP, imports the Ceph adapter.
`go test ./internal/...` is therefore **not** a Ceph-free entry point.
The [full Linux test run](docs/development/README.md) additionally pulls in the
Sieve test fixtures from `third_party`.

Build a local image and check only its configuration:

```bash
docker build --platform linux/amd64 -t shardpost:dev .
docker run --rm -e LOG_LEVEL=info shardpost:dev config check
```

`config check` validates the configuration and the secret files, but does not
connect to Ceph or Nauthilus. Actually starting the server requires those
dependencies, or an explicitly isolated test profile.

## Operational Profile and Current Evidence

The operational profile is `deploy/overlays/croessner-nauthilus`. The base
profile and `croessner` contain static test credentials; they are not a template
for a publicly reachable production service. The Makefile explicitly uses the operational profile as its default;
for isolated test environments, override the overlay deliberately.

On 2026-09-18, **ShardPost 0.13.0** was moved into the namespace `shardpost`
with the pools `shardpost-index`/`shardpost-data` (object-by-object verified
copy). Direct and Director acceptance runs (IMAP, LMTP, ManageSieve, Sieve,
IMAPSIEVE) passed for both test accounts; imaptest unchanged apart from the
known `fetch-envelope` case. `make lint` reports 0 issues.
This is a dated acceptance record, not a continuous status display.
Details: [ShardPost 0.13.0](docs/reports/2026-09-18-shardpost-0.13.0.md),
previously [0.12.0](docs/reports/2026-09-16-shardpost.md).

The [quality overview](docs/development/quality.md) distinguishes current tests
from older conformance results and from explicitly excluded cases. The
historical [expansion plan](ROADMAP.md) does not replace this overview.

## Licence

ShardPost is free software under the **GNU General Public License,
version 3 or later**. Every file of this repository falls under it,
whether or not it carries a header of its own; the third-party code in
`vendor/`, `third_party/go-sieve/` and `internal/inflate/` keeps the
licence it came with. See [COPYRIGHT.md](COPYRIGHT.md) for the scope and
[LICENSE](LICENSE) for the text.

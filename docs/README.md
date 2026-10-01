# Documentation

This documentation describes the ShardPost code base; the sections below
carry the version and date they were written for. The architecture describes mechanisms that exist; concrete
operational results carry a date and a piece of evidence. Goals are marked
as goals. The product name is ShardPost, since 0.13.0 also in every
operational identifier.

## Reading paths

**New to the project:** [Overview](../README.md) → [Architecture](../ARCHITECTURE.md)
→ [Runtime and UML](architecture/runtime.md) → [Developer guide](development/README.md).

**Storage change:** [Consistency and data model](architecture/storage.md)
→ [Architecture decisions](decisions/architecture.md) →
[Tests and limits](development/quality.md).

**Operations:** [Configuration](configuration.md) → [Operations and releases](operations/README.md)
→ [Logs, metrics, and traces](observability.md). The day-to-day commands
are documented as [manual pages](man/README.md).

| Document | Contents |
|---|---|
| [Architecture](../ARCHITECTURE.md) | System boundaries, components, deployment, impact of outages |
| [Protocols, features and RFCs](protocols.md) | Per-protocol reference: capabilities, commands, methods, limits, deviations, RFC index |
| [Runtime and UML](architecture/runtime.md) | Go interfaces, login, delivery, IMAP states |
| [RADOS storage](architecture/storage.md) | Object layout, CAS, shards, intents, references, repair |
| [Developer guide](development/README.md) | Code map, build, tests, change paths, vendor fork |
| [Quality](development/quality.md) | Test levels, conformance limits, current and historical evidence |
| [Stage 8: operational administration](development/admin-stage.md) | Planned: shardpostctl, OpenAPI, mailboxes, ACL, quota, FTS, repairs, and migration |
| [Operations](operations/README.md) | Dependencies, safe diagnosis, release and rollback, running without Kubernetes |
| [Configuration](configuration.md) | Viper, environment variables, secret files, log levels |
| [Sieve and learning](imapsieve.md) | Script execution, external programs, learning queue |
| [Quota](quota.md) | Grace, mailbox rules, warning programs, over flag |
| [Admin API and LDA](admin-api.md) | OpenAPI contract, shardpostctl, roles, local delivery |
| [JMAP](jmap.md) | Account state, session resource, API endpoint, blobs, what is built and what is not |
| [JMAP by example](jmap-examples.md) | A recorded send and receive through the Director, as JSON |
| [Manual pages](man/README.md) | `shardpostctl` command by command, with examples |
| [Observability](observability.md) | What the metrics mean, sampling, privacy, operational logs |
| [Architecture decisions](decisions/architecture.md) | Reasons for and consequences of the central design decisions |
| [Project name](decisions/project-name.md) | Candidates and a compatible rename |
| [Diagrams](diagrams/README.md) | Mermaid sources, SVGs, and reproducible rendering |

## Evidence and history

- [ShardPost 0.42.0](reports/2026-10-02-shardpost-0.42.0.md): intents, scrub and purge without per-account listings, sweep (scaling stage R3).
- [ShardPost 0.41.0](reports/2026-10-01-shardpost-0.41.0.md): auth cache with cluster-wide flush, configurable Nauthilus client, JMAP wait queue and limits.
- [ShardPost 0.40.0](reports/2026-10-01-shardpost-0.40.0.md): batch over all accounts, partitioned scrub with skip-if-idle, IMAP watch hub (scaling stage R2b).
- [ShardPost 0.39.0](reports/2026-10-01-shardpost-0.39.0.md): no pool listing in running paths, per-account limits (scaling stage R2a).
- [ShardPost 0.38.0](reports/2026-10-01-shardpost-0.38.0.md): job queue and expiry without listing, key rotation of the admin store (scaling stage R1b).
- [ShardPost 0.37.0](reports/2026-10-01-shardpost-0.37.0.md): account directory (scaling stage R1a), ESORT.
- [ShardPost 0.36.0/0.36.1](reports/2026-10-01-shardpost-0.36.1.md): the gaps of the protocol reference closed, imaptest without failures.
- [ShardPost 0.35.2](reports/2026-10-01-shardpost-0.35.2.md): protocol fixes from the documentation audit.
- [JMAP Core 0.27.0](reports/2026-09-21-jmap-core-0.27.0.md): account-wide state, JMAP Core, release evidence.
- [ShardPost 0.13.0](reports/2026-09-18-shardpost-0.13.0.md): lint cleanup, namespace/pool/identity move, current rollout.
- [ShardPost 0.12.0](reports/2026-09-16-shardpost.md): rename and compatibility.
- [Logging 0.11.3](reports/2026-09-16-logging.md): logging extension and the protocol evidence of the time.
- [Sieve fixes up to 0.11.2](reports/2026-09-16-sieve-conformance-fixes.md).
- [External programs](reports/2026-09-16-extprograms.md).
- [IMAPSieve](reports/2026-09-15-imapsieve.md) and [PROXY protocol](reports/2026-09-15-proxy-protocol.md).
- [Conformance results](../hack/conformance.md): versioned results, no blanket green.
- [Review round 1](review-round1.md), [2](review-round2.md), [3](review-round3.md),
  [4](review-round4.md), [5](review-round5.md), [6](review-round6.md): historical findings and changes.
- [Earlier architecture](history/architecture-before-2026-09-16.md),
  [earlier README](history/readme-before-2026-09-16.md), [build-out history](../ROADMAP.md).

Historical documents are kept as rationale and evidence. New
implementations follow the current code map and the interfaces that are
actually implemented. When a flow changes, its description, the Mermaid
source, and the rendered SVG are updated together.

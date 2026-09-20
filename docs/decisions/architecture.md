# Architecture decisions

Status: a description of the decisions in place as of 2026-09-16. This file
is not a resolution to implement additional product features.

| Decision | Rationale | Consequence and limit |
|---|---|---|
| RADOS directly instead of a file system | Messages and ordered mailbox metadata map well onto object/omap operations | We are responsible ourselves for indexes, consistency, repair and migration |
| Separate index and data pools | omap metadata and large payloads have different requirements | Backup, restore and capacity planning have to cover both pools |
| Shared pools, one namespace per account | Avoids one pool per user | Canonicalization and authorization have to be reliable; a namespace is not a credential boundary of its own |
| Interfaces between protocol and store | Protocol tests without RADOS; defined data contracts | The large user contract has to stay consistent in both backends; LMTP is more tightly coupled to Ceph |
| Object CAS and journals | Multiple writers without a central database service | No global transaction; conflicts and pending states are part of the normal model |
| Intents for multi-part changes | Aborts have to stay detectable and repairable | Scrub and age thresholds are part of the design, not optional cosmetics |
| Watch/notify plus persistent change detection | Fast updates without an additional message bus | Notifications are unreliable hints, not the sole source of truth |
| Canonical account from the auth result | Aliases and login forms should point at the same mail data | Changing the canonicalization is potentially a data migration |
| Sieve fork inside the project | Required interpreter features and specific conformance fixes | The fork, the vendor copy, the fixture set and the tests have to be maintained together |
| Allowlist for external programs | Replaceable processing with bounded execution | No shared transaction with Ceph; side effects can be repeated |
| Persistent learning copy and commit receipts | Learning should survive EXPUNGE and process aborts | Separate queue lifecycles, retries, dead jobs and storage demand |
| Structured slog logs | Trace sessions and errors across the cluster | Operational data contains client IP and account; run levels and retention deliberately |

## Rules for future decisions

A new decision describes the concrete trigger, the existing contract,
the options, the chosen solution, the consequences, the migration and the
verification. For RADOS changes, the crash matrix is part of it; for protocol
changes, state, permissions, response semantics and capability. For
infrastructure changes, other components and their owners must be named
explicitly.

The earlier [architecture description](../history/architecture-before-2026-09-16.md)
is kept as a historical account of how the system came about. It partly
contains stronger claims and earlier expansion goals that are not current
guarantees.

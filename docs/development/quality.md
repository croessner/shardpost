# Quality status and test strategy

[Developer guide](README.md) · [Conformance log](../../hack/conformance.md)

## What each level proves

| Level | Checks | Does not check automatically |
|---|---|---|
| Config/unit tests | Validation, data structures, defined error paths | Reachability of production services |
| TCP protocol tests with memstore | Parser, states, responses, ACL and logging contracts | RADOS CAS, OSD failures, real key rotation |
| Linux tests with a librados build | Compilation and tests on the server platform | Full operation against a real Ceph |
| cephstore tests against a throwaway cluster | `make test-docker` starts one monitor and one OSD on the in-memory object store inside the lint image ([hack/ceph-test-cluster.sh](../../hack/ceph-test-cluster.sh)); the store's RADOS paths (CAS, intents, crash windows of the scrub, the purge without listing, the sweep) run against it | Replication, EC pools, OSD failures, real cephx, load |
| Pigeonhole fork tests | Script compiler and interpreter in the test adapter | A complete Dovecot environment or external delivery |
| imaptest | Specific IMAP conformance groups | Capabilities that are not offered or are skipped |
| Native acceptance | The actual network, TLS, auth and store path | Other accounts, unknown client variants or long-term load |
| Load/abort tests | The load measured and the failure points tested | A general capacity or availability guarantee |

## Status: ShardPost 0.13.0, 2026-09-18

`make lint` (golangci-lint 2.13.2 in the Docker image with librados) reports
0 issues across the whole tree without any `//nolint`; `make test-docker`
passes with `-race` in all 17 test packages. The lint-driven refactorings were
reviewed against the state before the cleanup for behavioral equivalence.
For the live acceptance see [report 0.13.0](../reports/2026-09-18-shardpost-0.13.0.md).

## Previous status: ShardPost 0.12.0, 2026-09-16

For the rename, `go vet` and the complete Linux server race suite
`go test -p 1 -race -tags tentacle ./...` passed. Additional tests cover
new and old config variables, precedence rules, the OTel service identity and
IMAP ID. Runtime checks confirmed the binary aliases, the configuration and the
new helper path. After the rollout, both test accounts passed public IMAP and
ManageSieve including the ShardPost identity, TLS, login and complete log
correlation. See the [release report](../reports/2026-09-16-shardpost.md).

## Previous logging acceptance 0.11.3, 2026-09-16

For the logging change, `go vet`, the complete Linux server run
`go test -tags tentacle ./...` and targeted race tests for
config, logging, IMAP, POP3 and ManageSieve passed. New tests cover filtering,
validation, the PROXY address, the session ID and the absence of secrets in the
new debug events. `config check` passed in the finished runtime image.

After the rollout, both test accounts passed public IMAP and ManageSieve:
TLS, login, NOOP and logout as well as log correlation including the public
client IP. Two replicas were Ready, with no restarts. See the
[release evidence](../reports/2026-09-16-logging.md).

This version did not re-run all historical load, RADOS abort and
conformance suites. A green logging test does not replace them.

## Last documented conformance status

The [report up to 0.11.2](../reports/2026-09-16-sieve-conformance-fixes.md)
and [hack/conformance.md](../../hack/conformance.md) document, for
Pigeonhole, 139 passed, 0 failed and 9 explicitly excluded
files out of 148. Exclusions do not count as passed. When error counts are
involved, the adapter compares absence of errors against presence of errors,
not the exact number of Dovecot diagnostics. Linux/TZ=UTC is the reference for
this suite.

The last complete IMAP run, against 0.36.1 on 2026-10-01, had no
failed group: 72 groups, 0 of 460 base protocol and 0 of 267 extension
commands failed, 9 groups skipped for capabilities the server does not
offer (see [hack/conformance.md](../../hack/conformance.md)). The run
against 0.35.2 the same day had three failures (`fetch-envelope`,
`search-body`, `search-header`), all fixed in 0.36.0; messages stored
before 0.36.0 keep their cached envelope without the source route.

## Choosing integration scripts deliberately

Some of the older `hack/test*.sh` scripts assume static accounts and
test infrastructure. `acceptance-nauthilus.sh` writes messages and
temporarily changes mailboxes/ACLs. It is not a read-only health test.
`imapsieve-live-test.py` creates temporary mailboxes/scripts and can trigger
learning rules. Before running them, read the target profile, the accounts, the
side effects and the cleanup.

New release evidence records: source/image identity, test platform,
endpoints, account scope, side effects, result, cleanup and open gaps.
Neither a skipped test nor missing metrics is a success.

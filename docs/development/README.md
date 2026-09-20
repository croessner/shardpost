# Developer Guide

[Documentation index](../README.md) · [UML and runtime](../architecture/runtime.md)

## Prerequisites and build model

The Go version is declared in [go.mod](../../go.mod), currently `1.27.1`.
The server builds against Ceph Tentacle/librados with the build tag `tentacle`.
The [Dockerfile](../../Dockerfile) provides the matching Linux environment,
runs `go vet` and produces the server and the load generator. The runtime runs
as UID/GID `167:167`; the external programs stay root-owned.

`vendor` is the reproducible dependency set of the container build.
The go-sieve fork additionally lives in `third_party/go-sieve`; `go.mod`
carries a local `replace` for it. Changes to the fork must be deliberately
carried over into the vendor set. Changing only one of the two copies can
make tests and the shipped image use different code.

## Code map

| Area | Entry point | Responsibility |
|---|---|---|
| Startup/configuration | [main.go](../../cmd/shardpost/main.go), [config.go](../../internal/config/config.go) | Wiring, defaults, validation, lifecycle |
| Listener | [listen.go](../../cmd/shardpost/listen.go) | TCP, trust-bound PROXY, TLS |
| IMAP | [conn.go](../../internal/imapproto/conn.go), [wire.go](../../internal/imapproto/wire.go) | Parser, states, command dispatch, response encoding |
| Mailbox synchronization | [selected.go](../../internal/imapproto/selected.go), [notify.go](../../internal/imapproto/notify.go) | MODSEQ, snapshots, IDLE/NOTIFY |
| POP3 | [pop3.go](../../internal/pop3proto/pop3.go) | Maildrop, lease, deletion markers |
| LMTP | [lmtpd.go](../../internal/lmtpd/lmtpd.go) | Recipient resolution, buffering, per-recipient status |
| Delivery | [delivery](../../internal/delivery/delivery.go) | Sieve actions, storing, rollback, redirect/vacation/notify; shared by LMTP and LDA |
| Admin API | [admin](../../internal/admin/admin.go), [contract](../../api/admin/openapi.yaml), [shardpostctl](../../cmd/shardpostctl/main.go) | Listener, roles, audit, local delivery |
| Identity | [auth.go](../../internal/auth/auth.go), [SASL](../../internal/auth/sasl.go) | Request/result contract, Nauthilus, master user, OIDC |
| Store contract | [model.go](../../internal/model/model.go) | Persistence-independent types, user, watch, lease |
| Persistence | [cephstore](../../internal/cephstore/store.go), [user.go](../../internal/cephstore/user.go) | RADOS, account view, CAS, index operations |
| Test backend | [memstore.go](../../internal/memstore/memstore.go) | Protocol tests without librados |
| Search | [fts](../../internal/fts/tokenize.go), [postings](../../internal/postings/postings.go) | Tokenization and compact posting lists |
| Sieve | [sieve.go](../../internal/sieve/sieve.go), [ManageSieve](../../internal/sieve/managesieve.go) | Evaluation and script management |
| IMAPSieve | [engine.go](../../internal/imapsieve/engine.go), [learning.go](../../internal/imapsieve/learning.go) | Mail events and durable learning jobs |
| External programs | [runner.go](../../internal/extprograms/runner.go), [learn.py](../../programs/learn.py) | Allowlist, resource limits, replaceable classifier |
| Operations | [logging](../../internal/logging/logging.go), [metrics](../../internal/metrics/metrics.go), [telemetry](../../internal/telemetry/telemetry.go) | JSON logs, Prometheus, OTLP |
| Resources | [membudget](../../internal/membudget/membudget.go), [rookcreds](../../internal/rookcreds/rookcreds.go) | Message budget and Ceph key rotation |

## Quick tests without Ceph

From the project directory:

```bash
go test -race ./internal/config ./internal/logging ./internal/auth ./internal/imapproto ./internal/pop3proto ./internal/sieve ./internal/imapsieve ./internal/extprograms ./internal/fts ./internal/postings
```

Some of these tests open local sockets and start test programs. An environment
that blocks loopback or process execution is unsuitable for them. That has to
be distinguished from an actual protocol bug.

`make test` runs the cgo-free race suite. The former writing cluster test is
explicitly named `make test-static-cluster` and requires an isolated static
test profile.

A single regression test:

```bash
go test -race ./internal/imapproto -run TestSessionLoggingAndSecretSafety -count=1
```

`newTestServer` in [memstore_test.go](../../internal/imapproto/memstore_test.go)
creates a real TCP listener in front of the in-memory store. Additions should
check observable protocol behavior, not just internal implementation details.

## Full Linux build and tests

```bash
docker build --platform linux/amd64 --target build -t shardpost-build:dev .
docker run --rm \
  -v "$PWD/third_party:/src/third_party:ro" \
  shardpost-build:dev go test -tags tentacle ./...
docker build --platform linux/amd64 -t shardpost:dev .
docker run --rm -e LOG_LEVEL=info shardpost:dev config check
```

The fixture mount is required: one server regression test loads Pigeonhole
test files from `third_party`, and the production Dockerfile does not copy
that test data into its build stage. The Go packages are built from `vendor`.
`go test` is not a production Ceph end-to-end test here.

When changing concurrency, additionally run the complete Linux suite with
`go test -p 1 -race -tags tentacle ./...`. Record the scope and platform of
the checks actually run in a release report.

An isolated real server start requires Ceph pools, cephx access, TLS and an
authentication profile. Do not copy production secrets into shell history,
example configuration or logs. The existing Kubernetes profiles are specific
to that infrastructure; they are not a local one-command development system.

## Typical changes

**New IMAP command:** add the parser/dispatch path in `conn.go`, the matching
domain module, state and permission checks, response semantics, resource
release and tests against the TCP test server. Only advertise a capability
once its semantics hold up. Add the known command name for metrics and debug
logs; never turn freely entered arguments into labels.

**New store operation:** change the contract in `model.User`, the in-memory
implementation and the Ceph implementation together. Explain CAS, lease and
crash behavior in the storage chapter. When several objects are involved,
plan for a recovery strategy and tests at the abort boundaries.

**New configuration option:** add a typed field with `mapstructure`, `env`
for suitable scalar values, a default and validation. Test YAML, environment
precedence, invalid values and a secret-free error message. Prefer loading
new secrets through file references. Keep file examples and the operational
profile separate.

**New Sieve extension:** first read the fork contract and its
[AGENTS.md](../../third_party/go-sieve/AGENTS.md). Mind the loader and the
runtime, the capability/require check, the action semantics and both protocol
contexts. The fork has its own compiler and Pigeonhole tests. Afterwards check
vendor parity and the image that was actually built.

**New learning target:** provide a constrained helper and configure it as a
program. Input on stdin; no shell. Exit 0 means success, 75 a temporary error,
other codes a permanent error. An already stored job references the program
name; do not remove it without a transition.

## Resources and logging

Every opened `model.User` view, watch, lease and memory reservation needs a
matching teardown. Callback code must not keep using the RADOS IOContexts
after they have been released. Do not write a response concurrently with the
IMAP NOTIFY pusher; observe its `cmdMu` contract.

Logs use `slog`/JSON. For connections use `logging.Session` and
`logging.Begin`; `session_id`, `proto`, `remote` and `local` establish the
context. Info carries lifecycle and authentication result, debug carries known
command names and durations, warn a degraded operation and error a failed
operation. Never log whole authentication requests, mail bodies or script
arguments. Details: [log configuration](../configuration.md).

## Documentation and review

Before a change, read the affected contract and one real caller. After the
change, update source links, diagrams, configuration description and
regression test together. The [diagram tooling](../diagrams/README.md) keeps
Mermaid sources and SVGs in sync. Release and test reports state the version,
the platform, the checks actually run and the remaining gaps.

The current working directory has no Git metadata. No commit or tag evidence
can therefore be derived here. In a later repository, source state, vendor
state, tests and release digest belong in the same auditable approval process.

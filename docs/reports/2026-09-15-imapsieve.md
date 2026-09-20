# Viper configuration and IMAPSieve

## Result

Rolled out to production on 2026-09-15: **0.10.1**, two healthy server replicas.
Image: `docker.io/chrroessner/imapcephpoc:0.10.1@sha256:f9bdea21100a3bcd76a1e8de40fec52f4e70109b2e6770247307c79e8a25574b`.

- Typed Viper configuration with validator/v10, strict YAML/env conversion, secret files and `config check`. The existing env names remain as overrides.
- IMAPSieve for APPEND, COPY/MOVE and real flag changes including FETCH/Seen. Assignment via METADATA, bounded execution, ACL check and a separate delivery activation.
- Configurable Rspamd rules with a durable Ceph learning queue, independent payloads, atomic commit proofs, retry and preserved failed jobs.
- YAML: `deploy/overlays/croessner-nauthilus/server.yaml`; existing secret references preserved. Active ConfigMap: `imap/imap-server-config-8dhg8g7h7k`.
- ManageSieve capability: `sieve://mail.roessner-net.de:4190`, verified through the existing director for both test accounts.
- Rspamd: separate learning endpoint `192.168.0.2:11336`, worker source addresses, its own token, exactly POST /learnspam and /learnham, header allowlist and a 4 MiB limit.

## Acceptance

| Check | Result |
| --- | --- |
| Entire server project on Linux: `go test -race -tags tentacle ./...` | passed |
| Docker build including `go vet` | passed |
| Kubernetes YAML with the built image: `config check` | passed with isolated test secrets |
| OTLP HTTP integration test with an actual export | passed, including the base path |
| New Sieve IMAPSieve fixture | passed |
| Pigeonhole conformance | 117 passed, 31 failed out of 148; existing coverage gaps |
| Separate fork: `go test ./...` | TestTestAddress fails on the missing `test-address.svtest`; the remaining tested packages passed |
| Mailstack stack/secret/render/runtime contracts | passed |
| HAProxy 3.2.19 syntax and isolated access tests | passed; body unchanged, token removed |
| Native candidate acceptance with both test accounts | APPEND, FETCH/Seen, MOVE to Junk and back passed; 4 learning jobs confirmed |
| Native public acceptance through the director, both test accounts | passed |
| Public acceptance across the Ceph shard boundary, both test accounts | passed; target UID 21 at shard size 20 |
| Learning queue of the regular server after the public acceptance runs | 8 prepared, 8 confirmed; additionally 4 confirmed earlier on the candidate |
| Existing Rspamd admin ports of node1 | 11334 and 11335, HTTP 200 each |
| Unauthorized access to the learning port | admin path, wrong token and query, HTTP 403 each |
| Postflight | both servers healthy with an identical digest; primary/shadow Rspamd healthy; 0 IMAPSieve and 0 OTLP export errors during the observation period |

The native test `hack/imapsieve-live-test.py` creates unique test folders and an inactive script and removes them afterwards. No SMTP messages were sent. `IMAP_TEST_SHARD_BOUNDARY=20` exercises higher index shards.

The full imaptest suite was started twice against the candidate. Both runs aborted at `NO [UNAVAILABLE]` because of the already documented Nauthilus tempfail. The correlated test requests showed `authn=true`, `authz=true` and `director_routing:ok`, then `authenticated=tempfail`. These runs are **not passed**. In addition, the known `fetch-envelope` difference with stale source routes remains visible. No authentication checks were weakened.

## Review and fix rounds

Several independent rounds until completion without further P1/P2 findings. Among the things corrected were strict conversion, empty scripts, durable target copies before Deleted, ambiguous storage errors, queue fairness, DELETE/link contention, lock losses and SETACTIVE separation.

The candidate acceptance additionally found the missing signal path of the HTTP tracing exporter. Version 0.10.1 appends `/v1/traces` to the OTLP base URL; a real export test protects this behavior. 0.10.0 remains an already published predecessor image that was never used in production.

The ConfigMap namespace setting is applied exclusively at the generator. The independent full render check confirms all 17 resources including Ceph/RBAC in `rook-ceph`, the ServiceAccount assignment, the selectors and the secret/certificate references. Only the server deployment, the ConfigMap and the scrub CronJob were applied in production; Ceph/RBAC were not changed.

## Rollout and limits

The user explicitly approved the Docker Hub push and the full rollout despite the unused backup maintenance window. The proxy was recreated deliberately; Rspamd and other mail services were not restarted. Temporary candidate, test job and surplus ConfigMap resources as well as local copies of the learning token were removed.

Extprograms remain deferred. IMAP fileinto requires an existing target mailbox. Failed jobs stay stored for operator handling. Transport is at-least-once, there is no distributed exactly-once guarantee. The conformance and Nauthilus test gaps mentioned above remain open.

## Rollback

Deployment starting state: `/private/tmp/imap-server-before.json`, previous version 0.9.0; in addition, the existing Kubernetes deployment history. Source starting copy: `/private/tmp/imapcephpoc-before-imapsieve.tgz`.

Proxy backup on the mail host: the original source under `/srv/docker/mailstack/temp/imapsieve-rollback-20260915T184447/source.yaml`; the original rendered compose file and render env under `/srv/docker/mailstack/temp/imapsieve-rollback-20260915T184744/`. Recreate only the proxy, deliberately, with the restored compose configuration. Do not delete any mail, Rspamd or archive data.

The workspace directory has no `.git`; no commit/tag was created. The Kubernetes overlay references the published image by digest.

# ShardPost 0.12.0

Status: **ShardPost 0.12.0 published, rolled out and verified in production.**

The user chose ShardPost as the product name and approved the refactoring,
the image publication and the production rollout. The previous
production state was imapcephpoc 0.11.3.

Converted: the Go module itself and the imports, server/load generator, image targets,
protocol identities, config variable and mount, helper path, build instructions,
documentation and diagrams. Compatibility entry points and the existing data/
monitoring/routing contracts are listed in the [name decision](../decisions/project-name.md).
No RADOS data migration takes place.

Evidence will be added after the rollout. Starting backups:
`/private/tmp/shardpost-*-before.json` and
`/private/tmp/shardpost-source-before.tar.gz`.

## Local verification

- Full Linux build with `go vet` and `go test -p 1 -race -tags tentacle ./...` passed.
- Additional targeted race tests of all affected cgo-free packages passed.
- The new `SHARDPOST_CONFIG`, the old `IMAP_CONFIG` fallback and the CLI precedence tested.
- Runtime `config check` passed for both the new and the old binary invocation.
- IMAP ID returns ShardPost; the runtime helper was executed under the new path,
  old/new helper content identical, unchanged safe ownership/file modes.
- Tools image: swaks and curl runnable.
- The new ConfigMap compared against the live content: only the helper path and
  the explicit old OTel service identifier differ.
- Neither target tag existed before publication.
- Diagrams re-rendered; no broken local Markdown references.

Evidence: `/private/tmp/shardpost-linux-tests.log`,
`/private/tmp/shardpost-build.log`, `/private/tmp/shardpost-config-next.json`.

## Publication and production verification

- Server: `docker.io/chrroessner/shardpost:0.12.0@sha256:acb70171a0272f0a7e47a6da2b6d61d73b3348522f76f1a2cc69072e7c07e2e7`.
- Tools: `docker.io/chrroessner/shardpost-tools:0.12.0@sha256:45949884ff8fb6ca53ea2db173f07866147905e8ab341d6777b060fffe650049`.
- New ConfigMap: `imap-server-config-4f7c9bk44f`; the previous map is kept.
- Both server replicas Ready, zero restarts, the actual imageIDs match the new digest.
- `shardpost config check` successful against the configuration actually mounted on both pods.
- Both metrics endpoints queried through the new tools deployment; the existing metric families are present.
- Tools deployment rolled out successfully; scrub CronJob set to the same server digest.
- Both test accounts verified publicly over IMAP and ManageSieve: TLS, login,
  the ShardPost product identity, NOOP and logout successful.
- Full session logs including the public PROXY client IP and
  `service=shardpost`. 202 info lines in the acceptance window, no debug/warn/error lines.
- Full pod spec comparison: for the server only the image, the config env, the mount and
  the ConfigMap reference changed. For tools and the CronJob only the image changed.
- Pools, namespaces, credentials, TLS, network rules and stored
  learning job actions unchanged. No test mail or external delivery produced.

The automatic release check required an additional confirmation of the
two concrete Docker Hub targets; it was granted, after which both uploads
and registry readbacks completed successfully.

## Way back

Previous server image:
`docker.io/chrroessner/imapcephpoc:0.11.3@sha256:b32270400f58ccb976af35d879fde7f25a78a4a53ad3bb83dab5519027d9f982`.
The backups `shardpost-deployment-rollout-before.json`,
`shardpost-cronjob-rollout-before.json` and `shardpost-tools-rollout-before.json`
under `/private/tmp` contain the state verified immediately before the
respective patch. For the server, image, previous ConfigMap, mount and
IMAP_CONFIG must be set back together. No data migration is needed.
The declarative release manifests must be adjusted as well on a way back.

## Limits

The conversion ran the full server race suite, but did not re-run the entire
historical Pigeonhole/imaptest or load suite. Their documented
exceptions remain. The collector and the Grafana dashboards were
not renamed; their service/metric contract remains explicitly compatible.
The workspace folder keeps its previous path. Without git metadata,
no git commits or tags were created.

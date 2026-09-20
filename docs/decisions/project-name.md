# ShardPost: naming decision and migration

Status: **ShardPost chosen, refactoring/rollout approved**, 2026-09-16.
First release under the new name: **0.12.0**. Since **0.13.0** (2026-09-18)
the operational and external identifiers carry the name as well.

ShardPost denotes the distributed mailbox and processing layer across
several protocols. The former name `imapcephpoc` is now only a historical
or explicitly compatible identifier. The rename is not a migration
of the messages or their storage formats.

## Product identifiers that changed

| Area | New name |
|---|---|
| Product / protocol identity | ShardPost |
| Go module and internal imports | `shardpost` / `shardpost/internal/...` |
| Server source / binary | `cmd/shardpost` / `/usr/local/bin/shardpost` |
| Load generator | `cmd/shardpost-load` / `/usr/local/bin/shardpost-load` |
| Server image | `docker.io/chrroessner/shardpost:0.12.0` |
| Tools image | `docker.io/chrroessner/shardpost-tools:0.12.0` |
| Configuration selection | `SHARDPOST_CONFIG` or `--config` |
| Configuration mount in the operational profile | `/etc/shardpost/server.yaml` |
| External programs | `/usr/local/lib/shardpost/programs` |
| OTel default for new installations | `service.name=shardpost`, instrumentation scope `shardpost` |

The README, the developer guide, the architecture diagrams, the build
instructions, the test scripts and the release manifests use the new names.
Historical evidence keeps the images, digests and URLs of its time. The local
working directory is named `shardpost`.

## Transitional compatibility

The 0.12.x images contained the symlinks `imapcephpoc -> shardpost`,
`imapload -> shardpost-load` and
`/usr/local/lib/imapcephpoc -> /usr/local/lib/shardpost`. They were removed on
2026-09-18: all manifests and the active ConfigMap use
`/usr/local/bin/shardpost` and `/usr/local/lib/shardpost/programs`. A
rollback to a ConfigMap from before 0.12.0 therefore has to happen together
with the image of that time (as the section "Rollout and rollback path"
provides for anyway). Since that same day, the working directory is likewise
named `shardpost`.

Configuration path precedence: an explicit `--config`, then a set
`SHARDPOST_CONFIG`, otherwise `IMAP_CONFIG`. A deliberately empty
`SHARDPOST_CONFIG` means defaults without a file path and prevents the
fallback to the old variable. Protocol-specific settings such as
`IMAP_ADDR`, `POP3_ADDR` and `LMTP_ADDR` keep their factually fitting names.

Up to 0.12.x the Kubernetes names (`imap`, `imap-server`, `imap-scrub`,
service `imap`), the RADOS pools, secrets, certificates and the
director/Nauthilus contracts stayed unchanged. With 0.13.0 (2026-09-18) they
were carried along: namespace `shardpost`, deployment `shardpost-server`,
service `shardpost`, pools `shardpost-index`/`shardpost-data` (copied with
`hack/pool-copy.py`, verified object by object), cephx client `shardpost`,
director shard `shardpost`, OIDC client `shardpost-service`, LDAP health
account `mail_health_shardpost` and the SOPS file `shardpost.sops.yaml` of
the test accounts. Only the logins of the test accounts and the account
namespaces in RADOS remain unchanged. The procedure and the evidence are in
report [2026-09-18-shardpost-0.13.0](../reports/2026-09-18-shardpost-0.13.0.md).

The metric families `imapceph_*` and `imap_*` remain stable as an existing
monitoring API. Since 0.13.0, `telemetry.service_name` is `shardpost` in the
operational profile as well; `OTEL_SERVICE_NAME` can override it. The product name in the operational logs is `shardpost`.

## Rollout and rollback path

New images are pinned by digest. For the server deployment, the
image, the configuration mount and the environment selection are switched
together. In substance, the new ConfigMap differs only in the helper path and
the explicit telemetry compatibility name. The scrub CronJob and the tools
deployment switch to their respective new images.

The previous ConfigMap and the earlier manifests remain available for a
rollback. A rollback restores the old image, the old mount and the old
environment selection together; the old binary alone in front of a new
configuration is not enough, because of the new field
`telemetry.service_name`. Pool names, object formats and learning job actions
were not changed.

Verification: a full Linux build/race suite, old and new CLI configuration,
protocol identity, unchanged monitoring contracts and a real login of
both test accounts over IMAP and ManageSieve. The concrete production
evidence is in the [release report](../reports/2026-09-16-shardpost.md).

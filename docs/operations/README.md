# Operations and releases

[Documentation index](../README.md) · [Configuration](../configuration.md)

## Dependencies and responsibility boundaries

The active profile is `deploy/overlays/croessner-nauthilus`; since 0.13.0 the
namespace and the deployment are named `shardpost` and `shardpost-server`. The profile expects Rook
pools and cephx access, cert-manager/TLS, Nauthilus backchannel access,
director backend access and the configured learning and relay services.

The secrets are provided by the responsible infrastructure procedures.
The server reads Rook access with restricted RBAC. Renaming the
application changes pools, certificate names and director routes only together
with a data copy; see [naming migration](../decisions/project-name.md).

The base overlay uses static test credentials. `make deploy` selects the
`croessner-nauthilus` operational profile. Even so, verify the intended target
before applying. `make undeploy` also deletes Rook resources
and must not be used as a rollback.

## Read-only first diagnosis

```bash
kubectl --context croessner -n shardpost get pods -l app.kubernetes.io/name=shardpost-server
kubectl --context croessner -n shardpost rollout status deployment/shardpost-server --timeout=30s
kubectl --context croessner -n shardpost logs -l app.kubernetes.io/name=shardpost-server --since=15m --tail=500
kubectl --context croessner -n shardpost get deployment shardpost-server \
  -o jsonpath='{.spec.template.spec.containers[0].image}'
```

Do not dump whole secrets or environments into reports. The client IP,
the user and the mailbox can be personal data in operational logs. When
sharing, use only the events needed and, where appropriate, pseudonymized
correlation data.

| Symptom | Distinguish first |
|---|---|
| No client connection in the log | Level, correct pod/time range, director routing, a new versus an already running session |
| Only the director/pod IP | PROXY emission, trusted CIDRs and the actual access path |
| Authentication failed | `auth_failed` versus a temporary backend/OIDC error |
| Ready, but mail service disrupted | Readiness checks Ceph, not the whole auth/Sieve path |
| IDLE updates late | Watch/notify, journal/MODSEQ synchronization, polling |
| Learning delayed | Prepared/commit receipt, worker lease, budget, retry, dead, external helper |
| Storing fails | Quota, Ceph latency/errors, intents, RADOS and backend metrics |

At `info`, new connections, login, TLS setup and close become
visible. Regular director health checks also produce lifecycle logs.
`debug` adds command names and durations; a rollout is required for level
changes. `remote` shows the effective client, `session_id` ties events together.

## Release procedure

1. Record the starting image including its digest, the configuration, the
   replica count and any open operational anomalies. Secure a rollback path for
   the image and, where applicable, the ConfigMap.
2. Check the changed paths, the matching race/unit tests and the complete Linux
   build. For data model changes, additionally run real Ceph abort tests.
3. Build a new immutable version tag. Run the runtime `config check`.
   The publication target and the rollout must be within the authorized scope.
4. Publish the image and read back the registry digest. Pin the deployment to
   `tag@sha256:...`; carry the release overlay along.
5. Change the image and the intended settings in one controlled update.
   On competing changes, abort and compare again. Do not blindly
   apply a large overlay over an unknown live state.
6. Wait for the rolling update; check Ready, restarts and the actual `imageID`
   of every replica. The scrub CronJob should match the approved server state.
7. Test the affected user path. When the director/auth path is shared, check
   both designated test accounts and the relevant protocols. SMTP
   test deliveries are a side effect of their own.
8. Check logs/metrics and, where applicable, traces, remove temporary
   resources, and document the result and the remaining gaps.

For the logging release, only the image and `LOG_LEVEL` were changed in the pod
spec; the ConfigMap, the pools and the director stayed unchanged. That is an
example of a limited release, not a universal migration procedure.

## Rollback and data compatibility

For a pure code or logging change, the previous digest can be restored with
the saved settings. A blind `rollout undo`
is only suitable when the selected revision really contains the desired
complete state. The CronJob and the declarative overlay are part
of the rollback, so that a later apply does not roll forward again.

For changes to persistent formats, to the program names of stored jobs, to the
chunk size or to the index mapping, an old binary is not a sufficient rollback.
Define a compatibility strategy for old and new writers beforehand.
Do not rename or recreate pools and namespaces when rebranding.

A PROXY rollback requires a sequence: first disable the director's emission and
let its rollout finish, then remove the receiver's trust. Otherwise
the header can reach the server as a protocol command.

## Scrub, backup and observability

Scrub can change data. For an investigation, first define a dry run and the
scope. It repairs defined inconsistencies but does not reconstruct
lost mail content. Evidence of backup/restore has to cover both pools and
the associated durable state.

Prometheus, Loki and Tempo make different statements. A missing
series is not zero; a sampled trace is not every connection.
The server sets some user/mailbox/peer attributes in spans;
the documented redaction happens in the external collector. It must not be
assumed to be automatically given server-side when the telemetry is
rebuilt. See the [observability contract](../observability.md).

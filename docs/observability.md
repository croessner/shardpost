# Observability

## Signal sources

The service exposes Prometheus metrics at `/metrics` on `HTTP_ADDR`
(default `:8080`). `/healthz` checks the HTTP process; `/readyz`
additionally checks the store and takes the drain state into account. A
successful scrape alone is therefore no proof of working mail delivery.

OpenTelemetry exports traces as soon as `OTEL_EXPORTER_OTLP_ENDPOINT` is
set. `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` selects HTTP; otherwise the
service uses gRPC. `telemetry.service_name` and `OTEL_SERVICE_NAME`
determine `service.name` (default `shardpost`, since 0.13.0 in the
production profile as well; up to 0.12.x `imapcephpoc`). The collector
redaction does not depend on the service name; `service.instance.id` is the
host name. The sampler is ParentBased with a TraceIDRatioBased root
sampler: `OTEL_TRACES_SAMPLER_ARG=0.1` captures roughly ten percent of the
root traces. Without an argument the ratio is 1. A different
`OTEL_TRACES_SAMPLER` value does not change this selection, which is fixed in code.

## Reading the metrics correctly

| Metric | Meaning and limitation |
| --- | --- |
| `imapceph_connections{proto}` | Active IMAP/POP3 connections; not a complete connection count for all protocols. |
| `imapceph_command_seconds` | Histogram of the instrumented IMAP/POP3 commands. `_count` is suitable for the command rate; `_bucket` for quantiles. |
| `imapceph_commands_total` | Protocol-specific events. Not a universal request counter. Evaluate IMAP `ok/no/bad` separately from auth, connection and poll events. |
| `imapceph_lmtp_deliveries_total{outcome}` | Successful and failed instrumented delivery attempts. |
| `imapceph_lmtp_delivered_bytes_total` | Bytes of successful LMTP deliveries. |
| `imapceph_rados_op_seconds{op}` | Latencies of selected header/index operations: `header_read`, `header_cas`, `list_entries`, `get_entries`. Not a complete RADOS I/O profile. |
| `imapceph_rados_cas_retries_total{kind}` | Retries after compare-and-swap conflicts. |
| `imapceph_rados_notify_seconds` | Registered, but currently without a measuring call. Do not interpret it as a confirmed notify latency. |
| `imapceph_scrub_findings_total{kind}` | Findings in the respective scrubber process. A server scrape is no proof of a successful external scrub run. |

In `commands_total`, POP3 counts only selected events; there `DELE` counts
deleted messages. With IMAP, auth events can be counted in addition to the
protocol response. Using the sum of all outcomes as the denominator of an
error rate would mix different events. In the current histogram, IMAP `UID`
commands are recorded together under `UID`.

Missing series are not zero values. In particular, many labeled series only
come into existence with the first event. For quantiles, the histogram
buckets have to be summed across the selected pods before the quantile is
computed; per-pod quantiles must not be averaged. Without observations,
quantiles are not meaningful.

## Traces and access

IMAP/POP3 commands, `lmtp.deliver`, `delivery.deliver`, admin requests
and maintenance jobs are instrumented, and the store reports its own work
below them. A trace therefore shows where the time of a command actually
goes:

```
imap.FETCH
├─ store.entries        mailbox, number of entries
└─ store.blob_read      guid, bytes
```

Store spans: `store.deliver`, `store.read`, `store.blob_read`,
`store.blob_write`, `store.fts_index`, `store.fts_search`,
`store.entries`, `store.flags`, `store.copy`, `store.move`,
`store.expunge`, plus `rados.omap_cas` for the central compare-and-swap
loop. A failed operation carries the error and an error status on its
span.

The protocol layer hands the context of the running command to the store
(`model.RequestScoped`); a store that does not implement that interface
simply produces no store spans. Work that runs outside a command, such as
a background notifier, produces root spans instead.

The traces currently also contain user attributes (`imap.user`, `pop3.user`,
`lmtp.user`), and for IMAP possibly the mailbox and the peer
(`imap.mailbox`, `net.peer`). The collector configuration and the access
rights have to implement the applicable rules for this data. Credentials
belong neither in dashboards nor in export files or operational reports.

## Operational responsibility and acceptance

Enabling this in production belongs in the authoritative manifests of the
Kubernetes owner repository. Scrape annotations in this source project
alone are no proof of collection by a Prometheus server. The metrics owner
is responsible for the connection to `metrics.roessner-net.de`, for the data
sources and for the operator dashboard in the Grafana folder **Mail**.

A complete acceptance comprises ready pods, current Prometheus series for
every expected target, an actual service trace in Tempo as well as the
stored and rendered dashboard. A read-only IMAP command can verify the
trace path without a test delivery; with sampling, several commands may be
necessary. Missing live evidence explicitly stays open.

## Production acceptance on 2026-09-15

- Namespace `imap`, deployment `imap-server`: two ready pods; both central
  Prometheus targets `up=1` after the final rollout.
- Scrape labels: `job="kubernetes-pods"`, `namespace="imap"`,
  `app_kubernetes_io_name="imap-server"`, `pod`.
- OTLP: `http://192.168.0.102:4318`, `http/protobuf`, sampling `0.1`.
- [Mail Operator in Grafana](https://metrics.roessner-net.de/d/imapcephpoc-mail-operator/imapcephpoc-c2b7-mail-operator):
  version 2, 21 panels including section headings and interpretation;
  API readback, rendered rate/latency/LMTP panels and logs/Tempo links verified.
- A fresh, real trace after the final activation (13:58:38 UTC,
  `5e9e1e204e89112703b8ea9f284236f2`, `imap.STARTTLS`) was verified
  independently in Tempo by the Kubernetes owner: none of the five
  user/mailbox/peer attributes named above. The preceding synthetic filter test
  was confirmed by both owners.
- CPU/RAM series are currently not present in the central Prometheus.
  The dashboard shows the gap. Missing POP3/error series are not
  artificially set to zero. Rate queries use at least four minutes;
  this window was verified against existing series.

During the first filter rollout, the activation replaced the host file
atomically but did not force a new collector container. The still healthy
collector therefore kept exporting unfiltered traces. The service export was
rolled back, the collector was deliberately recreated, and the effect of the
filter was verified synthetically before the activation was explicitly
approved again. Older traces that are already stored are not cleaned up
retroactively by this.

Reproducible dashboard source in the metrics owner repository (since 0.13.0):
`grafana/tools/build-shardpost-operator-dashboard.py` and
`grafana/dashboards/shardpost-operator.dashboard.json`, dashboard UID
`shardpost-mail-operator`
([Grafana](https://metrics.roessner-net.de/d/shardpost-mail-operator/shardpost-c2b7-mail-operator),
since 2026-09-18; the old `imapcephpoc-mail-operator` has been backed up and removed). The queries cover the old and the new labels
(`namespace` `imap|shardpost`, `service.name` `imapcephpoc|shardpost`).
Kubernetes owner contract: `shardpost/telemetry-patch.yaml` and
`reports/2026-09-15-imapcephpoc-observability.md`.


## Structured session logs from 0.11.3

`slog` writes JSON to stdout; Kubernetes uses `LOG_LEVEL=info`.
`connection_open`, `auth_success`/`auth_failed`, `tls_established` and
`connection_close` make the connection lifecycle visible. `session_id`
ties the events together; `remote` is the effective PROXY client address.
Director health checks produce connection logs as well. Logs are not
filtered by the OTLP sampling rate.

Debug adds the known command names and durations, without arguments or
mail contents. For troubleshooting with a client, 0.18.0 introduced the
time-limited protocol trace per account (`event=protocol_trace`, see
[Admin API](admin-api.md#protocol-trace-for-an-account)). Details, limitations and a Loki example are in the
[configuration documentation](configuration.md), the dated evidence in the
[logging report](reports/2026-09-16-logging.md).

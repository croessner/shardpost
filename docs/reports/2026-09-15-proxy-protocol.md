# Real client IP behind the director

## Cause and result

Two configuration switches were missing: the list
`server.proxy_protocol_trusted_cidrs` was empty and the three imappoc backends
in the director had `haproxy.enabled: false`. The backend server could
therefore only see the director pod IP as the peer.

The fix is rolled out. For both test accounts it was demonstrated, in IMAP and
ManageSieve, that the public client IP of the director authentication matches
the IP of the subsequent backend identity lookup. The master user path checks
its own key and passes the client address on to the Nauthilus lookup. This
report contains no client IP in the clear.

## Change

- The server trusts the Calico pod network `172.16.0.0/12` as demonstrated live.
- A new `imap/imap-server-ingress` NetworkPolicy restricts all mail ports
  to director pods from namespace `director` and to controlled mailtest/
  imaptest pods from namespace `imap`. Namespace and pod label are required
  together; matching labels alone in other namespaces are not sufficient.
- The test tools explicitly share the proxy trust boundary. HTTP
  8080 remains reachable for health/monitoring. Egress remains unchanged.
- `haproxy.enabled: true` for `imappoc-imap`, `imappoc-sieve`, `imappoc-lmtp`.
  LMTP is included because the shared optional parser would otherwise wait
  five seconds before the greeting, while director health checks have three
  seconds. All other director settings are semantically
  unchanged; this was verified against the live starting state.
- Configuration only was changed: the server image remains 0.10.1 with the existing digest.

## Acceptance

- Independent review of the trust boundary, selectors, render and rollout order.
- Server dry run of both configurations passed.
- NetworkPolicy: an unlabeled pod reaches HTTP health, mail TCP is blocked.
- The intended mailtest tool still reaches the mail port.
- Two IMAP and three director replicas ready after the rollout.
- Public, TLS-verified IMAP and ManageSieve logins of both test accounts
  passed; STATUS INBOX and NOOP successful.
- Nauthilus correlation: in all four combinations (two accounts, two
  protocols) an identical public IP for frontend auth and backend lookup.
- The director control API confirms all three imappoc backends with
  `outbound_proxy_protocol=true`, `in_service=true`, `maintenance=disabled`.
- The existing director configuration tests for health identities and SASL
  EXTERNAL passed.
- LMTP verified successfully up to RCPT for both recipients, without DATA and without delivery.

## Sources

Server: `deploy/overlays/croessner-nauthilus/server.yaml`,
`network-policy.yaml` and `kustomization.yaml`; the test job label in
`hack/imaptest-job.yaml`. Director:
`/Users/croessner/OpenCloud/Persönlich/Kubernetes/director/configmap.yaml`
and the operations document `imappoc.md`.

## Rollback

First set the three director senders back to `haproxy.enabled: false`
and wait for the director rollout; then set the server receiver back to the
previous empty trust value. The NetworkPolicy can remain in place as an
access boundary. The director's starting configuration is stored
privately under `/private/tmp/imap-proxy-director-before.json`.

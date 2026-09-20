# ucm-cert-renew (contrib)

Keeps an LDAP client certificate of the UCM PKI (RNS Internal Root CA 2026 →
RNS Client Authentication Issuing CA 01) up to date in a Kubernetes secret.
Its own Go module and its own image, **not part of ShardPost**; it
lives here because it came about together with the ShardPost quota integration, and
is used for Nauthilus as well.

## Procedure

1. Read the secret (`tls.crt` leaf + issuer, `tls.key`, optional CA key).
2. Renew when the certificate or the key is missing, the subject or the key
   does not match, the chain against the pinned anchors or `clientAuth`
   is wrong, less than `UCM_RENEW_BEFORE` remains, or the
   CA anchor has changed.
3. New RSA 3072 key and CSR without extensions, subject exactly in the
   order and with the string types of the signer.
4. CSR over SSH to the restricted UCM signer (ForceCommand of the
   caller account, pinned host key); response at most 64 KiB.
5. Check the result (subject, key, chain, exactly `clientAuth`) and replace the
   secret with a `PUT` carrying `resourceVersion`; other keys are kept.

The private key never leaves the job, the signer only sees the CSR.

## Configuration

| Variable | Meaning |
| --- | --- |
| `UCM_RENEW_SUBJECT` | Subject in signer order, e.g. `C=DE,DC=de,DC=roessner-net,ST=Hessen,L=Alsfeld,O=R.N.S.,OU=Mail,CN=…,emailAddress=…` |
| `UCM_RENEW_SECRET`, `UCM_RENEW_NAMESPACE` | Target secret (must exist) and namespace (default: its own) |
| `UCM_RENEW_BEFORE` | Renewal lead time (default `1080h` = 45 days: with a 90 day lifetime the signer may be down for up to 45 days) |
| `UCM_RENEW_ROOT_FILE`, `UCM_RENEW_ISSUER_FILE` | Pinned root and issuing CA (PEM) |
| `UCM_RENEW_CA_KEY` | Also store the root under this secret key (optional) |
| `UCM_RENEW_SSH_ADDR`, `UCM_RENEW_SSH_USER` | Signer `host:port` and caller account |
| `UCM_RENEW_SSH_KEY_FILE`, `UCM_RENEW_SSH_HOST_KEY_FILE` | SSH key of the account, pinned host key (authorized_keys format) |
| `UCM_RENEW_TIMEOUT` | Total time (default `2m`) |

Exit 0: fresh or renewed, 1: error (the CronJob runs again). If the
signer issues for a shorter period than the lead time (for example because the issuing CA
expires soon), the result is stored but the run ends with an error, so that not every run
issues anew.

RBAC: `get` and `update` on exactly this secret (`resourceNames` applies to
`get`/`update`, not to `list`/`watch`). The secret must exist. An
annotation `kubectl.kubernetes.io/last-applied-configuration` (after an
earlier `kubectl apply` it contains the old key material) is removed by the
tool when writing; after that the secret must no longer be maintained with
`kubectl apply`.

SSH: the tool sends a `shell` request without a command; the server runs
the `ForceCommand` of the account. Only the type of the pinned
host key is offered (ed25519 recommended). One SSH key per namespace.

## Development

```sh
make contrib-ucm-renew-test   # go test -race
make contrib-ucm-renew-lint   # golangci-lint in the lint image
make contrib-ucm-renew-build  # image
```

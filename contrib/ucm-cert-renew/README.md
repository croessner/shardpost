# ucm-cert-renew (contrib)

Hält ein LDAP-Clientzertifikat der UCM-PKI (RNS Internal Root CA 2026 →
RNS Client Authentication Issuing CA 01) in einem Kubernetes-Secret aktuell.
Eigenes Go-Modul und eigenes Image, **kein Bestandteil von ShardPost**; es
liegt hier, weil es mit der ShardPost-Quota-Anbindung entstanden ist, und
wird ebenso für Nauthilus genutzt.

## Ablauf

1. Secret lesen (`tls.crt` Leaf + Issuer, `tls.key`, optional CA-Schlüssel).
2. Erneuern, wenn Zertifikat oder Schlüssel fehlen, Subject oder Schlüssel
   nicht passen, die Kette gegen die gepinnten Anker oder `clientAuth`
   nicht stimmt, weniger als `UCM_RENEW_BEFORE` bleibt oder sich der
   CA-Anker geändert hat.
3. Neuer RSA-3072-Schlüssel und CSR ohne Extensions, Subject exakt in der
   Reihenfolge und mit den String-Typen des Signers.
4. CSR per SSH an den eingeschränkten UCM-Signer (ForceCommand des
   Aufruferkontos, gepinnter Host-Key); Antwort höchstens 64 KiB.
5. Ergebnis prüfen (Subject, Schlüssel, Kette, exakt `clientAuth`) und das
   Secret per `PUT` mit `resourceVersion` ersetzen; andere Schlüssel bleiben.

Der private Schlüssel verlässt den Job nie, der Signer sieht nur den CSR.

## Konfiguration

| Variable | Bedeutung |
| --- | --- |
| `UCM_RENEW_SUBJECT` | Subject in Signer-Reihenfolge, z. B. `C=DE,DC=de,DC=roessner-net,ST=Hessen,L=Alsfeld,O=R.N.S.,OU=Mail,CN=…,emailAddress=…` |
| `UCM_RENEW_SECRET`, `UCM_RENEW_NAMESPACE` | Ziel-Secret (muss existieren) und Namespace (Standard: der eigene) |
| `UCM_RENEW_BEFORE` | Erneuerungsvorlauf (Standard `1080h` = 45 Tage: bei 90 Tagen Laufzeit darf der Signer bis zu 45 Tage ausfallen) |
| `UCM_RENEW_ROOT_FILE`, `UCM_RENEW_ISSUER_FILE` | gepinnte Root- und Issuing-CA (PEM) |
| `UCM_RENEW_CA_KEY` | Root zusätzlich unter diesem Secret-Schlüssel ablegen (optional) |
| `UCM_RENEW_SSH_ADDR`, `UCM_RENEW_SSH_USER` | Signer `host:port` und Aufruferkonto |
| `UCM_RENEW_SSH_KEY_FILE`, `UCM_RENEW_SSH_HOST_KEY_FILE` | SSH-Schlüssel des Kontos, gepinnter Host-Key (authorized_keys-Format) |
| `UCM_RENEW_TIMEOUT` | Gesamtzeit (Standard `2m`) |

Exit 0: frisch oder erneuert, 1: Fehler (der CronJob läuft erneut). Stellt
der Signer kürzer aus als der Vorlauf (etwa weil die Issuing-CA bald
abläuft), wird gespeichert, aber mit Fehler beendet, damit nicht jeder Lauf
neu ausstellt.

RBAC: `get` und `update` auf genau dieses Secret (`resourceNames` wirkt für
`get`/`update`, nicht für `list`/`watch`). Das Secret muss existieren. Eine
Annotation `kubectl.kubernetes.io/last-applied-configuration` (enthält nach
einem früheren `kubectl apply` das alte Schlüsselmaterial) entfernt das
Werkzeug beim Schreiben; das Secret darf danach nicht mehr per
`kubectl apply` gepflegt werden.

SSH: Das Werkzeug sendet einen `shell`-Request ohne Befehl; der Server führt
das `ForceCommand` des Kontos aus. Angeboten wird nur der Typ des gepinnten
Host-Keys (ed25519 empfohlen). Pro Namespace ein eigener SSH-Schlüssel.

## Entwicklung

```sh
make contrib-ucm-renew-test   # go test -race
make contrib-ucm-renew-lint   # golangci-lint im Lint-Image
make contrib-ucm-renew-build  # Image
```

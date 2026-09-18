# Echte Client-IP hinter dem Director

## Ursache und Ergebnis

Zwei Konfigurationsschalter fehlten: Die Liste
`server.proxy_protocol_trusted_cidrs` war leer und die drei imappoc-Backends
im Director hatten `haproxy.enabled: false`. Der Backendserver konnte daher
nur die Director-Pod-IP als Gegenstelle sehen.

Der Fix ist ausgerollt. Für beide Testaccounts wurde in IMAP und ManageSieve
nachgewiesen: Die öffentliche Client-IP der Director-Authentifizierung stimmt
mit der IP des nachfolgenden Backend-Identity-Lookups überein. Der Master-User-
Pfad prüft seinen eigenen Schlüssel und reicht die Clientadresse an den
Nauthilus-Lookup weiter. Der Bericht enthält keine Klartext-Client-IP.

## Änderung

- Server vertraut dem live nachgewiesenen Calico-Podnetz `172.16.0.0/12`.
- Neue `imap/imap-server-ingress` NetworkPolicy beschränkt sämtliche Mailports
  auf Director-Pods aus Namespace `director` und kontrollierte mailtest-/
  imaptest-Pods aus Namespace `imap`. Namespace und Podlabel sind gemeinsam
  erforderlich; reine passende Labels in anderen Namespaces genügen nicht.
- Die Testwerkzeuge teilen ausdrücklich die Proxy-Vertrauensgrenze. HTTP
  8080 bleibt für Health/Monitoring erreichbar. Egress bleibt unverändert.
- `haproxy.enabled: true` bei `imappoc-imap`, `imappoc-sieve`, `imappoc-lmtp`.
  LMTP ist einbezogen, da der gemeinsame optionale Parser andernfalls vor
  dem Greeting fünf Sekunden wartet, während Director-Healthchecks drei
  Sekunden Zeit haben. Alle übrigen Director-Einstellungen sind semantisch
  unverändert; dies wurde gegen den Live-Ausgangszustand geprüft.
- Nur Konfiguration geändert: Serverimage bleibt 0.10.1 mit bestehendem Digest.

## Abnahme

- Unabhängiger Review von Trust-Grenze, Selektoren, Render und Rolloutfolge.
- Server-Dry-Run beider Konfigurationen bestanden.
- NetworkPolicy: unmarkierter Pod erreicht HTTP-Health, Mail-TCP wird blockiert.
- Vorgesehenes mailtest-Werkzeug erreicht weiterhin den Mailport.
- Zwei IMAP- und drei Director-Replikas nach Rollout bereit.
- Öffentliche, TLS-verifizierte IMAP- und ManageSieve-Logins beider Testaccounts
  bestanden; STATUS INBOX und NOOP erfolgreich.
- Nauthilus-Korrelation: in allen vier Kombinationen (zwei Accounts, zwei
  Protokolle) identische öffentliche IP bei Frontend-Auth und Backend-Lookup.
- Director-Control-API bestätigt alle drei imappoc-Backends mit
  `outbound_proxy_protocol=true`, `in_service=true`, `maintenance=disabled`.
- Bestehende Director-Konfigurationstests für Health-Identitäten und SASL
  EXTERNAL bestanden.
- LMTP für beide Empfänger bis RCPT erfolgreich geprüft, ohne DATA und ohne Zustellung.

## Quellen

Server: `deploy/overlays/croessner-nauthilus/server.yaml`,
`network-policy.yaml` und `kustomization.yaml`; Testjoblabel in
`hack/imaptest-job.yaml`. Director:
`/Users/croessner/OpenCloud/Persönlich/Kubernetes/director/configmap.yaml`
und Betriebsdokument `imappoc.md`.

## Rollback

Zuerst die drei Director-Sender auf `haproxy.enabled: false` zurückstellen
und den Director-Rollout abwarten; anschließend den Serverempfänger auf den
vorherigen leeren Vertrauenswert zurückstellen. Die NetworkPolicy kann als
Zugriffsgrenze bestehen bleiben. Die Ausgangskonfiguration des Directors
liegt privat unter `/private/tmp/imap-proxy-director-before.json`.

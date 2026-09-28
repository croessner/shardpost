# JMAP goes live at jmap.roessner-net.de

Date: 2026-09-27. Status: live, healthy. Image 0.28.1, unchanged.

> **Superseded on 2026-09-28:** JMAP runs through nauthilus-director on
> `mail.roessner-net.de`, and everything this report set up for the test
> name has been removed; see
> [JMAP through nauthilus-director](2026-09-28-jmap-director-cutover.md).

## A test endpoint, not the lasting one

`jmap.roessner-net.de` exists for testing with the test accounts on the
ShardPost shard (`rbdpoc1`, `rbdpoc2`). No production client is pointed at
it, and none will be. Clients connect through `mail.roessner-net.de`,
rolled out by automx, and JMAP will follow that path once the Director
routes JMAP per account (roadmap stage 7). Until then an account on the
mailstack shard is refused (403, shard gate).

JMAP needs no host name of its own. RFC 8620 finds the server through
`/.well-known/jmap` on the name the client already has, and
`mail.roessner-net.de` resolves to the same VIPs `.233`/`.234`. The switch
to the lasting path, with the Director:

| Where | Today (test) | With the Director |
|---|---|---|
| Name clients use | `jmap.roessner-net.de` | `mail.roessner-net.de` (automx) |
| HAProxy `ft_nauthilus` | Host ACL `jmap.roessner-net.de` → `be_shardpost_jmap` (ShardPost directly) | Host `mail.roessner-net.de`, paths `/.well-known/jmap` and `/jmap/` → Director backend |
| Certificate on the LBRs | SAN `jmap.roessner-net.de` on the login certificate | SAN `mail.roessner-net.de` (or its own certificate) |
| `jmap.public_base_url` | `https://jmap.roessner-net.de` | `https://mail.roessner-net.de` |
| Nauthilus `shardpost-introspection` resource | `https://jmap.roessner-net.de/` | `https://mail.roessner-net.de/` |
| Shard gate | the only guard against foreign accounts | a safety net behind the Director's routing |
| Push subscriptions | test accounts only | clients subscribe anew once the name changes |

When the lasting path is live, the test name goes: its DNS records, the
SAN, the HAProxy ACL and backend (`jmap-route.py` backup or reverse edit)
and the cert-manager certificate `shardpost-jmap-tls`, if the Director
terminates towards ShardPost with another name.

## Path

```text
client ──https──▶ jmap.roessner-net.de (A/AAAA .233/.234, TTL 3600)
        ──▶ HAProxy lbr1/lbr2, frontend ft_nauthilus :443
            (TLS with the login.authserv.me certificate, SAN jmap.roessner-net.de;
             Host jmap.roessner-net.de → backend be_shardpost_jmap)
        ──TLS + PROXY v2──▶ kn1–kn3:30443 (Service shardpost-jmap, NodePort,
                              externalTrafficPolicy Local)
        ──▶ shardpost-server :9443 (certificate shardpost-jmap-tls, Let's Encrypt
                                    through cert-manager, reloaded on renewal)
```

## What changed

- **ShardPost overlay** (`croessner-nauthilus`):
  - `jmap.addr: ":9443"` (8443 is the admin listener)
  - `public_base_url: https://jmap.roessner-net.de`
  - shard gate `mailShard`/`shardpost`
  - push with the VAPID key `shardpost-jmap-vapid` and contact `postmaster@roessner-net.de`
  - submission through `relay.roessner-net.de:25587` (XCLIENT, STARTTLS) with `address_attributes: rnsMSRecipientAddress`
- **Trust and network:**
  - `server.proxy_protocol_trusted_cidrs` also names the LBR VLAN addresses `192.168.0.5/32` and `192.168.0.6/32`
  - the network policy admits port 9443 only from these two addresses and the operator test pods
- **New resources:** Certificate `shardpost-jmap-tls`, Service `shardpost-jmap`, Deployment `shardpost-push` (component `jmap-push`; its namespace was missing and is set now).
- **Secret created in the cluster, not in git:** `shardpost-jmap-vapid` (P-256, generated and written without being printed). Rotating it invalidates every push subscription.
- **Outside this repository:**
  - The login certificate on mgmt carries `jmap.roessner-net.de` as a SAN (the owner added it to `update-letsencrypt.sh`). It was issued at once with `rns_ssl_remaining_days=90` and distributed to lbr1/lbr2.
  - HAProxy on both LBRs got the Host ACL and the backend `be_shardpost_jmap`: validated first, backup under `/var/backups/haproxy-jmap-route-*`, soft reload. The script is in rns-server `services/haproxy/jmap-route.py`.
  - The DNS records were added through the PowerDNS API.
- **Found and fixed on mgmt:** `/root/.rns-secrets/ansible-become.yml` held only the bare value, not a YAML mapping. With it `-e @file` left Ansible without an inventory, so the nightly certificate distribution distributed nothing. The file was rewritten as a mapping without the value being read; the distribution works again.

## Acceptance

- **In the cluster** (test pod):
  - TLS verified for `jmap.roessner-net.de`, `/jmap/healthz` 200
  - session 200 with `core`, `mail`, `submission`, `webpush-vapid`
  - `Mailbox/get` with every role (inbox, drafts, sent, trash, junk, archive)
  - wrong password 401
- **Per LBR** before DNS (`--resolve`, IPv4 and IPv6): healthz 200, `login.authserv.me` unchanged 200.
- **Public, through DNS:**
  - healthz 200 with a verified certificate
  - session and `Mailbox/get` with the test account
  - the event source sends `ping`
- **Client address:** deliberate failed logins through lbr1 and through DNS are logged with the real client address (`217.70.202.49`, `2a00:12d0:4000:2b00:…`), not the LBR's. PROXY v2 and the trust list work.
- **Backends:** kn1/kn2 UP; kn3 DOWN, because no ShardPost pod runs there (externalTrafficPolicy Local), as intended.
- **Logs:** server and push worker 0 ERROR, 0 WARN. Zabbix maintenance 392 on 10478 removed; the load event 42162358 was open before it.

## Client tests

- **`hack/jmap-acceptance.py`** (Python, `jmapc` 0.3.0), run against the public name with `rbdpoc1`:
  - a draft to the account itself, submitted with `EmailSubmission` through the relay's XCLIENT listener
  - delivered to the INBOX after about 5 seconds
  - the copy moved to Sent by `onSuccessUpdateEmail`, nothing left in Drafts
  - a foreign From refused by ShardPost (`forbiddenFrom`)
  - a foreign envelope sender refused by ShardPost (`forbiddenMailFrom`) before the relay is asked; the relay's own refusal (`553`) was verified at the relay's rollout
  - every test message deleted again
- **aerc 0.22** (terminal client with JMAP; Homebrew) is set up for `rbdpoc2` in `~/Library/Preferences/aerc/accounts.conf`:
  - `jmap://` source and `outgoing = jmap://`
  - the password comes from SOPS through `source-cred-cmd`
  - It connects, lists the mailboxes, queries and fetches messages and threads, and listens for changes over the event source.
- **Not available here:**
  - Ltt.rs is an Android app, and there is no Android SDK on the operator's Mac.
  - Thunderbird 156 has no JMAP.
  - NeoMutt has no JMAP.
  - A browser client would need CORS, which the server does not send.

**Event source through HAProxy:** the frontend's `timeout client` is 1
minute. `http-request set-timeout client 30m if is_jmap` gives JMAP
requests 30 minutes; the login path keeps 1 minute. An event source
without pings stayed open beyond 80 seconds.

## Not verified yet

- Bearer (OAuth) logins with a real token: they need an interactive OIDC login with MFA.
- A Web Push subscription up to a device.

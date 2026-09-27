# JMAP goes live at jmap.roessner-net.de

Date: 2026-09-27. Status: live, healthy. Image 0.28.1, unchanged.

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

## Not verified yet

- A real JMAP client (e.g. Ltt.rs) with Basic and with Bearer.
- Sending: an approved test message, and the refusals of a foreign envelope sender (Postfix) and a foreign From (ShardPost).
- A Web Push subscription up to a device.
- The event source's lifetime through HAProxy: the frontend's `timeout client` is 1 minute. A client that asks for no ping or a ping above 60 seconds is cut; with `ping` ≤ 60 it holds.

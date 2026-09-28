#!/usr/bin/env python3
"""JMAP stage 6c acceptance against a live endpoint: shared mailboxes.

Usage: jmap-acceptance-stage6c.py   (pip install requests pyyaml)

Environment: JMAP_HOST (default mail.roessner-net.de), IMAP_HOST (default
mail.roessner-net.de, port 993), JMAP_ACCOUNTS (a SOPS file with accounts[0]
and accounts[1], login/password; default the rns-server test accounts).

Account 0 owns a fresh test mailbox with one message and grants account 1
rights on it over IMAP SETACL. Account 1 must then see a shared JMAP account
with that mailbox, the rights mapped to myRights, writes refused or allowed
by the rights, Email/copy into its own INBOX, and the account gone after the
grant is removed. Everything the test made is destroyed. Prints outcomes only.

The public endpoint accepts OAuth only; these password logins run through
kubectl port-forward with JMAP_BASE (see hack/jmapbase.py).
"""
import imaplib
import os
import subprocess
import uuid

import requests
import yaml

import jmapbase  # noqa: F401  JMAP_BASE: password logins through a port-forward

HOST = os.environ.get("JMAP_HOST", "mail.roessner-net.de")
IMAP = os.environ.get("IMAP_HOST", "mail.roessner-net.de")
SOPS = os.environ.get("JMAP_ACCOUNTS",
                      os.path.expanduser("~/src/rns-server/secrets/mail-test-accounts/shardpost.sops.yaml"))
CORE, MAIL = "urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"


class Account:
    """One authenticated JMAP account."""

    def __init__(self, login, password):
        self.login, self.password = login, password
        self.auth = (login, password)
        self.refresh()

    def refresh(self):
        self.session = requests.get(f"https://{HOST}/.well-known/jmap", auth=self.auth, timeout=30).json()
        self.id = self.session["primaryAccounts"][MAIL]

    def call(self, *calls, account=None):
        body = {"using": [CORE, MAIL],
                "methodCalls": [[n, {"accountId": account or self.id, **a}, str(i)] for i, (n, a) in enumerate(calls)]}
        r = requests.post(self.session["apiUrl"], json=body, auth=self.auth, timeout=60)
        r.raise_for_status()
        return [resp for resp in r.json()["methodResponses"]]

    def shared(self, owner):
        """The id of the non-personal account named after owner, or None."""
        for aid, acc in self.session["accounts"].items():
            if not acc.get("isPersonal") and acc.get("name", "").lower() == owner.lower():
                return aid
        return None

    def setacl(self, mailbox, who, rights):
        with imaplib.IMAP4_SSL(IMAP, 993, timeout=30) as c:
            c.login(self.login, self.password)
            if rights:
                typ, _ = c.setacl(mailbox, who, rights)
            else:
                typ, _ = c.deleteacl(mailbox, who)
            return typ


accs = yaml.safe_load(subprocess.check_output(["sops", "-d", SOPS]))["accounts"]
a, b = Account(accs[0]["login"], accs[0]["password"]), Account(accs[1]["login"], accs[1]["password"])
token = "share-acceptance-" + uuid.uuid4().hex[:8]
copied = []

# Owner: a test mailbox with one message
r = a.call(("Mailbox/set", {"create": {"m": {"name": token}}}))[0][1]
box = r["created"]["m"]["id"]
r = a.call(("Email/set", {"create": {"e": {"mailboxIds": {box: True}, "from": [{"email": a.login}],
                                           "to": [{"email": a.login}], "subject": "Shared " + token,
                                           "bodyValues": {"t": {"value": "shared test message"}},
                                           "textBody": [{"partId": "t", "type": "text/plain"}]}}}))[0][1]
print("owner mailbox and message:", bool(box), bool((r.get("created") or {}).get("e")))

try:
    print("before the grant, shared account listed:", b.shared(a.login) is not None)
    print("SETACL lr:", a.setacl(token, b.login, "lr"))
    b.refresh()
    sid = b.shared(a.login)
    acc = b.session["accounts"].get(sid, {})
    print("shared account:", bool(sid), "| isPersonal", acc.get("isPersonal"), "| isReadOnly", acc.get("isReadOnly"),
          "| capabilities", sorted(k.rsplit(":", 1)[-1] for k in acc.get("accountCapabilities", {})))

    boxes = b.call(("Mailbox/get", {"properties": ["name", "myRights", "totalEmails"]}), account=sid)[0][1]["list"]
    mine = next((m for m in boxes if m["name"] == token), None)
    print("mailboxes visible:", len(boxes), "| test mailbox:", bool(mine))
    if mine:
        rights = mine["myRights"]
        print("myRights: read", rights["mayReadItems"], "| add", rights["mayAddItems"],
              "| keywords", rights["maySetKeywords"], "| delete", rights["mayDelete"],
              "| submit", rights["maySubmit"], "| totalEmails", mine["totalEmails"])
        ids = b.call(("Email/query", {"filter": {"inMailbox": mine["id"]}}), account=sid)[0][1]["ids"]
        subj = b.call(("Email/get", {"ids": ids, "properties": ["subject"]}), account=sid)[0][1]["list"]
        print("Email/query+get:", [m["subject"] == "Shared " + token for m in subj])

        r = b.call(("Email/set", {"update": {ids[0]: {"keywords/$flagged": True}}}), account=sid)[0][1]
        print("keyword without w:", (r.get("notUpdated") or {}).get(ids[0], {}).get("type") or "NOT refused")

        inbox = next(m["id"] for m in b.call(("Mailbox/get", {"properties": ["role"]}))[0][1]["list"]
                     if m.get("role") == "inbox")
        r = b.call(("Email/copy", {"fromAccountId": sid, "accountId": b.id,
                                   "create": {"c": {"id": ids[0], "mailboxIds": {inbox: True}}}}))[0][1]
        copied = [c["id"] for c in (r.get("created") or {}).values()]
        print("Email/copy into own INBOX:", bool(copied), (r.get("notCreated") or ""))

        print("SETACL lrsw:", a.setacl(token, b.login, "lrsw"))
        r = b.call(("Email/set", {"update": {ids[0]: {"keywords/$flagged": True}}}), account=sid)[0][1]
        print("keyword with w:", ids[0] in (r.get("updated") or {}))

        r = b.call(("Email/set", {"destroy": ids}), account=sid)[0][1]
        print("destroy without t/e:", (r.get("notDestroyed") or {}).get(ids[0], {}).get("type") or "NOT refused")

    print("DELETEACL:", a.setacl(token, b.login, None))
    b.refresh()
    gone = b.call(("Mailbox/get", {"ids": None}), account=sid)[0]
    print("after revocation: listed", b.shared(a.login) is not None, "| Mailbox/get",
          gone[0] if gone[0] == "error" else "answered", gone[1].get("type", ""))
finally:
    r = a.call(("Mailbox/set", {"destroy": [box], "onDestroyRemoveEmails": True}))[0][1]
    print("cleanup owner mailbox:", box in (r.get("destroyed") or []))
    if copied:
        r = b.call(("Email/set", {"destroy": copied}))[0][1]
        print("cleanup copy:", len(r.get("destroyed") or []), "of", len(copied))

#!/usr/bin/env python3
"""JMAP stage 6c-3 acceptance against a live endpoint: push for shared accounts.

Usage: jmap-acceptance-stage6c3.py   (pip install requests pyyaml)

Environment: JMAP_HOST (default jmap.roessner-net.de), IMAP_HOST (default
mail.roessner-net.de), JMAP_ACCOUNTS (a SOPS file with accounts[0] and
accounts[1]; default the rns-server test accounts).

Account 1 holds an event source open. Account 0 shares a fresh mailbox with
it (IMAP SETACL lr): the new shared account must be pushed. A message stored
in the shared mailbox must be pushed for that account; a message stored in a
mailbox account 1 cannot see must not move the shared account's state or be
pushed for it. After the revocation the account is pushed as "revoked".
Everything the test made is destroyed. Prints outcomes only.
"""
import imaplib
import json
import os
import subprocess
import threading
import time
import uuid

import requests
import yaml

HOST = os.environ.get("JMAP_HOST", "jmap.roessner-net.de")
IMAP = os.environ.get("IMAP_HOST", "mail.roessner-net.de")
SOPS = os.environ.get("JMAP_ACCOUNTS",
                      os.path.expanduser("~/src/rns-server/secrets/mail-test-accounts/shardpost.sops.yaml"))
CORE, MAIL = "urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"


class Account:
    def __init__(self, login, password):
        self.login, self.password, self.auth = login, password, (login, password)
        self.refresh()

    def refresh(self):
        self.session = requests.get(f"https://{HOST}/.well-known/jmap", auth=self.auth, timeout=30).json()
        self.id = self.session["primaryAccounts"][MAIL]

    def call(self, *calls, account=None):
        body = {"using": [CORE, MAIL],
                "methodCalls": [[n, {"accountId": account or self.id, **a}, str(i)] for i, (n, a) in enumerate(calls)]}
        r = requests.post(self.session["apiUrl"], json=body, auth=self.auth, timeout=60)
        r.raise_for_status()
        return r.json()["methodResponses"]

    def shared(self, owner):
        for aid, acc in self.session["accounts"].items():
            if not acc.get("isPersonal") and acc.get("name", "").lower() == owner.lower():
                return aid
        return None

    def imap(self, *cmd):
        with imaplib.IMAP4_SSL(IMAP, 993, timeout=30) as c:
            c.login(self.login, self.password)
            return getattr(c, cmd[0])(*cmd[1:])[0]

    def store(self, box, subject):
        r = self.call(("Email/set", {"create": {"e": {"mailboxIds": {box: True}, "from": [{"email": self.login}],
                                                      "to": [{"email": self.login}], "subject": subject,
                                                      "bodyValues": {"t": {"value": "push test"}},
                                                      "textBody": [{"partId": "t", "type": "text/plain"}]}}}))[0][1]
        return r["created"]["e"]["id"]


class Events:
    """An event source in a background thread, collecting StateChange objects."""

    def __init__(self, acc):
        url = acc.session["eventSourceUrl"].replace("{types}", "*").replace("{closeafter}", "no").replace("{ping}", "0")
        self.resp = requests.get(url, auth=acc.auth, stream=True, timeout=(30, None),
                                 headers={"Accept": "text/event-stream"})
        self.changes, self.lock = [], threading.Lock()
        threading.Thread(target=self.run, daemon=True).start()

    def run(self):
        data = []
        try:
            for line in self.resp.iter_lines(decode_unicode=True):
                if line is None:
                    continue
                if line.startswith("data:"):
                    data.append(line[5:].strip())
                elif line == "" and data:
                    try:
                        obj = json.loads("\n".join(data))
                        with self.lock:
                            self.changes.append((time.time(), obj.get("changed", {})))
                    except ValueError:
                        pass
                    data = []
        except Exception:
            pass

    def since(self, t0):
        with self.lock:
            return [c for t, c in self.changes if t >= t0]

    def wait(self, t0, pred, timeout):
        deadline = time.time() + timeout
        while time.time() < deadline:
            hit = [c for c in self.since(t0) if pred(c)]
            if hit:
                return hit
            time.sleep(1)
        return []


accs = yaml.safe_load(subprocess.check_output(["sops", "-d", SOPS]))["accounts"]
a, b = Account(accs[0]["login"], accs[0]["password"]), Account(accs[1]["login"], accs[1]["password"])
token = "push-6c3-" + uuid.uuid4().hex[:8]
boxes = {m.get("role"): m["id"] for m in a.call(("Mailbox/get", {"properties": ["role"]}))[0][1]["list"]}
box = a.call(("Mailbox/set", {"create": {"m": {"name": token}}}))[0][1]["created"]["m"]["id"]
ev = Events(b)
time.sleep(2)
made = []

try:
    t0 = time.time()
    print("SETACL lr:", a.imap("setacl", token, b.login, "lr"))
    b.refresh()
    sid = b.shared(a.login)
    hit = ev.wait(t0, lambda c: sid in c, 90)
    print("new shared account pushed:", bool(hit), f"after ~{int(time.time() - t0)} s",
          "| types", sorted(hit[0][sid]) if hit else None)

    time.sleep(3)
    before = b.call(("Email/get", {"ids": []}), account=sid)[0][1]["state"]
    t0 = time.time()
    made.append(a.store(boxes["drafts"], token + " hidden"))
    time.sleep(40)  # longer than the event source's own poll
    hidden = [c for c in ev.since(t0) if sid in c]
    after = b.call(("Email/get", {"ids": []}), account=sid)[0][1]["state"]
    print("change in a hidden mailbox: pushed for the shared account", bool(hidden), "| state unchanged", before == after)

    t0 = time.time()
    made.append(a.store(box, token + " visible"))
    hit = ev.wait(t0, lambda c: "Email" in c.get(sid, {}), 60)
    now = b.call(("Email/get", {"ids": []}), account=sid)[0][1]["state"]
    print("change in the shared mailbox pushed:", bool(hit), f"after ~{int(time.time() - t0)} s",
          "| pushed state = /get state", bool(hit) and hit[-1][sid]["Email"] == now)
    ch = b.call(("Email/changes", {"sinceState": after}), account=sid)[0][1]
    print("Email/changes: created", len(ch.get("created", [])), "| newState = /get state", ch.get("newState") == now)

    t0 = time.time()
    print("DELETEACL:", a.imap("deleteacl", token, b.login))
    hit = ev.wait(t0, lambda c: sid in c and "revoked" in c[sid].values(), 90)
    print("revocation pushed as revoked:", bool(hit), f"after ~{int(time.time() - t0)} s")
finally:
    ev.resp.close()
    r = a.call(("Mailbox/set", {"destroy": [box], "onDestroyRemoveEmails": True}),
               ("Email/set", {"destroy": made[:1]}))
    print("cleanup: mailbox", box in (r[0][1].get("destroyed") or []), "| hidden message",
          len(r[1][1].get("destroyed") or []), "of", len(made[:1]))

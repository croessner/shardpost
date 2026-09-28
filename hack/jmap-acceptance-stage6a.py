#!/usr/bin/env python3
"""JMAP stage 6a acceptance against a live endpoint: Quota and VacationResponse.

Usage: jmap-acceptance-stage6a.py   (pip install requests pyyaml)

Environment: JMAP_HOST (default mail.roessner-net.de), JMAP_ACCOUNTS (a SOPS
file with accounts[0] and accounts[1], login/password; default the rns-server
test accounts). Account 1 gets a vacation, account 0 writes to it and must
receive exactly one automatic reply; the vacation is switched off and every
test message deleted afterwards. Prints outcomes only.
"""
import os
import subprocess
import time
import uuid

import requests
import yaml

HOST = os.environ.get("JMAP_HOST", "mail.roessner-net.de")
SOPS = os.environ.get("JMAP_ACCOUNTS",
                      os.path.expanduser("~/src/rns-server/secrets/mail-test-accounts/shardpost.sops.yaml"))
CORE, MAIL, SUB = "urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", "urn:ietf:params:jmap:submission"
QUOTA, VAC = "urn:ietf:params:jmap:quota", "urn:ietf:params:jmap:vacationresponse"


class Account:
    """One authenticated JMAP account."""

    def __init__(self, login, password):
        self.auth = (login, password)
        self.session = requests.get(f"https://{HOST}/.well-known/jmap", auth=self.auth, timeout=30).json()
        self.id = self.session["primaryAccounts"][MAIL]
        self.login = login

    def call(self, using, *calls):
        body = {"using": [CORE] + using, "methodCalls": [[n, {"accountId": self.id, **a}, str(i)]
                                                          for i, (n, a) in enumerate(calls)]}
        r = requests.post(self.session["apiUrl"], json=body, auth=self.auth, timeout=60)
        r.raise_for_status()
        return [resp[1] for resp in r.json()["methodResponses"]]

    def boxes(self):
        lst = self.call([MAIL], ("Mailbox/get", {"properties": ["role"]}))[0]["list"]
        return {m["role"]: m["id"] for m in lst if m.get("role")}

    def find(self, mailbox, needle):
        return self.call([MAIL], ("Email/query", {"filter": {"inMailbox": mailbox, "text": needle}}))[0]["ids"]


accs = yaml.safe_load(subprocess.check_output(["sops", "-d", SOPS]))["accounts"]
a, b = Account(accs[0]["login"], accs[0]["password"]), Account(accs[1]["login"], accs[1]["password"])
print("capabilities:", sorted(k.rsplit(":", 1)[-1] for k in a.session["capabilities"]))

# Quota
q = a.call([QUOTA], ("Quota/get", {}))[0]
print("Quota/get:", [(x["id"], x["resourceType"], x["used"], x["hardLimit"]) for x in q["list"]], "state", q["state"][:12] + "…")
ch = a.call([QUOTA], ("Quota/changes", {"sinceState": q["state"]}))[0]
print("Quota/changes from the same state:", ch.get("updated"), ch.get("created"), ch.get("destroyed"))

# Vacation on account b
token = "vacation-acceptance-" + uuid.uuid4().hex[:8]
res = b.call([VAC], ("VacationResponse/set", {"update": {"singleton": {
    "isEnabled": True, "subject": "Abwesend " + token, "textBody": "Automatic test reply " + token,
    "fromDate": None, "toDate": None}}}))[0]
print("VacationResponse/set:", "updated" if res.get("updated") else res.get("notUpdated"))
print("VacationResponse/get isEnabled:", b.call([VAC], ("VacationResponse/get", {}))[0]["list"][0]["isEnabled"])

# a writes to b
ab, bb = a.boxes(), b.boxes()
ident = a.call([SUB], ("Identity/get", {}))[0]["list"]
me = next(i for i in ident if i["email"].lower() == a.login.lower())
res = a.call([MAIL, SUB],
             ("Email/set", {"create": {"d": {"mailboxIds": {ab["drafts"]: True}, "keywords": {"$draft": True},
                                              "from": [{"email": me["email"]}], "to": [{"email": b.login}],
                                              "subject": "Hello " + token,
                                              "bodyValues": {"t": {"value": "please answer automatically"}},
                                              "textBody": [{"partId": "t", "type": "text/plain"}]}}}),
             ("EmailSubmission/set", {"create": {"s": {"identityId": me["id"], "emailId": "#d"}},
                                      "onSuccessUpdateEmail": {"#s": {f"mailboxIds/{ab['drafts']}": None,
                                                                      f"mailboxIds/{ab['sent']}": True,
                                                                      "keywords/$draft": None}}}))
print("sent to the vacation account:", bool(res[1].get("created")))

deadline, reply = time.time() + 120, []
while time.time() < deadline and not reply:
    reply = a.find(ab["inbox"], "Abwesend " + token)
    if not reply:
        time.sleep(5)
print("automatic reply in the sender's INBOX:", len(reply), f"after ~{int(120 - (deadline - time.time()))} s")
if reply:
    m = a.call([MAIL], ("Email/get", {"ids": reply[:1], "properties": ["subject", "from", "headers"]}))[0]["list"][0]
    hdr = {h["name"].lower(): h["value"].strip() for h in m["headers"]}
    print("reply from the vacation account:", m["from"][0]["email"].lower() == b.login.lower(),
          "| Auto-Submitted:", hdr.get("auto-submitted"))

# Off again, and clean up
b.call([VAC], ("VacationResponse/set", {"update": {"singleton": {"isEnabled": False}}}))
print("VacationResponse off:", b.call([VAC], ("VacationResponse/get", {}))[0]["list"][0]["isEnabled"] is False)
for acc, boxes in ((a, ab), (b, bb)):
    ids = []
    for role in ("inbox", "sent", "drafts"):
        ids += acc.find(boxes[role], token)
    if ids:
        d = acc.call([MAIL], ("Email/set", {"destroy": ids}))[0]
        print(f"cleanup {acc.login.split('@')[0]}: destroyed", len(d.get("destroyed") or []), "of", len(ids))

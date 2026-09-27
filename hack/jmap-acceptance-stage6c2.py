#!/usr/bin/env python3
"""JMAP stage 6c-2 acceptance against a live endpoint: principals, shareWith, notifications.

Usage: jmap-acceptance-stage6c2.py   (pip install requests pyyaml)

Environment: JMAP_HOST (default jmap.roessner-net.de), JMAP_ACCOUNTS (a SOPS
file with accounts[0] and accounts[1], login/password; default the rns-server
test accounts).

Account 0 finds account 1 as a principal by its exact address, creates a
mailbox shared with it through shareWith, widens and removes the share.
Account 1 must see the shared account appear and go, and one notification
for the grant, coalesced with the widening; grant and revocation cancel out. The mailbox and the notifications the test caused are destroyed.
Prints outcomes only.
"""
import os
import subprocess
import uuid

import requests
import yaml

HOST = os.environ.get("JMAP_HOST", "jmap.roessner-net.de")
SOPS = os.environ.get("JMAP_ACCOUNTS",
                      os.path.expanduser("~/src/rns-server/secrets/mail-test-accounts/shardpost.sops.yaml"))
CORE, MAIL = "urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"
PRINC, SHARE = "urn:ietf:params:jmap:principals", "urn:ietf:params:jmap:mail:share"
USING = [CORE, MAIL, PRINC, SHARE]


class Account:
    """One authenticated JMAP account."""

    def __init__(self, login, password):
        self.login, self.auth = login, (login, password)
        self.refresh()

    def refresh(self):
        self.session = requests.get(f"https://{HOST}/.well-known/jmap", auth=self.auth, timeout=30).json()
        self.id = self.session["primaryAccounts"][MAIL]

    def call(self, *calls, account=None):
        body = {"using": USING,
                "methodCalls": [[n, {"accountId": account or self.id, **a}, str(i)] for i, (n, a) in enumerate(calls)]}
        r = requests.post(self.session["apiUrl"], json=body, auth=self.auth, timeout=60)
        r.raise_for_status()
        return r.json()["methodResponses"]

    def shared(self, owner):
        for aid, acc in self.session["accounts"].items():
            if not acc.get("isPersonal") and acc.get("name", "").lower() == owner.lower():
                return aid
        return None

    def notices(self, token):
        lst = self.call(("ShareNotification/get", {"ids": None}))[0][1].get("list", [])
        return [n for n in lst if token in (n.get("name") or "")]


accs = yaml.safe_load(subprocess.check_output(["sops", "-d", SOPS]))["accounts"]
a, b = Account(accs[0]["login"], accs[0]["password"]), Account(accs[1]["login"], accs[1]["password"])
token = "share-6c2-" + uuid.uuid4().hex[:8]
print("capabilities:", sorted(k.rsplit(":", 1)[-1] for k in a.session["capabilities"] if "princip" in k or "share" in k))
cap = a.session["accounts"][a.id]["accountCapabilities"].get(PRINC, {})
print("currentUserPrincipalId is the login:", cap.get("currentUserPrincipalId") == a.login.lower())

# Principals: exact lookup finds account 1; a nonexistent address gives nothing
q = a.call(("Principal/query", {"filter": {"email": b.login.lower()}}))[0][1]
print("Principal/query exact:", q.get("ids") == [b.login.lower()])
q = a.call(("Principal/query", {"filter": {"email": f"nobody-{token}@exampleserver.de"}}))[0][1]
print("Principal/query unknown:", q.get("ids"))
g = a.call(("Principal/get", {"ids": [b.login.lower()]}))[0][1]["list"]
print("Principal/get:", [(p["type"], p["email"] == b.login.lower()) for p in g])

READ = {"mayReadItems": True, "mayAddItems": False, "mayRemoveItems": False, "maySetSeen": False,
        "maySetKeywords": False, "mayCreateChild": False, "mayRename": False, "mayDelete": False,
        "mayShare": False, "maySubmit": False}
before = {n["id"] for n in b.notices(token)}
r = a.call(("Mailbox/set", {"create": {"m": {"name": token, "shareWith": {b.login.lower(): READ}}}}))[0][1]
box = (r.get("created") or {}).get("m", {}).get("id")
print("Mailbox/set create with shareWith:", bool(box), r.get("notCreated") or "")

try:
    got = a.call(("Mailbox/get", {"ids": [box], "properties": ["shareWith", "myRights"]}))[0][1]["list"][0]
    sw = (got.get("shareWith") or {}).get(b.login.lower(), {})
    print("owner sees shareWith read-only:", sw.get("mayReadItems") is True and sw.get("maySetKeywords") is False,
          "| owner mayShare", got["myRights"].get("mayShare"))

    b.refresh()
    sid = b.shared(a.login)
    print("grantee: shared account", bool(sid))
    seen = b.call(("Mailbox/get", {"properties": ["name", "myRights", "shareWith"]}), account=sid)[0][1]["list"]
    m = next((x for x in seen if x["name"] == token), {})
    print("grantee: mailbox visible", bool(m), "| mayShare", m.get("myRights", {}).get("mayShare"),
          "| shareWith hidden", m.get("shareWith") is None)
    r = b.call(("Mailbox/set", {"update": {box: {f"shareWith/{b.login.lower()}/maySetKeywords": True}}}), account=sid)[0][1]
    print("grantee cannot widen its own rights:", (r.get("notUpdated") or {}).get(box, {}).get("type"))

    r = a.call(("Mailbox/set", {"update": {box: {f"shareWith/{b.login.lower()}/maySetKeywords": True}}}))[0][1]
    after = (r.get("updated") or {}).get(box) or {}
    print("owner widens (patch one right):", box in (r.get("updated") or {}),
          "| answered shareWith", (after.get("shareWith") or {}).get(b.login.lower(), {}).get("maySetKeywords"))

    ns = [n for n in b.notices(token) if n["id"] not in before]
    print("grantee notifications:", len(ns), "| newest newRights.maySetKeywords",
          (max(ns, key=lambda n: n["created"])["newRights"] or {}).get("maySetKeywords") if ns else None,
          "| changedBy", ns[0]["changedBy"].get("email") == a.login.lower() if ns else None)

    r = a.call(("Mailbox/set", {"update": {box: {f"shareWith/{b.login.lower()}": None}}}))[0][1]
    print("owner removes the share:", box in (r.get("updated") or {}))
    b.refresh()
    ns = [n for n in b.notices(token) if n["id"] not in before]
    # grant, widen and revoke of one mailbox coalesce back to where they started: no notice (RFC 9670 6.3)
    print("after revocation: shared account", b.shared(a.login) is not None,
          "| notices left for the mailbox", len(ns))
finally:
    if box:
        r = a.call(("Mailbox/set", {"destroy": [box], "onDestroyRemoveEmails": True}))[0][1]
        print("cleanup mailbox:", box in (r.get("destroyed") or []))
    ids = [n["id"] for n in b.notices(token)]
    if ids:
        r = b.call(("ShareNotification/set", {"destroy": ids}))[0][1]
        print("cleanup notifications:", len(r.get("destroyed") or []), "of", len(ids))

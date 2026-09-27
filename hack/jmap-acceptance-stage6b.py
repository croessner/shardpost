#!/usr/bin/env python3
"""JMAP stage 6b acceptance against a live endpoint: Blob and SieveScript.

Usage: jmap-acceptance-stage6b.py [account-index]   (pip install requests pyyaml)

Environment: JMAP_HOST (default jmap.roessner-net.de), JMAP_ACCOUNTS (SOPS file
with accounts[].login/password; default the rns-server test accounts).
Leaves the account's active Sieve script alone: the test script is created
inactive and destroyed again. Prints outcomes only.
"""
import base64
import hashlib
import os
import subprocess
import sys
import uuid

import requests
import yaml

HOST = os.environ.get("JMAP_HOST", "jmap.roessner-net.de")
SOPS = os.environ.get("JMAP_ACCOUNTS",
                      os.path.expanduser("~/src/rns-server/secrets/mail-test-accounts/shardpost.sops.yaml"))
CORE, MAIL = "urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"
BLOB, SIEVE = "urn:ietf:params:jmap:blob", "urn:ietf:params:jmap:sieve"

acc = yaml.safe_load(subprocess.check_output(["sops", "-d", SOPS]))["accounts"][int(sys.argv[1]) if len(sys.argv) > 1 else 0]
auth = (acc["login"], acc["password"])
session = requests.get(f"https://{HOST}/.well-known/jmap", auth=auth, timeout=30).json()
aid = session["primaryAccounts"][MAIL]


def call(using, *calls):
    body = {"using": [CORE] + using,
            "methodCalls": [[n, {"accountId": aid, **a}, str(i)] for i, (n, a) in enumerate(calls)]}
    r = requests.post(session["apiUrl"], json=body, auth=auth, timeout=60)
    r.raise_for_status()
    return r.json()["methodResponses"]


print("capabilities:", sorted(k.rsplit(":", 1)[-1] for k in session["capabilities"]))
cap = session["accounts"][aid]["accountCapabilities"][SIEVE]
print("sieve: maxSizeScriptName", cap.get("maxSizeScriptName"), "| maxNumberRedirects", cap.get("maxNumberRedirects"),
      "| extensions", len(cap.get("sieveExtensions") or []))

# Blob: text + base64 sources, then a second blob built from a range of the first
text, raw = "Hello JMAP blob ", b"\x00\x01binary\xff"
r = call([BLOB], ("Blob/upload", {"create": {
    "a": {"data": [{"data:asText": text}, {"data:asBase64": base64.b64encode(raw).decode()}], "type": "application/octet-stream"},
    "b": {"data": [{"blobId": "#a", "offset": 6, "length": 4}]}}}))[0][1]
a, b = r["created"]["a"], r["created"]["b"]
whole = text.encode() + raw
print("Blob/upload: sizes", a["size"], b["size"], "expected", len(whole), 4)
g = call([BLOB], ("Blob/get", {"ids": [a["id"], b["id"]], "properties": ["data:asBase64", "digest:sha-256", "size"]}))[0][1]["list"]
ga, gb = {x["id"]: x for x in g}[a["id"]], {x["id"]: x for x in g}[b["id"]]
print("Blob/get: content round-trips", base64.b64decode(ga["data:asBase64"]) == whole,
      "| range", base64.b64decode(gb["data:asBase64"]) == whole[6:10],
      "| sha-256", ga["digest:sha-256"] == base64.b64encode(hashlib.sha256(whole).digest()).decode())

# Blob/lookup for a real message
inbox = next(m["id"] for m in call([MAIL], ("Mailbox/get", {"properties": ["role"]}))[0][1]["list"] if m.get("role") == "inbox")
ids = call([MAIL], ("Email/query", {"filter": {"inMailbox": inbox}, "limit": 1}))[0][1]["ids"]
if ids:
    blob = call([MAIL], ("Email/get", {"ids": ids, "properties": ["blobId"]}))[0][1]["list"][0]["blobId"]
    lk = call([BLOB, MAIL], ("Blob/lookup", {"typeNames": ["Email", "Mailbox"], "ids": [blob]}))[0][1]["list"][0]["matchedIds"]
    print("Blob/lookup: message found", ids[0] in lk.get("Email", []), "| in INBOX", inbox in lk.get("Mailbox", []))

# SieveScript: upload content, create inactive, invalid script, rename keeps id, reserved name, destroy
before = call([SIEVE], ("SieveScript/get", {}))[0][1]["list"]
active_before = [s["name"] for s in before if s["isActive"]]
name = "acceptance-" + uuid.uuid4().hex[:6]
up = call([BLOB], ("Blob/upload", {"create": {
    "ok": {"data": [{"data:asText": 'require "fileinto";\r\nif header :contains "subject" "never-matches" { fileinto "INBOX"; }\r\n'}]},
    "bad": {"data": [{"data:asText": "this is not sieve;"}]}}}))[0][1]["created"]
r = call([SIEVE], ("SieveScript/set", {"create": {"s": {"name": name, "blobId": up["ok"]["id"]},
                                                   "x": {"name": name + "-bad", "blobId": up["bad"]["id"]}}}))[0][1]
sid = (r.get("created") or {}).get("s", {}).get("id")
print("SieveScript/set create:", bool(sid), "| invalid script:", (r.get("notCreated") or {}).get("x", {}).get("type"))
v = call([SIEVE], ("SieveScript/validate", {"blobId": up["bad"]["id"]}))[0][1]
print("SieveScript/validate invalid:", (v.get("error") or {}).get("type"))
r = call([SIEVE], ("SieveScript/set", {"update": {sid: {"name": name + "-renamed"}}}))[0][1]
now = {s["id"]: s for s in call([SIEVE], ("SieveScript/get", {"ids": [sid]}))[0][1]["list"]}
print("rename keeps id:", sid in now and now[sid]["name"] == name + "-renamed", "| inactive:", now[sid]["isActive"] is False)
res = call([SIEVE], ("SieveScript/set", {"create": {"v": {"name": "jmap-vacation", "blobId": up["ok"]["id"]}}}))[0][1]
print("reserved name refused:", (res.get("notCreated") or {}).get("v", {}).get("type"))
r = call([SIEVE], ("SieveScript/set", {"destroy": [sid]}))[0][1]
after = call([SIEVE], ("SieveScript/get", {}))[0][1]["list"]
print("destroyed:", sid in (r.get("destroyed") or []),
      "| active script unchanged:", [s["name"] for s in after if s["isActive"]] == active_before)

#!/usr/bin/env python3
"""JMAP acceptance against a live endpoint, with jmapc.

Usage: jmap-acceptance.py [account-index]   (pip install jmapc==0.3.0 pyyaml)

Environment: JMAP_HOST (default mail.roessner-net.de), JMAP_ACCOUNTS (a
SOPS file with accounts[].login/password, default the rns-server test
accounts). Prints outcomes only: no credentials, no message content beyond
the test subject token. Sends exactly one message, from the test account to
itself, checks delivery, the Sent copy, a foreign From and a foreign
envelope sender, and deletes every message it made.
"""
import os
import subprocess
import sys
import time
import uuid

import yaml
from jmapc import Client, EmailQueryFilterCondition, Ref
from jmapc.methods import (EmailGet, EmailQuery, EmailSet, EmailSubmissionSet,
                           IdentityGet, MailboxGet)
from jmapc.models import (Email, EmailAddress, EmailBodyPart, EmailBodyValue,
                          EmailSubmission, Envelope, Address)

HOST = os.environ.get("JMAP_HOST", "mail.roessner-net.de")
SOPS = os.environ.get("JMAP_ACCOUNTS", os.path.expanduser("~/src/rns-server/secrets/mail-test-accounts/shardpost.sops.yaml"))
index = int(sys.argv[1]) if len(sys.argv) > 1 else 0

acc = yaml.safe_load(subprocess.check_output(["sops", "-d", SOPS]))["accounts"][index]
client = Client.create_with_password(HOST, acc["login"], acc["password"])
print("session: api", client.jmap_session.api_url, "| account", client.account_id[:8] + "…")

ident = client.request(IdentityGet(), single_response=True).data
print("identities:", len(ident), "| login address among them:",
      any(i.email.lower() == acc["login"].lower() for i in ident))
me = next(i for i in ident if i.email.lower() == acc["login"].lower())

boxes = {m.role: m.id for m in client.request(MailboxGet(ids=None), single_response=True).data if m.role}
print("mailbox roles:", sorted(boxes))


def draft(ref, subject, sender):
    """A plain-text draft to the account itself."""
    return EmailSet(create={ref: Email(
        mailbox_ids={boxes["drafts"]: True}, keywords={"$draft": True},
        mail_from=[EmailAddress(email=sender)], to=[EmailAddress(email=me.email)],
        subject=subject,
        text_body=[EmailBodyPart(part_id="t", type="text/plain")],
        body_values={"t": EmailBodyValue(value="JMAP acceptance message, may be deleted.\r\n")})})


def created_ids(resp):
    return [c.id for c in (resp.created or {}).values()]


cleanup = []

# 1. The positive path: draft, submit, move to Sent, wait for delivery.
token = "jmap-acceptance-" + uuid.uuid4().hex[:10]
res = client.request([
    draft("d1", token, me.email),
    EmailSubmissionSet(create={"s1": EmailSubmission(identity_id=me.id, email_id="#d1")},
                       on_success_update_email={"#s1": {f"mailboxIds/{boxes['drafts']}": None,
                                                        f"mailboxIds/{boxes['sent']}": True,
                                                        "keywords/$draft": None}}),
])
email_set, sub_set = res[0].response, res[1].response
cleanup += created_ids(email_set)
print("submission:", "created" if sub_set.created else f"refused {sub_set.not_created}")

deadline, delivered = time.time() + 90, []
while time.time() < deadline and not delivered:
    q = client.request(EmailQuery(filter=EmailQueryFilterCondition(in_mailbox=boxes["inbox"], header=["Subject", token])),
                       single_response=True)
    delivered = q.ids
    if not delivered:
        time.sleep(5)
print("delivered to INBOX:", bool(delivered), f"after ~{int(90 - (deadline - time.time()))} s")
cleanup += list(delivered)

sent = client.request(EmailQuery(filter=EmailQueryFilterCondition(in_mailbox=boxes["sent"], header=["Subject", token])),
                      single_response=True).ids
print("copy in Sent (onSuccessUpdateEmail):", bool(sent))

# 2. A From the account may not use: ShardPost refuses before the relay.
res = client.request([
    draft("d2", token + "-foreign-from", "postmaster@roessner-net.de"),
    EmailSubmissionSet(create={"s2": EmailSubmission(identity_id=me.id, email_id="#d2")}),
])
cleanup += created_ids(res[0].response)
nc = res[1].response.not_created or {}
print("foreign From:", {k: v.type for k, v in nc.items()} or "NOT refused")

# 3. An envelope sender the account may not use.
res = client.request([
    draft("d3", token + "-foreign-envelope", me.email),
    EmailSubmissionSet(create={"s3": EmailSubmission(
        identity_id=me.id, email_id="#d3",
        envelope=Envelope(mail_from=Address(email="postmaster@roessner-net.de"),
                          rcpt_to=[Address(email=me.email)]))}),
])
cleanup += created_ids(res[0].response)
nc = res[1].response.not_created or {}
print("foreign envelope sender:", {k: v.type for k, v in nc.items()} or "NOT refused")

# Cleanup: every message the test made.
if cleanup:
    d = client.request(EmailSet(destroy=cleanup), single_response=True)
    print("cleanup: destroyed", len(d.destroyed or []), "of", len(cleanup))

"""Send the acceptance scripts' JMAP traffic to a port-forward instead of the public name.

The public endpoint (nauthilus-director on mail.roessner-net.de) accepts OAuth
Bearer tokens only. The scripts log in with passwords, which ShardPost itself
still accepts; its JMAP port is reachable only from the director and through
kubectl port-forward:

    kubectl -n shardpost port-forward svc/shardpost-jmap 19443:9443
    JMAP_BASE=https://127.0.0.1:19443 hack/jmap-acceptance.py

With JMAP_BASE set, every request to https://<JMAP_HOST> — the session URL
and the URLs the session resource names — goes to JMAP_BASE instead. The
certificate is issued by the internal CA for the service name, so TLS
verification is off for this local path only. Without JMAP_BASE nothing changes.
"""
import os

import requests
import urllib3

PUBLIC = "https://" + os.environ.get("JMAP_HOST", "mail.roessner-net.de")
BASE = os.environ.get("JMAP_BASE", "").rstrip("/")

if BASE:
    urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)
    _request = requests.Session.request

    def _redirected(self, method, url, *args, **kwargs):
        if isinstance(url, str) and url.startswith(PUBLIC):
            url = BASE + url[len(PUBLIC):]
            kwargs["verify"] = False
        return _request(self, method, url, *args, **kwargs)

    requests.Session.request = _redirected

# JMAP by example: sending and receiving

A real exchange recorded on 2026-09-28 against the production endpoint
`https://mail.roessner-net.de` (HAProxy → nauthilus-director → ShardPost
0.33.0) between the test accounts `rbdpoc1@exampleserver.de` and
`rbdpoc2@exampleserver.de`. The bodies are exactly what went over the
wire, pretty-printed; the message itself was deleted afterwards. The
German message text is the literal test data. For what each method
supports see [JMAP](jmap.md).

Every call is one `POST /jmap/api/` with HTTP authentication. The
recording used Basic; since the same day the public endpoint takes only
`Authorization: Bearer` (OAuth), and password logins run through a
port-forward to ShardPost (`hack/jmapbase.py`). The body names the capabilities it uses in
`using` and carries a list of method calls, each a triple of method
name, arguments and a call id that the matching response repeats. The
account id is the account name, the mailbox and identity ids come from
`Mailbox/get` and `Identity/get` (not shown).

## Sending

One request with two calls:

1. `Email/set` creates the message in Drafts. `"draft"` is a creation id:
   a name for an object whose id the server has not assigned yet.
2. `EmailSubmission/set` sends it. `"emailId": "#draft"` refers to the
   message the first call created in the same request. ShardPost checks
   that the identity may use the From and envelope addresses and hands the
   message to the relay (see [Sending](jmap.md#sending)).
   `onSuccessUpdateEmail` applies once the submission succeeded: out of
   Drafts, into Sent, without `$draft`.

```json
{
  "using": [
    "urn:ietf:params:jmap:core",
    "urn:ietf:params:jmap:mail",
    "urn:ietf:params:jmap:submission"
  ],
  "methodCalls": [
    [
      "Email/set",
      {
        "accountId": "rbdpoc1@exampleserver.de",
        "create": {
          "draft": {
            "mailboxIds": {
              "43e21b2db01414c5d7794f57da6b6aba": true
            },
            "keywords": {
              "$draft": true,
              "$seen": true
            },
            "from": [
              {
                "name": "rbdpoc1",
                "email": "rbdpoc1@exampleserver.de"
              }
            ],
            "to": [
              {
                "email": "rbdpoc2@exampleserver.de"
              }
            ],
            "subject": "JMAP-Demo 891ecb",
            "bodyValues": {
              "body": {
                "value": "Hallo rbdpoc2,\r\ndiese Nachricht wurde per JMAP verschickt.\r\n"
              }
            },
            "textBody": [
              {
                "partId": "body",
                "type": "text/plain"
              }
            ]
          }
        }
      },
      "a"
    ],
    [
      "EmailSubmission/set",
      {
        "accountId": "rbdpoc1@exampleserver.de",
        "create": {
          "send": {
            "identityId": "I8ec71376cc368ff4",
            "emailId": "#draft"
          }
        },
        "onSuccessUpdateEmail": {
          "#send": {
            "mailboxIds/43e21b2db01414c5d7794f57da6b6aba": null,
            "mailboxIds/42dcea153f7c65569a9f2112a1ac7060": true,
            "keywords/$draft": null
          }
        }
      },
      "b"
    ]
  ]
}
```

The response answers every call, plus a third one: the implicit
`Email/set` that `onSuccessUpdateEmail` causes, under the call id of the
submission. `createdIds` maps the creation ids to the real ids,
`deliveryStatus` carries the relay's SMTP reply, and `oldState` and
`newState` are what a client later passes to `/changes` to fetch only
what moved.

```json
{
  "createdIds": {
    "draft": "7b6ca1177794547c8239ec86c69fae8b",
    "send": "S3bc212be7e67badf6e5de8b9"
  },
  "sessionState": "16f9c1963abb09e5",
  "methodResponses": [
    [
      "Email/set",
      {
        "created": {
          "draft": {
            "blobId": "7b6ca1177794547c8239ec86c69fae8b",
            "id": "7b6ca1177794547c8239ec86c69fae8b",
            "size": 354,
            "threadId": "Tefc256df8ef4ef70"
          }
        },
        "updated": {},
        "notCreated": {},
        "notUpdated": {},
        "notDestroyed": {},
        "accountId": "rbdpoc1@exampleserver.de",
        "oldState": "196",
        "newState": "197",
        "destroyed": []
      },
      "a"
    ],
    [
      "EmailSubmission/set",
      {
        "created": {
          "send": {
            "deliveryStatus": {
              "rbdpoc2@exampleserver.de": {
                "smtpReply": "250 2.0.0 Ok: queued as 4htX8L0gltz1pk",
                "delivered": "queued",
                "displayed": "unknown"
              }
            },
            "dsnBlobIds": [],
            "envelope": {
              "mailFrom": {
                "parameters": null,
                "email": "rbdpoc1@exampleserver.de"
              },
              "rcptTo": [
                {
                  "parameters": null,
                  "email": "rbdpoc2@exampleserver.de"
                }
              ]
            },
            "id": "S3bc212be7e67badf6e5de8b9",
            "mdnBlobIds": [],
            "sendAt": "2026-09-28T06:53:38Z",
            "threadId": "Tefc256df8ef4ef70",
            "undoStatus": "final"
          }
        },
        "updated": {},
        "notCreated": {},
        "notUpdated": {},
        "notDestroyed": {},
        "accountId": "rbdpoc1@exampleserver.de",
        "oldState": "14",
        "newState": "15",
        "destroyed": []
      },
      "b"
    ],
    [
      "Email/set",
      {
        "created": {},
        "updated": {
          "7b6ca1177794547c8239ec86c69fae8b": null
        },
        "notCreated": {},
        "notUpdated": {},
        "notDestroyed": {},
        "accountId": "rbdpoc1@exampleserver.de",
        "oldState": "197",
        "newState": "200",
        "destroyed": []
      },
      "b"
    ]
  ]
}
```

## Receiving

Again one request with two calls: `Email/query` finds the newest message
in the INBOX, and `Email/get` fetches it through a back-reference
(`"#ids"` with `resultOf`, `name` and `path`) to the ids the query
returned, so the client needs no second round trip. `fetchTextBodyValues`
includes the decoded text.

```json
{
  "using": [
    "urn:ietf:params:jmap:core",
    "urn:ietf:params:jmap:mail",
    "urn:ietf:params:jmap:submission"
  ],
  "methodCalls": [
    [
      "Email/query",
      {
        "accountId": "rbdpoc2@exampleserver.de",
        "filter": {
          "inMailbox": "100d2e910326caf51eb460ff7ed54615"
        },
        "sort": [
          {
            "property": "receivedAt",
            "isAscending": false
          }
        ],
        "limit": 1
      },
      "q"
    ],
    [
      "Email/get",
      {
        "accountId": "rbdpoc2@exampleserver.de",
        "#ids": {
          "resultOf": "q",
          "name": "Email/query",
          "path": "/ids"
        },
        "properties": [
          "from",
          "to",
          "subject",
          "receivedAt",
          "keywords",
          "preview",
          "textBody",
          "bodyValues"
        ],
        "fetchTextBodyValues": true
      },
      "g"
    ]
  ]
}
```

The message arrived five seconds after it was sent. Empty `keywords`
mean unread (`$seen` would mark it read).

```json
{
  "sessionState": "74918b70d511e520",
  "methodResponses": [
    [
      "Email/query",
      {
        "accountId": "rbdpoc2@exampleserver.de",
        "queryState": "40",
        "ids": [
          "8b3e5867996962e964a563b799358efc"
        ],
        "position": 0,
        "canCalculateChanges": true
      },
      "q"
    ],
    [
      "Email/get",
      {
        "accountId": "rbdpoc2@exampleserver.de",
        "state": "40",
        "list": [
          {
            "bodyValues": {
              "1": {
                "value": "Hallo rbdpoc2,\r\ndiese Nachricht wurde per JMAP verschickt.\r\n",
                "isEncodingProblem": false,
                "isTruncated": false
              }
            },
            "from": [
              {
                "name": "rbdpoc1",
                "email": "rbdpoc1@exampleserver.de"
              }
            ],
            "id": "8b3e5867996962e964a563b799358efc",
            "keywords": {},
            "preview": "Hallo rbdpoc2, diese Nachricht wurde per JMAP verschickt.",
            "receivedAt": "2026-09-28T06:53:43Z",
            "subject": "JMAP-Demo 891ecb",
            "textBody": [
              {
                "blobId": "8b3e5867996962e964a563b799358efc_1",
                "charset": "utf-8",
                "cid": null,
                "disposition": null,
                "language": null,
                "location": null,
                "name": null,
                "partId": "1",
                "size": 60,
                "type": "text/plain"
              }
            ],
            "to": [
              {
                "name": null,
                "email": "rbdpoc2@exampleserver.de"
              }
            ]
          }
        ],
        "notFound": []
      },
      "g"
    ]
  ]
}
```

A real client does not poll for new mail: it keeps the event source
(`/jmap/eventsource/`) open, receives a `StateChange` naming `Email` and
`EmailDelivery`, and then calls `Email/changes` with its last state (see
[Push](jmap.md#push)).

The same flows run in the acceptance scripts under `hack/`
(`jmap-acceptance*.py`), which also clean up after themselves.

# API smoke test

- Run at: 2026-08-25T04:33:45Z
- Server: http://localhost:8080
- Account: `acc_2eb1534599f0105a8caef9f8`

## GET /api/v1/accounts/{id}

```bash
curl -H "Authorization: Bearer $API_KEY" http://localhost:8080/api/v1/accounts/acc_2eb1534599f0105a8caef9f8
```

**Response:** HTTP 200 — PASS (expected 200)

    Content-Type: application/json
    Content-Length: 248

```json
{
  "id": "acc_2eb1534599f0105a8caef9f8",
  "provider": "OUTLOOK",
  "email": "gauravrautela007@outlook.com",
  "name": "Gaurav Rautela",
  "status": "OK",
  "created_at": "2026-08-24T12:18:47Z",
  "updated_at": "2026-08-24T12:18:47Z",
  "last_synced_at": "2026-08-25T04:33:32Z"
}
```

## GET /api/v1/accounts/{id} — unknown id

```bash
curl -H "Authorization: Bearer $API_KEY" http://localhost:8080/api/v1/accounts/acc_does_not_exist
```

**Response:** HTTP 404 — PASS (expected 404)

    Content-Type: application/json
    Content-Length: 67

```json
{
  "error": {
    "code": "account_not_found",
    "message": "no such account"
  }
}
```

## GET /api/v1/emails — inbox, newest 3

```bash
curl -H "Authorization: Bearer $API_KEY" http://localhost:8080/api/v1/emails\?account_id=acc_2eb1534599f0105a8caef9f8\&folder_role=inbox\&limit=3
```

**Response:** HTTP 200 — PASS (expected 200)

    Content-Type: application/json

```json
{
  "items": [
    {
      "id": "AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoARgAAA6vzNxfvuglGmkzmGOk5gWUHAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAPwgStvB1mhPiPQMravX9EkAAAIG8QAAAA==",
      "account_id": "acc_2eb1534599f0105a8caef9f8",
      "thread_id": "AQQkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoAEABPdrzS-uGzTo2WaAlMxQ08",
      "folder_id": "AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoALgAAA6vzNxfvuglGmkzmGOk5gWUBAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAA==",
      "subject": "smoke-test 09:59:46",
      "from": {
        "name": "Gaurav Rautela",
        "email": "gauravrautela007@outlook.com"
      },
      "to": [
        {
          "name": "Gaurav Rautela",
          "email": "gauravrautela007@outlook.com"
        }
      ],
      "date": "2026-08-25T04:29:52Z",
      "snippet": "Sent by the API smoke test.",
      "body_type": "html",
      "read": false,
      "flagged": false,
      "draft": false,
      "has_attachments": false,
      "internet_message_id": "<DM6PR19MB4310CB3FA338674631CCFB0687AF2@DM6PR19MB4310.namprd19.prod.outlook.com>"
    },
    {
      "id": "AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoARgAAA6vzNxfvuglGmkzmGOk5gWUHAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAPwgStvB1mhPiPQMravX9EkAAAIG8AAAAA==",
      "account_id": "acc_2eb1534599f0105a8caef9f8",
      "thread_id": "AQQkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoAEACv4v8f6o5CQZfC_nGC2gU8",
      "folder_id": "AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoALgAAA6vzNxfvuglGmkzmGOk5gWUBAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAA==",
      "subject": "Re: test email",
      "from": {
        "name": "gaurav",
        "email": "gauravrautela16@gmail.com"
      },
      "to": [
        {
          "name": "gauravrautela007@outlook.com",
          "email": "gauravrautela007@outlook.com"
        }
      ],
      "date": "2026-08-24T15:28:54Z",
      "snippet": "++ attachment\r\n\r\nOn Mon, Aug 24, 2026 at 8:55\u202fPM gaurav <gauravrautela16@gmail.com> wrote:\r\ntest1234\r\n\r\nOn Mon, Aug 24, 2026 at 6:39\u202fPM gaurav <gauravrautela16@gmail.com> wrote:\r\ntest reply attachment\r\n\r\nOn Mon, Aug 24, 2026 at 6:03\u202fPM gaurav <gauravraute",
      "body_type": "html",
      "read": false,
      "flagged": false,
      "draft": false,
      "has_attachments": true,
      "internet_message_id": "<CAMxWFQjewJJFNbE40oZwXCL7FQ54TM_6RPLvOLXDUnY2Bt6sAg@mail.gmail.com>"
    },
    {
      "id": "AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoARgAAA6vzNxfvuglGmkzmGOk5gWUHAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAPwgStvB1mhPiPQMravX9EkAAAIG7wAAAA==",
      "account_id": "acc_2eb1534599f0105a8caef9f8",
      "thread_id": "AQQkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoAEACv4v8f6o5CQZfC_nGC2gU8",
      "folder_id": "AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoALgAAA6vzNxfvuglGmkzmGOk5gWUBAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAA==",
      "subject": "Re: test email",
      "from": {
        "name": "gaurav",
        "email": "gauravrautela16@gmail.com"
      },
      "to": [
        {
          "name": "gauravrautela007@outlook.com",
          "email": "gauravrautela007@outlook.com"
        }
      ],
      "date": "2026-08-24T15:25:33Z",
      "snippet": "test1234\r\n\r\nOn Mon, Aug 24, 2026 at 6:39\u202fPM gaurav <gauravrautela16@gmail.com> wrote:\r\ntest reply attachment\r\n\r\nOn Mon, Aug 24, 2026 at 6:03\u202fPM gaurav <gauravrautela16@gmail.com> wrote:\r\nyes this looks good.\r\n\r\n\r\nOn Mon, Aug 24, 2026 at 6:01\u202fPM gaurav <ga",
      "body_type": "html",
      "read": false,
      "flagged": false,
      "draft": false,
      "has_attachments": false,
      "internet_message_id": "<CAMxWFQiahz6Ct1PApHYd7Rq_8x3KN8ry+DGs6_CeCfKi5Ngh-Q@mail.gmail.com>"
    }
  ],
  "limit": 3,
  "offset": 0
}
```

## GET /api/v1/emails — missing account_id

```bash
curl -H "Authorization: Bearer $API_KEY" http://localhost:8080/api/v1/emails
```

**Response:** HTTP 400 — PASS (expected 400)

    Content-Type: application/json
    Content-Length: 75

```json
{
  "error": {
    "code": "missing_account_id",
    "message": "account_id is required"
  }
}
```

## GET /api/v1/emails — no API key

```bash
curl http://localhost:8080/api/v1/emails\?account_id=acc_2eb1534599f0105a8caef9f8
```

**Response:** HTTP 401 — PASS (expected 401)

    Content-Type: application/json
    Content-Length: 73

```json
{
  "error": {
    "code": "unauthorized",
    "message": "missing or invalid API key"
  }
}
```

## GET /api/v1/emails/{id}

```bash
curl -H "Authorization: Bearer $API_KEY" http://localhost:8080/api/v1/emails/AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoARgAAA6vzNxfvuglGmkzmGOk5gWUHAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAPwgStvB1mhPiPQMravX9EkAAAIG8AAAAA==\?account_id=acc_2eb1534599f0105a8caef9f8
```

**Response:** HTTP 200 — PASS (expected 200)

    Content-Type: application/json

```json
{
  "id": "AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoARgAAA6vzNxfvuglGmkzmGOk5gWUHAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAPwgStvB1mhPiPQMravX9EkAAAIG8AAAAA==",
  "account_id": "acc_2eb1534599f0105a8caef9f8",
  "thread_id": "AQQkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoAEACv4v8f6o5CQZfC_nGC2gU8",
  "folder_id": "AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoALgAAA6vzNxfvuglGmkzmGOk5gWUBAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAA==",
  "subject": "Re: test email",
  "from": {
    "name": "gaurav",
    "email": "gauravrautela16@gmail.com"
  },
  "to": [
    {
      "name": "gauravrautela007@outlook.com",
      "email": "gauravrautela007@outlook.com"
    }
  ],
  "date": "2026-08-24T15:28:54Z",
  "snippet": "++ attachment\r\n\r\nOn Mon, Aug 24, 2026 at 8:55\u202fPM gaurav <gauravrautela16@gmail.com> wrote:\r\ntest1234\r\n\r\nOn Mon, Aug 24, 2026 at 6:39\u202fPM gaurav <gauravrautela16@gmail.com> wrote:\r\ntest reply attachment\r\n\r\nOn Mon, Aug 24, 2026 at 6:03\u202fPM gaurav <gauravraute",
  "body": "<1800 chars>",
  "body_type": "html",
  "read": false,
  "flagged": false,
  "draft": false,
  "has_attachments": true,
  "internet_message_id": "<CAMxWFQjewJJFNbE40oZwXCL7FQ54TM_6RPLvOLXDUnY2Bt6sAg@mail.gmail.com>"
}
```

## GET /api/v1/emails/{id}/attachments

```bash
curl -H "Authorization: Bearer $API_KEY" http://localhost:8080/api/v1/emails/AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoARgAAA6vzNxfvuglGmkzmGOk5gWUHAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAPwgStvB1mhPiPQMravX9EkAAAIG8AAAAA==/attachments\?account_id=acc_2eb1534599f0105a8caef9f8
```

**Response:** HTTP 200 — PASS (expected 200)

    Content-Type: application/json
    Content-Length: 298

```json
{
  "items": [
    {
      "id": "AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoARgAAA6vzNxfvuglGmkzmGOk5gWUHAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAPwgStvB1mhPiPQMravX9EkAAAIG8AAAAAESABAA-IAM-L_Ol0ie8dwioBsqqg==",
      "name": "17th-dinner.pdf",
      "mime_type": "application/pdf",
      "size": 140994,
      "is_inline": false
    }
  ],
  "limit": 0,
  "offset": 0
}
```

## GET /api/v1/emails/{id}/attachments/{aid}

```bash
curl -H "Authorization: Bearer $API_KEY" http://localhost:8080/api/v1/emails/AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoARgAAA6vzNxfvuglGmkzmGOk5gWUHAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAPwgStvB1mhPiPQMravX9EkAAAIG8AAAAA==/attachments/AQMkADAwATNiZmYAZS1iN2ZjLWNjADdhLTAwAi0wMAoARgAAA6vzNxfvuglGmkzmGOk5gWUHAPwgStvB1mhPiPQMravX9EkAAAIBDAAAAPwgStvB1mhPiPQMravX9EkAAAIG8AAAAAESABAA-IAM-L_Ol0ie8dwioBsqqg==\?account_id=acc_2eb1534599f0105a8caef9f8
```

**Response:** HTTP 200 — PASS (expected 200)

    Content-Disposition: attachment; filename="17th-dinner.pdf"
    Content-Type: application/pdf

    (binary, 140774 bytes: PDF document, version 1.7, 1 pages)

## POST /api/v1/emails — send to self

```bash
curl -H "Authorization: Bearer $API_KEY" -X POST -H Content-Type:\ application/json -d \{\"account_id\":\"acc_2eb1534599f0105a8caef9f8\"\,\"to\":\[\{\"email\":\"gauravrautela007@outlook.com\"\}\]\,\"subject\":\"smoke-test\ 04:33:47\"\,\"body\":\"\<p\>Sent\ by\ scripts/smoke.sh\</p\>\"\} http://localhost:8080/api/v1/emails
```

**Response:** HTTP 202 — PASS (expected 202)

    Content-Type: application/json
    Content-Length: 18

```json
{
  "status": "sent"
}
```

## POST /api/v1/emails — missing recipients

```bash
curl -H "Authorization: Bearer $API_KEY" -X POST -H Content-Type:\ application/json -d \{\"account_id\":\"acc_2eb1534599f0105a8caef9f8\"\,\"subject\":\"x\"\,\"body\":\"y\"\} http://localhost:8080/api/v1/emails
```

**Response:** HTTP 400 — PASS (expected 400)

    Content-Type: application/json
    Content-Length: 67

```json
{
  "error": {
    "code": "missing_recipients",
    "message": "to is required"
  }
}
```

## Summary

- Passed: 10
- Failed: 0
- Sent one email to `gauravrautela007@outlook.com` with subject `smoke-test 04:33:47`

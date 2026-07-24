# consus-local-log

[![ci](https://github.com/consus-io/consus-local-log/actions/workflows/ci.yml/badge.svg)](https://github.com/consus-io/consus-local-log/actions/workflows/ci.yml)

A stateless reverse proxy you run inside your own network, between your tools
and the Consus gateway. It forwards every request untouched, relays responses
(including SSE streams) in real time, and appends one JSON line per request to
a local file that only you hold. It is one Go source file of about 600 lines
with zero dependencies, built so your security team can read every line before
running it.

## How it works

```
your tools ──▶ consus-local-log (:4000) ──▶ api.consus.io
                       │
                       └──▶ /var/log/consus/YYYY-MM-DD.jsonl  (one line per request)
```

Headers are copied verbatim in both directions (minus standard hop-by-hop
headers). Bodies are streamed as raw bytes and never parsed. After each
response completes, one JSON line is written — and that line is the only thing
the proxy ever retains.

## Quickstart

**Docker**

```sh
docker build -t consus-local-log .
sudo install -d -o 65532 /var/log/consus   # only for a bind mount: the host dir
docker run -d --name consus-local-log -p 4000:4000 \
  -v /var/log/consus:/var/log/consus consus-local-log
```

The image runs as UID 65532 and ships `/var/log/consus` owned by it, so a plain
`docker run` (no `-v`) logs to a Docker-managed volume with no setup. A **bind
mount** to a host directory keeps the host's ownership, so that directory must
be writable by 65532 — the `install -d` line above. If it isn't, the proxy
still forwards traffic and says so on startup; watch for it in `docker logs`.

**systemd** (binary at `/usr/local/bin/consus-local-log`, log dir owned by `consuslog`)

```ini
[Unit]
Description=Consus Local Log proxy
[Service]
ExecStart=/usr/local/bin/consus-local-log
User=consuslog
Restart=on-failure
[Install]
WantedBy=multi-user.target
```

**Local, from source**

```sh
LOCALLOG_DIR=./logs go run .
tail -f logs/$(date -u +%F).jsonl | jq .
```

## The one client change

Point your tool's base URL at the proxy instead of the API:

```sh
export OPENAI_BASE_URL=http://localhost:4000/v1    # or your SDK's base-url setting
```

Keys, paths, streaming, everything else: unchanged. The proxy forwards any
path, so new API endpoints work with zero changes here.

## Configuration

Environment variables only — no flags, no config file.

| Variable | Default | Meaning |
|---|---|---|
| `LOCALLOG_LISTEN` | `:4000` | listen address |
| `LOCALLOG_UPSTREAM` | `https://api.consus.io` | upstream base URL |
| `LOCALLOG_DIR` | `/var/log/consus` | log directory, created if missing (0750) |
| `LOCALLOG_MAX_CAPTURE` | `10485760` | per-direction capture cap in bytes |

## Health

`GET /healthz` is answered by the proxy itself and never forwarded:

```json
{"upstream_ok": true, "log_misses": 0, "uptime_s": 3421}
```

- `upstream_ok` — the cached result of a `HEAD` to `LOCALLOG_UPSTREAM`,
  refreshed on a background timer every 30s. Reading it never touches the
  network, so this endpoint is safe to use as a liveness or readiness probe
  even while the upstream is down.
- `log_misses` — entries that could not be written since startup. **This is
  the number to alert on.** It is the only signal that requests are being
  proxied but not recorded, and anything above zero means the audit trail has
  a hole in it. The reason is on stderr (`docker logs`, or the journal).
- `uptime_s` — seconds since startup.

## Log schema

One JSON object per line in `LOCALLOG_DIR/YYYY-MM-DD.jsonl` (UTC date). Every
field is always present.

| Field | Type | Meaning |
|---|---|---|
| `ts` | string | request start time, RFC 3339 with milliseconds, UTC |
| `consus_request_id` | string | `x-consus-request-id` response header; `""` if absent |
| `key_sha256` | string | hex SHA-256 of the raw `Authorization` header value; `""` if absent |
| `path` | string | request path with query |
| `method` | string | HTTP method |
| `model` | string | best-effort `"model"` field found in the request body; `""` if not found |
| `status` | number | HTTP status returned to the client |
| `latency_ms` | number | request start to response end, milliseconds |
| `stream` | bool | response `Content-Type` contained `text/event-stream` |
| `truncated` | bool | the capture is not the whole body — see below |
| `client_disconnected` | bool | client hung up before the response completed |
| `request` | string | captured request body bytes |
| `response` | string | captured response body bytes; for SSE, the verbatim event transcript |

`truncated` is true whenever what was logged is less than what crossed the
wire, for either of two reasons: the body exceeded `LOCALLOG_MAX_CAPTURE`, or
the request body was never fully read by the upstream — which is what happens
when the gateway rejects on headers alone, for instance a 401 on an expired
key. Treat a `truncated` line as evidence that a request occurred, not as the
complete text of it.

**Binary bodies.** Bodies that are not valid UTF-8 — a gzip-encoded response,
which is what most SDKs ask for by default — cannot be stored in a JSON string
without corruption. Those captures are base64-encoded behind a `base64:`
prefix. Anything without that prefix is verbatim text. To read one:

```sh
jq -r 'select(.response | startswith("base64:")) | .response[7:]' \
  /var/log/consus/*.jsonl | base64 -d | gunzip
```

## jq recipes

Usage by key and model:

```sh
jq -s 'group_by([.key_sha256, .model])
       | map({key_sha256: .[0].key_sha256, model: .[0].model, requests: length})
       | sort_by(-.requests)' /var/log/consus/*.jsonl
```

Full-text search across request bodies (a `base64:` capture is not text and
will not match — decode it first, as above):

```sh
jq 'select(.request | test("quarterly report"; "i"))' /var/log/consus/*.jsonl
```

Failed requests:

```sh
jq 'select(.status >= 400) | {ts, method, path, status, latency_ms}' /var/log/consus/*.jsonl
```

Request ids, for joining against a Consus statement:

```sh
jq -r 'select(.consus_request_id != "")
       | [.consus_request_id, .ts, .key_sha256, .model] | @csv' /var/log/consus/*.jsonl
```

## Rotation and archival

The proxy already starts a new file each UTC day, so rotation is only
compression and expiry. `delaycompress` leaves the newest rotation
uncompressed in case it is still receiving the current day's writes (the
writer follows the rename until midnight UTC, then opens the new date file):

```
# /etc/logrotate.d/consus-local-log
/var/log/consus/*.jsonl {
    daily
    missingok
    notifempty
    compress
    delaycompress
    maxage 90
    nocreate
}
```

Because the files already carry their date, rotation is only compressing and
expiring them — if you would rather not have `.1` suffixes on date-named
files, two `find` lines in cron do the same job:

```sh
find /var/log/consus -name '*.jsonl' -mtime +1 -exec gzip {} +
find /var/log/consus -name '*.jsonl.gz' -mtime +90 -delete
```

Nightly archive to a bucket you own (skips the active day's file):

```
15 0 * * * aws s3 sync /var/log/consus/ s3://YOUR-BUCKET/consus-local-log/ --exclude "$(date -u +\%F).jsonl"
```

## Making it mandatory

The log is only a complete record if traffic cannot go around it. Enforce
that with one egress rule: **only the proxy host may reach `api.consus.io`
on 443.** In an AWS security group, that means the gateway CIDR egress rule
exists only on the proxy host's group; on a network firewall, allow
`proxy-host → api.consus.io:443` and deny it from everywhere else. Client
machines then physically cannot bypass the proxy — an unlogged request is a
failed request.

## What Local Log never does

- **Hold credentials** — the `Authorization` header is copied like every other
  header; the only other touch is the SHA-256 written to the log line.
- **Validate keys** — it has no opinion on whether a request is authorized;
  that's the gateway's job.
- **Modify traffic** — no headers added or removed (beyond standard hop-by-hop),
  no bytes rewritten, no compression negotiation of its own.
- **Parse payloads** — bodies are raw bytes in, raw bytes out, raw bytes logged.
- **Phone home** — the only host it ever connects to is `LOCALLOG_UPSTREAM`:
  your traffic, plus one `HEAD` every 30s so `/healthz` can answer without
  waiting on the network. Nothing is reported anywhere else, ever.
- **Retain anything** after the log line is written — no queues, no state, no
  copies.

## Running on Windows

The binary cross-compiles as-is (`GOOS=windows go build`). Notes:

- Set `LOCALLOG_DIR` explicitly (e.g. `C:\ProgramData\consus\logs`); the Unix
  default works but lands on the current drive.
- Ctrl+C shuts down gracefully. A service-manager hard kill skips the drain:
  in-flight streams are severed and up to 256 buffered log lines can be lost.
- To run as a Windows service, use a wrapper such as NSSM or WinSW. Native
  service integration would require a dependency, which this project rejects.
- Unix permission bits (0750/0640) are advisory on NTFS; use ACLs on the log
  directory.

## Decisions where the spec is silent

The spec asks that anything it doesn't pin down be resolved simply and noted
here:

1. `request`/`response` capture **bodies only**; headers are never captured.
   The only header-derived fields are `key_sha256` and `consus_request_id`.
2. `model` is a raw text search for the first `"model":"…"` in the captured
   body — a nested key can match, and values containing escapes yield `""`.
3. WebSockets are unsupported: `Upgrade` is hop-by-hop and stripped, per spec.
4. The listener is plain HTTP/1.1 — there is no TLS on it, so no HTTP/2.
   Outbound connections use HTTP/2 when the upstream negotiates it, and are
   bounded by a 30 s dial timeout and a 10 s TLS handshake timeout, so an
   upstream that blackholes traffic produces a 502 instead of a hang. Nothing
   bounds the response itself: a model may take as long as it takes.
5. Trailers are dropped in both directions (`Trailer`/`Transfer-Encoding` are
   hop-by-hop).
6. Request headers are limited by Go's default 1 MB cap; not configurable.
7. `/healthz` (exact path, any method) is always served by the proxy; an
   upstream route with that name is unreachable through it.
8. `upstream_ok` means the probe completed (connection, TLS, response),
   regardless of status code; the probe times out after 5 s. It runs on a
   background timer rather than inside the request, so `/healthz` answers
   immediately even while the upstream is unreachable — a health check that
   blocked would get the proxy restarted at the worst possible moment.
9. Shutdown waits for in-flight requests with no deadline (the spec forbids
   one under 60 s); your service manager's kill timeout is the real bound. A
   second SIGINT/SIGTERM during that wait terminates the process immediately.
10. The 502 body is the fixed line `upstream unreachable`. If the failure was
    the client hanging up mid-upload, `client_disconnected` is also set.
11. Bodies are forwarded byte-exact. The *log copy* is a JSON string, so
    captures that are not valid UTF-8 are base64-encoded behind a `base64:`
    prefix rather than corrupted — see the schema section.
12. A response whose status code is outside 100–999 cannot be relayed (it is
    not HTTP any client library can represent); the client gets 502 and the
    log records 502.
13. Each entry is written to the file for the UTC day the request *started*,
    so a stream that runs past midnight is filed under the day it began.
14. Worst-case memory is roughly `2 × LOCALLOG_MAX_CAPTURE × (256 + in-flight
    requests)`; lower the cap on small hosts.
15. A log write that fails is reported on stderr (once per distinct error) and
    counted in `log_misses`; the request is unaffected. A partially written
    line is rolled back so every line in the file always parses.

## Development

```sh
go build ./...               # compile
go vet ./...                 # static checks
go test -race ./...          # everything
go test -race -short ./...   # everything except the tests that build the binary
```

Tests are split by layer: `unit_test.go` for the pieces in isolation,
`acceptance_test.go` for the nine criteria in
`specifications/consus-local-log-SPEC.md`, `regression_test.go` for defects
found in review, and `e2e_test.go` for runs of the real binary. Those nine
criteria are the contract and are never weakened. Fixtures use fabricated
payloads only — no real prompts, keys, or customer names.

One test needs a filesystem small enough to fill, to prove that a log write
interrupted by a full disk leaves no unparseable line behind. It skips unless
you point it at one; CI mounts a 1 MB tmpfs. Locally:

```sh
# Linux
sudo mkdir -p /mnt/small && sudo mount -t tmpfs -o size=1m tmpfs /mnt/small
sudo chmod 1777 /mnt/small
LOCALLOG_TEST_SMALLDIR=/mnt/small go test -race -run TestPartialWriteRollback .

# macOS
dev=$(hdiutil attach -nomount ram://16384 | awk '{print $1}')
diskutil erasevolume HFS+ smallfs "$dev"
LOCALLOG_TEST_SMALLDIR=/Volumes/smallfs go test -race -run TestPartialWriteRollback .
diskutil eject "$dev"
```

## Reporting a vulnerability

Privately, please — see [SECURITY.md](SECURITY.md) for the channel and for what
is in and out of scope. Notable changes, including anything that touches the
log schema, are recorded in [CHANGELOG.md](CHANGELOG.md).

## License

Apache 2.0 — see [LICENSE](LICENSE).

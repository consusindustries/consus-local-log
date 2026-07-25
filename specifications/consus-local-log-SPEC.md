# SPEC: consus-local-log v0.1

This document is the complete implementation spec. Build exactly this. Where this spec is silent, choose the simplest option and note it in the README.

## What this is

A stateless reverse proxy in Go. It listens inside a customer's network, forwards every request to the Consus gateway with all headers untouched, relays responses (including SSE streams) to the client with no added buffering, and appends one JSON line per request to a local log file after the response completes.

Design intent: a security engineer must be able to read the entire source in one sitting. Optimize for that over cleverness.

## Hard constraints

1. Go standard library only. Zero third-party dependencies. `go.mod` contains no requires.
2. Single `main.go` is preferred. Split only if it clearly aids readability.
3. the proxy never reads, validates, stores, or special-cases the Authorization header. It is copied in the same loop as every other header.
4. the proxy never parses request or response bodies. Raw bytes only.
5. No durable queues, no retries on log writes, no state that survives a request except the log file itself.

## Deliverables

- `main.go` — the program
- `main_test.go` — tests (see acceptance criteria)
- `go.mod` — module `github.com/consus-io/consus-local-log`, go 1.22+
- `Dockerfile` — two-stage, final stage `FROM scratch`, one binary, runs as nonroot UID
- `README.md` — see README requirements
- `LICENSE` — Apache 2.0

## Configuration

Env vars only. No flags, no config file.

| Var | Default | Meaning |
|---|---|---|
| `LOCALLOG_LISTEN` | `:4000` | listen address |
| `LOCALLOG_UPSTREAM` | `https://api.consus.io` | upstream base URL |
| `LOCALLOG_DIR` | `/var/log/consus` | log directory, created if missing (0750) |
| `LOCALLOG_MAX_CAPTURE` | `10485760` | per-direction capture cap in bytes |

## Runtime behavior

### Listener
Plain HTTP server on `LOCALLOG_LISTEN`. No TLS in v0.1 (deployed on localhost or behind the customer's internal LB). No read/idle timeouts that would kill long-lived SSE streams; write timeout unset.

### Forwarding
For every inbound request on any path:
1. Read the request body into a capped buffer (`LOCALLOG_MAX_CAPTURE`). If the body exceeds the cap, keep forwarding all bytes upstream but stop retaining them; set `truncated: true`.
2. Build the upstream request: same method, same path and query appended to `LOCALLOG_UPSTREAM`, all headers copied verbatim except hop-by-hop headers (Connection, Keep-Alive, Proxy-*, TE, Trailer, Transfer-Encoding, Upgrade). Set Host to the upstream host.
3. Send via a shared `http.Transport` with sane connection pooling. Do not follow redirects; pass them through.
4. On upstream connection failure, return 502 to the client with a one-line plain-text body, and still write a log line (status 502, empty response).

### Response relay and capture
1. Copy upstream status code and headers to the client verbatim (minus hop-by-hop).
2. Relay the body with a tee: every chunk read from upstream is written to the client immediately and appended to a capped capture buffer. Call `http.Flusher.Flush()` after each chunk so SSE streams in real time.
3. Past the cap, keep relaying, stop capturing, set `truncated: true`.
4. If the client disconnects mid-stream, stop relaying, drain nothing further from upstream, and still write a log line with the bytes captured so far and `client_disconnected: true`.

### Log line
After the response completes (or fails), build one JSON object and send it to the writer goroutine over a buffered channel (size 256). If the channel is full, drop the entry and increment the miss counter. Never block a request on logging.

Schema (all fields always present):

```json
{
  "ts": "RFC3339 with millis, UTC, request start time",
  "consus_request_id": "value of x-consus-request-id response header, empty string if absent",
  "key_sha256": "hex SHA-256 of the raw Authorization header value, empty string if header absent",
  "path": "request path with query",
  "method": "HTTP method",
  "model": "best-effort value of a top-level \"model\" JSON field in the request body, empty string if not found; a simple search, never full JSON parsing of untrusted size",
  "status": 200,
  "latency_ms": 0,
  "stream": "true if response Content-Type contains text/event-stream",
  "truncated": false,
  "client_disconnected": false,
  "request": "captured request bytes as string",
  "response": "captured response bytes as string; for SSE this is the verbatim event transcript"
}
```

### Writer
One background goroutine owns the file. It receives entries from the channel, marshals, appends `\n`, writes. It rolls to a new file when the UTC date changes: `LOCALLOG_DIR/YYYY-MM-DD.jsonl`, opened with O_APPEND|O_CREATE, 0640. On any write error: increment miss counter, continue. On graceful shutdown: drain the channel, close the file.

### Health
`GET /healthz` on the same listener is served by the proxy itself, never forwarded. Response JSON: `{"upstream_ok": bool, "log_misses": n, "uptime_s": n}`. `upstream_ok` is the cached result of a HEAD/GET to the upstream base URL performed at most every 30s, never in the request path.

### Shutdown
On SIGINT/SIGTERM: stop accepting, let in-flight requests finish (no hard deadline shorter than 60s), drain the log channel, flush, exit 0.

## Acceptance criteria (implement as tests)

1. Passthrough: request against a stub upstream arrives with method, path, query, body, and all headers (including Authorization) byte-identical; hop-by-hop headers stripped.
2. Streaming: stub upstream emits 10 SSE events with 50ms gaps; client receives each event before the next is sent (proves flushing); log line contains the verbatim 10-event transcript.
3. Truncation: 20 MB body with a 1 MB cap → upstream receives all 20 MB, log retains 1 MB, `truncated: true`.
4. Concurrency: 100 concurrent streaming requests → exactly 100 well-formed, non-interleaved JSON lines (every line parses).
5. Log failure: unwritable log dir → requests still return 200, `/healthz` shows misses > 0.
6. Client disconnect mid-stream → log line written with `client_disconnected: true`.
7. Request id: stub sets `x-consus-request-id` → appears in the log line.
8. Day roll: entries written across a mocked date boundary land in two files.
9. Upstream down → client gets 502, log line written with status 502.

## README requirements

- Quickstart: docker run one-liner and systemd unit, both under 10 lines
- The one client change: set the tool's base URL to the proxy address
- Log schema table
- Four jq recipes: usage by key/model, full-text search of requests, failed requests, extracting request ids to join against a Consus statement
- Logrotate config snippet and a one-line cron example archiving to a customer-owned bucket
- "Making it mandatory": the egress rule — only the proxy host may reach api.consus.io on 443
- "What Local Log never does" list: hold credentials, validate keys, modify traffic, parse payloads, phone home, retain anything after the line is written

## Non-goals (do not build)

Redaction, sampling, retention/rotation, TLS termination, auth on the proxy, metrics endpoints, multi-upstream, a UI, config files, third-party logging integrations.

## Changes since v0.1

This section records deliberate schema evolution so the spec stays the source
of truth. CHANGELOG.md carries the customer-facing history.

- 2026-07-25: two fields added to the log line for the enterprise deployment
  (attribution and durable gap evidence):
  - `consus_key_id` — value of the `x-consus-key-id` response header, empty
    string if absent. Placed after `consus_request_id`.
  - `dropped` — number of entries lost (queue full or write failure) since the
    previous line was written; 0 when the log is complete. Placed after
    `client_disconnected`.

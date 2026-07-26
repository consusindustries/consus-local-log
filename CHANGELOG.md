# Changelog

Notable changes to consus-local-log. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
follows [semantic versioning](https://semver.org/spec/v2.0.0.html) once it
reaches 1.0.0.

The log schema is the public interface most consumers depend on. Any change to
a field's name, type, or meaning will appear here first.

## [Unreleased]

### Added

- `consus_key_id` log field: the gateway's `x-consus-key-id` response header,
  joining each line to its owner on the Consus portal's API Keys page. Empty
  only when the gateway never answered; those lines still carry `key_sha256`.
- `dropped` log field: entries lost since the previous line was written,
  whether from a full queue or a write failure. The log now carries durable
  evidence of its own gaps; `/healthz`'s `log_misses` counter resets on
  restart, this does not.
- `deploy/`: reference deployment assets — a hardened systemd unit, a Fluent
  Bit shipper configuration, a logrotate file, and the alert definitions
  (starting with the one that fires when logging itself stops).
- Per-cloud deployment guides: single-file, self-contained HTML one-pagers
  for AWS GovCloud, Azure Government, and Google Cloud, following the same
  ten steps on each platform.
- Reproducible releases: pushed tags build byte-for-byte rebuildable binaries
  for five platforms with published SHA256 checksums, gated on the race
  suite. The README documents how customers verify or reproduce a release.
- A contributions policy: the source is public for audit and reproduction;
  pull requests are not accepted and close automatically with an explanation.
- The proxy itself: forwards any path to `LOCALLOG_UPSTREAM` with headers
  copied verbatim, relays responses with a flush after every chunk so SSE
  streams in real time, and appends one JSON line per request to
  `LOCALLOG_DIR/YYYY-MM-DD.jsonl`.
- `/healthz`, reporting cached upstream reachability, dropped log entries, and
  uptime.
- Configuration through `LOCALLOG_LISTEN`, `LOCALLOG_UPSTREAM`, `LOCALLOG_DIR`,
  and `LOCALLOG_MAX_CAPTURE`.
- Graceful shutdown on SIGINT/SIGTERM: in-flight streams finish, buffered log
  entries are flushed, and a second signal terminates immediately.
- A startup check that reports an unwritable log directory rather than
  proxying silently without logging.
- Captures that are not valid UTF-8, such as gzip-encoded response bodies, are
  base64-encoded behind a `base64:` prefix so they stay recoverable.
- Two-stage `Dockerfile` producing a `FROM scratch` image that runs as UID
  65532 and logs with no host setup.
- Tests in four files: unit, the nine acceptance criteria from the spec,
  regressions, and end-to-end runs of the real binary. CI runs them on Linux
  and Windows under the race detector, and checks that the built image logs.

### Changed

- `key_sha256` (log schema) now fingerprints whichever credential header the
  client sent: the `Authorization` value when present, otherwise `x-api-key`.
  Previously only `Authorization` was hashed, which left the field empty for
  Anthropic-style clients. When both headers are present, `Authorization` is
  the one fingerprinted; `consus_key_id` remains the authoritative
  attribution.

### Notes

- `truncated` means the logged capture is smaller than what crossed the wire,
  whether because the capture cap was reached or because the upstream answered
  without reading the whole request body.
- Entries are filed under the UTC day the request *started*, so a stream that
  runs past midnight stays with the day it began.

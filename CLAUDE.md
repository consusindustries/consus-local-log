# CLAUDE.md

## What this is

consus-local-log: a stateless reverse proxy that defense contractors run inside their own boundary. It forwards OpenAI-compatible traffic to the Consus gateway with headers untouched and appends one JSON line per request to a local file. Customers' security engineers read the entire source before running it. That is a feature requirement, not a nicety: every change is judged first by whether the program stays readable in one sitting.

## Source of truth

`specifications/consus-local-log-SPEC.md` defines behavior. This file defines process and invariants. On behavior conflicts, the spec wins.

## Invariants. Never violate, never "improve"

1. Stdlib only. go.mod never gains a require. If a task appears to need a dependency, stop and ask.
2. The credential headers — Authorization and x-api-key — are never read, validated, stored, or special-cased. They are copied in the same loop as every other header. The only other touch is the SHA-256 fingerprint for the log line (Authorization if present, else x-api-key).
3. Bodies are never parsed. Raw bytes forwarded, raw bytes logged. No endpoint-specific logic, ever. New API shapes must work with zero code changes.
4. No durable state except the log file. No queues, no persisted retries, nothing survives a request.
5. Availability beats capture. A logging failure never fails or delays a request.
6. Nothing phones home. The only outbound connection is the forward to LOCALLOG_UPSTREAM.

## Code style

- Single main.go unless readability clearly demands a split
- Boring and explicit beats clever. No abstractions with a single implementation
- Comments explain why, and only where a reviewer would otherwise pause

## Commands

```
go build ./...
go vet ./...
go test ./...
LOCALLOG_DIR=./logs go run .    # local run, then point any client at :4000
```

## Testing

- The nine acceptance criteria in the spec are the contract, and live in `acceptance_test.go`. Never weaken, skip, or delete those tests
- Tests are split by layer: `unit_test.go`, `acceptance_test.go`, `regression_test.go`, `e2e_test.go`, with shared fixtures in `helpers_test.go`. A defect found in review gets a regression test that fails without the fix
- New behavior lands test-first
- Fixtures use fabricated payloads only. Never real prompts, real keys, or customer names anywhere in the repo

## Never commit

- *.jsonl files (captured traffic from local runs; .gitignore enforces this, do not fight it)
- Real API keys or live model output
- Customer names in code, comments, fixtures, or commit messages

## This repo will go public

Private today, public at v1.0.0 with full history intact. Write every commit message and every file as if it is already public.

## Feature requests

The non-goals in the spec are deliberate product decisions: no redaction, sampling, UI, config files, TLS termination, metrics endpoints, retention logic, or third-party integrations. If a change would add one of these or a dependency, stop and ask Eric. The correct next step is usually a customer conversation, not code.
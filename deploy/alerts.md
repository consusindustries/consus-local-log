# Alerts

The proxy's availability posture is that a logging failure never fails a
request — which means a broken audit trail is silent by design at the traffic
level, and the alerts below are what make it loud. The first two are the ones
that matter most: for a system whose purpose is the record, **"logging
stopped" is a worse failure than "proxy down"** — a down proxy stops traffic,
a dead log lets traffic continue unrecorded.

Queries are given as `jq` against the raw files; translate to your SIEM's
syntax (the field names are identical everywhere since the shipper forwards
lines unmodified).

## 1. Entries are being lost (page)

Two independent signals, alert on either:

- `/healthz` reports `log_misses > 0` — total since process start.

  ```sh
  curl -s localhost:4000/healthz | jq -e '.log_misses == 0' >/dev/null || echo ALERT
  ```

- Any log line with `dropped > 0` — the durable form; survives restarts and
  is visible in the SIEM without reaching the host.

  ```sh
  jq 'select(.dropped > 0) | {ts, dropped}' /var/log/consus/*.jsonl
  ```

The cause is on the proxy's stderr (journald: `journalctl -u
consus-local-log`). Ship stderr to the SIEM too — the messages that explain a
degrading audit trail should not live only on the host.

## 2. The log went quiet (page during business hours)

No new line for N minutes while traffic is expected means either nobody is
using AI (fine) or traffic is bypassing the proxy / the shipper died (not
fine). This is a SIEM-side absence alert on the ingested stream; 15 minutes
is a reasonable starting window. Pair it with the egress rule: if the only
path to the gateway is the proxy, a quiet log during work hours is a real
signal.

## 3. Disk (warn at 80%, page at 90%)

A full disk stops the record. The proxy survives it (entries are counted and
reported via `dropped` once writes recover, and a partially written line is
rolled back so the file always parses), but everything lost in between is
gone. Alert on the log volume's usage; heavy users produce 50–100 MB/day raw
before compression.

## 4. Proxy health (standard service alerting)

- Process up: `systemctl is-active consus-local-log`, or an HTTP check on
  `/healthz` (it answers instantly even when the upstream is down; in the
  first moments after startup `upstream_ok` is `false` until the initial
  probe returns).
- `upstream_ok: false` for more than ~2 minutes: the gateway is unreachable
  from the instance — an egress or DNS problem on your side, or a Consus
  outage. Client traffic is failing fast with 502s during this window and
  every 502 is still logged.

## Traffic alerts

Content-level alerts — named program keywords in requests, error-rate spikes,
per-key volume anomalies — are deployment-specific and belong in the SIEM
where review already happens. Useful starting shapes:

```sh
# error-rate: 5xx/4xx per window
jq 'select(.status >= 400) | {ts, consus_key_id, path, status}' /var/log/consus/*.jsonl

# per-key volume, join consus_key_id against the portal for the owner
jq -s 'group_by(.consus_key_id) | map({key: .[0].consus_key_id, n: length}) | sort_by(-.n)' /var/log/consus/*.jsonl

# keyword search across request bodies (text lines only; base64: captures
# must be decoded first — see the README's schema section)
jq 'select(.request | test("PROGRAM-KEYWORD"; "i")) | {ts, consus_key_id, path}' /var/log/consus/*.jsonl
```

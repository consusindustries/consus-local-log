# Verifying a release on a local VM

This is a maintainer procedure, not a customer one. It answers a single
question before a release ships: **do the artifacts in this directory actually
work on a real Linux host?**

It takes about fifteen minutes and costs nothing.

## Why a local VM and not a cloud instance

The proxy makes no cloud API calls. No SDK, no IAM, no ARNs — its entire cloud
surface is a Linux process, a directory, and outbound 443. So "does it work on
AWS" is very nearly the question "does it work on Linux," and a cloud instance
buys almost nothing over a local VM while costing an afternoon.

What is genuinely untested before a run like this is
[`systemd/consus-local-log.service`](systemd/consus-local-log.service). It is
the one artifact shipped here that nobody executes during `go test`, and its
hardening is aggressive enough to plausibly break the proxy or — worse —
silently break only its logging. A customer would discover that on their first
`systemctl start`.

Use **Rocky Linux 9**, which ships systemd 252, the same major version as
Amazon Linux 2023. Matching the systemd version is what makes this a valid
test; matching the distribution is not important.

## What this covers, and what it does not

Covered:

- the hardened unit starts, stays running, and logs
- `ProtectSystem=strict` and `LogsDirectory=` cooperate rather than fighting
- the log directory lands at 0750 with the service user owning it
- an empty `CapabilityBoundingSet` and `MemoryDenyWriteExecute=true` do not
  break the Go runtime or outbound TLS
- real traffic: passthrough, SSE streaming, model extraction, attribution
  fields, and log lines for requests the gateway rejects

Not covered — and none of it should hold up a release:

- **TLS termination.** That is the customer's load balancer.
- **Log shipping and alarms.** Stock tooling reading a plain JSONL file.
- **The `base64:` capture path**, unless the upstream happens to return a
  compressed body during the run. The acceptance tests cover it.

## Prerequisites

[OrbStack](https://orbstack.dev), a Go toolchain, and a working API key for
the upstream in `$CONSUS_API_KEY`.

## 1. Create the machine

```sh
orb create rocky:9 loglab
```

OrbStack creates a Linux user matching your macOS username and mounts your Mac
filesystem at `/mnt/mac`, so `$USER` and the paths below line up on their own.

## 2. Build the artifact on the host

Build outside the repository. `.gitignore` covers the exact path
`/consus-local-log` and nothing else, so a differently-named binary in the repo
root would show up as untracked.

```sh
mkdir -p ~/loglab-build
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o ~/loglab-build/consus-local-log .
cd ~/loglab-build && shasum -a 256 consus-local-log > SHA256SUMS
```

`GOARCH=arm64` for Apple Silicon, `amd64` on Intel. To test the exact artifact
a customer downloads regardless of your hardware, create the machine with
`orb create -a amd64 rocky:9 loglab` and build `amd64` — emulated, slower, and
rarely worth it for a pure-Go stdlib binary.

## 3. Install it the way the guide says to

Everything from here runs inside the machine (`orb -m loglab`). Follow the
deployment guide verbatim rather than scripting it: the documentation is what
is under test, and automating these steps would only test the automation.

```sh
getenforce
sudo setenforce 0 2>/dev/null || true
```

Amazon Linux 2023 ships SELinux **permissive**; Rocky ships it **enforcing**.
Without this you will chase denials no customer will ever see.

```sh
sudo useradd --system --shell /usr/sbin/nologin consuslog
cd /mnt/mac/Users/$USER/loglab-build
sha256sum -c SHA256SUMS
sudo install -m 0755 consus-local-log /usr/local/bin/consus-local-log
sudo cp /mnt/mac/Users/$USER/path/to/repo/deploy/systemd/consus-local-log.service \
  /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now consus-local-log
```

## 4. Read the result

```sh
systemctl status consus-local-log --no-pager
sudo journalctl -u consus-local-log --no-pager -n 20
ls -ld /var/log/consus
curl -s localhost:4000/healthz; echo
systemd-analyze security consus-local-log --no-pager | tail -5
```

A pass looks like: `active (running)`; a journal whose only line is
`listening on :4000, forwarding to ..., logging to /var/log/consus`, with **no**
`WARNING: log directory ... is not writable`; `/var/log/consus` as
`drwxr-x--- consuslog consuslog`; and `/healthz` reporting `upstream_ok: true`.

`systemd-analyze security` scores the unit around 5.3 MEDIUM, which is good for
a network-facing service. It flags `UMask=` as making files group-readable —
that is deliberate, not a defect. `UMask=0027` is what lets a SIEM shipper read
the log through the `consuslog` group, and a reviewer running this command will
ask about it.

## 5. Point real clients at it

The machine is reachable from the host at `<name>.orb.local`, so clients can
run natively on your Mac while the proxy and its log stay inside the VM. That
is the customer topology — tools on the laptop, proxy in the boundary, log on a
host the tool's user cannot read — and it is a better test than localhost to
localhost.

```sh
export OPENAI_BASE_URL=http://loglab.orb.local:4000/v1
export ANTHROPIC_BASE_URL=http://loglab.orb.local:4000
```

Send traffic — including from a real SDK or coding agent, which is the strongest
version of this test, since it exercises the "one client change" claim against
software you did not write:

```sh
curl -s -N http://loglab.orb.local:4000/v1/chat/completions \
  -H "x-api-key: $CONSUS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"MODEL_ID","stream":true,
       "messages":[{"role":"user","content":"Count from 1 to 5."}]}'
```

Watch the log fill in from the host, without entering the VM:

```sh
orb -m loglab sudo tail -f /var/log/consus/$(date -u +%F).jsonl \
  | jq -c '{ts,status,model,stream,consus_key_id,consus_key_label}'
```

Two terminals — an agent on one side, audit lines appearing on the other — is
also the clearest demo of the product there is.

## 6. What to check in the lines

```jsonc
{"ts":"...","status":403,"model":"","stream":false,"consus_key_id":"","consus_key_label":""}
{"ts":"...","status":200,"model":"MODEL_ID","stream":true,"consus_key_id":"EXAMPLEKEYID","consus_key_label":"example-label"}
```

- `consus_key_id` and `consus_request_id` are populated on authenticated lines.
  This is the attribution claim, and only real gateway traffic proves it.
- **Use two keys, one of them labelled.** `consus_key_label` is empty both when
  a key has no label and when the gateway does not send the header at all, so a
  single unlabelled key cannot tell those apart. Two keys also demonstrate
  multi-user attribution in one file.
- Rejected requests still produce lines. A 4xx from the gateway is a request
  that happened, and the record should say so.
- An agentic client emits several requests per user-visible turn, often a
  second apart. The log records API calls, not conversations.

## 7. Tear down

```sh
orb delete loglab
rm -rf ~/loglab-build
```

## Known divergences from a cloud VM

OrbStack runs machines under LXC — `systemctl status` shows a
`zzz-lxc-service.conf` drop-in — so `PrivateDevices`, `ProtectKernelTunables`,
and `RestrictNamespaces` may be softened relative to a real VM. The interaction
that carries actual risk, `ProtectSystem=strict` against `LogsDirectory=` with
an empty capability set, is fully exercised.

If a deployment needs to be demonstrated on a specific cloud, that is a sales
artifact rather than an engineering one. Worth doing when a customer asks by
name; not worth doing on the way to a release.

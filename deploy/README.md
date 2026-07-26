# Deployment reference

Deploying on a government cloud? Start with the one-page guide for your
platform — each is a single self-contained HTML file, printable, with every
command inline:

- [`aws-govcloud.html`](aws-govcloud.html)
- [`azure-government.html`](azure-government.html)
- [`gcp.html`](gcp.html)

The three follow the same ten steps; only the service names change.

Before a release ships, the files in this directory are exercised on a real
Linux host — see [`local-vm-verification.md`](local-vm-verification.md). That
is a maintainer procedure; skip it if you are here to deploy.

Everything here is example configuration for running one Local Log instance
as a company-wide service. The binary itself needs none of it — these files
exist so that a production deployment is a half-day of copying and reviewing,
not a design exercise. Adapt freely; nothing in this directory is executed or
required by the proxy.

Order of operations:

1. **Service user** — `sudo useradd --system --shell /usr/sbin/nologin consuslog`
2. **Binary** — download a pinned release, verify it (`sha256sum -c
   SHA256SUMS`; see the README's "Verifying a release"), install to
   `/usr/local/bin/consus-local-log`
3. **Service** — [`systemd/consus-local-log.service`](systemd/consus-local-log.service).
   Read the `TimeoutStopSec` comment before deploying: it must be longer than
   your longest model generation. The unit creates the log directory itself.
4. **Verify** — `curl -s localhost:4000/healthz`, send one request through,
   confirm the line lands in `/var/log/consus/`
5. **Shipper** — [`fluent-bit/`](fluent-bit/) tails the directory into your
   SIEM. Also ship the service's stderr (journald), which carries the
   why behind any logging failure.
6. **Rotation** — the cron `find` lines in the README's "Rotation and
   archival", or [`logrotate/consus-local-log`](logrotate/consus-local-log)
   if you standardize on logrotate.
7. **Alerts** — [`alerts.md`](alerts.md). At minimum: entries being lost, and
   the log going quiet.
8. **Enforcement** — once the pilot holds, the egress rule from the README's
   "Making it mandatory": outbound 443 to the gateway from this host only.
   Use REJECT rather than DROP so stragglers fail instantly with a clear
   error instead of hanging until their SDK times out.

Sizing notes:

- **Disk**: 50–100 MB per heavy user per day raw; compression recovers ~10x.
- **Memory**: worst case ≈ `2 × LOCALLOG_MAX_CAPTURE × (256 + concurrent
  requests)`. At the 10 MB default that is multiple GB for a busy shared
  instance — set the cap deliberately and size the host from it.
- **Scale-out**: N instances behind one load balancer, **each writing its own
  directory** — the single-writer guarantee is per process, and two processes
  appending to the same file will interleave. The SIEM merges the streams.

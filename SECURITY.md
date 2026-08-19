# Security policy

## Reporting a vulnerability

Please report suspected vulnerabilities privately, through GitHub's **Report a
vulnerability** button on the [Security
tab](https://github.com/consusindustries/consus-local-log/security) of this
repository. That opens a private advisory only the maintainers can see.

If you cannot use GitHub, open a public issue asking for a private channel —
without any details of the finding — and a maintainer will follow up.

Please include, as far as you can: the version or commit, the configuration
(`LOCALLOG_*` values, with any real key redacted), and the smallest set of
steps that reproduces the behavior. Never include a real API key, a real
prompt, or customer data in a report; fabricated values reproduce these issues
just as well.

We aim to acknowledge a report within three working days and to agree on a
disclosure timeline with you from there. Findings that are reported privately
and confirmed will be credited in the advisory unless you prefer otherwise.

## What is in scope

This is a proxy that sits in the path of authenticated traffic and writes an
audit log, so the interesting failures are about confidentiality of what it
holds and integrity of what it records:

- Anything causing the `Authorization` header, or any other credential, to be
  written to disk, logged, or sent anywhere other than `LOCALLOG_UPSTREAM`.
- Anything causing traffic to be modified in transit rather than relayed
  byte-for-byte.
- Log entries that misrepresent the request or response they describe, or a
  request that is proxied but silently not logged at all.
- Any outbound connection to a host other than `LOCALLOG_UPSTREAM`.
- Log files written with permissions wider than 0640, or directories wider
  than 0750.
- Denial of service reachable by a client of the proxy: unbounded memory
  growth, a hung listener, a panic that takes the process down.

## What is out of scope

- The proxy listens over plain HTTP by design (see the spec's non-goals); it
  is meant to run on localhost or behind the operator's own TLS. "No TLS on
  the listener" is a documented decision, not a vulnerability.
- The proxy performs no authentication or authorization of its own, and does
  not validate API keys. That is the gateway's job.
- The log file contains full request and response bodies by design. Its
  protection is filesystem permissions and the operator's own controls.
- Findings that require an attacker who already has write access to the log
  directory or the ability to run code as the proxy's user.

## Supported versions

Until v1.0.0, only the tip of `main` is supported. Fixes ship there.

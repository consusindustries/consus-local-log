# Contributing

The short version: **read everything, file issues, send no code.**

## Why the source is public

This program sits in the path of controlled traffic inside customer networks.
Its entire trust story is that a security engineer can read the whole source
in one sitting, know exactly what it does, and rebuild the shipped binary
byte-for-byte (see "Verifying a release" in the README). The code is public
so you can do that — audit it, reproduce it, point at specific lines when
something looks wrong.

## Why pull requests are not accepted

Not from strangers, not from friends, not for typos. Every accepted change is
something every downstream security review has to re-read, and a program
whose value is total auditability is one where the changes themselves are the
attack surface. Keeping authorship to a single, deliberately narrow channel
is a feature of the product, in the same way that the empty `go.mod` is.
Pull requests are closed automatically with a pointer to this file; it is not
personal.

## What is genuinely welcome

- **Bug reports** — an issue with the version or commit, the `LOCALLOG_*`
  configuration (redact any real key), and the smallest reproduction you
  have. Fabricated payloads only; never a real prompt, key, or customer name.
- **Security findings** — privately, through the channel in
  [SECURITY.md](SECURITY.md). These get priority over everything else.
- **Documentation gaps and questions** — if the README left you guessing,
  that is a defect in the README; say so in an issue.
- **Feature requests** — read the spec's non-goals first
  (`specifications/consus-local-log-SPEC.md`); most requests are answered
  there deliberately. If yours is not, open an issue describing the problem
  rather than the feature. The usual outcome is a topology answer or a
  documentation fix, not new code — and that is on purpose.

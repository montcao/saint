# Security policy

saint holds API credentials and decides what a sandboxed agent may reach, so a
bug in it can cost someone their keys or their repository. Reports are welcome
and will be taken seriously.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting: **Security → Report a
vulnerability** on this repository. That opens a private advisory visible only
to the maintainers.

Please do not open a public issue for a security bug, and please do not include
a working exploit in the first message — a description of the class of problem
and how to reproduce it is enough to start.

There is no bounty. This is a single-maintainer project; expect an
acknowledgement rather than an SLA.

## Supported versions

There are no released versions yet. Only the current `main` is supported.

## What saint claims, and what it does not

Reports are most useful when they are about a boundary saint actually asserts.
These are the claims worth testing:

- A guest never holds an upstream API key in key mode. The proxy strips the
  session's token and injects the real credential on the host side.
- A session's proxy token identifies exactly one session and cannot be used to
  read another session's usage, credentials, or traffic.
- The host-authoritative copy of a session's token lives outside every mount, so
  a guest cannot rewrite its own identity.
- Repository mounts are bounded by configured mount roots.
- saint's own inspection of a mounted repository never executes anything the
  guest can write there. A guest controls the repository's `.git/config`,
  hooks and `.gitattributes`; when saint runs git on the host to read branch
  and status, git does not read that repository's configuration. This covers
  saint, not your own later `git` commands in that repository, which run its
  configuration as git always does.
- The forward proxy, when enabled, refuses loopback and link-local targets by
  default, including the cloud metadata address.

These are **not** claims, and a report that they fail is a documentation bug
rather than a vulnerability:

- **The proxy does not inspect TLS.** For forward-proxied traffic it sees
  hostnames and byte counts, never content. Data leaving through a host the
  policy allows is not visible to saint.
- **Guest egress proxying is not, on its own, egress control.** The environment
  variables are advisory: a static binary, a non-HTTP protocol, or a guest that
  unsets them all leave unobserved. Enforcement requires putting sessions on a
  network with no other route out, which is designed and measured in `SPIKES.md`
  but not yet built.
- **Subscription mode puts a real credential inside the guest.** This is
  inherent, not a defect: a gateway credential replaces the subscription and
  bills per token, so plan billing requires the guest to hold its own token. The
  interface says so where the choice is made. Key mode is the mode where the
  proxy holds custody.
- **The microVM is the isolation boundary, not the agent's own permission
  prompts.** Sessions created with permissions skipped are behaving as
  documented.
- Sessions sharing a container network can reach each other. Measured, recorded
  in `SPIKES.md`, and not yet addressed.

## Handling of secrets in this repository

Every credential-shaped string in the tests is a fixture. If you believe you
have found a real key, token, or personal detail committed here, report it
through the same private channel rather than opening an issue.

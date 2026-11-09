# Configuration

Reference for what the [README](../../README.md) leaves out: who pays for a
session, where secrets live, what a session can see, and what the token counts
mean. Settings live in `~/.saint/config.json`.

## Known limits

- No signed releases, so `make allow-firewall` is a required install step.
- Token counts carry no cost figure, and will not match the agent's own (see
  [What the token counts mean](#what-the-token-counts-mean)).
- OpenAI context windows are unlisted, so Codex sessions show counts without a
  percentage.
- Guest egress proxying is opt-in, and records traffic rather than blocking it.
- Sessions sharing a container network can reach each other's microVMs.
- Which account subscription traffic bills is visible only on claude.ai.

## The API proxy

Every session's API traffic goes through a local proxy, which authenticates it,
attributes it to a session, and meters it. What it does with credentials
depends on the session's mode.

### Who pays: `key` vs `subscription`

| | `key` | `subscription` |
|---|---|---|
| Credential lives | on the host, in the proxy | in the guest |
| Guest sends | a random per-session token | its own Claude OAuth token |
| Proxy does | strip it, inject the real key | forward it untouched |
| Billed to | the account owning the API key, per token | your Claude plan |

**You cannot have both properties.** A claude.ai subscription is billed as the
plan only while Claude Code holds it and the gateway forwards it verbatim; a
gateway that injects its own credential replaces the subscription. So in `key`
mode the proxy is a credential boundary, and a compromised agent never sees a
real key. In `subscription` mode it is **an observer, not a boundary**: the
guest holds the subscription token. That is the price of plan billing, and
saint states it rather than implying it away.

Attribution survives either way: in `subscription` mode the session token
travels in `x-saint-session` (set through `ANTHROPIC_CUSTOM_HEADERS`) and is
stripped before the request leaves the host. The mode comes from the session
record, not the request, so a guest cannot talk the proxy into handing over a
real key.

`proxy_auth` in `~/.saint/config.json` selects the mode:

| Value | Meaning |
|---|---|
| `auto` (default) | subscription when a token resolves, otherwise `key` |
| `key` | always the API key |
| `subscription` | prefer the subscription; fall back to `key` with a warning |

The resolved mode is shown by `saint auth` and `saint doctor`. saint is never silent about which account is paying. Codex
sessions are always `key`.

### Setting up

Authentication is once per install, not per session. Sessions never prompt.

```bash
saint auth              # what is set, and which account new sessions bill
saint auth claude       # store a Claude subscription token
saint auth anthropic    # store an Anthropic API key
saint auth openai       # store an OpenAI API key
saint auth mode key     # auto | key | subscription
saint auth clear claude
```

For subscription billing a session picks one of two credentials:

| Credential | Lifetime | Setup |
|---|---|---|
| `live login` | hours | none; saint reads your existing Claude Code login |
| `stored token` | one year | `claude setup-token`, then `saint auth claude` |

saint never copies your live login: it reads it only when a session is created,
and macOS prompts once for keychain access. **Both credentials are shared by
every session.** The short-lived one shrinks the blast radius in time, hours of
exposure rather than a year, not in scope. Only saint's per-session proxy token
is unique, and deleting a session revokes it.

A session gets **whatever life is left** on the live login at creation. Under
fifteen minutes saint refuses it rather than start a session that would be
stranded mid-run.

**A session's credential is a copy.** It is written into the container's
environment at creation and never sees a refresh, which is why the plan login
can have hours left while a session's credential has expired. Start a new
session to get a fresh one; repositories are host mounts, so work on disk
carries over.

The token reaches the container through `container run --env-file`, never argv,
from a 0600 file removed as soon as the run returns and never written into the
guest-writable session directory.

### Where secrets live

Each credential names its own store in `~/.saint/config.json`:

```json
"credentials": {
  "claude_subscription": "op://Private/Claude Code/token",
  "anthropic": "keychain",
  "openai": "cmd:my-secret-script"
}
```

| Spec | Source |
|---|---|
| `keychain` or omitted | macOS Keychain, under the credential's own name |
| `keychain:<account>` | macOS Keychain, explicit account |
| `op://<vault>/<item>/<field>` | 1Password, via the `op` CLI |
| `cmd:<command>` | the single line a command prints |

A `cmd:` entry runs code from your own config file, the same trust boundary as
Claude Code's `apiKeyHelper`: anything that can edit `~/.saint/config.json` can
already run code as you. Output of more than one line is rejected.

In key mode the host-authoritative copy of each session token lives in
`~/.saint/tokens/`, which is **not** mounted into any container, so a guest
cannot rewrite its token to impersonate another session. Two sessions
presenting the same token are both refused.

### Proxy settings

The proxy starts on demand, the first time a session needs it, and keeps
running in the background; its output is in `~/.saint/proxy.out`. If it stops,
containers keep running but their agents lose API access until a pane or
`saint attach` starts it again. `saint proxy` runs it in the foreground.

| Key | Default | Meaning |
|---|---|---|
| `proxy_bind` | `auto` | discovers the container bridge gateway at runtime; a guest cannot reach host loopback |
| `proxy_port` | `8787` | listen port |
| `proxy_network` | `default` | network whose gateway is discovered |

## Sessions

### What a session can see

A guest can read and write every directory mounted into it, so what gets
mounted is the real boundary. A directory does **not** have to be a git
repository; it only has to sit under a **mount root**. `mount_roots` in
`~/.saint/config.json` names them, and empty means your home directory:

```json
"mount_roots": ["~/Dev", "/Volumes/work"]
```

Refused regardless of the mount roots, after resolving symlinks:

- system directories: `/etc`, `/usr`, `/bin`, `/sbin`, `/proc`, `/sys`, `/dev`,
  `/System`, `/Library`, `/Applications`, `/opt`, `/nix`, `/Network`, `/cores`
- `/tmp` and `/private/tmp`
- your home directory itself, and `~/.saint`
- credential and tool-state directories: `~/.ssh`, `~/.aws`, `~/.gnupg`,
  `~/.config`, `~/.claude`, `~/.cursor`, `~/.docker`, `~/.kube`, `~/.gcloud`,
  `~/.azure`, `~/.npm`, `~/.gem`, `~/.cargo`, `~/Library`

Containers stay on the runtime's default network with normal internet access,
so `npm install`, `git fetch` and the agents' web tools work. That buys key
custody and attribution, **not** egress control.

### Permissions and pinned runtimes

By default the agent edits without asking: the microVM is the boundary, and an
agent that stops to ask before every edit inside its own sandbox is mostly
friction. Use `saint new -ask` when the mounted contents matter more than the
speed. herdr panes always skip prompts.

The images pin exact agent versions, and sessions run with
`DISABLE_UPDATES=1` so an agent cannot fetch and execute a new version of
itself inside the sandbox. Update a runtime by bumping the version in
`images/<runtime>/Dockerfile` and rebuilding; `saint doctor` prints the command.

### Agents and providers

| Agent | Provider |
|---|---|
| `claude` | Anthropic |
| `codex` | OpenAI |
| `hermes` | planned, not yet integrated |

When an agent reaches a provider outside the proxy's upstream allowlist, saint
is **not in the path**: that traffic is direct, unmetered and uncustodied, and
the journal has no record of it. Widening the allowlist is deliberately not the answer;
the closed list is what stops a credential-injecting hop becoming a pivot.

## What the token counts mean

saint counts what crossed the proxy. The agent counts its own conversation.
They will not agree, and the gap is not a bug.

- **Each request is counted in full.** A Claude Code turn re-sends the whole
  history, mostly as cache reads, so summing a long session's records gives
  roughly *conversation size × turns*: large, correct, and matching nothing
  the agent shows.
- **saint meters requests the agent does not show you**, such as title
  generation and subagent calls under the same session token.
- **Cache tokens are separate counters**: `in`, `out`, `cache read` and
  `cache write` are recorded per request.

saint publishes no cost figure. Prices differ by model, by cache read versus
write, and by billing mode.

Metering is observation only: it never rewrites a response, and a parse failure
abandons the measurement, never the request. Prompts and completions are never
retained; only counters and the model name survive.

### The request journal

Per-request records and usage go to `~/.saint/proxy.jsonl`, read back with
`saint proxy log` (`--session`, `--json`, `-n`). The journal holds session IDs,
methods, paths, providers, auth modes, statuses, durations and token counters.
Never a credential, a session token, a prompt or a response body.

Writing never blocks a request; a full queue drops records and writes the count
of drops as its own record rather than leaving a silent gap. It rotates by
size:

| Key | Default | Meaning |
|---|---|---|
| `proxy_log_requests` | `true` for a new install | write per-request records; usage is always written |
| `proxy_log_max_bytes` | `4194304` | rotate at this size |
| `proxy_log_keep_files` | `5` | files retained, so at most ~20 MiB |

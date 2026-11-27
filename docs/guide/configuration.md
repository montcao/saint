# Configuration

Reference for what the [README](../../README.md) leaves out: who pays for a
session, where secrets live, what a session can see and reach, and what the
token counts mean. Settings live in `~/.saint/config.json`.

`~/.saint` is the state directory: config, sessions, tokens and the request
journal. Set `SAINT_HOME` to an absolute path to use another one, for example
to run a second copy of saint from another checkout without either touching
the other's sessions or proxy. Set it in the environment herdr is started
from, so the panes and the proxy they start all see it. Paths below that say
`~/.saint` mean the state directory.

## Known limits

- No signed releases, so a session whose proxy the macOS firewall blocks asks
  to let saint through it (`saint firewall`; macOS asks for your password in
  its own dialog). The proxy runs from a fixed
  copy at `~/.saint/bin/saint` so the exception is not tied to herdr's install
  folder; whether it survives that copy being replaced by an update is not yet
  measured.
- Token counts carry no cost figure, and will not match the agent's own (see
  [What the token counts mean](#what-the-token-counts-mean)).
- OpenAI context windows are unlisted, so Codex sessions show counts without a
  percentage.
- Network decisions are per hostname, for every session at once: an allowed
  host is allowed for all of them, and saint cannot see what is sent to it.
- A session started outside herdr (`saint new` in a plain terminal) has nobody
  to ask, so any host not already allowed is refused; use `saint allow`.
- Sessions created before each session had its own network keep the shared
  network, with open internet access, until recreated.
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
containers keep running but their agents lose API and network access until a
pane or `saint attach` starts it again. `saint proxy` runs it in the foreground.

The proxy listens on each running session's own network, on that network's
gateway, which exists only once the session's container runs. A pane (or
`saint new` and `saint attach`) asks the running proxy to listen there through
its control socket, `~/.saint/proxy.sock`; a proxy that restarts listens on
every running session's network by itself. `saint doctor` asks the proxy which
networks it serves and checks each can be reached.

Each session's guest is given the proxy's address, port included, when the
session is created, so changing `proxy_port` strands existing sessions until
they are recreated.

| Key | Default | Meaning |
|---|---|---|
| `proxy_port` | chosen once per state directory | listen port; a free port is picked on first run and written here, and a value you set is kept |
| `proxy_bind` | `auto` | only for sessions from before per-session networks: where the proxy listens for them; `auto` is the gateway of `proxy_network` |
| `proxy_network` | `default` | only for those older sessions: the shared network they run on |

## Network access

Each session runs on an internal container network of its own. Measured on
Apple `container`, a guest there has no route out and no resolver
([spikes](../research/spikes.md)): its only way to any host is saint's proxy,
on its network's gateway. The guest's `HTTP_PROXY` and `HTTPS_PROXY` (both
cases) point there, with the session token as the proxy password. Sessions
cannot reach each other's microVMs, and a session's token is accepted only on
its own network.

Every host a session asks for is decided in this order:

| | Result |
|---|---|
| on `proxy_deny_hosts` | refused, never asked |
| on `proxy_allow_hosts` (your saved hosts) | allowed |
| shipped with saint: the provider APIs, GitHub (`github.com` and `codeload.github.com`, not its user-content hosts), npm, PyPI, the Go proxy, crates.io, and Claude Code's updater and logging intake | allowed |
| anything else | asked in the session's herdr pane |

When a host is asked about, the session's pane is marked **blocked** with the
host in the message, and a prompt opens in a split below it. saint does not
move you there: the blocked status is the signal, and focus stays where you
left it.

- **Allow always** lets the request through and saves the host for every
  session, running ones included.
- **Deny** refuses this request only. It is not remembered.
- Esc, `q` or closing the prompt deny. So does no answer within 2 minutes:
  the request fails with a 403 rather than hanging.

Keys pressed in the moment after the prompt appears are ignored, so a key
pressed while clicking into it cannot answer a question you have not read. Once nothing is waiting, the pane goes
back to herdr's own reading of the agent. A session with no herdr pane, or
whose pane is gone, is refused at once and the reason is written to
`~/.saint/proxy.out`. So is one whose pane ID now names a different pane, as
happens after herdr restarts; reattaching the session records its pane again.

```bash
saint allow                  # saved and shipped hosts
saint allow registry.example # save a host
saint allow '*.example.com'  # a domain and its subdomains
saint allow -rm example.com  # remove a saved host; it is asked about again
```

Saved hosts live in `proxy_allow_hosts` in `~/.saint/config.json`, and a change
applies from the next request. Every decision is in the request journal
(`saint proxy log`) with its reason: shipped allowlist, saved rule, answered,
or no answer. saint sees the host and the bytes crossing; it never sees inside
TLS, so a host you allow can be sent anything.

### Claude Code's own traffic

Claude Code sends metrics and error reports to a Datadog log intake and checks
`downloads.claude.ai` for updates. Both ship allowed (`*.datadoghq.com`, every
Datadog region) so a new install is not asked about them. To refuse them, run
`saint telemetry off`, or the herdr action "saint: turn Claude Code telemetry
on or off":

- The proxy refuses `downloads.claude.ai` and `*.datadoghq.com` for every
  session from the next request, ahead of anything you saved, without asking.
- New Claude sessions start with `DISABLE_TELEMETRY`, `DISABLE_ERROR_REPORTING`,
  `DISABLE_AUTOUPDATER` and `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` set, so
  Claude Code stops trying. A running session keeps its environment until it is
  recreated; its attempts are refused meanwhile. The last switch also turns off
  `/feedback` and Claude Code's feature flags.

`saint telemetry on` (or the action again) allows them again. The setting is
`claude_telemetry_off` in `~/.saint/config.json`. Allowing Datadog has a cost:
its intake accepts logs for any Datadog account, so an allowed guest could send
data to an account of its own.

## Passive proxy summary

Each attachment requests a one-line title in herdr's own pane display: unique
hosts reached, policy refusals and whether the guest holds a real subscription
credential or only the proxy placeholder. Counts cover observations since the
proxy started, independent of request-journal settings. A host counts only
after a successful connection or upstream response; allowed policy alone does
not establish reachability. Host tracking is bounded and shows `+` at its cap.

Only changes update metadata. No activity pane opens, no focus changes and no
redraw timer runs. Network questions still use herdr's `blocked` state when a
person must act. `saint proxy log --session <session>` provides the full journal
on demand. Live herdr metadata rendering remains unverified on macOS.

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

What a session can reach on the network is in
[Network access](#network-access).

### Your Claude Code setup inside a session

**Off until you turn it on.** A new session starts with none of your Claude Code
setup and no git identity. To share it with new Claude sessions:

```bash
saint claude-config on    # share with new sessions
saint claude-config off   # stop, including when a session renews
saint claude-config       # say which it is and what is shared
```

**What turning it on exposes.** The agent can read everything shared. It can
also send what it reads out through any host its session may reach, the same as
your own agent on the host could. Turning it on is how you approve that.

`~/.claude` is never mounted, because it also holds every prompt you have typed,
every transcript and your login state. Instead, when a Claude session is
created, saint copies a filtered snapshot into `~/.saint/guest-config/<session>`
and mounts it read-only. The agent sees it as its own `~/.claude`. If
`CLAUDE_CONFIG_DIR` is set, saint copies from there instead.

Everything is denied unless it is on this list:

| Copied | Not copied |
|---|---|
| `CLAUDE.md` | `~/.claude.json`: your account and MCP servers |
| `settings.json`, reduced to `model`, `outputStyle`, `autoMode`, `tui`, `language`, `alwaysThinkingEnabled`, `cleanupPeriodDays`, `includeCoAuthoredBy`, `spinnerTipsEnabled`, and the `deny` rules of `permissions` | settings keys that hold secrets or run commands: `env`, `apiKeyHelper`, `hooks`, `statusLine`, plugins, every MCP key, and permission `allow` rules (they record approved commands word for word, and a command can carry a token) |
| `skills/<name>/`, only folders Claude Code would load as a skill (a skill file at their top level) | history, transcripts (`projects/`), credentials, plugins |
| `agents/`, `commands/`, `output-styles/` | symlinks at any level, including a skill installed as a link to a folder elsewhere (the folder's other files would cross with it), and files that look like credentials (`.env`, `*.pem`, `*.key`, `id_*`) |

- **Sharing a linked skill:** copy its folder into `~/.claude/skills` instead
  of linking it.
- **Size limits:** each file is limited to 2 MiB and the snapshot to 32 MiB.
- **Skipped files:** anything left out is named in saint's log, with the reason.
- **MCP servers stay off.** Their definitions often carry tokens.
- **It's a snapshot, not a live view.** A change on the host reaches new
  sessions only. Renewing a session's credential keeps the copy the session
  was created with, for the same reason a changed skill blocks renewal: nobody
  reviewed the new version for that session. Turning sharing off applies at the
  next renew.
- **`settings.json` is a copy.** The agent can change its own copy, but the
  snapshot replaces it each time the session starts, so the host stays the
  source.
- **Commits carry your name.** The guest gets your git `user.name` and
  `user.email`, and nothing else from your git configuration, as
  `GIT_AUTHOR_*` and `GIT_COMMITTER_*`.

### Permissions and pinned runtimes

By default the agent edits without asking: the microVM is the boundary, and an
agent that stops to ask before every edit inside its own sandbox is mostly
friction. Use `saint new -ask` when the mounted contents matter more than the
speed. herdr panes always skip prompts.

saint does not accept Claude Code's bypass-permissions screen for you. The first
time a session starts, Claude Code asks, and accepting it is your statement that
the microVM, not the agent's prompts, is the boundary.

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

Only the two providers above go through the proxy's credential path. Any
other provider an agent calls is ordinary forward-proxied traffic: asked
about like any host, never given a credential by saint, and not metered.
Widening the credential path is deliberately not the answer; the closed list
is what stops a credential-injecting hop becoming a pivot.

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

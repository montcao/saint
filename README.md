# saint

**Run AI coding agents in isolated Apple `container` microVMs, with a local
proxy that holds the credentials and meters every request.**

Each session is Claude Code or Codex running *unmodified* in its own microVM,
with your repositories mounted in. A local proxy sits between every agent and
its provider and is both the credential boundary and the usage meter.

It exists because agentic development means several agents running largely
unattended, against real credentials and real repositories. The blast radius is
large and nobody is watching.

saint is a plugin for [herdr](https://herdr.dev), the terminal workspace for
coding agents. herdr owns panes, layout and agent status; saint opens panes
whose agent is already running in a microVM, and owns the proxy, metering and
egress control.

## What it does

- One microVM per session; repositories mounted read-write, mount roots bounded
- In key mode the proxy holds the credential and the guest gets a per-session
  token, never your API key
- Per session: a live Claude Code login, a stored subscription token, or an API
  key, with remaining life shown *before* the session is created
- Per-request token metering, and the hosts each session reached, without the
  agent cooperating, in a journal read with `saint proxy log`
- herdr shows each sandboxed agent as working, idle, or blocked waiting for you
- Pinned agent versions with self-update blocked

**Never show an invented number.** An unknown is `unknown`: no context
percentage for a model whose window is unverified, `tokens=unreported` where a
provider sends no counters, `no git` for a directory with no history. A
confident wrong number is worse than a gap.

## What it is not

**The proxy is a credential boundary and a meter. It is not egress control.**
The forward proxy can record every host a session reaches, but it is off by
default, and the environment variables that point a guest at it are advisory: a
static binary, a non-HTTP protocol, or a guest that unsets them all leave
unobserved. Enforcement is designed and measured in [`docs/research/spikes.md`](docs/research/spikes.md) but
**not built**.

saint does not inspect TLS: for forward-proxied traffic it sees hostnames and
byte counts, never content.

## Status

Working, installed from source. Not yet packaged or released.

Known gaps:

- no signed releases, so `make allow-firewall` is a required install step
- token counts carry no cost figure
- saint's token counts will not match the agent's own; they are different
  measurements (see [What the token counts mean](#what-the-token-counts-mean))
- OpenAI context windows are unlisted, so Codex sessions show counts without a
  percentage
- guest egress proxying is opt-in and is visibility, not enforcement
- sessions sharing a container network can reach each other's microVMs
- which account subscription traffic bills is visible only on claude.ai, not
  from saint

## Get started with herdr

saint runs inside [herdr](https://herdr.dev): a saint action opens a pane
beside the one you are in, and the agent in that pane is already running in a
microVM with that pane's folder mounted.

### 1. Requirements

- macOS `26` or newer, on Apple silicon (`arm64`)
- Apple's [`container`](https://github.com/apple/container) CLI `1.3.0` or newer
- herdr `0.9.5` or newer
- Go, to build saint from source (`go.mod` names the version)

These are minimums; `saint doctor` checks them and accepts newer releases.

### 2. Build saint and its agent images

```bash
git clone https://github.com/montcao/saint
cd saint
make build                    # builds ./saint, the binary herdr will run
container system start        # if the container service is not running
./saint doctor --build-images # check the setup and build the agent images
```

`saint doctor` checks the macOS and `container` versions, the runtime service,
the agent images, keychain entries, whether the proxy is reachable (and whether
the firewall is what blocks it), and that global `core.hooksPath` is unset.
Where a fix is a command, doctor prints it.

### 3. Let the proxy through the firewall

```bash
make allow-firewall BINDIR="$PWD"   # asks for your password
```

Containers reach saint's proxy on the container bridge, not on loopback, and
the macOS application firewall filters that interface. Until `saint` is allowed
through, connections are **dropped rather than refused**, so agents hang on
every API call instead of reporting an error. A `go build` binary is ad-hoc
signed, so "allow signed software" does not cover it, and the allowance is
tied to the binary's path: allow the binary herdr runs, which is this
checkout's `./saint`. A rebuild can need it again. Signed releases will remove
this step.

### 4. Add a credential

Once per install; sessions never ask.

```bash
./saint auth claude      # a Claude subscription token, from `claude setup-token`
./saint auth anthropic   # or an Anthropic API key, held by the proxy
./saint auth             # what is set, and which account new sessions bill
```

[Who pays](#who-pays-key-vs-subscription) explains the difference.

### 5. Add saint to herdr

```bash
herdr plugin link "$PWD"
```

Then bind the actions to keys in `~/.config/herdr/config.toml`:

```toml
[[keys.command]]
key = "prefix+s"
type = "plugin_action"
command = "saint.new"
description = "sandboxed Claude pane"

[[keys.command]]
key = "prefix+x"
type = "plugin_action"
command = "saint.new-codex"
description = "sandboxed Codex pane"
```

Pick any free keys. Without a binding, run the action from a shell in herdr:
`herdr plugin action invoke saint.new`.

`herdr plugin install montcao/saint` also works and runs `make build` for you,
but builds into a directory herdr manages: find it as `plugin_root` in
`herdr plugin list`, and run steps 2 to 4 with that directory's `./saint`.

### 6. Open a sandboxed pane

Focus a pane in the project you want to work on and press your key. The new
pane:

1. offers to start Apple's container service if it is stopped;
2. creates a session for the focused pane's folder, in its own microVM;
3. starts the credential proxy if it is not already running, detached so it
   outlives the pane;
4. attaches you to the agent, which herdr tracks as idle, working or blocked.

The first time, Claude Code asks you to accept bypass-permissions mode: the
microVM is the boundary, so saint runs agents without per-action prompts.

Closing a pane ends only its attach; the agent keeps running in its microVM.
`./saint ls` lists sessions. To reopen one in a pane:

```bash
herdr plugin pane open --plugin saint --entrypoint session \
  --env SAINT_SESSION=<session> --env HERDR_AGENT=claude
```

If a pane reports an error, it stays open until you press Enter.
`./saint doctor` diagnoses most of them.

### Without herdr

`make install` puts `saint` in `~/.local/bin` (override with `PREFIX=` or
`BINDIR=`), and `make allow-firewall` allows that copy. `saint new <repo>` and
`saint attach <session>` then work in any terminal.

## Commands

```bash
saint new [-rt codex] [-ask] <repo>...  # create a session and attach, outside herdr
saint attach <session>  # reattach; starts it and the proxy if needed
saint ls                # sessions, their state, and files to review
saint stop <session>    # stop a session's microVM, keep the session
saint rm <session>      # delete a session; your repositories are not touched
saint doctor            # check the setup
saint proxy log         # what the agents called, most recent first
SAINT_DEBUG=1 saint ls  # verbose logging to ~/.saint/saint.log
```

A session is a `<session>` by name, ID, ID prefix or container name
(`saint-<id>`). `saint new` and `saint attach` start the proxy the same way a
herdr pane does.

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

A session gets **whatever life is left** on the live login at creation, and the
spawn form shows it before you commit. Under fifteen minutes saint refuses it
rather than start a session that would be stranded mid-run.

**A session's credential is a copy.** It is written into the container's
environment at creation and never sees a refresh, which is why the plan login
can have hours left while a session's credential has expired. `c` renews it by
replacing the microVM: the agent loses its conversation and shell history, but
work on disk survives because repositories are host mounts. Renewing refuses
rather than falling back to an API key, because repairing a credential should
not quietly change which account pays.

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

## Contributing

```bash
make check    # gofmt, go vet, race-enabled tests, structural tests
```

Run it and quote the output. "Should work" is not a result. Two rules matter
more than style:

- **Never show an invented number.** Every display has a state meaning "not
  measured", distinct from zero.
- **Nothing machine-local in code or tests.** No absolute home paths, no
  discovered IP addresses, no exact host versions as gates.

[`AGENTS.md`](AGENTS.md) is the map for contributors and agents: the
commands, every rule in one line, and where each is written in full.
[`ARCHITECTURE.md`](ARCHITECTURE.md) says which package owns what, and
[`docs/research/spikes.md`](docs/research/spikes.md) what was measured rather than
assumed.

## Security

saint holds API credentials and decides what a sandboxed agent may reach.
[`SECURITY.md`](SECURITY.md) describes how to report a vulnerability privately,
and which boundaries saint does and does not claim.

## License

Apache License 2.0. See [`LICENSE`](LICENSE).

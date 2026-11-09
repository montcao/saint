# Spikes

What was measured rather than assumed, and what saint's code and security
claims rest on.

Each section names the runtime it was measured on. Most findings are about
Apple's `container` CLI on macOS (Apple silicon), because that is the only
sandbox backend saint has today. They describe that runtime's behaviour, not
a requirement of saint: a different runtime, a later `container` release or
another host may behave differently, and nothing here is a value saint
hardcodes. Before relying on a finding on your setup, re-run the probe it
describes. When a runtime surprises you, measure it and add a section.

## Apple `container`: runtime basics

- **Listing.** `container ls -a --format json` works, and an empty list is
  `[]`. saint parses the JSON, never the table. `container image inspect` does
  not accept `--format`.
- **Mounts.** A repo mounted with `-v /path:/path` appears in the guest at the
  same absolute path. `:ro` is enforced (`Read-only file system`), so a
  read-only mount is a real boundary.
- **Persistence.** Files written in the guest survive `container stop` and
  `container start`.
- **Reaching the host.** A guest cannot reach host loopback without
  `container system dns create ... --localhost`, which needs `sudo`. There is
  no built-in `host.container.internal` name. saint binds its proxy to the
  bridge gateway instead, discovered at runtime because the subnet differs
  between hosts and networks.
- **The macOS application firewall.** A binary can bind the bridge address,
  show as listening in `lsof`, and still have every connection time out until
  it is allowed through the firewall (`socketfilterfw --add` and
  `--unblockapp`). The allowance is keyed by binary path. A probe with
  `python3 -m http.server` is misleading, because `/usr/bin/python3` is
  usually already allowed. A successful bind proves nothing, which is why saint
  probes its own listener and `saint doctor` separates "not running" from
  "bound but unreachable".
- **A stopped service is not a missing runtime.** After a reboot the CLI can
  be on `PATH` with its service unregistered. Every command then exits 1, and
  only stderr tells the cases apart. `container ls` prints
  `XPC connection error: Connection invalid` and
  `Ensure container system service has been started with`
  `` `container system start` ``.
  `sandbox.classifyRunError` matches those phrases. A CLI that rewords them
  falls through to an unclassified error, shown as `unknown`.

## Subscription auth through the proxy

From the Claude Code gateway documentation, and confirmed on a live
subscription-mode session:

- A gateway credential variable (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`)
  or an `apiKeyHelper` *replaces* the subscription. Usage is then billed per
  token to whoever owns the forwarded credential.
- `ANTHROPIC_BASE_URL` alone, with no credential variable, keeps the saved
  claude.ai login active, along with its limits and billing.
- The gateway must forward `anthropic-beta` verbatim. It carries an OAuth
  capability, and stripping it fails the request with `401`.
- Headers and body fields are open lists. A gateway pinned to the fields that
  existed when it was written breaks on the next release that adds one.

So subscription billing and proxy-held credentials are mutually exclusive, and
saint offers both modes rather than pretending otherwise.

What the live session showed:
- `ANTHROPIC_CUSTOM_HEADERS=x-saint-session: <token>` attributes requests to
  the session. Every proxied request produced a usage record for the right
  session, which a wrong syntax could not do.
- The guest held `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_BASE_URL` and
  `ANTHROPIC_CUSTOM_HEADERS`, with `ANTHROPIC_API_KEY` and
  `ANTHROPIC_AUTH_TOKEN` absent. Either of those two would silently switch the
  session to API billing.
- `container run --env-file` keeps the token out of argv. Passing it with
  `--env` instead would expose it through `ps(1)` to every process of the same
  user.

Not verifiable from saint: which account the usage lands on. Only the
provider's usage pages show that.

## Metering saw nothing in gzipped responses

This applies to any Go proxy, not one runtime.

Every metered response once carried no model and no token counts. Go's
`http.Transport` only decompresses responses whose `Accept-Encoding` it added
itself. A client that asks for gzip explicitly, as Claude Code does, gets the
compressed stream untouched, so the meter scanned gzip bytes for `data:`
frames and found none.

The fix decodes the meter's copy only, never the bytes the guest receives. It
holds an encoded body to a bound and decodes it once at the end. An encoding
saint does not read (`br`, `zstd`) reports nothing rather than guessing.

It went unnoticed because the counts were absent, not wrong, and
`tokens=unreported` is a state the interface shows calmly by design.

## Egress: an internal network is a real boundary

Measured on Apple `container`. On a network created with
`container network create --internal`:

| Probe from the guest | Result |
|---|---|
| TCP straight to a public address | timeout |
| DNS lookup | `EAI_AGAIN`, because the guest has no resolver of its own |
| `CONNECT` through a proxy on the host bridge, then TLS | completes against the real certificate |

The guest reaches exactly what the host proxy is willing to reach for it. Name
resolution happens on the host, inside the proxy, so DNS as an exfiltration
channel is absent rather than policed. Unsetting `HTTP_PROXY` in the guest
does not open a path; it closes the only one.

**Containers on one network can reach each other.** A probe from one
container to another on the same internal network got `ECONNREFUSED`: the
packet arrived and the peer answered. Isolating sessions needs a network per
session, not one per install.

**The bridge address exists only while a container is attached.** Binding the
gateway before any container exists fails with
`bind: can't assign requested address`. `container network ls` reports the
gateway from configuration, not from a live interface. Measured:

| Step | Result |
|---|---|
| `container create` on the network, not started | bridge not raised |
| `container run -d` on the network | bridge up in under a second, before guest userspace |
| bind a host listener once the address appears | succeeds |
| remove the last container while the listener is bound | address goes away, the listener survives |
| start a new container afterwards | reaches the still-bound listener with no rebind |

So only the first bind is hard. The sequence that handles it:
1. Start the container with its init process waiting, not running the agent.
2. Poll for the gateway address, with a bound.
3. Bind the proxy.
4. Launch the agent with `container exec`.

Nothing runs in the guest between the bridge coming up and the proxy binding.

Not measured: whether `--dns` can point a guest at the host proxy for
resolution.

## PID 1 in a saint container

Measured on Apple `container`. The tmux and shell findings hold for any Linux
guest.

- **Use `--init`.** PID 1 in a namespace ignores signals left at their default
  disposition. A shell run directly as PID 1 never saw `container stop`'s
  SIGTERM and was killed after a timeout of several seconds. With
  `container run --init` the stop took a fraction of a second, and orphans are
  reaped. `trap` is not a substitute: a shell runs traps only between
  commands, and the loop spends its time in `sleep`.
- **`tmux new-session` returns 0 for an agent it cannot run.**
  `tmux new-session -d -s agent /nonexistent; echo $?` prints `0`; the session
  is gone a moment later. Checking that the session still exists after a
  second catches this, and was measured to leave a healthy agent alone.
- **No `set -e` in a PID 1 script.** One failed write, such as an unwritable
  `$HOME`, exits PID 1 and stops the container before the agent starts. Under
  `set -u` alone, the same failure leaves the agent running.

## Host-side git ran guest-planted commands

This applies to any setup that runs git on the host against a repo a guest can
write to.

saint's periodic refresh ran `git status` on the host, and a mounted repo's
`.git` is writable from the guest. Three plants each ran a command on the host
from that call, with no user action:
- `core.fsmonitor` in `.git/config`;
- a `[filter]` clean command plus `* filter=<name>` in `.gitattributes`;
- `core.fsmonitor` reached through an `[include]`.

`-c core.fsmonitor=false` blocks the first and not the second, so a denylist is
not a fix.

The fix (`internal/tui/git_isolated.go`) runs git against a throwaway git
directory. It holds:
- a config saint writes;
- a validated copy of HEAD;
- symlinks to the real index, objects and refs.

System and global config are off, submodules are ignored and optional locks are
off. All three plants then ran nothing. Branch and dirty count were still
right, linked worktrees still read correctly, and the real index was left
untouched. The regression test runs plain git first to prove each plant is
live.

## herdr: a plugin pane can host a sandboxed session

Measured with herdr 0.9.5 and Apple `container`, in an isolated named herdr
session.

**One pane command serves every session, chosen through `env`.** Plugin pane
commands are fixed in the manifest. `herdr plugin pane open --env KEY=VALUE`
passes an environment to that command, so one host wrapper can read
`SAINT_SESSION` and exec `container exec -it <container> ...`.
- Both `placement = "split"` and `"tab"` work. A split needs `--target-pane`,
  and combining it with `--workspace` is rejected.
- The pane starts in the plugin root, so relative commands resolve.
- Closing the pane kills only the `container exec` client. The container keeps
  running.

**herdr can track the agent's state through the wrapper.**
- With no hint, herdr reports the pane as `unknown`, because its process is
  `container`.
- With `HERDR_AGENT=claude` set on the host wrapper, herdr reads the guest's
  screen and reports the agent. A hint set only inside the guest is invisible
  to herdr.
- `herdr pane report-agent --source custom:<name> --agent <a> --state <s>`
  sets `working` or `blocked` directly, overrides screen detection, and wakes
  `herdr agent wait`. saint's proxy already knows when a request is in flight,
  so it can report `working` itself.
- `herdr pane release-agent` (which needs both `--agent` and `--source`)
  returns the pane to screen detection.

Gotchas:
- `agent wait` on a pane that herdr does not yet treat as an agent fails at
  once with `agent_not_found`.
- herdr's plugin registry is global per user (`~/.config/herdr/plugins`).
  `HOME`, `XDG_CONFIG_HOME` and `HERDR_CONFIG_PATH` do not isolate it.
- A shell inside a herdr pane inherits `HERDR_SOCKET_PATH`, so herdr commands
  run from it act on the live session.

Not verified: `working` and `blocked` produced by a real authenticated agent
through the wrapper.

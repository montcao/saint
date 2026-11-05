# SPIKES

What was measured on real hardware rather than assumed. Each section is dated
and names the host versions it ran on; those are observations from one machine,
not values saint hardcodes or requires. When a runtime behaviour surprises you,
measure it and add a section here.

## First runtime checks (2026-08-25)

macOS `26.3` on `arm64`, `container` `1.3.0`.

- **Listing:** `container ls -a --format json` works, and an empty list is `[]`.
  saint uses the JSON form rather than parsing the table.
  `container image inspect` does **not** accept `--format`.
- **Mounts:** a repo mounted with `-v /path:/path` appears inside the guest at
  the same absolute path, `:ro` is enforced (`Read-only file system`), and
  writes to a mounted session directory appear on the host.
- **Persistence:** a file written in the guest survives `container stop` and
  `container start`.
- **Networks:** on the default network the guest has internet access and its
  default route is the bridge gateway. On a network created with
  `container network create --internal`, DNS and internet fail while a host
  server bound on the bridge interface is still reachable. With
  `--network none` there is no route at all. Reaching a host service bound only
  to loopback would need `container system dns create ... --localhost ...`,
  which requires `sudo`.
- **Host reachability and the firewall:** a probe with `python3 -m
  http.server` was reachable from a guest, but that did not generalise:
  `/usr/bin/python3` is already allowed by the macOS application firewall. A
  freshly built `saint` binds the bridge address and `lsof` shows it
  listening, yet every connection times out, from host and guest alike, until
  the binary is allowed through
  (`socketfilterfw --add` and `--unblockapp`). The allowance is keyed by binary
  path. A successful bind proves nothing here, which is why saint probes its
  own listener and `saint doctor` separates "not running" from "bound but
  unreachable". There is no built-in `host.container.internal` name.
- **Images:** the `node:22-bookworm-slim` base already has a UID/GID `1000`
  user (`node`), so saint's Dockerfiles rename it rather than creating a second
  user with the same IDs.
- **Builder failures that are not saint's:** a broken BuildKit builder showed
  up as `read-only file system` errors from both saint's Dockerfile and a
  minimal control one; `container builder stop`, `delete` and `start` fixed
  it. Low disk space separately broke builds and runs with `No space left on
  device` until unused images were removed.

## Subscription auth through the proxy

Verified on `container` 1.3.0, Claude Code 2.1.247, `op` 2.34.0.

**`container run --env-file` exists** (`container run --help`), so the
subscription token never has to travel through argv. `--env` remains the
fallback and would be a real downgrade: `ps(1)` exposes argv to every process
running as the same user for the lifetime of the run.

**Documented behaviour that drove the design**, from the Claude Code gateway
reference:

- A gateway credential variable (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`) or
  an `apiKeyHelper` *replaces* the subscription. Traffic is then billed per token
  to whoever owns the credential the gateway forwards.
- Setting only `ANTHROPIC_BASE_URL`, with no credential variable, leaves a saved
  claude.ai login as the active credential, and its limits and billing apply.
- Such a gateway must forward `anthropic-beta` verbatim: it carries an OAuth
  capability the upstream requires, and stripping it fails the request with a
  `401`.
- Headers and body fields are open lists. A gateway pinned to the paths and
  headers that existed when it was written breaks on the release that adds the
  next one.

So subscription billing and proxy-held credential custody are mutually
exclusive, and saint offers both modes rather than pretending otherwise.

**Verified 2026-09-17** against a live subscription-mode session
(`saint-jpn5w3mw`, running for a day):

1. **`ANTHROPIC_CUSTOM_HEADERS` syntax works.** The guest carries
   `ANTHROPIC_CUSTOM_HEADERS=x-saint-session: <token>` and the proxy journal
   holds **177 usage records attributed to that session**. A wrong syntax could
   not produce an attributed record: `authenticate()` would have failed and the
   requests would have been refused at the proxy.

2. **The guest's environment is the one subscription billing requires.**
   Measured inside the container, names only:

   | Variable | State |
   |---|---|
   | `CLAUDE_CODE_OAUTH_TOKEN` | set |
   | `ANTHROPIC_BASE_URL` | set |
   | `ANTHROPIC_CUSTOM_HEADERS` | set |
   | `ANTHROPIC_API_KEY` | **absent** |
   | `ANTHROPIC_AUTH_TOKEN` | **absent** |

   Either of the last two would silently revert the session to API billing.
   Upstream accepted the requests with no API key present, so the credential it
   accepted was the subscription OAuth token.

**Still not verified, and only observable outside saint:** which account the
usage lands on. That is claude.ai's usage page moving while the Console
account's does not. saint cannot see either.

## Token metering was silently reporting nothing (2026-09-17)

Found while verifying the above, and much worse than the thing being verified.

Of those 177 attributed responses, **every one carried no model and no token
counts, and 176 of 177 were marked partial**. The proxy was attributing traffic
correctly and measuring nothing at all.

The cause is one sentence of Go's documentation: `http.Transport` decompresses
only the responses whose `Accept-Encoding` it added itself. A client that asks
for gzip explicitly - Claude Code does - gets the compressed stream passed
through untouched. The meter was scanning gzip bytes for `data:` frames, finding
none, and therefore reporting no usage and never seeing `message_stop`.

Nothing in `internal/proxy` handled `Content-Encoding`.

Reproduced as a test before the fix: a gzipped SSE response through the real
proxy, asserting model and token counts come out. It failed exactly as
production did.

The fix decodes **the meter's copy only**. The bytes the guest receives are
never touched, because a response the proxy re-encoded is a response the proxy
is responsible for. An encoded body is held to a bound and decoded in one pass
at the end rather than streamed through a decompressor: streaming needs a pipe
and a goroutine to pull on it, and the usage frames sit at both ends of the
stream anyway, so there is no earlier partial answer to be had. An encoding
saint does not read - `br`, `zstd` - reports nothing rather than guessing.

**Why it survived so long:** the counts were not wrong, they were *absent*, and
`tokens=unreported` is a state this interface displays calmly and by design. A
wrong number would have been noticed in a day.

**`op` state on the development host:** installed, but `op whoami` fails with
`connecting to desktop app: 1Password CLI couldn't connect to the 1Password
desktop app`. `saint doctor` reports this and prints the fix. Being installed
says nothing about being usable.

---

## Egress enforcement (2026-08-31)

Measured on `container CLI version 1.3.0`, macOS `26.3`, arm64. The question:
can a guest be *prevented* from reaching the internet except through saint, or
is `HTTP_PROXY` the best that can be done? `HTTP_PROXY` is advisory - an agent
that unsets it, or any code that opens a socket, ignores it entirely.

### `container network create --internal` is a real boundary

```
$ container network create --internal saint-spike-internal
$ container run --rm --network saint-spike-internal saint/claude:latest ...
```

From inside a container on that network:

| Probe | Result |
|---|---|
| default route | present, gateway `192.168.128.1` (the host bridge) |
| `/etc/resolv.conf` | `nameserver 192.168.128.1` (the host) |
| TCP to `1.1.1.1:443`, direct | timeout, `rc=124` |
| `dns.lookup("api.anthropic.com")` | `EAI_AGAIN` |
| `CONNECT api.anthropic.com:443` via host proxy, then TLS + GET | `HTTP/1.1 405 Method Not Allowed` |

The 405 is a real answer from `api.anthropic.com` - a completed TLS handshake
against the real certificate, tunnelled by a proxy on the host bridge. So the
guest reaches exactly one thing: whatever the host proxy is willing to reach for
it.

Two consequences worth stating separately from the mechanism:

- **The guest has no DNS of its own.** Name resolution happens on the host,
  inside the proxy, when it dials a `CONNECT` target. DNS as an exfiltration
  channel is not policed, it is absent.
- **This is structural, not advisory.** Unsetting `HTTP_PROXY` inside the guest
  does not open a path; it closes the only one.

### Containers on one internal network are not isolated from each other

A second container on the same network, probed from the first:

```
lateral: ECONNREFUSED (packet reached the peer)
```

`ECONNREFUSED` rather than a timeout means the packet arrived and the peer
answered. Sessions sharing a network can reach each other's microVMs. Isolating
them means a network per session, not a network per install.

### The bridge address exists only while a container is attached

`bridge101` carried `192.168.128.1` while containers ran on the network and did
not after the last one was removed:

```
$ ifconfig | grep -c 192.168.128.1     # containers running
1
$ container rm ...                     # last container gone
$ ifconfig | grep -c 192.168.128.1
0
```

A host process binding that address before any container exists fails with
`bind: can't assign requested address` - observed directly while running the
spike proxy.

This is a chicken-and-egg for saint: the proxy binds at startup, the address it
binds does not exist until a session starts, and a session cannot start without
being told the proxy's address. `container network ls` reports `ipv4Gateway` from
configuration rather than from a live interface, so `GatewayAddr` returns an
address that is correct and not yet bindable. Any design that moves sessions onto
an internal network has to solve this; it is the reason the work is not a
one-line change of network name.

### Not measured

- Whether the `default` network's bridge address persists with no containers
  attached. It is present on this host, but sessions were running throughout, so
  the observation proves nothing either way.
- Whether `--dns` can point a guest at the host proxy for resolution instead of
  leaving it with none.

### Binding the proxy to an internal bridge (2026-08-31)

Follow-on to the chicken-and-egg above: the proxy must bind the bridge gateway,
which does not exist until a container attaches, and the container has to be
told the proxy's address before it starts. Measured, in order:

| Step | Result |
|---|---|
| `container create --network <internal>` (no start) | bridge address **not** raised |
| `container run -d` on that network | bridge address up after **0.46s**, before guest userspace |
| bind a host listener to the gateway once it appears | succeeds |
| `container exec` a client after binding | reaches the internet through the proxy; direct lookup still `EAI_AGAIN` |
| remove the **last** container while the listener holds the socket | address disappears; **listener survives**, process alive |
| start a new container afterwards and reach the still-bound listener | works, no rebind |

The last row is the one that shapes the design. A socket bound to the bridge
address outlives the address: the listener keeps working when the bridge is torn
down and serves the next container that brings it back. There is no need for a
supervisor that watches the interface and rebinds.

That leaves exactly one hard case - the very first bind, before any container
has ever attached to the network. The measured sequence that handles it:

1. Start the container with its init process waiting rather than running the
   agent, which raises the bridge.
2. Poll for the gateway address (bounded; it took 0.46s here).
3. Bind the proxy.
4. Launch the agent with `container exec`.

Nothing runs inside the guest between the bridge coming up and the proxy being
bound, so there is no window in which an agent is live and unproxied.

**Consequence for the container's init process.** saint's PID 1 today is
`while tmux has-session -t agent; do sleep 2; done`, which exits - and kills the
container - once the agent's tmux session ends. If the agent is no longer started
at boot, that loop finds no session on its first iteration and the container dies
before step 4. Splitting boot from agent launch requires the init process to wait
for the session to appear before it starts watching for it to leave.

**Not measured:** whether the `default` network's bridge address persists with no
containers attached. Sessions were running throughout, so the observation proves
nothing either way.

### PID 1 in a saint container (2026-08-31)

Three measurements, all on `saint/claude:latest`.

**Stopping a container was always a SIGKILL.** PID 1 in a namespace ignores
every signal whose disposition is the default, and saint ran the bootstrap shell
directly as PID 1, so `container stop`'s SIGTERM reached a process that could
not act on it:

| PID 1 shape | `container stop` |
|---|---|
| `sh -lc <script>` (what saint shipped) | **5.3s** |
| same, plus `trap "exit 143" TERM` | 4.2s |
| same, with `container run --init` | **0.1s** |

The trap is not a substitute: a shell only runs a trap between commands, and the
loop spends its time inside `sleep`. `--init` also reaps orphans, which a shell
never does - measured 0 zombies after an orphan under `--init`.

Verified `--init` keeps the behaviour it could have broken: the container still
stops when the bootstrap script exits, and `tmux`/`container exec` attach still
work.

**`tmux new-session` returns 0 for an agent it cannot run.** This one contradicts
the obvious reading of the exit status, and a fix written against that reading
does not work:

```
tmux new-session -d -s agent /nonexistent-agent-binary ; echo $?   ->  0
```

tmux reports success as long as it could create the session. The command then
fails, the window closes, and the session is gone a moment later. A guard on
`new-session`'s status catches nothing; checking that the session still exists
after a second catches it, and was measured to leave a healthy agent alone.

**`set -e` in the bootstrap script made onboarding fatal.** With the script as
PID 1, one failed write - an unwritable `$HOME`, say - exited PID 1 and stopped
the container before the agent ever started. Confirmed live: under `set -u` a
deliberately failing onboarding write leaves the agent running normally, where
under `set -eu` the container is simply gone.

## Why scrolling an attached session does nothing (2026-09-16)

Every scroll gesture saint offers - wheel, PgUp/PgDn, shift+arrow - is a no-op
in a real attach, and has been since the feature was written. The tests pass
because the fixture does not resemble production.

### The agent holds the alternate screen

Measured against a live session's tmux and its raw `pipe-pane` transcript:

```
$ tmux display-message -p 'pane_in_alt=#{alternate_on} history=#{history_size}/#{history_limit} cmd=#{pane_current_command}'
pane_in_alt=1  history=6/2000  cmd=claude
```

Escape-sequence census of 683,693 bytes of that transcript:

| Sequence | Count |
|---|---|
| `\e[?1049h` enter alternate screen | 4 |
| `\e[?1049l` leave alternate screen | 2 |
| `\e[?1000h` / `\e[?1002h` / `\e[?1006h` mouse reporting | 84 each |

Claude Code is a full-screen alternate-screen application. Alternate-screen
content never scrolls off, so nothing accumulates anywhere: `charmbracelet/x/vt`
returns only the *primary* screen's buffer from `Scrollback()`, so saint's
emulator reports a scrollback length of zero, and tmux's own history is **6
lines out of a 2000-line limit** for the same reason.

Every scroll path then computes `min(offset, ScrollbackLen()) == 0` and moves
nothing. There is no bug in the wheel, the key handling, or the offset
arithmetic. There is simply nothing behind the window to move to.

**This also rules out the obvious fix.** Delegating to tmux's copy-mode finds
the same 6 lines, because tmux is not the alternate-screen program - the agent
is. Guest tmux is `3.3a` (measured, `tmux -V`).

### What the agent is asking for, 84 times

The same transcript shows Claude Code requesting mouse reporting on every
repaint. saint's emulator consumes those requests and saint never forwards a
mouse event into the guest, so the agent's own scrolling - which is where the
history it is painting actually lives - cannot be reached.

The coherent fix is therefore the inverse of the current design: stop
maintaining a second, permanently empty history on the host, and forward wheel
notches into the guest as SGR reports so the agent scrolls its own view.

### Why the tests did not catch it

`fillPaneScrollback` writes plain numbered lines to `/bin/cat` - a primary-screen
producer. Nothing in the suite ever enters the alternate screen, and production
is never primary-screen. A test whose fixture cannot fail is worse than no test,
because it reports the feature as working.

### Ghostty, settled

Ghostty's alternate-scroll branch (DECSET 1007, default on) requires
`mouse_event == .none`. `tea.WithMouseCellMotion()` sets modes 1002 and 1006, so
mouse reporting wins and saint receives real `MouseMsg` events today. Before
saint enabled mouse reporting, the wheel became bare `\e[A`/`\e[B`, which
`updateAttached` forwarded to the agent - which is exactly why scrolling used to
walk the prompt history.

## Detecting that an agent is waiting for a person (2026-09-16)

saint measures "working" from traffic crossing the proxy, which is fact. The
state it cannot measure is the valuable one: an agent stopped at a permission
prompt makes no requests, so it is indistinguishable from an agent that finished
an hour ago - and the difference is whether somebody needs to go to the
keyboard.

The only evidence is what the agent has drawn. Measured against three real
sessions on this host.

### The transcript cannot be pattern-matched directly

`tmux pipe-pane` writes the raw byte stream, and a full-screen agent paints at
absolute cursor positions. Searching the file finds a phrase wherever it was
last written, not what is on screen. In a 472 KB transcript the only occurrence
of `to confirm` was inside documentation the agent had printed about something
else - a false positive from the first thing anyone would try.

So the tail is replayed through a headless emulator and the rules run against
the reconstructed screen, matching only the bottom lines. Cost, measured:
**1-4 ms for transcripts up to 765 KB**.

### Render output is styled, which hides a phrase from a matcher

`Emulator.Render()` emits SGR sequences between words: `Enter to confirm` is
drawn as `Enter<sgr> to<sgr> confirm<sgr>`. Matching before stripping finds
nothing. This was a **false negative on two real sessions that were sitting at a
permission prompt** and it is invisible without real data - the rules were
correct and the input never reached them.

### A headless emulator hangs on a device query

The emulator answers device queries into a synchronous pipe. With no reader the
first answer blocks the write forever, holding the lock. Observed as a hang, not
a slow path, on the first attempt.

A reader cannot be shut down cleanly: `SafeEmulator` deliberately does not lock
`Read`, so that a blocked read cannot deadlock a write, and `Close` then races
that read on a plain `bool` inside the library - the race detector reports it
immediately.

The fix is to remove the cause. Device queries (`DSR`, `DA1`/`DA2`, `DECRQM`,
OSC colour queries, `XTGETTCAP`) are stripped before replay, so nothing is ever
written to the pipe. Nothing was listening anyway: the real terminal answered
those questions when the session was live, and the answers are already in the
stream.

### Results on real sessions

| session | transcript | detected | footer |
|---|---|---|---|
| `iqn2uxy2` | 1.7 KB | `waiting to confirm` | `❯ 2. No (recommended)` / `Enter to confirm · Esc to cancel` |
| `pjkhd4ah` | 1.2 KB | `waiting to confirm` | same |
| `jpn5w3mw` | 766 KB | none | `… · esc to interrupt · …` |

### A correction, from a later snapshot

An earlier snapshot of a bypass-permissions session carried `esc to interrupt`
in its footer, and the first version of this note concluded that such a session
keeps the hint permanently and can therefore never report blocked. A second
snapshot of the same session, idle at its prompt, reads:

```
✻ Cooked for 2m 40s · done 11:42 AM
❯ update the design doc to reflect the drift-block decision
  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents
```

No interrupt hint. So the string is a generating indicator after all, the guard
is correct, and the limitation was an artefact of reading one frame. Recorded
because the wrong conclusion was reached from real data, which is the failure
this file exists to catch: one observation is a frame, not a behaviour.

## A stopped container service is not a missing runtime (2026-10-28)

Observed on one host, `container` 1.3.0, after a reboot. The CLI was on PATH;
its service was not registered:

- `container system status` printed `apiserver is not running and not registered with launchd`, exit 1
- `container system status --format json` printed `{"status":"unregistered", ...}` with every other field empty, exit 1
- `container ls -a --format json` exited 1 with this on stderr:

```
Error: internalError: "failed to list containers" (cause: "interrupted: "XPC connection error: Connection invalid"")
Ensure container system service has been started with `container system start`.
```

saint's first-run screen read that failure as "Apple container runtime not
found". The CLI exits 1 for everything, so the only thing that distinguishes a
stopped service is stderr. `sandbox.classifyRunError` matches the two phrases
above; a CLI that rewords them falls back to an unclassified error, which the UI
renders as `unknown` rather than as a wrong answer.

Through `sandbox.AppleContainer` on the same host, with a default kernel
already installed:

- `StartSystem` (`container system start --enable-kernel-install`): returned nil in about 0.9s, and `List` succeeded straight after
- `StopSystem` (`container system stop`): returned nil in about 40ms, and the next `List` was classified as `ErrServiceStopped` again

Not measured: how long start takes when it has to download the kernel, and what
`system stop` does to a container that is running at the time. The stop
confirmation says only what the CLI's help says, that it stops every service.
The proxy finds its address when saint launches, so starting the service from
inside saint does not bring up a proxy that failed at launch. saint says so in
the status line rather than implying the fix is complete.

## The repo refresh ran guest-planted commands on the host (2026-11-05)

Found in a security review. Measured on one host with the system git, on a
throwaway repository.

saint's 3s refresh ran `git -C <repo> status --porcelain` on the host, and
every mounted repo is read-write in its guest, `.git` included. Two plants
both ran a command on the host from that exact call:

- `core.fsmonitor = <command>` in `.git/config`
- `filter.<name>.clean = <command>` in `.git/config` plus `* filter=<name>` in
  `.gitattributes`

Neither needed the user to do anything; the refresh triggered them. The
existing `.git` audit flagged `core.fsmonitor`, but only on the refresh that
had already run it, and it does not look at `[filter]` or `[include]` at all.
`-c core.fsmonitor=false` blocked the first plant and not the second, which is
why the fix is not a denylist.

The fix (`internal/tui/git_isolated.go`) runs git against a throwaway git
directory: a config saint writes, a copied and validated HEAD, and symlinks to
the real index, objects and refs. With system and global configuration off,
submodules ignored and optional locks off, the same three plants (the two
above and `fsmonitor` reached through `[include]`) ran nothing, while branch
and dirty count still came out right, a linked worktree still read correctly,
and the real index was left byte-for-byte and mtime unchanged. The regression
test runs plain git first to prove each plant is live, so it cannot pass by
testing a git that ignores them.

## A herdr plugin pane can exec into a session's microVM (2026-11-04)

The question was whether a herdr plugin can put a *particular* session's
microVM in a pane, since herdr's plugin v1 declares pane commands statically in
the manifest and its docs say runtime argv pane creation is not part of v1.

Observed on one host, herdr `0.9.5` and `container` `1.3.0`, in an isolated
named herdr session with a throwaway plugin and probe container, all removed
afterwards.

**Yes, through `env` rather than argv.** `plugin.pane.open` (and
`herdr plugin pane open --env KEY=VALUE`) passes an environment to the
manifest's fixed pane command. One entrypoint, a host wrapper that reads
`SAINT_SESSION` and execs `container exec -it <container> ...`, serves every
session. The bundled API schema shows `layout.apply` nodes also take an argv
`command`, which is a second route not exercised here.

- `placement = "split"` and `"tab"` both opened. A split must name the pane it
  splits with `--target-pane`; adding `--workspace` alongside it made the CLI
  report `split and zoomed plugin panes target an existing pane; use
  target_pane_id`, so the two flags should not be combined.
- The pane starts in the plugin root, so a relative command such as
  `./bin/attach` resolves.
- The pane was interactive: text sent with `herdr pane send-text` ran inside the
  guest (`Linux aarch64`, user `agent`, hostname the container's name) and the
  output was readable with `herdr pane read`.
- Closing the pane killed only its `container exec` client. The container kept
  running, which is the same detach semantics saint's own pane has.
- herdr reported the pane's foreground process as `container` (argv
  `container exec -it ...`) and `agent_status` as `unknown`. Agent detection
  through the wrapper is a separate question and is not answered here.

Also seen during setup, not caused by herdr: after the runtime service had been
stopped by a reboot, the first `container run` sat with no output for minutes
and was killed. A retry with `--debug` fetched the 65.8 MB init image and
started in about 6 seconds. Which step the first attempt was stuck on was not
captured.

**Footprint to know about before running a spike like this:** herdr's plugin
registry is global for the user (`~/.config/herdr/plugins`), and
`HERDR_CONFIG_PATH`, `XDG_CONFIG_HOME` and `HOME` did not isolate it. A shell
running inside a herdr pane also inherits `HERDR_SOCKET_PATH`, so herdr
commands from it act on the live session unless that variable is replaced. A
named session (`herdr --session <name> server`) isolates panes and the socket,
not plugins.

## herdr can track a sandboxed agent's state (2026-11-05)

The second herdr question: with the pane's process being `container exec`,
can herdr still say whether the agent inside is idle, working or blocked?
Same host and versions as above, same isolation and cleanup.

The guest ran the real `claude` binary (Claude Code `2.1.247`) with **no
credential variables set**, so it stopped at its first-run theme picker. That
is real agent UI with nothing authenticated and nothing billed, and it is also
why `working` and `blocked` could not be produced by the agent itself here.

**Screen detection works through the wrapper when the wrapper names the
agent.**

| Pane | herdr reported |
|---|---|
| `claude` in the guest, no hint | `agent_status: unknown`, no agent |
| `claude` in the guest, `HERDR_AGENT=claude` on the host wrapper | `agent: claude`, `agent_status: idle` |
| a shell in the guest | `agent_status: unknown` |

`HERDR_AGENT` was passed with `plugin pane open --env`, so it sits on the host
wrapper and is inherited by the `container exec` client. herdr's process info
still named the process `container`, but `pane read --source detection` showed
it reading the guest's screen (the theme picker and its diff preview). `idle`
on a prompt it has no rule for is herdr's documented fallback; it marks
`blocked` only for approval UI it recognises. The docs' warning that a hint set
only inside the VM is invisible to herdr is consistent with this: the hint has
to be on the host side.

**Reported state works, and takes precedence.** `herdr pane report-agent
<pane> --source custom:saint --agent claude --state <s>`:

- turned a plain guest shell from `unknown` into `claude` `working`, then
  `blocked`;
- woke an `herdr agent wait <pane> --until blocked` that was already waiting;
- on the screen-detected pane, held `working` while the screen still showed
  the idle theme picker, so a report overrides the screen.

`herdr pane release-agent` (which needs `--agent` as well as `--source`) handed
the pane back: it read `unknown` four seconds later and `idle` from the screen
again by nineteen seconds.

Gotchas:

- `agent wait` on a pane herdr does not yet consider an agent fails at once
  with `agent_not_found`, rather than waiting for it to become one.
- The `--message` text on a report appeared in neither `pane get` nor
  `agent list`. Whether the TUI shows it was not checked.

**Not verified:** `working` and `blocked` produced by a real, authenticated
agent through the wrapper, which needs a credential and would bill an account.
What this does show is the plumbing in both directions: herdr reads the guest's
screen once the wrapper names the agent, and saint's proxy, which already knows
when a request is in flight, can report `working` itself.

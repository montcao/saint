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
  "bound but unreachable". Because the allowance follows the path, the proxy
  runs from a fixed copy in the state directory, not from herdr's install
  folder, which changes with every plugin update. Not measured: whether the
  allowance survives the copy's bytes changing at the same path.
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

**Separate internal networks cannot.** Two internal networks, a listener in a
container on the second (Apple `container` 1.3.0):

| Probe | Result |
|---|---|
| container on the same network to the listener | connects |
| container on the other network to the listener | timeout |
| container on the same network to a host listener on that network's gateway | connects |
| container on the other network to that host listener | timeout |

So a session on its own network reaches neither another session's microVM nor
the proxy listener on another session's gateway. saint still accepts a
session's token only on its own network's listener, as a second lock.

**A new network's gateway is known before any container exists.**
`container network create --internal` returns at once, and `network ls`
reports the network (mode `hostOnly`) with its `ipv4Gateway` straight away,
from configuration, so a guest's proxy URL can be written before the container
is created. Deleting a network that does not exist exits 1 with a generic
`failed to delete one or more networks`, so saint checks `network ls` first.
Creating one that exists fails with `network ... already exists`. A deleted
network's subnet is handed to the next network created.

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

**saint end to end** (herdr 0.9.5, Apple `container` 1.3.0, the session image
of this repository). From inside a session's guest:

| Probe | Result |
|---|---|
| DNS lookup of a public name | `EAI_AGAIN` |
| TCP straight to a public address on 443 | timeout |
| `CONNECT github.com:443` through the proxy (shipped allowlist) | 200, TLS completes, `HTTP/1.1 200 OK` |
| `CONNECT example.com:443`, prompt answered "Allow always" | 200, TLS completes; the host is saved |
| the same host after a proxy restart | 200 without asking |
| an unlisted host, prompt answered "Deny" | 403 |
| an unlisted host, no answer in 20 s (the timeout then; now 2 minutes) | 403, journalled as no answer |
| an unlisted host after the session's pane was closed | 403 in about 0.1 s |

On its first start, Claude Code 2.1.247 asked to reach
`http-intake.logs.us5.datadoghq.com` and `downloads.claude.ai`. Both are now
on the shipped allowlist, so a new install is not asked about them.

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

The fix (`internal/inventory/git_isolated.go`) runs git against a throwaway git
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
- `herdr pane report-agent <pane> --source <id> --agent <a> --state <s>`
  sets `working` or `blocked` directly, overrides screen detection, and wakes
  `herdr agent wait`. `--source saint` is accepted; `--source custom:saint`
  is rejected as `unknown option` by 0.9.5. saint reports `blocked` while a
  host waits for an answer.
- `herdr pane release-agent <pane> --source <id> --agent <a>` returns the pane
  to screen detection. With the pane ID after the options, 0.9.5 rejects it
  (`unknown option: saint`), so saint puts the pane first in both commands.
- `--message` is accepted, but `pane get` and `api snapshot` do not return it,
  so whether herdr shows it was not observed from the CLI.

**A pane ID can come back naming a different pane; its terminal ID cannot.**
Within one herdr server a closed pane's ID was not reused: after `w1:p2` was
closed, the next split was `w1:p3`, and `pane get w1:p2` answered
`pane_not_found`. After the server restarted, a new workspace's first pane was
`w1:p1` again, with a different `terminal_id` (`pane get` returns it). So a
recorded pane ID alone can point at somebody else's pane, and saint records the
terminal ID with it and checks both before asking anything there.

**An overlay plugin pane opens only on the active pane.**
`herdr plugin pane open --placement overlay --target-pane <pane>` fails with
`overlay and popup plugin panes target the active pane`. `herdr agent focus
<pane>` makes a pane active, but switches workspace under whoever is typing
elsewhere. saint therefore opens the prompt as a split with `--target-pane`
and `--no-focus`, and relies on the pane's blocked status to draw the person
over. A plugin pane also receives
`HERDR_PANE_ID`, `HERDR_SOCKET_PATH` and `HERDR_BIN_PATH`, which is how a
session's pane is recorded for the proxy.

Gotchas:
- `agent wait` on a pane that herdr does not yet treat as an agent fails at
  once with `agent_not_found`.
- herdr's plugin registry is global per user (`~/.config/herdr/plugins`).
  `HOME`, `XDG_CONFIG_HOME` and `HERDR_CONFIG_PATH` do not isolate it.
- A shell inside a herdr pane inherits `HERDR_SOCKET_PATH`, so herdr commands
  run from it act on the live session.

Not verified: `working` and `blocked` produced by a real authenticated agent
through the wrapper.

## Pane parity harness: baseline not accepted

`make parity` drives host and saint panes with herdr send-text, send-keys,
wait-output and read. Arguments and response envelopes follow herdr 0.9.5.
The first [Mac attempt](https://github.com/montcao/saint/issues/35#issuecomment-7162904332)
on 2026-11-25 used harness commit `5dc0daf` and an older installed plugin
`d3b2547`. It recorded 14 failed probes and 15 unverified checks: neither pane
returned complete framed output, so its 0/29 score does not establish failure
of the underlying features. Host Claude self-updated during the attempt;
host/guest agent versions also differed. No accepted parity baseline, startup
timing distribution or visual/lifecycle result has been recorded.

### Run automated comparisons on a Mac

Use Apple container, herdr, saint's installed plugin and an authenticated
agent. Record the checkout commit, installed saint plugin revision, and agent versions
on both sides before and after the run. Align the installed plugin with the
intended checkout before treating results as that revision's behavior. Record
version drift rather than silently treating different versions as equivalent.
Target two dedicated idle agent panes with empty input in the same disposable
repository. First run only the execution preflight:

```sh
make parity PARITY_ARGS='-host-pane w1:p2 -saint-pane w1:p3 -preflight-only -report /tmp/parity-preflight.json'
```

This sends one harmless, uniquely framed printf command per side, host first.
It stops on the first failed side; no later pane submissions or comparisons
follow. A passing preflight proves the expected output was returned, not that
input never reached a model. Inspect the panes locally for unexpected model
handling before proceeding. Use a fresh report path for the full comparison:

```sh
make parity PARITY_ARGS='-host-pane w1:p2 -saint-pane w1:p3 -report /tmp/parity-run.json'
```

Or open fresh pane pairs beside a target with an absolute repository path
and literal text that appears only when the agent is ready:

```sh
make parity PARITY_ARGS='-target-pane w1:p1 -repo /path/to/repo -ready "ready prompt text" -runs 5 -report /tmp/parity-run.json'
```

The default is one pair. `-runs` accepts 1–20; **every created pane and saint
session is retained**, including on failure, for inspection. Five pairs create
ten panes and five guest sessions; ensure enough memory and disk first.
Each fresh pair must pass preflight before the next pair is opened. A failed
preflight stops further creation and writes unverified comparison rows when
`-report` is supplied. Existing panes also undergo preflight before comparisons.
Close the dedicated panes and stop/delete their saint sessions when finished.
The harness never closes unrelated panes or silently restarts herdr.

New-pane runs time each CLI open through confirmed ready text, sequentially
for host then saint. With at least five samples, the table reports raw-count
nearest-rank p50/p95 in milliseconds. This includes CLI/readback overhead;
it is not an instrumented keypress latency. The default timing budget is
saint p50 and p95 no slower than the corresponding host quantiles. Set an
explicit `-max-startup-ratio` of at least 1 to change that budget; the table
records it. Fewer than five samples or existing panes leave timing unverified.
Choose a ready marker that cannot occur in onboarding or a banner, and inspect
the panes to confirm it means readiness. Agent execution is not inferred from
herdr's exit code alone.

The default agent is Claude; choose `-agent codex` for Codex. The harness sends
`!` with send-keys before pasting the command and submitting Enter, rather than
pasting `!` as part of the command. This input sequence still needs live Mac
verification; it is not a guarantee of shell-only routing. Confirm the pinned
agent supports this mode;
`-shell-prefix=""` is only for dedicated shell panes and does not establish
agent-input parity. Never target panes with unsent input or active work.
Ctrl-C cancels outstanding CLI calls; panes already created remain available.

Each automated probe requires uniquely framed output and exit status zero on
both sides. Reports identify the failing side and stage (shell-mode key,
idle input, shell input, pending input, command entry/submission, wait-output,
readback, return to idle, framing or exit status)
without retaining pane contents or raw command errors. Transport or framing
failure stops subsequent submissions and leaves remaining probes NOT VERIFIED;
a framed nonzero exit is a probe failure and permits the next probe.
Echo cannot satisfy completion markers; timeout, missing output,
either-side failure and different output fail. The report contains result
summaries and startup samples, not raw configuration or identity values.
The config comparison covers selected non-credential settings, CLAUDE.md and
skill names, not plugin execution or credential exclusion.

### Record the remaining observations

`-report` creates two mode-0600 files, refusing to overwrite either:

- The JSON run report contains the run ID, UTC time, herdr/container versions,
  agent, pane IDs, automated results and any startup samples.
- Its `.observations.json` companion contains run-bound manual checks with
  procedures and explicit pass criteria. Null answers mean not verified.

Follow each procedure on the same test run. Record `host_succeeded`,
`saint_succeeded` and `equivalent` as booleans, plus concrete `evidence`:
versions, inputs, observations and measurements, without credentials.
A pass requires both sides to succeed and behave equivalently. Two failures
are not parity. Leave checks that were not exercised null.

The procedures cover rendered width/color, resize propagation, modified keys
and CSI-u, scrollback/mouse selection, clipboard, title/bell, authenticated
fetch/push, effective plugins, default-path dialogs, conversation resume after
close/restart and update handoff. Use disposable repositories/branches for
pushes and a dedicated herdr server for disruptive lifecycle checks. The
clipboard procedure changes the clipboard; preserve and restore its prior
contents. The dialog probe requires zero dialogs on both configured paths.
The harness itself does not perform those disruptive actions.

Evaluate the saved run and observations, on any OS, without driving panes:

```sh
make parity PARITY_ARGS='-evaluate /tmp/parity-run.json -observations /tmp/parity-run.json.observations.json'
```

Evaluation accepts only the matching run ID/schema and known unique probes.
It rejects incomplete answers, missing evidence, reduced probe sets and
attempts to overwrite measured results. Unanswered observations remain in
the denominator as NOT VERIFIED; an operator-observed pass is labeled as such.
Files are local operator evidence, not signed attestations. Retain both files
when sharing a score so the distinction stays reviewable.

For a full comparison, the harness exit status is 0 only when every probe
passes, 1 for failure/unverified probes, and 2 for setup or preflight errors.
With `-preflight-only`, 0 means both execution checks passed; all comparison
rows remain NOT VERIFIED and offline evaluation still returns 1. The `go run`
wrapper and make may map these to their own nonzero statuses; record both the
printed harness status and the outer command status. Driving panes is refused on Linux;
offline evaluation and unit tests do not establish a Mac baseline. Record the
first real Mac run and observations here before treating parity as accepted.

### Explicit diagnostic configuration and bounded input checks

Claude's shell mode can execute a command directly and then produce a model
reply. The documented `respondToBashCommands` default is `true`; a reply alone
does not prove shell-mode entry failed. A host-only stepwise check with Claude
2.1.332 and this explicit **task-local** `.claude/settings.local.json` override
executed a harmless printf and returned to empty input without an observed
reply, including a delayed readback:

```json
{"respondToBashCommands": false}
```

This override changes the diagnostic configuration and must be recorded. It
is not silently installed by the harness; do not edit global settings. Output
still enters conversation context, and absence of a visible reply is not a
network audit. Check support and effective behavior on each installed agent;
the older guest has not been verified. Do not assume a JSON key being present
means the runtime honors it. See the official [shell-mode documentation](https://code.claude.com/docs/en/interactive-mode)
and [settings reference](https://code.claude.com/docs/en/settings-reference#respondtobashcommands).

Before each command, the harness polls the **visible** input box for empty
ordinary input, sends the shell-mode key, polls for empty shell input, enters
text, checks the exact pending command, and only then sends Enter. It checks
return to empty ordinary input after extracting output. Each poll is bounded
by `-timeout` and cancellation. Unknown borders, wrapping, prompt glyphs or
nonmatching content fail closed, without retrying input. The supported
single-row box and `Try "..."` placeholder are observations of one host UI,
not a universal agent protocol or proof of no background work. Operators must
still confirm idle state and inspect for model handling. Dedicated shell
panes with an empty prefix skip agent UI checks and do not establish agent parity.

Output parsing accepts plain anchored frames or exactly one observed `⎿`
prefix (two leading spaces, then two spaces or space plus NBSP after the glyph)
on the begin line, with exactly five decoration spaces on subsequent nonempty
lines. It preserves payload indentation beyond those five spaces. Duplicate,
out-of-order, mixed or malformed frames are rejected; arbitrary whitespace
and model prose are not normalized into a pass. This is rendering support,
not evidence that a model never handled the command.

Next Mac verification: with explicit diagnostic settings, first exercise one
framed command producing multiple lines on a fresh host and guest, inspecting
every input step and checking older-guest support rather than assuming it.
Stop on a model reply or any failure. Only after both sides succeed, run one
revised preflight; only after that succeeds, run the baseline. Keep unperformed
manual checks null. Existing retained panes may contain pending text and must
not be reused without inspection. No guest verification, network audit or
passing harness result is claimed by the host-only observation or unit tests.

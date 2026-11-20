# saint

**Sandboxed coding agents for [herdr](https://herdr.dev).** Press a key and get
a new pane running Claude Code or Codex inside its own Apple `container`
microVM, with your project mounted in and your API key kept out of reach.

- **Isolated:** each session is its own microVM. The agent can touch the
  project you opened it on, not the rest of your machine.
- **Keys stay on the host:** a local proxy holds your API key and gives each
  session a throwaway token instead.
- **Metered:** every request is counted per session, without the agent's help.
- **No prompts:** the microVM is the boundary, so agents run without asking
  before every edit.

## Get started

You need a Mac with Apple silicon on macOS 26 or newer, Apple's
[`container`](https://github.com/apple/container) 1.3.0+, herdr 0.9.5+, and Go
(until saint publishes prebuilt releases).

**1. Install the plugin.**

```bash
herdr plugin install montcao/saint
```

**2. Run setup** in any herdr pane:

```bash
herdr plugin action invoke setup --plugin saint
```

A pane opens and asks one question per step; Enter takes the default, and
keys pressed before a question appears are ignored. It starts Apple's
container service if it is stopped, builds the Claude image (a few minutes,
once; nothing needs pressing meanwhile), adds a credential (your Claude plan
token or an API key), and binds two keys, one for a Claude pane and one for
Codex. If you already use those keys, it offers the next free ones. saint
backs up herdr's `config.toml` and has herdr check the new one before
replacing it. It ends with what each step did and the key to press next, also
sent as a herdr notification, and keeps a copy of its output in
`~/.saint/setup.log`. Type `q` and Enter to close the pane.

**3. Press the key** in any pane inside your project. herdr keys start with
its *prefix*, `Ctrl+b` unless you changed it: press it, let go, then press the
second key.

| To open | Press | Written in `config.toml` as |
|---|---|---|
| A Claude pane | `Ctrl+b`, then `a` | `prefix+a` |
| A Codex pane | `Ctrl+b`, then `Shift+a` | `prefix+shift+a` |

Setup prints the keys it actually bound, and `Ctrl+b`, then `?` lists every
key herdr knows. The agent starts and asks you to accept bypass-permissions
mode. Anything setup skipped, such as the Codex image, is asked for the first
time a pane needs it.

To bind keys yourself instead, add this to `~/.config/herdr/config.toml`, then
press `Ctrl+b`, then `Shift+r` to reload it:

```toml
[[keys.command]]
key = "prefix+a"
type = "plugin_action"
command = "saint.new"          # saint.new-codex for Codex
description = "sandboxed agent"
```

## Using saint in herdr

- **Start a session:** press `Ctrl+b`, then `a` (or the key setup printed) in
  a pane inside the project. saint mounts that pane's folder, opens a split
  beside it and starts the agent there. `Ctrl+b`, then `Shift+a` does the same for Codex.
- **Watch it like any agent:** herdr shows each session as working or idle;
  saint marks it working whenever the agent is waiting on the model. **Blocked**
  from saint means one thing: the session wants a host you have not allowed.
- **Know what it bills:** before the agent starts, the pane says whether the
  session draws on your Claude plan or bills an API key, and when its
  credential expires.
- **Answer network questions:** a small pane opens below the blocked session,
  without taking your focus. Go to it and press `a` to allow the host for every
  session, or `d` to refuse this request. No answer within 2 minutes refuses.
- **See session activity:** a split shows host decisions, provider status and
  measured usage as they happen. Close the activity split whenever you like.
  It opens again the next time you attach in herdr.
- **Close and come back:** closing a new session's agent pane or quitting the
  agent stops its microVM. `saint attach <session>` starts the agent again in
  the same microVM. Older tmux sessions still reconnect to their running
  agents. `saint ls` lists sessions.
- **Turn off Claude Code's telemetry:** bind `saint.telemetry` to toggle it;
  herdr shows a notification saying whether it is now on or off.

## Commands

The first session puts the `saint` command in `~/.saint/bin`; add that to your
`PATH` to use these.

```bash
saint ls                # sessions and their state
saint attach <session>  # reopen a session
saint stop <session>    # stop a session, keep it
saint rm <session>      # delete a session; your files are untouched
saint doctor            # check the setup
saint proxy log         # what each agent called, and every host decision
saint allow [host]      # hosts every session may reach; -rm <host> removes one
saint telemetry off     # refuse Claude Code's own telemetry and updater traffic
saint firewall          # let the proxy through the macOS firewall again
saint new <repo>        # start a session without herdr
```

## Good to know

- **Plan or API key.** With an API key the proxy holds it and the agent never
  sees it. With your Claude plan, the agent has to hold the plan's token
  itself; that is how plan billing works. `saint auth` shows which one new
  sessions use.
- **Network access is asked for.** Each session runs on its own network with
  no way out but saint's proxy. Provider APIs, GitHub and the main package
  registries work out of the box; any other host is asked about in herdr, as
  above. `saint allow` lists and edits what you have allowed.
- **Not packaged yet.** saint builds from source and is unsigned, so if the
  macOS firewall blocks a session's proxy, the pane offers to let it through,
  and macOS asks for your password in its own dialog.
- **Runtime images.** `saint doctor --build-images` rebuilds the images for
  new sessions. Apple `container` shares one builder across projects. After
  all builds finish, `container builder stop` releases its VM resources.

[`SECURITY.md`](SECURITY.md) lists exactly what saint does and does not
protect. [`docs/guide/configuration.md`](docs/guide/configuration.md) covers
billing modes, secret stores, mount limits and the request journal.

## Contributing

Start with [`AGENTS.md`](AGENTS.md), and run `make check` before opening a pull
request. To run your checkout as the plugin, use `make build` and
`herdr plugin link "$PWD"` instead of `herdr plugin install`.

## License

Apache License 2.0. See [`LICENSE`](LICENSE).

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
[`container`](https://github.com/apple/container) 1.3.0+, herdr 0.9.5+, and Go.

**1. Build saint and its agent images.**

```bash
git clone https://github.com/montcao/saint && cd saint
make build
container system start
./saint doctor --build-images
```

`saint doctor` checks the whole setup and prints the fix for anything missing.

**2. Let the proxy through the macOS firewall.**

```bash
make allow-firewall BINDIR="$PWD"
```

Without this, agents hang on every API call. Run it again after a rebuild if
they do.

**3. Add a credential.** Once per install; sessions never ask.

```bash
./saint auth claude      # your Claude plan, via a token from `claude setup-token`
./saint auth anthropic   # or an Anthropic API key
```

**4. Add saint to herdr** and bind a key in `~/.config/herdr/config.toml`:

```bash
herdr plugin link "$PWD"
```

```toml
[[keys.command]]
key = "prefix+s"
type = "plugin_action"
command = "saint.new"          # saint.new-codex for Codex
description = "sandboxed agent"
```

**5. Press the key** in any pane inside your project. A new pane opens with the
agent already running in a microVM. The first time, Claude Code asks you to
accept bypass-permissions mode.

## Using saint in herdr

- **Start a session:** press your key in a pane inside the project. saint
  mounts that pane's folder, opens a split beside it and starts the agent
  there. Bind a second key to `saint.new-codex` for Codex.
- **Watch it like any agent:** herdr shows each session as working or idle.
  **Blocked** from saint means one thing: the session wants a host you have not
  allowed.
- **Answer network questions:** a small pane opens below the blocked session,
  without taking your focus. Go to it and press `a` to allow the host for every
  session, or `d` to refuse this request. No answer within 2 minutes refuses.
- **Close and come back:** closing the pane leaves the agent running.
  `saint attach <session>` in any herdr pane reconnects, and network questions
  follow it there. `saint ls` lists sessions.
- **Turn off Claude Code's telemetry:** bind `saint.telemetry` to toggle it;
  herdr shows a notification saying whether it is now on or off.

## Commands

```bash
saint ls                # sessions and their state
saint attach <session>  # reconnect to a session
saint stop <session>    # stop a session, keep it
saint rm <session>      # delete a session; your files are untouched
saint doctor            # check the setup
saint proxy log         # what each agent called, and every host decision
saint allow [host]      # hosts every session may reach; -rm <host> removes one
saint telemetry off     # refuse Claude Code's own telemetry and updater traffic
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
- **Not packaged yet.** saint builds from source and is unsigned, which is why
  the firewall step exists.

[`SECURITY.md`](SECURITY.md) lists exactly what saint does and does not
protect. [`docs/guide/configuration.md`](docs/guide/configuration.md) covers
billing modes, secret stores, mount limits and the request journal.

## Contributing

Start with [`AGENTS.md`](AGENTS.md), and run `make check` before opening a pull
request.

## License

Apache License 2.0. See [`LICENSE`](LICENSE).

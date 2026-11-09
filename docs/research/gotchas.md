# Gotchas that cost real time

Do not rediscover these.

- **A herdr pane closes when its command exits.** An error printed by a pane
  command is gone before anyone reads it, so `saint herdr pane` waits for
  Enter after any failure.
- **A plugin pane starts in the plugin root, not the user's folder.** The
  folder to mount comes from the action's `HERDR_PLUGIN_CONTEXT_JSON`
  (`focused_pane_cwd`) and is passed to the pane as `SAINT_REPO`.
- **`HERDR_AGENT` must be on the host process.** Set in the guest, herdr never
  sees it and the pane reads as `unknown`.
- **A shell inside a herdr pane inherits `HERDR_SOCKET_PATH`**, so herdr
  commands run from it act on the live session. Test against a named session
  (`herdr --session <name> server`) and point `HERDR_SOCKET_PATH` at its
  socket. The plugin registry is global per user even then.
- **The proxy cannot bind before the first container exists.** The bridge
  address appears only while a container is attached, so start the container,
  then the proxy (`docs/research/spikes.md`).
- **A fresh state directory needs a local port.** The first run binds
  `127.0.0.1:0` to choose the proxy port, so in an agent sandbox that forbids
  local binding even `saint proxy log` fails with `choose proxy port: ...
  operation not permitted`. Allow local binding, or run it outside the sandbox.
- **saint refuses to mount `/tmp` and `/private/tmp`.** A scratch project for a
  test has to live under a mount root, such as a folder in your home directory.
- **commitlint rejects any capital in the subject**, including in a word like
  "microVM".

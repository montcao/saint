# Architecture

Which package owns which answer, and the boundaries between them. Boundaries
marked *checked* fail `make check` when broken (`internal/archtest`).

## Packages

| Package | Owns |
|---|---|
| `internal/inventory` | `saint ls`: which sessions exist, their state, and files in their repositories to review |
| `internal/proxy` | the credential boundary and usage metering |
| `internal/session` | session lifecycle and what a guest may mount |
| `internal/sandbox` | the Apple `container` CLI |
| `internal/credential` | where secrets come from: keychain, 1Password, command |
| `internal/store` | JSON state under `~/.saint` |
| `internal/usage` | token accounting and context windows |
| `internal/egress` | carrying "may this session reach this host?" from the proxy to whoever answers it: the queue of open questions, one per session and host |
| `internal/keychain` | the macOS `security(1)` CLI; secrets go to it on stdin, never in argv |
| `internal/doctor` | `saint doctor`: checks of the host, the runtime, credentials and the proxy, with a fix where one is a command |
| `cmd/saint` | the binary: CLI entry points, wiring, starting the proxy on demand, the proxy process (`proxy_run.go`: one listener per session network, and the control socket in `control.go` through which panes ask it to listen and prompts answer it), asking in herdr about a host (`egress_prompt.go`, `herdr_egress.go`), and the herdr plugin's action and pane entrypoints (`herdr.go`, with the manifest in `herdr-plugin.toml`) |
| `internal/archtest` | structural tests for the boundaries below |
| `scripts/docs-check` | `make docs-check`: documents that point at files, targets or documents that do not exist |

`docs/research/spikes.md` records what was measured rather than assumed, and on which runtime.
When a runtime behaviour surprises you, measure it and add a section.
`docs/design/principles.md` holds what saint is for and the rules that follow.

## Boundaries

- **The proxy never imports what answers its questions.** `internal/egress`
  carries them: the proxy takes a function (`ForwardConfig.Ask`), and
  `cmd/saint` builds that function from the egress queue and herdr. `egress`
  does not import `proxy`, and `proxy` does not import `egress`, directly or
  through any chain of imports. *Checked.*
- **The control socket is the only way into a running proxy.** It is 0600 in
  the state directory, outside every mount, so only the user's own processes
  reach it. Not checked.
- **Nothing machine-local.** No real home directory in any file, and no
  private-network address in a production string literal; both are discovered
  at runtime. *Checked.*
- **Files stay a reviewable size.** No Go file over 800 lines. *Checked.*
- **Host-side git never reads a mounted repository's configuration.** A guest
  can write `.git/` in every mount, so the repository inspection runs git
  against a throwaway git directory (`internal/inventory/git_isolated.go`). Covered
  by regression tests that plant `core.fsmonitor`, a filter and an `[include]`.
- **The sandbox layer stays behind an interface.** `internal/sandbox` runs every
  `container` command that changes state. `internal/doctor` also calls the CLI,
  read-only, to diagnose it (`--version`, `system status`, `image inspect`).
  Not checked.

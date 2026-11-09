# Architecture

Which package owns which answer, and the boundaries between them. Boundaries
marked *checked* fail `make check` when broken (`internal/archtest`).

## Packages

| Package | Owns |
|---|---|
| `internal/tui` | the Bubble Tea UI; `layout.go` owns how columns are divided, `palette.go` is the only file naming a colour |
| `internal/proxy` | the credential boundary and usage metering |
| `internal/session` | session lifecycle and what a guest may mount |
| `internal/sandbox` | the Apple `container` CLI |
| `internal/credential` | where secrets come from: keychain, 1Password, command |
| `internal/store` | JSON state under `~/.saint` |
| `internal/usage` | token accounting and context windows |
| `internal/egress` | carrying "may this session reach this host?" from the proxy to the interface, so neither imports the other |
| `internal/keychain` | the macOS `security(1)` CLI; secrets go to it on stdin, never in argv |
| `internal/doctor` | `saint doctor`: checks of the host, the runtime, credentials and the proxy, with a fix where one is a command |
| `cmd/saint` | the binary: CLI entry points, wiring, the proxy's lifecycle inside the TUI process, and the herdr plugin's action and pane entrypoints (`herdr.go`, with the manifest in `herdr-plugin.toml`) |
| `internal/archtest` | structural tests for the boundaries below |
| `scripts/docs-check` | `make docs-check`: documents that point at files, targets or documents that do not exist |

`docs/research/spikes.md` records what was measured rather than assumed, and on which runtime.
When a runtime behaviour surprises you, measure it and add a section.
`docs/design/` holds design notes that are proposals, not decisions.

## Boundaries

- **The proxy and the interface never import each other.** `internal/egress`
  carries questions between them: the proxy takes a function, the interface
  takes a queue. `egress` imports neither `proxy` nor `tui`; `proxy` imports
  neither `tui` nor `egress`; `tui` does not import `proxy` directly. *Checked.*
- **One file names a colour.** Only `internal/tui/palette.go` passes a literal
  to `lipgloss.Color`; everything else uses its `col*` constants. *Checked.*
- **Nothing machine-local.** No real home directory in any file, and no
  private-network address in a production string literal; both are discovered
  at runtime. *Checked.*
- **Files stay a reviewable size.** No Go file over 800 lines; the few already
  past it may shrink but not grow. *Checked.* Splitting them is tracked in
  `docs/plans/tech-debt.md`.
- **Host-side git never reads a mounted repository's configuration.** A guest
  can write `.git/` in every mount, so the repository inspection runs git
  against a throwaway git directory (`internal/tui/git_isolated.go`). Covered
  by regression tests that plant `core.fsmonitor`, a filter and an `[include]`.
- **The sandbox layer stays behind an interface.** `internal/sandbox` runs every
  `container` command that changes state. `internal/doctor` also calls the CLI,
  read-only, to diagnose it (`--version`, `system status`, `image inspect`).
  Not checked.

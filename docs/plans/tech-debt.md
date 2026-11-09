# Tech debt

Known gaps in the code and the harness, what would close each, and whether it
needs the owner's decision. Product limits that users should know about are
stated in `docs/guide/configuration.md` (Known limits) and `SECURITY.md` (what saint does not
claim); this ledger points at them rather than repeating them.

| Gap | What would close it | Decision |
|---|---|---|
| Nothing refuses repo-wide staging (`git add -A`, `git add .`), which `docs/operations/workflow.md` forbids. Git has no hook that runs on `git add`. | An agent-host hook (for example a Claude Code `PreToolUse` hook) that rejects those commands. | needed: host settings are untracked by choice (next row) |
| Agent host settings (`.claude/`, `.cursor/`) are untracked by design, so sandbox and permission bounds are per developer rather than versioned. | Track a shared settings file that denies reads of `.env*` and key files, leaving per-user files ignored. | needed |
| No end-to-end check against a real `container` runtime: start the service, create a session, list it, stop it. | A script for a macOS host with `container` installed. Hosted CI runners cannot run it. | needed: where and when it runs |
| The forward proxy's allow, deny and ask decisions exist (`internal/proxy/decision.go`, `internal/egress`) but nothing sets `ForwardConfig.Ask`, so egress is still recorded rather than controlled. Since the TUI was removed, nothing consumes `internal/egress` at all. | Wire `Ask` to the egress broker and a prompt in herdr, which needs a spike on how a plugin can ask a person (a popup pane or `notification.show`). | needed: product behaviour |
| No quality grade per area. | A document under `docs/plans/` grading each package with evidence (tests, known defects). Not written yet, because a grade without evidence would be invented. | none needed |
| `internal/usage`'s meter has no consumer since the TUI was removed: `saint proxy` passes none, so in-flight counts and totals exist only in the journal. | Use `OnRequestStart` and the in-flight count to report `working` to herdr with `herdr pane report-agent`. | none needed |
| A herdr pane's agent shows Claude Code's first-run bypass-permissions screen, because panes always skip prompts. | Decide whether saint pre-accepts it in the guest, given the microVM is the boundary. | needed: product behaviour |

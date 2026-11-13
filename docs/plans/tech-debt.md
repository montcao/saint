# Tech debt

Known gaps in the code and the harness, what would close each, and whether it
needs the owner's decision. Product limits that users should know about are
stated in `docs/guide/configuration.md` (Known limits) and `SECURITY.md` (what saint does not
claim); this ledger points at them rather than repeating them.

| Gap | What would close it | Decision |
|---|---|---|
| Nothing refuses repo-wide staging (`git add -A`, `git add .`), which `docs/operations/workflow.md` forbids. Git has no hook that runs on `git add`. | An agent-host hook (for example a Claude Code `PreToolUse` hook) that rejects those commands. | needed: host settings are untracked by choice (next row) |
| Agent host settings (`.claude/`, `.cursor/`) are untracked by design, so sandbox and permission bounds are per developer rather than versioned. | Track a shared settings file that denies reads of `.env*` and key files, leaving per-user files ignored. | needed |
| No end-to-end check against a real `container` runtime: start the service, create a session, list it, stop it. | A script for a macOS host with `container` installed. Hosted CI runners cannot run it. | deferred: to be planned before it is built |
| No quality grade per area. | A document under `docs/plans/` grading each package with evidence (tests, known defects). Not written yet, because a grade without evidence would be invented. | none needed |
| `internal/usage`'s meter has no consumer since the TUI was removed: `saint proxy` passes none, so in-flight counts and totals exist only in the journal. | Use `OnRequestStart` and the in-flight count to report `working` to herdr with `herdr pane report-agent`. | none needed |
| A herdr pane does not say which account the session bills or which credential it carries before the agent starts, which `docs/design/principles.md` asks for. | Print the resolved auth mode and credential lifetime in the pane before attaching. | none needed |
| A session's herdr pane record goes stale when the pane is closed outright (its process is killed before it can clear it). The proxy clears it when herdr reports the pane gone, but a pane ID that herdr reuses for a different pane would be asked in. | Catch the pane process's hangup to clear the record, or check that the pane still runs this session (`herdr pane process-info`) before reporting to it. | none needed |
| Claude Code asks for `downloads.claude.ai` and a Datadog log intake on first start, so every new install is asked about both. | Ship them on the allowlist, or set Claude Code's switch for non-essential traffic in the guest. | needed: product behaviour |
| The pane's blocked message is passed to herdr with `--message`, but herdr 0.9.5's CLI does not return it, so that it is shown was not observed. | Check in a herdr client, or a herdr version that exposes it. | none needed |
| Every session takes one `/24` from the runtime's address pool for its network, and saint does not know how many the pool holds. | Measure the pool, and say so in `saint doctor` when it is near full. | none needed |

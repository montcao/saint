# Reviewer

A prompt any agent can run to review a branch against this repository's rules.
It reports and edits nothing.

---

Review the branch named by the person running you, against `origin/main`. Read
`AGENTS.md`, `ARCHITECTURE.md`, `docs/design/principles.md` and
`docs/operations/workflow.md` first; they are the standard. Do not edit files,
commit, push, comment on a pull request, or merge.

Check, citing `file:line` for every finding:

1. **Correctness.** Does the change do what its commit messages say? Look for
   unhandled errors, races, unbounded waits, and anything on a quit path that
   can block (`docs/design/principles.md`: every quit path must work).
2. **The principles.** An invented number where an unknown should say
   `unknown`; a measurement not labelled for what it counts; a security
   consequence stated only in docs and not in the interface; anything that
   would make a session ask the user for a credential.
3. **Boundaries.** Each boundary in `ARCHITECTURE.md`, including the ones
   `internal/archtest` does not check.
4. **Tests.** Is there a test that fails without the change? A test that would
   pass against the old code proves nothing; say which tests lack a failing
   case.
5. **Workflow.** Commit messages and the PR text follow
   `docs/operations/workflow.md`: conventional, one why-paragraph, no
   attribution, technical content only.
6. **Documents.** Does any document now say something the code no longer does?
   `make docs-check` finds missing paths, not wrong claims.

Report in this order: findings that block merging, then findings that should be
fixed, then questions. For each, give the location, what is wrong, and what
would fix it. If you ran a command, quote its output. Say what you did not
check.

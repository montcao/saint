# Commits, branches and pull requests

These rules apply to every agent working here, whatever the model or harness.
Where a harness's defaults disagree (co-author trailers, "Generated with" lines,
its own title style), this file wins.

`AGENTS.md` is the map; this file says how work reaches `main`.

## Before starting

- **One branch and one worktree per task.** Never commit on `main`.
  `git worktree add .claude/worktrees/<name> -b <branch> origin/main`, then
  install dependencies inside it (`go mod download`).
- **Claim the work.** Agents push as the same GitHub account, so an author or
  assignee says nothing about who is working. Before starting an issue, or
  pushing to, closing or replacing a pull request you did not open, check for a
  `claimed` label and a claim comment naming a worktree. If someone else holds
  it, comment or review; do not push to it, close it, or open a replacement.
  Claim with the label plus a comment naming your worktree (relative to the
  repo, never an absolute home path) and branch. Release with a reason if you
  stop. A claim quiet for three days is stale: comment before taking it over.
- **Re-check state right before acting.** Before pushing to, commenting on or
  merging a pull request, fetch its current state (open, merged, closed, head
  commit). Lists go stale in minutes when several agents work at once.

## Commits

- **Stage paths by name.** Never `git add -A`, `git add .` or `git commit -a`:
  they sweep up another agent's files.
- **Conventional commits:** `type(scope): subject`.
  - Types: `feat`, `fix`, `docs`, `refactor`, `perf`, `test`, `build`, `ci`,
    `chore`, `revert`. Only `feat` and `fix` bump the version.
  - Scope: one of `tui`, `proxy`, `session`, `sandbox`, `credential`, `store`,
    `usage`, `egress`, `tiling`, `doctor`, `keychain`, `cli`; omit it when a
    change spans several.
  - Subject: lower case, no full stop, imperative.
  - Breaking change: `type(scope)!: subject` plus a `BREAKING CHANGE:` footer.
- **Body: one unwrapped paragraph saying why.** What was wrong or missing, what
  the change does about it, and what it leaves alone. No bullets, no headings.
  Lists, tables and measurements go in the pull request.
- **No attribution.** No `Co-Authored-By` trailers and no "Generated with"
  line, in commits or pull request descriptions.
- **Technical content only.** Commit messages, pull request titles and
  descriptions, and review comments say what the code was doing wrong and what
  the change does. They carry no product strategy, roadmap or positioning, no
  audience or release timing ("before launch", "for investors"), and no
  owner's business decisions, even when that context is why the work was asked
  for. If the technical reason cannot be stated without it, state the technical
  reason only. These texts are public and permanent; a decision the owner has
  not announced must not be announced by a commit.
- **Lint before committing.** `make commitlint-msg MSG=<file>` (or
  `echo "<message>" | npx commitlint`), which reads `commitlint.config.mjs`. Do
  the same for the pull request title, because squash-merge makes it the commit
  on `main`; CI lints both.
- **Never bare `git stash`.** The stash is shared across worktrees. Use a
  temporary WIP commit, or `git stash create` and `git stash apply <sha>` by
  SHA.

## Before pushing

- **Run what your change touches**: its test file, the formatter on changed
  files, and `make check` (it is fast here). CI runs it again on macOS. Push,
  then read the checks.
- **Hooks:** `make hooks` installs `.githooks` for your clone: `commit-msg` runs
  commitlint, `pre-push` lints the new commits and runs gofmt, vet and the
  structural tests. Use `--no-verify` only when deliberate, and say why (for
  example, the hook misreads a force-push range that passes commit by commit).
- **Force-push only** your own branch, or one you were asked to change, and
  only with a full-SHA lease:
  `git push --force-with-lease="refs/heads/<branch>:<full sha you last saw>"`.
  If the push says "new branch", the remote branch was deleted (its PR was
  likely closed). Stop, and find out why.

## Pull requests

- **Open as a draft** (`gh pr create --draft`), push as often as needed, then
  `gh pr ready` when done. Marking ready is the signal that the run that counts
  has started.
- **Title** follows the commit conventions.
- **Description** has these sections:
  - **Why:** the problem, with links to issues or runs.
  - **What changed:** per file or area.
  - **Verified:** the exact commands and their results.
  - **Not verified:** what was not checked, said plainly.
  - Merge-order notes, if any.
  - No attribution, and technical content only.
- **Conflicts:** rebase onto `origin/main`. Resolve by taking `main`'s version
  of each conflicted hunk and re-applying your intended change. Never keep a
  stale copy of someone else's edits. Rerun the touched tests, then force-push
  with a lease.
- **Dependency bot PRs:** if a bump needs a fix, push the fix onto the bot's
  branch (after rebasing), or open a replacement, never both. Check first that
  no other agent has done either.

## Merging

- **Merge only when asked**, and only when every required check passed on the
  exact head commit: `gh pr merge <N> --squash --match-head-commit <sha>`. Not
  while checks run, not over a red check, not while the mergeable state is
  `CONFLICTING` or `UNKNOWN` (wait and re-read).
- **Anything that releases, or touches secrets, environments or permissions,
  needs an explicit decision from the owner.** Say what it will trigger before
  merging.
- **After merging**, confirm the run on `main` passed. If a run on `main`
  fails, check whether another merge caused it before fixing it yourself.

## Reporting

- **Say what was verified and what was not.** If a measurement cannot tell two
  states apart, say so instead of picking one.
- **Correct yourself.** If an earlier report turns out wrong (a PR already
  merged, a claim that didn't hold), say so plainly.
- **Keep personal data out**: no absolute home paths, emails or real names in
  commits, comments or fixtures.

## Before you claim something works

```bash
make check    # gofmt, vet, race tests
```

Run it. Quote the output. "Should work" is not a result, and several bugs in
this repo's history were found only because a test asserted the thing rather
than the code implying it.

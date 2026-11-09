# Working on saint

saint is a herdr plugin that opens panes whose AI coding agent is already
running in an Apple `container` microVM. It ships publicly, so treat every file
as something an outside developer will read, run, and copy.

This file is the map. Every agent follows it, whatever the model or host, and
it overrides any harness default about attribution or commit style. Each rule
is one line here; the linked document has the full text.

## Commands

```bash
make check        # gofmt, vet, race tests, structural tests; run it and quote the output
make docs-check   # documents that point at missing files, targets or documents
make hooks        # install .githooks for this clone (commit-msg, pre-push)
make build        # build ./saint
```

## Principles

Full text: [`docs/design/principles.md`](docs/design/principles.md).

- **Secure and seamless at once.** When the safe path and the easy path conflict, say so and let the user choose.
- **Never show an invented number.** An unknown is `unknown`.
- **Nothing machine-local in code or tests.** Discover at runtime, compare minimums, use `t.TempDir()`.
- **State blast radius where the choice is made, not just in docs.**
- **Authenticate once per install, never per session.**
- **One signal, one meaning.** herdr's `blocked` means a person has to act, and nothing else.
- **Quiet is the default.** Report on a change of state, never on a timer.
- **Say which measurement a number is.**
- **Every quit path must work.**

## Workflow

Full text: [`docs/operations/workflow.md`](docs/operations/workflow.md).

- **One branch and one worktree per task**, under `.claude/worktrees/`, from `origin/main`. Never commit on `main`.
- **Claim before starting** with the `claimed` label and a comment naming your worktree and branch.
- **Re-check state right before acting** on a pull request.
- **Stage paths by name.** Never `git add -A`, `git add .` or `git commit -a`.
- **Conventional commits**, linted by `commitlint.config.mjs`; the body is one paragraph saying why.
- **Technical content only** in commits, pull requests and comments, and **no attribution**.
- **Never bare `git stash`.**
- **Run what you touched and `make check` before claiming it works**, and quote the output.
- **Force-push only your own branch**, with a full-SHA lease.
- **Pull requests open as drafts**, with Why, What changed, Verified and Not verified.
- **Merge only when asked**, on green checks for the exact head commit, squashed.
- **Anything that releases, or touches secrets, environments or permissions, needs the owner's decision.**
- **Say what was verified and what was not**, and correct yourself when an earlier report was wrong.

When the same mistake happens twice, turn it into a check (an AGENTS.md line, a
structural test, or a docs-check rule) instead of something a reviewer must
remember.

## Where things live

| Looking for | Read |
|---|---|
| Which package owns what, and the boundaries | [`ARCHITECTURE.md`](ARCHITECTURE.md) |
| What saint is for and the rules that follow | [`docs/design/principles.md`](docs/design/principles.md) |
| Commits, branches, pull requests, merging | [`docs/operations/workflow.md`](docs/operations/workflow.md) |
| What was measured, and on which runtime | [`docs/research/spikes.md`](docs/research/spikes.md) |
| Traps that cost real time | [`docs/research/gotchas.md`](docs/research/gotchas.md) |
| What saint does and does not protect | [`SECURITY.md`](SECURITY.md) |
| Configuration, billing modes, known limits | [`docs/guide/configuration.md`](docs/guide/configuration.md) |
| Known gaps and what would close them | [`docs/plans/tech-debt.md`](docs/plans/tech-debt.md) |
| Reviewing a branch; checking docs against code | [`docs/agents/`](docs/agents/) |
| Every document, what it decides, when it was checked | [`docs/index.md`](docs/index.md) |

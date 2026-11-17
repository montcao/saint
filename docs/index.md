# Document index

Every tracked document, what it decides, and when it was last reviewed.
`make docs-check` fails when a document is missing from this table or its
review date is more than 90 days old.

**Reviewed** is `read` when someone checked the document claim by claim against
the code on that date (see [`docs/agents/doc-gardener.md`](agents/doc-gardener.md)),
and `edited` when the date is only its last commit. An `edited` date says
nothing about whether the document is still true.

| Document | Kind | Decides | Reviewed |
|---|---|---|---|
| [`AGENTS.md`](../AGENTS.md) | entry point | the map: commands, one-line rules, where everything lives | Reviewed: 2026-11-09 edited |
| [`CLAUDE.md`](../CLAUDE.md) | entry point | nothing; points Claude Code at `AGENTS.md` | Reviewed: 2026-11-06 edited |
| [`ARCHITECTURE.md`](../ARCHITECTURE.md) | architecture | which package owns which answer, and the boundaries between them | Reviewed: 2026-11-09 edited |
| [`README.md`](../README.md) | user guide | what saint is and how to start using it with herdr | Reviewed: 2026-11-09 edited |
| [`docs/guide/configuration.md`](guide/configuration.md) | user guide | billing modes, secret stores, mount limits, the request journal, known limits | Reviewed: 2026-11-09 edited |
| [`SECURITY.md`](../SECURITY.md) | security | how to report a vulnerability; which boundaries saint claims | Reviewed: 2026-11-09 edited |
| [`docs/design/principles.md`](design/principles.md) | design | what saint is for and the rules that follow from it | Reviewed: 2026-11-09 edited |
| [`docs/operations/workflow.md`](operations/workflow.md) | operations | commits, branches, pull requests, merging, reporting | Reviewed: 2026-11-09 edited |
| [`docs/research/gotchas.md`](research/gotchas.md) | research | traps in herdr plugin work and testing that cost real time | Reviewed: 2026-11-09 edited |
| [`docs/research/spikes.md`](research/spikes.md) | research | runtime behaviour measured rather than assumed, and on which runtime | Reviewed: 2026-11-09 edited |
| [`docs/plans/tech-debt.md`](plans/tech-debt.md) | plan | known gaps, what would close each, and which need a decision | Reviewed: 2026-11-09 edited |
| [`docs/agents/reviewer.md`](agents/reviewer.md) | agent prompt | how an agent reviews a branch against these rules | Reviewed: 2026-11-06 edited |
| [`docs/agents/doc-gardener.md`](agents/doc-gardener.md) | agent prompt | how an agent checks documents against the code | Reviewed: 2026-11-06 edited |

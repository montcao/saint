# Doc gardener

A prompt any agent can run to check documents against the code and correct
drift. A person runs it on a cadence; `make docs-check` fails when a review
date in `docs/index.md` is more than 90 days old.

---

Work in your own branch and worktree, following `docs/operations/workflow.md`.

1. Run `make docs-check` and fix what it reports.
2. Pick the documents in `docs/index.md` with the oldest Reviewed dates, or the
   ones the person names.
3. For each, check every claim against the code: a path, a command, a default,
   a behaviour, a number. Read the code that implements it; do not trust another
   document. Mark each claim as confirmed, wrong, or unverifiable from the
   repository.
4. Fix what is wrong in the smallest edit that makes it true. Do not rewrite
   rules or reorganise documents; if a rule itself looks wrong, or two documents
   contradict each other and the code does not settle it, stop and ask.
5. Update the document's row in `docs/index.md` to
   `Reviewed: <today> read` only if you checked it claim by claim. A document
   you only edited gets `Reviewed: <today> edited`.
6. Open a draft pull request listing, per document, the claims you checked, the
   ones you corrected, and the ones you could not verify.

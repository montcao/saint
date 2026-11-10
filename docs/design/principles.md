# Principles

What saint is for and the rules that follow from it. `AGENTS.md` lists each
rule in one line; this is the full text.

## What saint is for

**Inherently secure, and seamless for agentic development.** Both at once.

A design that is secure but makes people paste long tokens, re-authenticate per
session, or read a manual has failed. A design that is seamless but leaves a
year-long credential in every container has also failed.

They are the same goal, not a trade. Agentic development means many sandboxed
agents running unattended against real credentials and real repositories: the
blast radius is large and nobody is watching. Friction is what pushes people
into the insecure shortcut.

When the safe path and the easy path genuinely conflict, say so and let the user
choose. Do not quietly pick for them.

## Rules that have earned their place

**Never show an invented number.** An unknown is `unknown`. saint has a context
bar with no percentage for models whose window is unverified, a `tokens=unreported`
state for providers that send no counters, and a `no git` state for directories
with no history. Each exists because a confident wrong number is worse than a
gap. `internal/usage/limits.go` is the pattern: a table of values someone
actually checked, and `ok=false` for everything else.

**Nothing machine-local in code or tests.** No `/Users/...` paths, no discovered
IP addresses, no exact host versions as gates. Discover at runtime, compare
minimums, and use `t.TempDir()`. Observations from one host belong in prose,
labelled as observations.

**State blast radius where the choice is made, not just in docs.** What a
compromised guest holds, and for how long. `saint auth` and `saint doctor` say
which account new sessions bill and which credential they carry, and a pane
says it before the agent starts.

**Authenticate once per install, never per session.** Sessions get a random
per-session token for attribution; they never ask the user for anything.

**One signal, one meaning.** saint has no screen of its own; what it shows
goes through herdr. herdr's `blocked` state means a person has to act, and saint
uses it for nothing else: a pending egress question is `blocked`, a slow
request is not. A state is always said in words, never carried by colour
alone.

**Quiet is the default.** saint speaks when something needs a person or has
changed, and is otherwise silent. It reports to herdr on a change of state,
never on a timer; it sends no notification for routine events; and an idle
saint polls nothing, so it wakes up for nothing. A signal that is always
present is wallpaper within a day.

**Say which measurement a number is.** saint counts what crossed the proxy; an
agent counts its own conversation, and the two never agree. A figure that
excludes cache tokens is labelled for what it counts, not called a total. This
is the same rule as the one above it, in the place it is easiest to break.

**Every quit path must work.** An interface that can become unquittable is the
worst failure this codebase has shipped, in the TUI that preceded the herdr
plugin. A pane, an attach or a proxy shutdown must never wait without a bound,
and a failure that ends a pane waits for the person to read it rather than
vanishing with it.

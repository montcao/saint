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

**State blast radius in the UI, not just in docs.** What a compromised guest
holds, and for how long. The header always says which account a session bills;
the sidebar says which credential it carries.

**Authenticate once per install, never per session.** Sessions get a random
per-session token for attribution; they never ask the user for anything.

**One colour, one meaning.** `internal/tui/palette.go` is the only file that
names an ANSI index. There is one saturated colour and it means a person has to
act; everything else is a step on a grey ramp, so a frame with no colour in it
means nothing needs you. Colour is never the only carrier of a state - which
binds harder now that almost all hue is gone, so anything that leaned on it has
to say the word.

**Stillness is the default.** Nothing in the interface moves unless a session is
working, and then exactly one cell does. The animation clock is not paused when
the deck is idle, it is not scheduled, so an idle saint wakes up for nothing.
Motion that is always present is wallpaper within a day.

**Say which measurement a number is.** saint counts what crossed the proxy; an
agent counts its own conversation, and the two never agree. A figure that
excludes cache tokens is labelled for what it counts, not called a total. This
is the same rule as the one above it, in the place it is easiest to break.

**Every quit path must work.** A TUI that can become unquittable is the worst
failure this codebase has shipped. `ctrl+q` is handled before any forwarding,
`Close()` is bounded by a timeout, and there are regression tests using a
deliberately wedged child process.

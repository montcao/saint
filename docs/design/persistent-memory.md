# Persistent memory across saint sessions

A design note, not a plan of record. The proposal is a store of facts that
outlives a single microVM and is readable by any agent saint launches, so a
Codex session can use what a Claude session learned about the same repository.

This document argues for a narrow version of that, against the version most
people picture, and against one mechanism outright. Read §3 and §5 first if you
only read two sections; those are where the design is decided.

## 1. What this actually solves

Every saint session is a fresh microVM with an empty home directory. That is
visible in the code: `onboardingLines` in `internal/sandbox/applecontainer.go`
exists only because a new container opens on the theme picker and the
trust-this-folder dialog, having never seen the user before.

Three concrete losses today, in order of how annoying they are:

**Renewing a credential destroys the agent's history.** `RenewCredential` in
`internal/session/auth.go` calls `Sandbox.Remove` and then `Sandbox.Create`.
The session directory mounted at `/saint` survives, so `transcript.log`
survives. The guest's home directory does not, and that is where Claude Code
keeps the conversation `--continue` resumes. Press `c` to repair an expiring
credential and the agent forgets the last four hours, while saint's own record
of the session is untouched. This is the sharpest instance of the problem and it
does not need a graph database to fix.

**A second session starts from zero on a repo the first one already learned.**
Two sessions on the same directory, or one Claude and one Codex, share the
filesystem and nothing else. Whatever the first worked out — that the test suite
needs a service running, that a dependency is pinned for a reason, that a
particular file is generated — is in a transcript nobody reads.

**Nothing is ever written back.** A developer who wants an agent's finding to
persist has to notice it and ask for it. The default is that it evaporates.

### The competing option, which is strong and free

Most of the durable value here is already served by files in the repository.
`CLAUDE.md` and `AGENTS.md` are read natively by the agents saint runs, with no
saint involvement at all. They are versioned, reviewable in a pull request,
shared with colleagues, and they survive saint being uninstalled. saint already
notices when an agent edits them: `.claude/` and `.codex/` are in
`reviewDirSegments` in `internal/inventory/repo_status.go`, so an
agent-modified instruction file is flagged for review by `saint ls` today.

That is a very hard baseline to beat, and any honest version of this feature has
to concede most of the territory to it. A "durable fact about this repository"
belongs in the repository. Building a second, private, unreviewed place to put
the same sentence makes the repository's copy less trustworthy, not more.

What files genuinely do not cover:

- Facts about **this machine** rather than this repo — which are not committable
  and do not belong in a shared file.
- Facts about directories that are **not repositories at all**. saint
  deliberately allows a session on any directory under a mount root, and shows
  `no git` rather than `clean` for those. They have nowhere to put a file that
  means anything.
- **Episodic** knowledge with an hour-long lifetime — what another session
  already tried and what it cost. Committing that would be noise.
- The **write path**. A repo file is something a human authors. Nothing today
  captures what an agent learned without a human transcribing it.

So the honest claim is narrow: the unserved part is the episodic and the
machine-local, plus a write path that makes promotion to a repo file cheap. That
is a smaller feature than "memory", and it is the one worth building.

## 2. Four kinds of memory, which are not one thing

| Kind | Example from saint's own history | Lifetime | Who wrote it | Where it should live |
|---|---|---|---|---|
| Durable repo fact | `make check` is gofmt, vet and race tests | years | a human, reviewed | the repo (`CLAUDE.md`) |
| Decision and rationale | subscription mode leaves the credential in the guest; that is the price of plan billing | years | a human, reviewed | the repo (`README.md`, `SECURITY.md`, `docs/research/spikes.md`) |
| Episodic session history | session `alpha` already established that `Scrollback()` returns the primary buffer only, so the pane has no history to scroll | hours to days | an agent, unreviewed | saint, scoped and expiring |
| User preference | plain declarative prose, no emoji, never an invented number | years | the user, directly | saint, global |

These differ on every axis that matters. Lifetimes span three orders of
magnitude. Two are human-authored and reviewed; one is machine-authored and
never reviewed by anyone. The first two are safe to treat as instructions; the
third is at best a hint and at worst an injection (see §5).

**A single bucket would give the unreviewed hourly material the same standing as
the reviewed committed material.** That is the whole argument against one store,
and it is enough. The recommendation is therefore:

- Kinds 1 and 2: saint stores **nothing**. It can make writing them easy — a
  command that turns a finding into a diff against `CLAUDE.md` for the user to
  accept — but the store of record is git.
- Kind 3: saint stores it, scoped to the repo path, with provenance, visible,
  deletable, and not trusted by default.
- Kind 4: saint stores it, globally, but only the user writes it. An agent that
  can edit the user's stated preferences is an agent that can rewrite its own
  instructions.

## 3. How an agent reaches it without saint owning the agent

First, a distinction that unlocks this section. "saint does not own the agent"
means the binary is unmodified and pinned. It has never meant saint leaves guest
configuration alone: `onboardingLines` already writes `~/.claude.json` inside
the container at bootstrap. **Writing agent config is established precedent;
patching the agent is the line.**

Second, a constraint from `docs/research/spikes.md`: a guest on an internal network has no DNS
and exactly one reachable address, the host bridge gateway. Whatever the channel
is, it terminates on the host, at an address saint already discovers at runtime
and already binds.

### The options

**A. MCP server on the host, declared in guest config at bootstrap.**
The guest reaches it over the bridge gateway, authenticating with the session's
existing proxy token. That mechanism is already proven in this codebase: the
forward proxy accepts `HTTP_PROXY=http://saint:TOKEN@host:port` and turns it
into `Proxy-Authorization`, using the token the session already has. No new
credential, no prompt, "authenticate once per install" preserved. Reads and
writes are attributable for free, because the endpoint knows which session
called it.

Claude Code speaks MCP. **Whether the pinned Codex image does, and through which
config key, is not verified here** — nothing has yet
measured the image's MCP surface. That is a spike, not an assumption
(§7). If it turns out Codex cannot, option A is Claude-only and this design
loses its whole cross-agent premise, so the spike is blocking.

**B. A filesystem mount the agent is told to read.**
Mount a memory directory into the guest and name it in the session's
instructions. Works for every agent that can read a file, which is all of them,
with no protocol and no adapter. `docs/research/spikes.md` verified `:ro` is enforced on
`container` 1.3.0, so a read-only mount is a real boundary rather than a
convention.

Its weakness is that it has no query — the agent reads a whole file — and a
read is invisible to the host. Its strength is that a read-only mount
**cannot** be the write channel, which matters more than it first appears.

**C. The proxy injecting context into requests.**
Technically available today. The guest speaks plaintext HTTP to the proxy; TLS
starts at the proxy. `internal/proxy/proxy.go` already rewrites headers per
request, and adding a body rewrite is not hard. It is the only option that works
for every agent with zero cooperation and that **the agent cannot ignore**.

That property is real, and it is not worth the five things it costs:

1. It edits the user's prompt without the user or the agent knowing. The agent
   then reasons, in a transcript the user is watching in the pane, about text
   the user never wrote and cannot see. That is structurally a prompt injection,
   performed by the tool installed to reduce the blast radius of prompt
   injection.
2. It contradicts an invariant already written into this codebase.
   `internal/proxy/usage.go` states that body content must never be retained and
   that the model name is read from the response because "the request body is
   the whole conversation and this proxy does not touch it". Injection turns the
   boundary into an author.
3. Prompt caching. Both providers cache on a stable prefix. Memory that changes
   between turns and is inserted near the front invalidates it. saint meters
   `cache_read` and `cache_write` per session, so the regression would appear in
   the sidebar attributed to the agent rather than to saint. In key mode that is
   money; in subscription mode it is the user's plan limits.
4. `docs/research/spikes.md` already warns that headers and body fields are open lists, and
   that a gateway pinned to what existed when it was written breaks on the next
   release. A forwarding gateway degrades by missing a field. A parse-and-
   reserialise gateway degrades by emitting a malformed request.
5. In subscription mode the proxy forwards the guest's own credential verbatim
   and is explicitly "an observer, not a boundary". Rewriting bodies on traffic
   billed to the user's plan makes saint responsible for tokens the user did not
   ask for, in the one mode where saint has the least standing to do so.

**Reject C.** Not "later" — the mechanism.

**D. An `apiKeyHelper`-style shim.** Not a memory channel. It is a hook for
producing one credential string; it can carry a secret, not a corpus. Using it
to smuggle context means doing C by another route. Noted and dismissed.

**E. The agent's own documented context flags.** Both pinned runtimes are launched with an argv saint
composes, and each agent documents some way to seed a session with additional
instructions at launch. saint
already builds the agent's argv in `runtimeCommand`, so composing a short memory
preamble there is a small change, the agent knows about the text, and it appears
in the agent's own transcript rather than being spliced in behind it. The costs
are that it is one adapter per runtime, and that it is static: memory injected at
launch cannot answer a question asked an hour later. **The exact flags per
runtime are not verified here** and must not be written into a plan until they
are checked inside the pinned images.

### Recommendation

**B for reads, A for writes, E as the launch-time seed where a runtime supports
it.** Concretely: a read-only mount the agent can read at any time without
asking anyone, and a host endpoint that is the only way to write. The asymmetry
is the point — the channel that cannot be trusted is the one that is mediated
and attributable, and the channel that is convenient is the one that cannot
modify anything.

One qualification, argued in §5 and acted on in §7: a mount is unattributable,
so the first version serves reads over the host endpoint too, and the mount is
added once there is evidence anyone is reading at all.

### What happens when the agent ignores it

Every option except C can be ignored, and an agent will ignore them sometimes.
That is not a bug to be engineered away; it is a constraint on what the feature
may be used for. **Memory is an accelerator, never a system of record.** If a
fact has to be honoured, it goes in the repo, where the agents already read it
and a human already reviews it.

## 4. A graph model, not a graph database

The queries this has to answer, written out:

1. What is known about repo `R`? — scoped lookup by key.
2. What did any session learn about file or symbol `X`? — text search.
3. Who asserted this, when, from which session and model? — one hop.
4. What supersedes this? — a chain, usually one or two links.
5. What did session `beta` do in the last hour? — range scan by session and time.

Four of the five are point lookups, prefix scans and full-text search. Exactly
one is traversal, and it is shallow and of known shape.

What a graph genuinely buys: variable-depth traversal whose shape you do not
know in advance, and new edge types without a schema migration. Provenance
chains are the honest example — "this claim was derived from that one, which
came from a session that read this file" — and if saint ever wants to answer
"show me everything downstream of the entry I just found to be poisoned", that
is a real traversal and a real reason.

What it costs here is larger than it looks. saint today has **no database at
all**: `internal/store` is JSON files under `~/.saint`, and `go.mod` lists six
direct dependencies, all pure Go, none of them storage. A graph engine adds a
query language, a binary format `cat` cannot read, and a dependency whose
failure modes nobody on this project has debugged. It also has no built-in
answer to query 2, which is the one a user will actually issue.

**Recommendation: SQLite, with an explicit node-and-edge schema and FTS5.** An
`entity` / `claim` / `edge` shape *is* a graph — an adjacency table is a graph —
and it is one that `sqlite3` can inspect and `.dump` can back up. Query 4 is a
recursive CTE. If traversal ever outgrows that, the data is already in node/edge
form and porting is mechanical rather than a rewrite.

This is an argument about the engine, not about the model. The instinct behind
"graph backend" is right: memory is relational, entries refer to other entries,
provenance is a chain, and a flat list of strings throws all of that away. Keep
the graph model. Do not take on a graph database to hold five tables.

One caveat, stated rather than implied: even SQLite is a dependency saint does
not have today, and a pure-Go driver is a non-trivial addition to a binary this
small. The smallest useful version (§7) needs no database at all, and the
database decision should wait behind the measurement that says the feature is
worth keeping.

## 5. Security, which is the most likely reason to kill this

Shared memory is a persistent, shared, mutable channel between sandboxes saint
spent real effort isolating.

`docs/research/spikes.md` measured that containers on one internal network already reach each
other — a lateral probe returned `ECONNREFUSED`, meaning the packet arrived and
the peer answered — and concluded that isolating sessions means a network per
session. **Memory would reintroduce at the application layer exactly the channel
that work exists to remove, and this one survives a restart.** That comparison
should be in the feature's own documentation, not discovered later.

### Poisoned facts

A session works on a repository containing a hostile string in a README, an
issue body, or a dependency's changelog. The agent, doing what it was told,
records a durable fact: the release command is such-and-such. Tomorrow a
different session on a different agent reads it as context and runs it.

The mechanism is **laundering**: content moves from untrusted (repo) to trusted
(memory) by passing through an agent, and arrives in the second session with
none of the markers that would have made the first one cautious. This is the
reason agent-written entries cannot be readable by other sessions without a
human in between.

### Memory as an exfiltration channel

Two sessions that should not talk — different repositories, possibly different
credentials — can now talk. Session A writes, session B reads. The bandwidth is
low and the value is high, because the things worth exfiltrating are small.
Mitigation: **scope by repo path, by default, always.** An entry written by a
session mounting `~/Dev/foo` is visible only to sessions mounting `~/Dev/foo`,
unless a human promotes it. Global scope is a deliberate act, never a default.

### Provenance

Every entry carries: session ID, runtime, model as reported upstream, timestamp,
repo scope, and origin (`agent` / `user` / `derived`). saint already has all of
that at the boundary — the proxy knows the session, the store knows the runtime
and the repos, the meter reads the model off the response.

One honest limit: "from what evidence" cannot be verified. An agent's account of
its own reasoning is itself agent-authored. So the display must say
`session alpha asserted this at 15:04`, never `this is true because X`. saint's
rule against invented numbers has a twin here — an unverifiable justification
presented as one is the same failure in prose.

### Do writes need confirmation?

A prompt on every write is the friction that kills the feature, and saint's
whole positioning is that friction is what pushes people into the insecure
shortcut. But an unconfirmed write is how the poisoning in the first subsection
happens.

The resolution, and the single most important decision in this document:

> A write always succeeds and never blocks the agent. It lands **quarantined**:
> readable immediately by the session that wrote it, and invisible to every
> other session until a human promotes it.

The writing session gets memory that works within its own life, at no cost. The
human is asked exactly once, on the one edge that carries risk — the crossing
from one sandbox to another. Nobody is asked about anything else. That is the
same shape as the credential design: the boundary is where the decision lives,
and everywhere else is seamless.

### Are reads attributable?

Yes, and cheaply, if reads go through a host endpoint rather than only a mount.
That is the argument for serving reads over the same authenticated hop even
though a mount is simpler, and it earns its cost the first time a bad entry is
found: the question then is *which sessions read it before it was deleted*, and
without read records there is no answer. `proxy.jsonl` is the model — record the
entry ID, the session, and the time, and never the body.

This is the one place the recommendation in §3 is in tension with itself. A
read-only mount is safe and unattributable; an endpoint is attributable and
ignorable. §7 resolves it by starting with the endpoint, because a feature whose
first question is "did anyone actually read this" needs the counter more than it
needs the convenience.

### The blast-radius sentence

In key mode today, a compromised guest holds a random per-session token and its
mounted repositories. With shared memory it also holds a **write handle to
everything every future session will read**. That is an increase, it is not
obvious from the feature's name, and under saint's own rules it belongs on
screen and not only here.

## 6. What the interface shows

Blast radius on screen, or the feature does not ship.

**In the sidebar**, for the selected session: entries read this session, entries
written, entries pending promotion. Observed counts only — `-` when nothing is
measuring, matching how session activity already reads `-` when no proxy in this
process is carrying the traffic. No relevance scores, no confidence percentages;
those would be invented numbers with a decimal point.

**A memory screen**, reached by a key, in the shape of the existing control rail
and proxy screens: entries newest first, each one line of content plus a
provenance line reading `claude · session alpha · 2026-09-16 15:04 · ~/Dev/foo`,
a state of `quarantined` / `shared` / `superseded`, and `d` to delete with the
same confirm the session list already uses for `d`. Filtering with `/`, the same
as everywhere else.

**Pending promotion is the one state that may use saint's single saturated
colour**, because it is the one state where a person has to act. Everything else
is plain. And as everywhere else, the colour is never the only carrier: the word
`quarantined` is on the row.

**Deletion is deletion**, plus a tombstone that records what was removed and
when, so the audit question in §5 stays answerable after the entry is gone.

**No entry is served that saint cannot show.** If provenance is missing or the
scope is unresolvable, the entry does not go out. A memory nobody can inspect is
worse than no memory, and a memory nobody can correct is worse still.

Narrow terminals drop the panel whole, like the sidebar and the rail.

## 7. The smallest version worth building

Deliberately smaller than everything above.

**M0.** One host endpoint on the proxy's existing authenticated hop — a single
path, authenticated by the session's existing proxy token, returning a
plain-text memory document scoped to the session's primary repo path. The file
is written **by the user**, by hand, in `~/.saint/memory/`. No writes from the
guest. No database. No cross-session sharing, because with no agent writes there
is nothing to share that a human did not author. The agent learns the endpoint
exists through whichever of §3's mechanisms the spikes say works for both
runtimes.

Note one implementation detail so it is not a surprise: `route()` in
`internal/proxy/proxy.go` falls through to "forward anything with an
`anthropic-version` header upstream" in subscription mode, so a saint-local path
has to be matched **before** that fallback or it will be proxied to Anthropic.

What M0 proves, in order of how likely it is to kill the feature:

1. **Do the agents read it when told?** Unknown today, and it is the entire
   premise.
2. **Is the content worth anything a week later?** Also unknown, and it cannot
   be simulated.

### The measurement, in `docs/research/spikes.md`'s spirit

Three things to measure, all on real use over a fixed window of at least a week,
all reported as counts with the denominator stated:

- **Read rate.** Sessions that fetched the endpoint at least once, and fetches
  per session, **broken out by runtime**. A runtime that never reads is reported
  as `0`, not folded into an average that makes the feature look alive. This
  needs no new machinery beyond the endpoint's own log line.
- **Decay.** Of the entries present at the start of the window, how many were
  still true at the end, and how many a human had to correct. A memory with a
  high correction rate is worse than none, and this number is the kill criterion.
  It requires a human to reread the entries at the end; there is no automatic
  version of it.
- **Token cost.** Input tokens and `cache_read` / `cache_write` per request, with
  and without the memory preamble, using the meter that already exists. This one
  is measurable immediately and settles the cost side of option E before anyone
  argues about it.

Blocking spikes, before M0 is planned rather than after:

- Does the pinned Codex image support MCP, and through which config key?
  Unverified here. If the answer is no, the cross-agent premise fails and the
  design changes shape.
- Which launch-time context flags each pinned agent actually accepts, checked
  inside the images rather than from memory of the documentation.
- Does `container run` take a second `--volume` with `:ro` alongside the session
  directory mount, with saint's exact argument set? `:ro` enforcement is verified
  in `docs/research/spikes.md`; this specific combination is not.

## 8. What I would not build

**Proxy-side prompt injection.** §3, option C. The reasons are five, and the
first one is sufficient on its own. It is also a one-way door: once saint has
edited a prompt, no user can be told again that the proxy only observes.

**A vector index.** Embedding requires an embedding model, which means the memory
feature makes API calls the user did not ask for, billed to a session that did
not make them or to no session at all, which breaks the per-session attribution
the proxy exists to provide. And retrieval quality
cannot be evaluated here without a labelled set, so it would be tuned on
impressions. FTS5 is unglamorous and measurable.

**Automatic promotion by heuristic.** "Two sessions agreed" is worthless when two
sessions read the same poisoned file. Promotion is a human act.

**Global memory by default.** §5. Scope is the cheapest mitigation available and
it costs one field.

**Automatic expiry or confidence decay.** A confidently deleted true fact is the
same class of error as a confidently displayed wrong number. Show the age; let a
person delete.

**A team or shared-server backend.** A memory shared across people is a memory
shared across trust domains, and the §5 analysis would have to be redone from
the start.

**Memory that stores kinds 1 and 2 from §2.** Durable facts and decisions belong
in the repository, reviewed. If saint wants to help, it should help write the
diff, not keep a private copy.

## Open questions

The one that cannot be resolved from here: **does a shared memory beat a
committed `CLAUDE.md` for anything a developer cares about?** Every argument
above concedes the durable ground to repo files and keeps only the episodic and
the machine-local. Whether that remainder is valuable enough to justify a new
trust boundary between sandboxes is not something this document can settle by
reasoning, and M0's decay measurement is the closest available answer.

The one that gates the design's shape: **Codex's MCP support in the pinned
image.** Unverified. If it is absent, "any agent, any model" becomes "Claude
Code, plus a file", and the honest response is to say so on screen rather than
to describe the feature as cross-agent.

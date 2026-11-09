# Mounting skills into a session

A design note, not a plan of record. The proposal is a host-side directory of
agent skills that saint mounts read-only into a session at creation, so a user
who has vetted a skill once can hand it to every future session without typing
anything into the guest.

The question this grew out of: can saint hand an already-running (or
about-to-start) session a plugin the user installed elsewhere, the way they
tried to do by hand with `npx` before this session existed. The short answer
worked out in conversation is: not live, into a running microVM - but yes, at
creation, as a mount, the same way a repo is a mount. This document is the
shape of that, and specifically the two pieces that make it safe rather than
just convenient: **mount validation** and **the hash check that gives a
session's "carries skill X" claim something to stand on**.

Read §3 and §4 first if you only read two sections; those are the parts that
were asked for by name, and they are where this stops being a mount flag and
starts being something worth shipping.

## 1. What a skill actually is here

Not a binary, not a protocol. A directory of files - instructions, maybe a
reference doc, maybe a small script - that an agent reads the same way it
reads `CLAUDE.md` today. CLAUDE.md's own first line to the agent is "these
instructions OVERRIDE any default behavior and you MUST follow them exactly as
written." A mounted skill is that sentence, pointed at a directory the user
chose instead of a file the repo's maintainers reviewed.

That framing matters more than the mount mechanics: **the security question is
almost entirely about trust in content, not about container escape.** The
microVM already contains what a compromised *agent* can do. It does nothing
about what an agent faithfully does because a file told it to.

## 2. `store.Config` schema

Mirrors the existing `Credentials` / `DefaultCredential` / `MountRoots`
pattern in `internal/store/store.go`:

```go
// Skills maps a skill name to where its content lives on disk, so it can be
// swapped without touching every session that references it by name.
//
// Empty means "not installed." There is deliberately no fallback mode like
// CredentialAPIKey - a skill has no lesser mode to fall back to, an
// unresolvable name just isn't mounted, and Create refuses the session
// rather than starting it silently short a skill it was asked for.
Skills map[string]string `json:"skills,omitempty"`

// DefaultSkills are mounted into every new session unless a request names
// its own set. Empty means none. Unlike DefaultCredential there is no "auto"
// concept worth guessing at here: handing every session a skill it did not
// ask for is the silent scope creep the rest of this file argues against.
DefaultSkills []string `json:"default_skills,omitempty"`

// SkillsRoot is where every path in Skills must resolve to live under, and
// the only directory a future `saint skills install` may write into. Empty
// means ~/.saint/skills.
//
// This is the positive gate, playing the same role MountRoots plays for
// repos: not a denylist of bad places, the one place a skill is ever
// permitted to come from.
SkillsRoot string `json:"skills_root,omitempty"`
```

```go
func (c Config) SkillPath(name string) string {
	return strings.TrimSpace(c.Skills[name])
}

func (c Config) DefaultSkillsOrEmpty() []string {
	return append([]string(nil), c.DefaultSkills...)
}

func (c Config) SkillsRootOrDefault(home string) string {
	if root := strings.TrimSpace(c.SkillsRoot); root != "" {
		return root
	}
	return filepath.Join(home, ".saint", "skills")
}
```

Nothing here fetches anything. Populating `~/.saint/skills/<name>/` is a
separate, explicit, user-run step (`saint skills install <name>` or just
`cp -r`), never something `session.Create` does on the caller's behalf. That
is the single decision that keeps this from repeating the `npx` problem that
started the conversation: session creation must never be the moment saint
reaches out to a registry.

## 3. Mount validation

Two things need validating, for two different reasons, and they are checked
at two different points in the call path for the same reason
`ValidateContainerName` is checked both at load and immediately before exec in
`internal/sandbox/sandbox.go`: "no future caller can skip it."

**The name** reaches a mount target path (`/saint/skills/<name>`) and a
`--volume` flag. It is not attacker-controlled today - it comes out of
`Config.Skills`, which only the user edits - but neither was the container
name, and that file's own comment is explicit about why it is still validated:
"there is no shell involved so this is not command injection, but an
unvalidated name is still a way to smuggle flags." Same shape, same rule:

```go
// skillNamePattern is the skills equivalent of containerNamePattern.
var skillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func ValidateSkillName(name string) error {
	if !skillNamePattern.MatchString(name) {
		return fmt.Errorf("invalid skill name %q", name)
	}
	return nil
}
```

**The path** needs the positive-gate treatment `normalizeRepoPath` and
`checkMountRoot` already give repos - resolve symlinks, then require the
result live under `SkillsRoot`, not just be *some* directory the user typed.
Unlike a repo, a skill has no legitimate reason to live outside its root: a
repo can be anywhere under a mount root because that is where a user's work
already is, but a skill is something saint itself is meant to be the sole
writer of the catalog for. So the check is simpler than `normalizeRepoPath` -
one allowlist, no denylist needed on top, because there is nothing else a
skill is ever supposed to be:

```go
// resolveSkills turns requested skill names into validated, hashed mounts.
// It is the skills-side counterpart to normalizeRepos: a refusal here means
// the session does not start, the same as an unmountable repo.
func (s *Service) resolveSkills(cfg store.Config, names []string) ([]sandbox.SkillMount, error) {
	if len(names) == 0 {
		return nil, nil
	}
	home := resolveOrSelf(s.Store.Paths().Home)
	root := resolveOrSelf(cfg.SkillsRootOrDefault(home))

	mounts := make([]sandbox.SkillMount, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}

		if err := sandbox.ValidateSkillName(name); err != nil {
			return nil, fmt.Errorf("skill %q: %w", name, err)
		}

		configured := cfg.SkillPath(name)
		if configured == "" {
			return nil, fmt.Errorf("skill %q is not configured", name)
		}

		resolved, err := normalizeSkillPath(configured)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", name, err)
		}
		if !underPath(resolved, root) {
			return nil, fmt.Errorf(
				"skill %q resolves to %q, outside skills_root (%s)",
				name, resolved, root)
		}

		hash, err := hashSkillTree(resolved)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", name, err)
		}

		mounts = append(mounts, sandbox.SkillMount{
			Name: name,
			Path: resolved,
			Hash: hash,
		})
	}
	return mounts, nil
}

func normalizeSkillPath(raw string) (string, error) {
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", raw, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks for %q: %w", raw, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", resolved)
	}
	return resolved, nil
}
```

`underPath` and `resolveOrSelf` already exist in `internal/session/service.go`
- this reuses them rather than adding a parallel copy.

One deliberate omission: no check against `blockedSystemRoots` or
`blockedHomeDirs`. Those exist because a *repo* mount can point almost
anywhere the user's filesystem allows. A skill mount cannot - it is already
constrained to a single directory tree saint itself defines the boundary of -
so the denylist that exists to catch a too-wide repo mount has nothing left to
catch here. The allowlist is already narrower than the denylist would be.

## 4. The hash check

This is the piece that turns "the sidebar says this session carries skill
`apple-design`" from an assertion into something checked, and it exists
because of one specific gap: `Config.Skills[name]` is a path, and the file at
that path is ordinary, user-writable state. Nothing stops the user (or
anything running as the user) from editing it between the moment one session
is created and the moment another one asks the sidebar what the first one is
carrying. A name alone answers "which skill was requested," never "was it the
same skill both times."

**What gets hashed and where it's recorded.** At mount-resolution time
(`resolveSkills`, above), walk the skill's directory tree and fold it into one
digest:

```go
// hashSkillTree returns a deterministic digest of a skill directory's
// content, independent of file order or OS-reported timestamps - only path
// and byte content affect the result, so the same skill hashes the same on
// every machine that has it.
func hashSkillTree(dir string) (string, error) {
	var entries []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// A symlink inside a skill directory can point anywhere on the host,
		// including outside SkillsRoot - hashing its target would hash
		// content nobody validated as a skill at all. Reject rather than
		// follow, the same posture blockedSystemRoots takes toward paths
		// that reach outside their own boundary.
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("skill contains a symlink at %q, which is not allowed", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		entries = append(entries, rel+" "+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(entries)
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
```

The digest lands on the session record, `internal/store`:

```go
// SkillHashes records, per skill name, the digest of what was actually
// mounted at Create time - not what is on disk now. It exists so a later
// question ("is this session still carrying the skill I reviewed?") is a
// comparison, not a guess. See hashSkillTree.
SkillHashes map[string]string `json:"skill_hashes,omitempty"`
```

**What it's checked against, and when.** The mount, once made, is fixed for
the container's life - `RenewCredential`'s own comment already establishes
this for the subscription token: "the only way to hand a running guest a new
environment is to give it a new container." Skills inherit the same fact for
free, so the hash is not a live gate on every guest read. It is a drift
check, run on demand (a `saint doctor`-style pass, or a check when the
session list is drawn):

```go
// SkillDrift reports, for a running session, which of its mounted skills no
// longer match what was hashed at Create time. It never re-mounts anything -
// see RenewCredential's own note that a running guest cannot be handed a new
// environment - it only tells the user a renewal (new container) would pick
// up a change.
func (s *Service) SkillDrift(session store.Session) (map[string]bool, error) {
	cfg, err := s.Store.EnsureConfig()
	if err != nil {
		return nil, err
	}
	drift := make(map[string]bool, len(session.SkillHashes))
	for name, was := range session.SkillHashes {
		path := cfg.SkillPath(name)
		if path == "" {
			drift[name] = true // no longer configured at all
			continue
		}
		resolved, err := normalizeSkillPath(path)
		if err != nil {
			drift[name] = true
			continue
		}
		now, err := hashSkillTree(resolved)
		if err != nil {
			drift[name] = true
			continue
		}
		drift[name] = now != was
	}
	return drift, nil
}
```

**The honest limit.** There is a gap between "hash computed" and "container
started" - both `resolveSkills` and `AppleContainer.Create` run in the same
host process, but not atomically, so a change written to the skill directory
in that window would mount content the recorded hash does not describe. This
is the same class of imprecision `resolveAuth` already accepts for credential
expiry - it checks `Status(ctx)` before `Get(ctx)`, not at the instant the
guest first uses the token - and the mitigation is the same: the hash is a
provenance record checked before starting, not a cryptographic guarantee
enforced by the mount itself. Closing it fully would mean staging an
immutable copy of the skill tree per session before mounting, which is a real
option if the gap ever turns out to matter, and is deliberately not proposed
here as a first version.

**`RenewCredential` is not just a reader of drift - it is the one place drift
gets enforced.** `SkillDrift` above is the passive, on-demand form: compute,
compare, report, never refuse. Renewal is different, because it is the one
moment a session's mounts actually change - "the only way to hand a running
guest a new environment is to give it a new container" applies to skills as
much as it does to the subscription token `RenewCredential`'s own doc comment
was written for. That makes renewal the single point where a drifted skill
could otherwise slip into a session unreviewed, silently, on an action the
user framed as "fix my credential," not "also accept whatever changed in my
skills directory since I last looked."

So `RenewCredential` re-resolves every skill the session already carries
(`resolveSkills(cfg, names)`, keyed off `session.SkillHashes`) before it does
anything destructive, and compares each result against the hash already on
the session:

- **No drift** - the freshly resolved mounts (same content, same hash) are
  carried into the new container's `CreateOptions.SkillMounts`. This is
  "refresh," not "keep the old value": the mount always has to bind to
  whatever is on disk right now regardless of which hash gets stored, since
  nothing is snapshotted anywhere - so recomputing is the only way
  `SkillHashes` stays an honest description of what the new container
  actually carries, rather than a number that was true once and is now just
  asserted.
- **Any drift** - refuse the whole renewal before `sessionDir`,
  `proxyToken`, or `Sandbox.Remove` are touched, the same ordering
  `resolveAuth`'s own refusal already uses ("resolve before removing
  anything... leave the session exactly as it was, which can still be
  undone"). The existing container keeps running unchanged. A credential has
  a fallback for this situation - key mode, with a logged warning - because a
  session that bills the API is still better than no session. A skill has no
  equivalent lesser mode: there is nothing between "the reviewed content" and
  "different content," so drift means refusing, not degrading. See
  `service.go:867-891`.

This resolves the "refresh vs. keep-original-hashes" question the way stated
above - refresh, because keeping a stale hash while still mounting current
disk content would be the exact thing this whole feature exists to prevent, a
confident number that no longer describes what it claims to - but adds the
part that question alone did not settle: refreshing silently was never the
right default, because it would let a renewal - an action about billing, not
about skills - quietly carry forward content nobody re-reviewed. Blocking is
what keeps that decision in the user's hands rather than automatic.

## 5. Sandbox plumbing

```go
// sandbox.go
type SkillMount struct {
	Name string
	Path string // host directory, already validated and resolved
	Hash string // from hashSkillTree, for the manifest below
}
```

`CreateOptions` gets `SkillMounts []SkillMount`. In
`AppleContainer.Create`, alongside the existing repo-mount loop
(`applecontainer.go:143`):

```go
for _, skill := range opts.SkillMounts {
	// Always :ro. A repo's ReadOnly is a per-mount choice because the
	// second repo might genuinely need write access; a skill mount never
	// does, because it is shared across every future session, not just
	// this one - see the security section below.
	mount := skill.Path + ":/saint/skills/" + skill.Name + ":ro"
	args = append(args, "--volume", mount)
}
```

And one file written into the session directory (already mounted read-write
at `/saint`, already how the proxy token and bootstrap error file reach the
guest today) rather than baked into the volume flags: a small JSON manifest
listing each mounted skill's name and hash. Not because the guest needs to
verify anything - it has no reason to distrust its own mount - but because it
gives the agent, and anyone reading the transcript, a plain answer to "what
skills is this session running with" without shelling out to compute a hash
by hand.

Discovery - how each runtime is actually told these directories exist and
should be read - is runtime-specific and is **not resolved by this
document**. It is the same category of open question §3 of the persistent
memory design note leaves open for MCP support: verify inside the pinned
images before writing it into a plan.

## 6. Security architecture

**Read-only solves the write-forward problem, not the read problem.** A
guest that cannot write into `/saint/skills/*` cannot poison what the *next*
session sees. It does nothing for *this* session: the agent reads the
content the moment it starts, before any human has looked at anything that
run does. There is no boundary-and-swap trick available the way there is for
credentials - the proxy can hold the real API key and hand the guest a token
because the guest never needs the real value, only its effect. A skill's
entire value is its content reaching the agent verbatim. The mitigation has
to be provenance and review on the host side, because there is no
equivalent interception point on the guest side.

**Skills are data, not executables, as a rule, not an assumption.** Nothing
in the bootstrap script should ever auto-source, `chmod +x`, or execute
anything found under `/saint/skills`. Left implicit, this is the difference
between "a skill is a trust decision the same size as `CLAUDE.md`" (accepted
already, in this same repo, in this same file) and "a skill is arbitrary code
execution at container boot, before the agent or the user has done anything."
The first is a known, accepted category. The second is a new one, and nothing
about the feature needs it - an agent that reads instructions and chooses to
act on them already covers every legitimate use a skill is for.

**No auto-fetch, restated as the load-bearing decision it is.** §2 already
makes this the only way `Config.Skills` gets populated. Worth restating here
because it is the actual security boundary of the whole feature, more than
the mount flags are: the moment session creation can reach a registry on its
own, the feature is fetching and running unreviewed third-party instructions
on a schedule the user does not control, which is precisely the shape of the
`npx` problem that motivated this conversation.

**Name collisions across skills are a namespace problem, not a security
one**, given the validation in §3 - two skills cannot collide on a mount
target because the target is derived from the same validated name that
selects the config entry. Worth naming so it is clear it was considered, not
missed.

**A shared-team skill registry is out of scope here for the reason
`persistent-memory.md`'s §8 already gives for a shared memory backend**:
the moment a skill's source is "everyone with write access to a shared
location" rather than "this user, on this machine," the trust analysis above
has to be redone from the start, because the actor who can poison a skill is
no longer the same actor the rest of saint's design already trusts.

**The blast-radius sentence.** Today, in key mode, a compromised guest holds
a random per-session token and its mounted repositories. With skills
mounted, it also holds - and more importantly, the *agent inside it acts on*
- every instruction in every mounted skill, the moment the session starts,
with no review step in between. That is not a new class of compromise
(`CLAUDE.md` already carries the same authority) but it is a wider one, since
one skill can now be attached to every repo a user works in rather than
living in just one repository. Under saint's own rule - state the blast
radius on screen, not only in a document like this one - the sidebar entry
for a session's skills is not a nice-to-have, it is the same disclosure the
credential line already makes mandatory.

## 7. What the interface shows

This section was written against saint's TUI, which has since been removed in
favour of the herdr plugin. Its layout reasoning still applies to whatever
herdr surface shows a session, through `pane.report_metadata`; the file and
line references do not.

The TUI's sidebar (`sidebar.go`) grouped "what this session carries" above
"what this session has used": name, runtime and state, last run, then
`billingLabel` and `credentialLifeLine` (`sidebar.go:271-289` and `:238-265`),
then a blank line before the context and token blocks. Skills belong in that
first group, for the reason §6 already gives - carrying a skill is the same
category of disclosure as carrying a credential, not a usage statistic.

Ordinary state - two skills mounted, both matching their recorded hash. No
line says "verified"; an unremarkable fact does not get a word, matching how
`billingLabel` is silent about a credential that is simply fine:

```
╭──────────────────────────────────╮
│ SESSION                          │
│ saint-tui-skills                 │
│ claude · running                 │
│ last run 4m ago                  │
│ subscription                     │
│                                   │
│ SKILLS                           │
│ apple-design                     │
│ release-notes                    │
│                                   │
│ CONTEXT                          │
│ ▓▓▓▓▓▓▓▓▓░░░░░░░░░░░░░░░░  38%    │
│                                   │
│ TOKENS                           │
│ in                         12.4k │
│ out                            891│
│ cache read                 44.2k │
│ cache write                  6.1k│
│ requests                        9│
╰──────────────────────────────────╯
```

Drifted state - `release-notes` on disk no longer hashes to what this
session was created with (`SkillDrift`, §4). This is the one row that may use
the single saturated colour, because it is the one row where a person has to
act - and per the palette rule, the colour is never the only carrier, so the
row still says the word:

```
│ SKILLS                           │
│ apple-design                     │
│ release-notes     changed on disk│  ← attention colour, no key wording
```

**Resolved: the row does not say "c renews it," and that is deliberate, not
an omission.** `credentialLifeLine` only ever pairs its saturated colour with
action wording (`"· c renews it"`) because, for that row, the key genuinely
does fix it. Skills break that pairing: `RenewCredential` now refuses to
renew a session with a drifted skill, the same way it already refuses a
subscription session it cannot read a fresh credential for (§4,
`service.go:867-891`) - so for a credential row `c` means "try it," and for a
skill row `c` means "this will fail until you have gone and looked at the
skill." Printing "c renews it" on the skill row would be the sidebar
asserting something false about its own interface, which is a worse failure
than the drift itself - the entire feature exists to stop a stale claim from
sitting on screen unchecked.

So the skill row carries the fact, "changed on disk," and nothing else. That
fact is already the word the palette rule requires - it does not depend on
colour alone to say something is wrong - and it does not promise a remedy the
interface cannot deliver from this screen. The actual remedy (review the
change, then a *new* session picks up fresh content; the drifted one stays
put) lives in the error text `RenewCredential` already returns when `c` is
pressed and refused (§4's error message, verbatim), not duplicated into 30
columns of sidebar. A user who presses `c` anyway is not stuck or misled by
it - they get told exactly why, in full sentences, the moment they ask; the
sidebar's job before that point is only to flag that asking will not go the
way the credential row's identical colour might suggest.

A session that was never asked to carry any skills renders no `SKILLS`
heading at all - the block does not appear as "none", the same way
`credentialLifeLine` renders nothing rather than a blank credential line
when there is nothing to report.

## 8. What I would not build

**Skills fetched at session-create time**, from `npx`, a marketplace, or
anywhere else. §2, §6.

**Executable skills** - anything the bootstrap script runs automatically
rather than the agent choosing to read. §6.

**A cross-session hash comparison at `Create`.** Raised in the original open
questions as: should a *new* session's freshly resolved hash ever be checked
against a hash some *other*, earlier session recorded for the same skill
name, and warn or block on a mismatch, the way `resolveAuth` warns on a bad
credential? Resolved here as no. `RenewCredential`'s drift check (§4) asks
"does this session still carry what it carried before" - a comparison against
that same session's own prior state, which is the only state a mount can
meaningfully drift from. A new session has no such prior state of its own: at
`Create`, `resolveSkills` hashes whatever is on disk right now, and that *is*
the baseline, not a candidate to be judged against someone else's history. A
different session's stored hash records what *that* session's user reviewed,
at whatever point they reviewed it - it says nothing about whether today's
content is fit for a session that has not started yet. Surfacing a mismatch
here would present another session's history as if it were a defect in this
one, which it is not. If "you last touched this skill on `<date>`" is useful
context on its own, it belongs in a `saint doctor`-style listing that shows
skill history for its own sake, not as a gate or warning wired into `Create`.

**A denylist in place of `SkillsRoot`.** The positive gate is already
narrower and simpler than any denylist could be, for the reason given in §3 -
there add one only if a future version legitimately needs skills to live
outside a single root, and if that day comes, reuse `checkMountRoot`'s
pattern rather than inventing a second one.

**Live injection into a running container.** Raised and set aside in the
original conversation: possible in principle (write to the shared `/saint`
mount, have the agent re-scan) but the actual capability - "give this
session a new skill" - is already served by a renewal, the same as a
credential refresh, and does not need a second mechanism with its own
failure modes.

**Automatic trust by hash match.** A matching hash means "the content is the
same one that was reviewed before," never "the content is safe." Nothing
here should read as approval of the content itself - only as an honest
record of whether it changed.

## Open questions

**Per-runtime discovery.** §5. Not resolved here; blocking before any of this
is planned rather than sketched.

**Size and content limits.** A skill directory today has no cap. Given it is
read entirely at every session start, an unbounded one is a resource
question worth a number before this ships, not a security one.

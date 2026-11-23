# Experimental tool profile (draft)

Implementation is tracked in [#39](https://github.com/montcao/saint/issues/39).
Security, performance, recovery and storage qualification are tracked separately
in [#40](https://github.com/montcao/saint/issues/40). Do not open duplicate issues
or treat synthetic checks as completion of either gate.

This opt-in prototype attaches an EROFS `/usr` disk to an exclusive writable
ext4 root, using raw EROFS or an LZFSE/ULFO wrapper. It is not enabled by default,
is not release-qualified, and does not ship bootable profiles or binary disks.

## Implementation status under #39

The profile handoff, adapter and ownership checks are implemented. The prototype
still uses the existing tmux bootstrap: closing a pane detaches, and `saint stop`
stops its VM. This **does not satisfy #39's direct-pane lifecycle requirement**.
Coordinate direct attached starts, readiness markers and activity panes with
[#38](https://github.com/montcao/saint/pull/38). Complete tool-profile build recipes,
positive agent/tool execution, terminal parity and concurrent-pane acceptance
also remain #39 work, rather than qualification-only follow-ups.

## Selecting a profile

Build Saint normally. Build the optional helper separately on an ARM64 Mac:

```sh
make build
scripts/build-tool-disk-adapter /absolute/private/profile/ToolDiskAdapter
```

The script pins the entire Swift resolution, uses owned temporary caches and
works around stale CLT private SwiftPM interfaces without editing the installed
SDK. It does not install a daemon, change the runtime or set a default kernel.

After reviewing a manifest and its security report, select its **exact bytes**:

```sh
saint new --experimental-profile '/absolute/private/manifest.json#SHA256' /allowed/disposable/repo
saint herdr open claude --experimental-profile '/absolute/private/manifest.json#SHA256'
```

The latter runs in a herdr plugin action context. The profile is passed explicitly
as `SAINT_EXPERIMENTAL_PROFILE` to the fixed `session` pane entrypoint. Ordinary
plugin actions explicitly clear that variable, even when their parent inherited
it. The plugin manifest and default launch command are unchanged. There is no
config default, automatic fallback or automatic download. Reopened sessions use
their saved selection. Claude key-mode proxy sessions only are supported in this
draft; subscription credentials, credential renewal and Codex profiles refuse.
Select API-key billing explicitly (`proxy_auth=key`, `default_credential=api-key`
or `saint new --credential api-key`); the experiment never reads a subscription
or silently switches its billing mode.

## Manifest and artifact contract

Schema 1 has these fields (see `internal/toolprofile/profile.go` for the exact
JSON contract):

| Field | Requirement |
| --- | --- |
| `schema` | `1` |
| `runtime_commit` | `a15b46097aa4c2325da13c1fb5192b9274aab083` (Apple container 1.7.1) |
| `format` | `erofs` or `ulfo` |
| `adapter`, `tools`, `root`, `kernel`, `review` | Each is `{ "path": "/absolute/canonical/path", "sha256": "64 lowercase hex digits" }` |
| `tool_sha256` | SHA256 of the EROFS bytes, also checked inside the mounted ULFO image |
| `image_reference`, `image_digest` | Already-present scaffold OCI reference ending in `@sha256:…`, and matching digest |
| `init_reference`, `init_digest` | Already-present init OCI reference ending in `@sha256:…`, and matching digest |

Artifacts must be owned regular files, mode 0600 (0700 for the helper), without
symlinks or hardlink aliases. Ancestors must not permit untrusted replacement.
The manifest itself is SHA256 pinned; unknown fields and trailing JSON fail.
The review file must cover the exact tool inventory, root, kernel, init image
and dependency lock. **A review-file hash records a local review decision; it
is not a vendor signature, vulnerability scan or automatic security approval.**
Publisher provenance and release signing remain gates below.

The private root template must include the non-root `agent` account (uid/gid
1000), `/home/agent`, mountpoints, merged-/usr symlinks and configuration matching
the tools. `/usr` must contain Claude, tmux, shell, Git, SSH client, ripgrep,
Python and required libraries/stdlib, curl, less and certificates. Each session
gets a distinct copied/cloned inode at `tool-profiles/<container>/root.ext4`.
Full content hashing is intentional; the older host-specific sparse receipt is
not accepted as a portable artifact digest. A compact root template avoids
hashing hundreds of gigabytes of logical holes.

ULFO must contain a regular `tools.erofs` at its filesystem root. The helper
attaches it readonly at a private session mountpoint, checks backing image,
owner uid, readonly state and LZFSE type, then checks the inner EROFS hash. It
sets both the VM block attachment and guest mount readonly. No host disk-image
mount is needed for the raw control. The kernel is supplied per VM; the global
kernel setting is never changed.

## Ownership and recovery

An exclusive directory reservation and random 256-bit owner nonce precede any
network/VM mutation. The durable pending session is discoverable even when
creation fails. VM labels bind the owner and manifest; network labels bind the
same owner. All manifests, disks and ownership records must be outside every
share requested by that session. No SSH forwarding is enabled and the helper
receives a minimal host environment, without provider keys or runtime overrides.

Every helper mutation holds a nonblocking per-session lock. Removal verifies VM
labels, stops and deletes exactly that VM, confirms its absence, normally
detaches only the matching owned ULFO volume, and removes only known private
files. It never force-detaches, prunes, deletes shared images/snapshots, or runs
global content GC. Network ownership is rechecked before network deletion.
Session metadata is removed last. An error retains state for an explicit
`saint rm <id>` retry; ownership failures never fall back to legacy deletion.
Creation rollback uses a fresh bounded context after cancellation. A small
ownership tombstone is retained after successful cleanup, with launch commands,
mounts and session-token environment scrubbed. Artifact templates are not owned
by a session and are never removed by session cleanup.

A user-owned host process can still change its own trusted artifacts. This is
not a security boundary against a malicious host user. Root/mount crash recovery,
other-session interference and power-loss durability require positive testing;
file and directory synchronization does not establish a power-loss guarantee.

## Dependency baseline and qualification under #40

The adapter pins Apple container 1.7.1 and Containerization 0.54.0, including the
transitive revisions in `tools/tool-disk-adapter/Package.resolved`. The running
daemon must match the pinned commit. Generic dependency references:
[container advisories](https://github.com/apple/container/security/advisories),
[Containerization advisories](https://github.com/apple/containerization/security/advisories),
[upstream release](https://github.com/apple/container/releases/tag/1.7.1).
A dependency pin is not complete security qualification. The helper never
upgrades the runtime or changes its global settings.

Use #40 for authenticated manifests, provenance, complete dependency review,
update/rollback policy, bounded retained generations and resources, paired
performance runs, complete storage accounting and recovery qualification.
Compare identical tool profiles and workloads; account separately for shared
runtime data, private roots, caches and temporary extraction. Neither sparse
apparent size nor summed filesystem blocks establishes unique physical storage.
Default enablement stays blocked until the agreed gates pass. No storage ratio
or host-resource reduction is claimed here.

## Public content policy

Commit only implementation code, clearly synthetic fixtures and generic
documentation. Keep machine specifications, local paths, real session identities,
configuration fingerprints, runtime inventories, raw logs, private artifacts and
experiment measurements outside the checkout and public PR description. A
hashed configuration is still private evidence. Do not publish private security
findings through the issue tracker.

## Reproducing checks

```sh
make check
make vulncheck
make docs-check
PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_tool_profile_privacy.py
scripts/build-tool-disk-adapter /absolute/private/ToolDiskAdapter
python3 scripts/test-tool-profile-gate.py \
  --saint /absolute/saint --adapter /absolute/private/ToolDiskAdapter \
  --herdr /absolute/herdr --output /private/output/gate.private.json
```

The Python test intentionally requires an incompatible *running* daemon. It
uses the real Saint binary, actual plugin manifest and an installed compatible herdr server,
with an owned HOME, config, socket, workspace and disposable Git repository.
Its nonbootable artifacts exercise verification and the runtime gate only.
It neither contacts a model service nor uses credentials. It tests three
open/error/close/reopen cycles, neighbor responsiveness and resource cleanup.
A supported daemon makes this negative test refuse to run; it is not a substitute
for #39 acceptance or #40 qualification. The output contains only allowlisted
pass/fail assertions; host inventory and configuration comparisons stay in memory.
Keep even that output private unless intentionally prepared for publication.

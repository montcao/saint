# Developer tool-profile build

This recipe is opt-in and does not run during pane launch. Use an isolated Linux
ARM64 builder. It does not require privileged mode, host device mounts, SSH
forwarding, provider credentials or personal project shares. Do not run a shared
Apple builder as part of qualification without separately arranging ownership.
Only this build context is sent to the builder.

The `baseline` stage contains Claude, Bash, Git, Python (including venv/pip), SSH
client, ripgrep, curl, less, terminfo, certificates and their Debian runtime
closure. The `disks` stage splits that **same** filesystem into EROFS `/usr` and
a journaled ext4 private root. Builder packages are outside both compared profiles.
The native Claude tarball, base OCI image and signed Debian snapshot are pinned.
A dated snapshot fixes package resolution, but is not proof of current security
support. Refresh pins and review the full package inventory before qualification.

On the isolated builder, use a new private output directory:

```sh
umask 077
docker build --platform linux/arm64 --target baseline \
  -t saint-profile-baseline:developer tools/tool-profile
docker build --platform linux/arm64 --target disks \
  --output type=local,dest=/absolute/private/new-profile tools/tool-profile
```

No builder is started or stopped by these scripts. Tags above are explicit
local developer names, not trusted release references. Record the resulting OCI
digest before importing the baseline/scaffold into a qualification runtime. The
adapter requires that exact digest to be already present; it never pulls images.
Use the baseline as the scaffold descriptor for the disk-backed guest, so process
metadata and platform agree; its image bytes are a separately counted shared cost.

The assembly emits `tools.erofs`, `root.ext4`, `packages.tsv` and
`build-receipt.json`. A fixed-size 256 MiB root has a journal initialized before
use. Its logical size is an input choice, not an allocated-storage measurement.
EROFS takes a fixed timestamp; ext4 tooling can include generated metadata, so
byte-for-byte disk reproducibility across builds is not asserted. The receipt
binds this build's actual disk bytes and package inventory. Preserve the explicit
normal image build workflow for developers who do not select a tool profile.

Transfer outputs into a private directory on the ARM64 Mac, verify transport
hashes independently, and apply private file permissions. Build the adapter using
`scripts/build-tool-disk-adapter`. Supply a separately reviewed stock guest kernel,
pinned init image and review document; neither kernel discovery nor runtime
configuration mutation is part of this recipe. The review must cover the base,
snapshot packages, native Claude archive, kernel/init and Swift lock. A review
hash records the developer's decision; it does not authenticate a publisher.

Create the optional wrapper, using the raw SHA256 from the receipt:

```sh
python3 scripts/wrap-tool-disk.py --raw /absolute/private/tools.erofs \
  --sha256 RAW_SHA256 --output /absolute/private/tools.dmg
```

The wrapper script invokes Apple's `hdiutil`, which may temporarily mount its own
image. It records ownership in private scratch, never detaches any existing
mount, and retains scratch on failure/timeout/uncertain mount absence. Inspect
that ownership record before manual recovery; never globally prune or detach.
The adapter verifies the inner raw hash when launching, not just the wrapper hash.

For each `erofs` or `ulfo` candidate, create a **new** manifest:

```sh
python3 scripts/make-tool-profile.py --format erofs \
  --adapter /absolute/private/ToolDiskAdapter \
  --tools /absolute/private/tools.erofs --raw-tools /absolute/private/tools.erofs \
  --root /absolute/private/root.ext4 --kernel /absolute/private/vmlinux \
  --review /absolute/private/review.txt --receipt /absolute/private/build-receipt.json \
  --packages /absolute/private/packages.tsv \
  --image 'registry.example/scaffold@sha256:REVIEWED_DIGEST' \
  --init 'registry.example/init@sha256:REVIEWED_DIGEST' \
  --output /absolute/private/raw-manifest.json
```

For ULFO use `--format ulfo --tools /absolute/private/tools.dmg`, the same
`--raw-tools`, root, kernel, scaffold and init. The script prints the exact
`manifest-path#sha256` selection. Placeholder digests above intentionally fail
validation. No runnable binary or fabricated checksum is distributed here.

Offline checks:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_tool_profile_build.py
make check
```

The Linux recipe and actual tool/terminal behavior still require live build and
qualification under #40. Do not infer a performance or storage ratio from these
recipes. Keep generated manifests, local paths, inventories, logs and receipts
private; public changes contain code and synthetic tests only.

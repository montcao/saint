# Roadmap

Work that is decided but not started, and what each one needs first. Known
gaps with no decision yet stay in [`tech-debt.md`](tech-debt.md).

| Item | Why | Needs |
|---|---|---|
| Signed and notarized release binaries | A binary signed with a Developer ID is allowed through the macOS application firewall automatically when "Automatically allow downloaded signed software" is on, which is the default. Then neither setup nor a session has to ask for an administrator password to let the proxy accept connections from its guests (#22). Signing also lets macOS verify the prebuilt binary `scripts/plugin-build` installs, on top of the checksum it checks now. | An Apple Developer ID certificate, a release workflow that signs and notarizes `saint-darwin-arm64`, and a check that `ensureStableBinary`'s copy keeps its signature. |

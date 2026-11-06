# Gotchas that cost real time

Do not rediscover these.

- **A headless `vt` emulator hangs on any device query**, because it answers
  into a synchronous pipe with no reader. `blocked.go` strips queries before
  replay. A reader cannot be shut down cleanly: `SafeEmulator` does not lock
  `Read`, and `Close` then races it on a plain bool inside the library.
- **`Emulator.Render()` emits SGR.** Strip styling before pattern-matching, or
  phrases arrive as `Enter<sgr> to<sgr> confirm` and every match silently
  fails.
- **A raw transcript cannot be pattern-matched.** A full-screen agent paints at
  absolute cursor positions; replay it through an emulator first, then read
  only the footer.
- **Test fixtures must enter the alternate screen.** Scroll tests that drove
  `/bin/cat` on the primary screen passed for as long as the feature was
  broken.
- **Hit-testing must measure** by rendering the same functions the view
  renders. A second copy of the layout arithmetic drifts and clicks land on the
  wrong rows.

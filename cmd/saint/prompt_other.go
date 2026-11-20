//go:build !darwin

package main

import "os"

// flushTerminalInput does nothing off macOS, which saint does not run on; it
// exists so the package still builds for tools that scan other platforms.
func flushTerminalInput(*os.File) error { return nil }

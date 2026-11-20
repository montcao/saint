//go:build darwin

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// flushTerminalInput discards what the terminal has received but nothing has
// read yet: tcflush(fd, TCIFLUSH), which macOS implements as TIOCFLUSH.
func flushTerminalInput(f *os.File) error {
	return unix.IoctlSetPointerInt(int(f.Fd()), unix.TIOCFLUSH, unix.TCIFLUSH)
}

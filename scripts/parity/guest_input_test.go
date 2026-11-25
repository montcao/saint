package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func guestBox(input string) string {
	top := strings.Repeat("─", 16) + " fixture ─"
	return guestBorderStyle + top + "\x1b[0m\n" + input + "\n" + guestBorderStyle + strings.Repeat("─", len([]rune(top))) + "\x1b[0m\n"
}
func TestGuestOrdinaryReadiness(t *testing.T) {
	placeholder := "\x1b[0m\x1b[2mTry \"synthetic\"\x1b[0m"
	for _, input := range []string{"__", "__" + placeholder} {
		if !inputMatches(guestBox(input), "❯", "") {
			t.Fatal("demonstrated guest input rejected")
		}
	}
	for _, input := range []string{`__Try "synthetic"`, "__ ", "__" + placeholder + "typed", "__\x1b[2mTry \"synthetic\"\x1b[0m", "__\nextra", "__printf"} {
		if inputMatches(guestBox(input), "❯", "") {
			t.Fatalf("nonempty/unknown guest input accepted: %q", input)
		}
	}
	if inputMatches(strings.Replace(guestBox("__"), " fixture ", " fixture/path ", 1), "❯", "") {
		t.Fatal("unknown border accepted")
	}
	if inputMatches(guestBox("__"), "!", "") {
		t.Fatal("ordinary input accepted as shell")
	}
}

func TestGuestShellReadiness(t *testing.T) {
	const prefix = "\x1b[0m\x1b[38;5;1m!_"
	const reset = "\x1b[0m"
	if !inputMatches(guestBox(prefix+reset+"\x1b[2mTry \"synthetic\""+reset), "!", "") {
		t.Fatal("shell placeholder rejected")
	}
	if !inputMatches(guestBox(prefix+reset+"printf"), "!", "printf") {
		t.Fatal("exact pending input rejected")
	}
	for _, input := range []string{`Try "synthetic"`, " printf", "printf ", "printf extra", "printf\nextra", "\x1b[2mprintf"} {
		if inputMatches(guestBox(prefix+reset+input), "!", "printf") || inputMatches(guestBox(prefix+reset+input), "!", "") {
			t.Fatal("uncertain shell input accepted")
		}
	}
}

func TestGuestUncertainInputSendsNoEvents(t *testing.T) {
	for _, input := range []string{`__Try "synthetic"`, "__ ", "__printf", "__\nextra", "__\x1b[0m\x1b[2mTry \"synthetic\"\x1b[0mtyped"} {
		d := driver{timeout: time.Millisecond, prefix: "!", run: func(_ context.Context, _ string, args ...string) (string, error) {
			if args[1] != "read" {
				t.Fatal("sent input into uncertain guest")
			}
			return guestBox(input), nil
		}}
		if _, err := d.probe(t.Context(), "guest", "true", "synthetic"); err == nil {
			t.Fatal("uncertain guest accepted")
		}
	}
}

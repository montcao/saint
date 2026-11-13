package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/store"
)

const allowUsage = "usage: saint allow [<host>] | saint allow -rm <host>"

// runAllow manages the saved allowlist: the hosts every session may reach
// through the proxy beyond the shipped ones. A running proxy applies a change
// from its next request; nothing restarts.
//
//	saint allow               list the saved and the shipped hosts
//	saint allow <host>        save a host (or *.example.com for a domain)
//	saint allow -rm <host>    remove a saved host
func runAllow(st *store.Store, args []string, out io.Writer) error {
	switch {
	case len(args) == 0:
		cfg, err := st.EnsureConfig()
		if err != nil {
			return err
		}
		fmt.Fprint(out, formatAllowlist(cfg.ProxyAllowHosts, cfg.ProxyDenyHosts, proxy.DefaultAllowedHosts()))
		return nil
	case len(args) == 1 && !strings.HasPrefix(args[0], "-"):
		added, err := st.AllowHost(args[0])
		if err != nil {
			return err
		}
		rule, _ := store.NormalizeHostRule(args[0])
		if added {
			fmt.Fprintf(out, "allowed %s for every session\n", rule)
		} else {
			fmt.Fprintf(out, "%s was already allowed\n", rule)
		}
		return nil
	case len(args) == 2 && (args[0] == "-rm" || args[0] == "--rm"):
		removed, err := st.DisallowHost(args[1])
		if err != nil {
			return err
		}
		rule, _ := store.NormalizeHostRule(args[1])
		if !removed {
			return fmt.Errorf("%s is not on the saved allowlist", rule)
		}
		fmt.Fprintf(out, "removed %s; sessions are asked about it again\n", rule)
		return nil
	}
	return errors.New(allowUsage)
}

// formatAllowlist says which hosts are allowed and where each rule came from,
// so a person can tell what they chose from what saint ships.
func formatAllowlist(saved, denied, shipped []string) string {
	var b strings.Builder
	b.WriteString("Saved (yours; remove with saint allow -rm <host>):\n")
	if len(saved) == 0 {
		b.WriteString("  none\n")
	}
	for _, host := range saved {
		b.WriteString("  " + host + "\n")
	}
	if len(denied) > 0 {
		b.WriteString("\nRefused without asking (proxy_deny_hosts):\n")
		for _, host := range denied {
			b.WriteString("  " + host + "\n")
		}
	}
	b.WriteString("\nShipped with saint:\n")
	for _, host := range shipped {
		b.WriteString("  " + host + "\n")
	}
	b.WriteString("\nAny other host is asked about in the session's herdr pane, and refused\nwhen there is no pane or no answer.\n")
	return b.String()
}

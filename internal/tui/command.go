package tui

import "strings"

// commandResult is what running a `:` command asks the model to do.
type commandResult int

const (
	// commandUnknown means the input matched nothing; the model shows an error
	// and keeps the command line open state unchanged.
	commandUnknown commandResult = iota
	// commandNone means there was nothing to run (an empty `:`).
	commandNone
	// commandQuit means leave saint.
	commandQuit
	// commandDetach means close the embedded session pane but stay in saint.
	commandDetach
	// commandHelp means toggle the expanded help footer.
	commandHelp
	// commandRefresh means reload live state now.
	commandRefresh
	// commandNew means open the new-session dialog.
	commandNew
	// commandAuth means open the credentials and billing screen.
	commandAuth
)

// runCommand interprets a vim/k9s style command line.
//
// A leading ":" is accepted so both ":q" and "q" work, since the prompt already
// shows the colon and typing a second one is a common reflex.
func runCommand(input string) commandResult {
	cmd := strings.TrimSpace(input)
	cmd = strings.TrimPrefix(cmd, ":")
	cmd = strings.ToLower(strings.TrimSpace(cmd))

	switch cmd {
	case "":
		return commandNone
	// q! and wq are accepted purely because they are muscle memory; saint has
	// nothing to write, so they behave exactly like q.
	case "q", "q!", "qa", "qa!", "quit", "quit!", "wq", "x", "exit":
		return commandQuit
	case "d", "det", "detach":
		return commandDetach
	case "h", "help", "?":
		return commandHelp
	case "r", "refresh", "reload":
		return commandRefresh
	case "n", "new":
		return commandNew
	case "a", "auth", "creds", "credentials", "login":
		return commandAuth
	default:
		return commandUnknown
	}
}

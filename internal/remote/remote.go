// Package remote builds ssh invocations.
//
// Everything here returns an argv slice rather than running anything, so the
// construction — which is where quoting bugs live — is testable without a network
// or a remote host.
package remote

import (
	"fmt"
	"strings"
)

// Quote renders s as a single POSIX shell word.
//
// Arguments cross two shells on the way to a remote command: the local one (which
// we bypass by using argv directly) and the remote login shell, which ssh invokes
// with the command as a single string. Anything not quoted here is re-split and
// re-expanded on the far side, so a path with a space, or a prompt containing $ or
// a backtick, would otherwise be mangled or executed.
//
// Single quotes suppress every form of expansion. The only character that cannot
// appear inside them is a single quote, which is emitted by closing the string,
// escaping the quote with a backslash, and reopening. See the test for the
// exact forms, which are round-tripped through a real shell.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\r\"'\\$`&|;<>()*?[]#~=%!{}") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuotePath quotes a path while still letting a leading ~ expand on the remote
// host.
//
// Quote deliberately protects "~" along with every other expansion character, so
// a config value of "~/projects" would reach the remote as a literal directory
// named "~/projects" — which does not exist. Paths are the one case where that
// expansion is wanted, since inventories and command lines are written with "~"
// and the home directory differs per host and per user.
//
// Only a leading "~/" (or a bare "~") is treated as home; "~user" is not
// supported, and a tilde anywhere else stays literal.
func QuotePath(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		// "$HOME" is double-quoted so a home directory containing spaces
		// survives; the remainder is quoted normally and concatenated.
		return `"$HOME"/` + Quote(rest)
	}
	return Quote(p)
}

// QuoteAll joins args into one shell-safe command string.
func QuoteAll(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = Quote(a)
	}
	return strings.Join(quoted, " ")
}

// Options control how an ssh invocation is built.
type Options struct {
	// TTY forces pseudo-terminal allocation (ssh -tt). Required for anything
	// interactive, including tmux attach.
	TTY bool

	// Dir is an optional remote working directory.
	Dir string

	// ConnectTimeout in seconds. Zero uses DefaultConnectTimeout.
	ConnectTimeout int

	// BatchMode disables password/passphrase prompts. Used for probes, where a
	// hung prompt would stall the whole fleet listing.
	BatchMode bool
}

// DefaultConnectTimeout bounds how long a probe waits on an unreachable host.
const DefaultConnectTimeout = 10

// Command builds the argv to run cmd on target.
//
// The remote command is passed as a single quoted string. When Dir is set the
// command is prefixed with a cd, and `exec` replaces the wrapper shell so signals
// and exit status belong to the real process rather than to an intermediary.
func Command(target string, opts Options, cmd []string) []string {
	argv := []string{"ssh"}

	timeout := opts.ConnectTimeout
	if timeout == 0 {
		timeout = DefaultConnectTimeout
	}
	argv = append(argv, "-o", fmt.Sprintf("ConnectTimeout=%d", timeout))

	if opts.BatchMode {
		argv = append(argv, "-o", "BatchMode=yes")
	}
	if opts.TTY {
		// -tt forces allocation even when stdin is not a terminal; plain -t
		// declines when ssh thinks there is no local tty, which breaks attach
		// in some terminal multiplexers.
		argv = append(argv, "-tt")
	}

	argv = append(argv, target, "--")

	remote := QuoteAll(cmd)
	if opts.Dir != "" {
		remote = fmt.Sprintf("cd %s && exec %s", Quote(opts.Dir), remote)
	}
	argv = append(argv, remote)
	return argv
}

// ExitCodeSSHFailure is what ssh returns for its own errors — but a remote
// command is also free to exit 255, so the code alone is ambiguous.
const ExitCodeSSHFailure = 255

// sshFailureMarkers appear in ssh's own diagnostics, never in the output of a
// remote command that merely happens to exit 255.
var sshFailureMarkers = []string{
	"ssh: connect to host",
	"Connection refused",
	"Connection timed out",
	"Could not resolve hostname",
	"Permission denied (",
	"Host key verification failed",
	"No route to host",
	"Operation timed out",
	"kex_exchange_identification",
}

// IsConnectionFailure disambiguates "ssh could not connect" from "the remote
// command exited 255".
//
// This matters because the two demand opposite responses: a connection failure
// means try another host or fix the network, while a command exiting 255 is a
// real result that should be reported as-is. Guessing from the exit code alone
// would misreport one as the other.
func IsConnectionFailure(exitCode int, stderr string) bool {
	if exitCode != ExitCodeSSHFailure {
		return false
	}
	for _, marker := range sshFailureMarkers {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

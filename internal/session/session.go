// Package session builds the tmux commands that run on the far side of ssh.
//
// tmux is the whole reason remote sessions can be interactive without any custom
// protocol: the session lives on the remote host, survives disconnection, and can
// be reattached from anywhere. A dropped network stops being a lost agent.
//
// As with package remote, everything here returns a script string rather than
// running it, so quoting is testable offline.
package session

import (
	"fmt"
	"strings"

	"github.com/bborn/on/internal/remote"
)

// Prefix marks sessions created by `on`, so listings can tell them apart from
// whatever else the user runs on the host.
const Prefix = "on-"

// Name derives a tmux session name from a command.
//
// tmux treats "." and ":" as address separators, so they cannot appear in a
// session name; anything else outside a conservative set is folded to "-" to keep
// names shell- and eye-friendly.
func Name(cmd []string) string {
	base := "shell"
	if len(cmd) > 0 {
		base = cmd[0]
		// Use the basename of a path so /usr/local/bin/claude -> claude.
		if i := strings.LastIndex(base, "/"); i >= 0 && i+1 < len(base) {
			base = base[i+1:]
		}
	}
	return Prefix + sanitize(base)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "shell"
	}
	return out
}

// CreateOrAttachScript returns a shell script that attaches to an existing
// session of that name, or creates it first if absent.
//
// Create-or-attach is the default because re-running `on <host> claude` almost
// always means "get me back to my agent", not "start a second one". Forcing a new
// session is the explicit case, handled by choosing a different name.
//
// The attach is exec'd so the ssh channel is handed directly to tmux; without it
// an intermediate shell would sit in the middle owning signals and exit status.
func CreateOrAttachScript(name, dir string, cmd []string) string {
	return fmt.Sprintf("%s\nexec tmux attach-session -t %s",
		createScript(name, dir, cmd), remote.Quote(name))
}

// CreateScript returns a script that creates the session detached, doing nothing
// if it already exists.
func CreateScript(name, dir string, cmd []string) string {
	return createScript(name, dir, cmd)
}

func createScript(name, dir string, cmd []string) string {
	var b strings.Builder

	// has-session is the idempotency guard: creating a session that already
	// exists is an error in tmux, and re-running `on` must not be an error.
	fmt.Fprintf(&b, "tmux has-session -t %s 2>/dev/null || tmux new-session -d -s %s",
		remote.Quote(name), remote.Quote(name))

	if dir != "" {
		fmt.Fprintf(&b, " -c %s", remote.Quote(dir))
	}
	if len(cmd) > 0 {
		// tmux takes the command as a single shell-command argument, so the
		// already-quoted command line is quoted once more to survive as one word.
		fmt.Fprintf(&b, " %s", remote.Quote(remote.QuoteAll(cmd)))
	}
	return b.String()
}

// AttachScript attaches to an existing session, failing if it is absent.
func AttachScript(name string) string {
	return fmt.Sprintf("exec tmux attach-session -t %s", remote.Quote(name))
}

// KillScript terminates a session.
func KillScript(name string) string {
	return fmt.Sprintf("tmux kill-session -t %s", remote.Quote(name))
}

// ListFormat is the tmux format used by ListScript, chosen so output can be split
// on tabs without ambiguity.
const ListFormat = "#{session_name}\t#{session_windows}\t#{session_attached}\t#{session_created}"

// ListScript lists sessions on the host. It exits 0 with no output when the tmux
// server is not running, which is a normal state rather than an error.
func ListScript() string {
	return fmt.Sprintf("tmux list-sessions -F %s 2>/dev/null || true", remote.Quote(ListFormat))
}

// Info is one parsed row from ListScript.
type Info struct {
	Name     string
	Windows  string
	Attached bool
	Created  string
}

// IsOn reports whether the session was created by `on`.
func (i Info) IsOn() bool { return strings.HasPrefix(i.Name, Prefix) }

// ParseList parses ListScript output.
func ParseList(out string) []Info {
	var infos []Info
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 4 {
			continue
		}
		infos = append(infos, Info{
			Name:     parts[0],
			Windows:  parts[1],
			Attached: parts[2] == "1",
			Created:  parts[3],
		})
	}
	return infos
}

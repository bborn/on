package session

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/bborn/on/internal/remote"
)

func TestName(t *testing.T) {
	tests := []struct {
		name string
		cmd  []string
		want string
	}{
		{"simple command", []string{"claude"}, "on-claude"},
		{"uses basename of a path", []string{"/usr/local/bin/claude"}, "on-claude"},
		{"ignores arguments", []string{"claude", "--resume"}, "on-claude"},
		{"empty command", nil, "on-shell"},
		// tmux uses . and : as address separators; they must never survive.
		{"dots are folded", []string{"a.b"}, "on-a-b"},
		{"colons are folded", []string{"a:b"}, "on-a-b"},
		{"leading junk trimmed", []string{"...x"}, "on-x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Name(tt.cmd); got != tt.want {
				t.Errorf("Name(%v) = %q, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestNameNeverContainsTmuxSeparators(t *testing.T) {
	for _, in := range []string{"a.b", "a:b", "a b", "a/b/c", "!!!", "", "ünïcode"} {
		got := Name([]string{in})
		if strings.ContainsAny(got, ".:") {
			t.Errorf("Name(%q) = %q contains a tmux separator", in, got)
		}
		if got == Prefix {
			t.Errorf("Name(%q) produced a bare prefix with no body", in)
		}
	}
}

func TestCreateOrAttachIsIdempotent(t *testing.T) {
	got := CreateOrAttachScript("on-claude", "/w", []string{"claude"})
	// Without has-session, a second run would fail: tmux errors when creating a
	// session that already exists.
	if !strings.Contains(got, "has-session -t on-claude 2>/dev/null ||") {
		t.Errorf("missing idempotency guard: %q", got)
	}
	if !strings.Contains(got, "exec tmux attach-session -t on-claude") {
		t.Errorf("should exec the attach: %q", got)
	}
}

func TestCreateScriptQuotesDirAndCommand(t *testing.T) {
	got := CreateScript("on-claude", "/home/user/my projects", []string{"claude", "--flag", "a b"})
	if !strings.Contains(got, `-c '/home/user/my projects'`) {
		t.Errorf("directory not quoted: %q", got)
	}
	// The command reaches tmux as a single argument that runs a login shell, so
	// it survives two levels of quoting. Assert structure here; the exact
	// escaping is covered by the round-trip test below.
	if !strings.Contains(got, "-lc") {
		t.Errorf("command should run under a login shell: %q", got)
	}
	if !strings.Contains(got, "claude --flag") {
		t.Errorf("command line missing: %q", got)
	}
}

func TestCreateScriptOmitsEmptyDirAndCommand(t *testing.T) {
	got := CreateScript("on-shell", "", nil)
	if strings.Contains(got, " -c ") {
		t.Errorf("empty dir should be omitted: %q", got)
	}
	if strings.Contains(got, "-lc") {
		t.Errorf("empty command should be omitted: %q", got)
	}
}

func TestLoginShellWrap(t *testing.T) {
	// tmux starts commands in a non-login, non-interactive shell, so ~/.profile
	// never runs and PATH is whatever sshd provided. Tools under ~/.local/bin or
	// managed by a version manager are then "not found", which reads as a broken
	// tool rather than a missing environment.
	got := LoginShellWrap([]string{"claude", "--resume"})
	if !strings.HasPrefix(got, "${SHELL:-/bin/sh} -lc ") {
		t.Errorf("should invoke a login shell: %q", got)
	}
	if !strings.Contains(got, "'claude --resume'") {
		t.Errorf("command should be one quoted argument: %q", got)
	}
}

func TestCreateScriptDefersShellExpansionToTheRemote(t *testing.T) {
	got := CreateScript("on-claude", "~/projects", []string{"claude"})
	// ${SHELL} must reach the remote unexpanded; resolving it locally would pick
	// the wrong host's login shell.
	if !strings.Contains(got, "${SHELL:-/bin/sh}") {
		t.Errorf("SHELL must be resolved remotely: %q", got)
	}
}

func TestCreateScriptKeepsFailedPaneVisible(t *testing.T) {
	got := CreateScript("on-claude", "", []string{"claude"})
	// A command that fails instantly would otherwise take the session with it,
	// leaving a vanished session and no error to read.
	if !strings.Contains(got, "remain-on-exit failed") {
		t.Errorf("a failed command should leave its error readable: %q", got)
	}
	// Older tmux rejects the "failed" value; that must not fail the whole run.
	if !strings.Contains(got, "|| true") {
		t.Errorf("remain-on-exit should tolerate older tmux: %q", got)
	}
}

// The layered quoting is only correct if the command survives every hop, so
// replay the path a real invocation takes: the ssh login shell strips one layer,
// then tmux runs the result through sh -c.
func TestCommandSurvivesQuotingLayersInRealShells(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh available")
	}
	tmuxArg := remote.Quote(LoginShellWrap([]string{"printf", "%s|%s", "a b", "it's"}))

	// Layer 1: the shell running our script hands tmux exactly one argument.
	out, err := exec.Command("sh", "-c", "printf %s "+tmuxArg).Output()
	if err != nil {
		t.Fatalf("layer 1 rejected the argument: %v", err)
	}

	// Layer 2: tmux runs that string via sh -c. SHELL is pinned so the test does
	// not depend on the developer's own login shell.
	out, err = exec.Command("sh", "-c", "SHELL=/bin/sh; "+string(out)).Output()
	if err != nil {
		t.Fatalf("layer 2 rejected the command: %v", err)
	}
	if got, want := string(out), "a b|it's"; got != want {
		t.Errorf("arguments did not survive quoting: got %q, want %q", got, want)
	}
}

func TestListScriptToleratesNoTmuxServer(t *testing.T) {
	// A host with no tmux server running is normal, not an error; without the
	// fallback, `on ps` would report a failure for every idle host.
	if !strings.Contains(ListScript(), "|| true") {
		t.Errorf("list should tolerate a missing tmux server: %q", ListScript())
	}
}

func TestParseList(t *testing.T) {
	out := "on-claude\t2\t1\t1735689600\nother\t1\t0\t1735689000\n"
	got := ParseList(out)
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(got))
	}
	if got[0].Name != "on-claude" || !got[0].Attached || got[0].Windows != "2" {
		t.Errorf("first row parsed wrong: %+v", got[0])
	}
	if !got[0].IsOn() {
		t.Error("on-claude should be recognised as an on session")
	}
	if got[1].IsOn() {
		t.Error("other should not be recognised as an on session")
	}
	if got[1].Attached {
		t.Error("second session should be detached")
	}
}

func TestParseListHandlesEmptyAndMalformed(t *testing.T) {
	if got := ParseList(""); len(got) != 0 {
		t.Errorf("empty input should yield no sessions, got %v", got)
	}
	if got := ParseList("garbage\nalso garbage\n"); len(got) != 0 {
		t.Errorf("malformed rows should be skipped, got %v", got)
	}
}

func TestCreateScriptExpandsTildeWorkdir(t *testing.T) {
	got := CreateScript("on-claude", "~/projects", []string{"claude"})
	// A quoted tilde would make tmux look for a literal "~/projects" directory.
	if strings.Contains(got, `-c '~/projects'`) {
		t.Errorf("tilde must not be quoted literally: %q", got)
	}
	if !strings.Contains(got, `-c "$HOME"/projects`) {
		t.Errorf("tilde should expand on the remote host: %q", got)
	}
}

func TestCreateScriptFailsFast(t *testing.T) {
	// Without set -e a failed new-session fell through to the attach, which
	// reported "can't find session" and hid the real cause.
	if !strings.HasPrefix(CreateScript("n", "/w", []string{"x"}), "set -e\n") {
		t.Error("create script should abort on failure")
	}
}

func TestStatusScriptUsesInventoryNameNotTmuxHostname(t *testing.T) {
	got := StatusScript("on-claude", "ol-agents")
	// #H would report the machine's hostname, which is identical for two users
	// on one box — exactly the distinction the inventory name preserves.
	if strings.Contains(got, "#H") {
		t.Errorf("should not use tmux's hostname: %q", got)
	}
	if !strings.Contains(got, "[ol-agents]") {
		t.Errorf("status should name the inventory host: %q", got)
	}
	if !strings.Contains(got, "|| true") {
		t.Errorf("a status bar is not worth failing a launch over: %q", got)
	}
}

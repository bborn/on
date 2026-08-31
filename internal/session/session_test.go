package session

import (
	"strings"
	"testing"
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
	got := CreateScript("on-claude", "/home/b/my projects", []string{"claude", "--flag", "a b"})
	if !strings.Contains(got, `-c '/home/b/my projects'`) {
		t.Errorf("directory not quoted: %q", got)
	}
	// The command reaches tmux as one argument, so it is quoted twice: once to
	// build the command line, once to keep it a single word for tmux.
	if !strings.Contains(got, `'claude --flag '\''a b'\'''`) {
		t.Errorf("command not double-quoted for tmux: %q", got)
	}
}

func TestCreateScriptOmitsEmptyDirAndCommand(t *testing.T) {
	got := CreateScript("on-shell", "", nil)
	if strings.Contains(got, " -c ") {
		t.Errorf("empty dir should be omitted: %q", got)
	}
	if !strings.HasSuffix(got, "-s on-shell") {
		t.Errorf("empty command should be omitted: %q", got)
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

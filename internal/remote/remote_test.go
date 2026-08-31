package remote

import (
	"os/exec"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain word needs no quoting", "claude", "claude"},
		{"empty string becomes empty word", "", "''"},
		{"space", "two words", "'two words'"},
		{"single quote", "it's", `'it'\''s'`},
		{"dollar is not expanded", "$HOME", "'$HOME'"},
		{"backtick is not executed", "`whoami`", "'`whoami`'"},
		{"semicolon cannot chain", "a; rm -rf /", "'a; rm -rf /'"},
		{"path with space", "/a b/c", "'/a b/c'"},
		{"tilde is quoted so it stays literal", "~/x", "'~/x'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Quote(tt.in); got != tt.want {
				t.Errorf("Quote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Quoting is only correct if the remote shell recovers the original bytes, so
// assert round-tripping through a real shell rather than trusting the encoding.
func TestQuoteRoundTripsThroughShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh available")
	}
	inputs := []string{
		"claude", "two words", "it's", "$HOME", "`whoami`",
		"a; rm -rf /", "/a b/c", `back\slash`, "new\nline",
		"quote\"inside", "star*glob", "tilde~x", "!bang", "a&b|c",
	}
	for _, in := range inputs {
		out, err := exec.Command("sh", "-c", "printf %s "+Quote(in)).Output()
		if err != nil {
			t.Fatalf("shell rejected Quote(%q): %v", in, err)
		}
		if string(out) != in {
			t.Errorf("round trip of %q produced %q", in, string(out))
		}
	}
}

func TestCommandBuildsExpectedArgv(t *testing.T) {
	got := Command("builder", Options{}, []string{"claude"})
	want := []string{"ssh", "-o", "ConnectTimeout=10", "builder", "--", "claude"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCommandTTYUsesForcedAllocation(t *testing.T) {
	got := Command("bigbox", Options{TTY: true}, []string{"tmux", "attach"})
	if !contains(got, "-tt") {
		t.Errorf("TTY option should force allocation with -tt, got %v", got)
	}
}

func TestCommandWithDirCdsAndExecs(t *testing.T) {
	got := Command("devbox", Options{Dir: "/home/user/projects/my app"}, []string{"rails", "test"})
	remote := got[len(got)-1]

	if !strings.HasPrefix(remote, "cd '/home/user/projects/my app' && exec ") {
		t.Errorf("expected quoted cd then exec, got %q", remote)
	}
	// exec matters: without it the wrapper shell owns the exit status and signals.
	if !strings.Contains(remote, "exec rails test") {
		t.Errorf("expected exec'd command, got %q", remote)
	}
}

func TestCommandDoesNotLetArgumentsEscape(t *testing.T) {
	got := Command("h", Options{}, []string{"echo", "hi; rm -rf /"})
	remote := got[len(got)-1]
	if strings.Contains(remote, "hi; rm") && !strings.Contains(remote, `'hi; rm -rf /'`) {
		t.Fatalf("argument escaped quoting: %q", remote)
	}
}

func TestIsConnectionFailure(t *testing.T) {
	tests := []struct {
		name   string
		code   int
		stderr string
		want   bool
	}{
		{"ssh could not connect", 255, "ssh: connect to host bigbox port 22: Connection refused", true},
		{"unresolvable host", 255, "ssh: Could not resolve hostname nope", true},
		{"remote command legitimately exited 255", 255, "my program failed\n", false},
		{"ordinary command failure", 1, "boom", false},
		{"success", 0, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsConnectionFailure(tt.code, tt.stderr); got != tt.want {
				t.Errorf("IsConnectionFailure(%d, %q) = %v, want %v",
					tt.code, tt.stderr, got, tt.want)
			}
		})
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestQuotePathExpandsLeadingTilde(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Regression: Quote() protects ~ like any other expansion character, so a
		// workdir of ~/projects reached tmux as a literal directory of that name,
		// the session was never created, and the attach reported the misleading
		// "can't find session".
		{"leading tilde expands", "~/projects", `"$HOME"/projects`},
		{"bare tilde", "~", `"$HOME"`},
		{"tilde path with a space", "~/my projects", `"$HOME"/'my projects'`},
		{"absolute path is unaffected", "/home/user/x", "/home/user/x"},
		{"absolute path with space still quoted", "/home/user/my x", "'/home/user/my x'"},
		{"tilde not at the start stays literal", "/a/~/b", "'/a/~/b'"},
		{"~user is not supported and stays literal", "~deploy/x", "'~deploy/x'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := QuotePath(tt.in); got != tt.want {
				t.Errorf("QuotePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestQuotePathResolvesToRealHomeInShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh available")
	}
	out, err := exec.Command("sh", "-c", "HOME=/tmp/somehome; printf %s "+QuotePath("~/projects")).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "/tmp/somehome/projects"; got != want {
		t.Errorf("expanded to %q, want %q", got, want)
	}
}

func TestCommandExpandsTildeInWorkingDirectory(t *testing.T) {
	// Latent sibling of the mirror and session bugs: Options.Dir is a path, so
	// it needs QuotePath rather than Quote.
	got := Command("h", Options{Dir: "~/projects/app"}, []string{"ls"})
	remoteCmd := got[len(got)-1]
	if strings.Contains(remoteCmd, "cd '~/projects") {
		t.Errorf("tilde must not be quoted literally: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, `cd "$HOME"/projects/app`) {
		t.Errorf("tilde should expand on the remote: %q", remoteCmd)
	}
}

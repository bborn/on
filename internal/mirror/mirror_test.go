package mirror

import (
	"strings"
	"testing"
)

func TestPathIsStableAndCollisionFree(t *testing.T) {
	a := Path("~/projects", "/Users/x/code/myapp")
	b := Path("~/projects", "/Users/x/code/myapp")
	if a != b {
		t.Errorf("same tree should map to the same mirror: %q vs %q", a, b)
	}

	// Two worktrees of one repo share a basename. Several agents working at once
	// is the normal case, so these must not land in the same directory.
	c := Path("~/projects", "/Users/x/worktrees/one/myapp")
	if a == c {
		t.Errorf("different trees collided on %q", a)
	}

	// The basename stays visible so the directory is recognisable over ssh.
	if !strings.Contains(a, "myapp-") {
		t.Errorf("mirror path should carry the basename: %q", a)
	}
	if !strings.Contains(a, MirrorRoot) {
		t.Errorf("mirror should live under %s: %q", MirrorRoot, a)
	}
}

func TestPathHandlesTrailingSlashInWorkdir(t *testing.T) {
	if got := Path("~/projects/", "/a/b"); strings.Contains(got, "//") {
		t.Errorf("doubled separator: %q", got)
	}
}

func TestDefaultExcludesCoverPlatformSpecificBuilds(t *testing.T) {
	// These hold binaries compiled for the local architecture. Copying a macOS
	// arm64 build to a Linux x86 host fails in ways that look like broken code
	// rather than a broken environment, which is the worst kind of failure.
	for _, must := range []string{"node_modules/", "vendor/bundle/", "target/", ".venv/"} {
		found := false
		for _, e := range DefaultExcludes {
			if e == must {
				found = true
			}
		}
		if !found {
			t.Errorf("%s must be excluded: it holds architecture-specific builds", must)
		}
	}
}

func TestRsyncArgs(t *testing.T) {
	got := strings.Join(RsyncArgs("builder", "/local/app", "~/w/.on-mirrors/app-abc", Options{Delete: true}), " ")

	if !strings.Contains(got, "--delete") {
		t.Error("delete should be requested when asked for")
	}
	// Without the trailing slash rsync would nest the tree inside itself.
	if !strings.Contains(got, "/local/app/ ") {
		t.Errorf("source needs a trailing slash: %q", got)
	}
	if !strings.Contains(got, "builder:~/w/.on-mirrors/app-abc/") {
		t.Errorf("destination wrong: %q", got)
	}
	// The mirror directory will not exist on first run.
	if !strings.Contains(got, "mkdir -p") {
		t.Errorf("should create the remote directory: %q", got)
	}
	if !strings.Contains(got, "--exclude node_modules/") {
		t.Errorf("default excludes missing: %q", got)
	}
}

func TestRsyncArgsOmitsDeleteByDefault(t *testing.T) {
	got := strings.Join(RsyncArgs("h", "/a", "/b", Options{}), " ")
	if strings.Contains(got, "--delete") {
		t.Errorf("delete should be opt-in: %q", got)
	}
}

func TestRsyncArgsExtraExcludesAddToDefaults(t *testing.T) {
	got := strings.Join(RsyncArgs("h", "/a", "/b", Options{Extra: []string{"secrets/"}}), " ")
	if !strings.Contains(got, "--exclude secrets/") {
		t.Errorf("extra exclude missing: %q", got)
	}
	if !strings.Contains(got, "--exclude node_modules/") {
		t.Errorf("extra excludes should add to defaults, not replace them: %q", got)
	}
}

func TestRsyncArgsExplicitExcludesReplaceDefaults(t *testing.T) {
	got := strings.Join(RsyncArgs("h", "/a", "/b", Options{Excludes: []string{"only/"}}), " ")
	if strings.Contains(got, "node_modules/") {
		t.Errorf("explicit excludes should replace defaults: %q", got)
	}
	if !strings.Contains(got, "--exclude only/") {
		t.Errorf("explicit exclude missing: %q", got)
	}
}

func TestRunScript(t *testing.T) {
	got := RunScript("~/w/mirror", "bundle install --quiet", []string{"bin/rails", "test"})

	if !strings.HasPrefix(got, "cd ") {
		t.Errorf("should cd into the mirror first: %q", got)
	}
	// A failed setup must report itself rather than surfacing later as a
	// confusing error from the command.
	if !strings.Contains(got, "bundle install --quiet || exit $?") {
		t.Errorf("setup failure should abort: %q", got)
	}
	if !strings.Contains(got, "exec bin/rails test") {
		t.Errorf("command should be exec'd so it owns the exit status: %q", got)
	}
}

func TestRunScriptWithoutSetup(t *testing.T) {
	got := RunScript("/m", "", []string{"ls"})
	if strings.Contains(got, "|| exit $?") {
		t.Errorf("no setup step should be emitted: %q", got)
	}
}

func TestRunScriptQuotesArguments(t *testing.T) {
	got := RunScript("/m", "", []string{"ruby", "-e", "puts 'hi there'"})
	if !strings.Contains(got, `'puts '\''hi there'\'''`) {
		t.Errorf("arguments must survive the remote shell: %q", got)
	}
}

func TestRsyncArgsExpandsTildeInRemoteMkdir(t *testing.T) {
	// Regression: quoting the tilde made mkdir create a literal "~" directory
	// while rsync wrote to the real home, so the transfer failed with a
	// confusing "No such file or directory" from a mkdir -p that had "worked".
	got := strings.Join(RsyncArgs("h", "/a", "~/w/.on-mirrors/app-abc", Options{}), " ")
	if strings.Contains(got, `mkdir -p '~/w`) {
		t.Errorf("tilde must not be quoted literally in the remote mkdir: %q", got)
	}
	if !strings.Contains(got, `mkdir -p "$HOME"/w/.on-mirrors/app-abc`) {
		t.Errorf("tilde should expand on the remote: %q", got)
	}
}

func TestRsyncArgsHonoursGitignoreAndSkipsGitByDefault(t *testing.T) {
	// A fixed exclude list cannot keep up with a working tree: the first real
	// run synced 623MB of compiled binaries and history. The tree already
	// declares what is derived, so reuse it.
	got := strings.Join(RsyncArgs("h", "/a", "/b", Options{}), " ")
	if !strings.Contains(got, "--filter :- .gitignore") {
		t.Errorf("should honour .gitignore: %q", got)
	}
	if !strings.Contains(got, "--exclude .git/") {
		t.Errorf(".git is usually the largest thing and rarely needed: %q", got)
	}
}

func TestRsyncArgsCanIncludeGitAndSkipGitignore(t *testing.T) {
	got := strings.Join(RsyncArgs("h", "/a", "/b", Options{IncludeGit: true, NoGitignore: true}), " ")
	if strings.Contains(got, "--exclude .git/") {
		t.Errorf("IncludeGit should keep .git: %q", got)
	}
	if strings.Contains(got, ".gitignore") {
		t.Errorf("NoGitignore should drop the filter: %q", got)
	}
}

func TestSupportsProgress2(t *testing.T) {
	tests := []struct {
		name   string
		banner string
		want   bool
	}{
		// macOS ships this; it rejects --info=progress2 and fails the transfer.
		{"openrsync", "openrsync: protocol version 29", false},
		{"gnu 3.2", "rsync  version 3.2.7  protocol version 31", true},
		{"gnu 3.1", "rsync  version 3.1.0  protocol version 31", true},
		{"gnu 3.0 predates the flag", "rsync  version 3.0.9  protocol version 30", false},
		{"gnu 2.6.9", "rsync  version 2.6.9  protocol version 29", false},
		{"unparseable", "something else entirely", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SupportsProgress2(tt.banner); got != tt.want {
				t.Errorf("SupportsProgress2(%q) = %v, want %v", tt.banner, got, tt.want)
			}
		})
	}
}

func TestRunScriptExpandsTildeInCd(t *testing.T) {
	// Third instance of this bug in one codebase: any path crossing to the
	// remote must use QuotePath, since Quote protects ~ from expansion and the
	// literal directory does not exist.
	got := RunScript("~/w/.on-mirrors/app-abc", "", []string{"ls"})
	if strings.Contains(got, "cd '~/w") {
		t.Errorf("tilde must not be quoted literally: %q", got)
	}
	if !strings.Contains(got, `cd "$HOME"/w/.on-mirrors/app-abc`) {
		t.Errorf("tilde should expand on the remote: %q", got)
	}
}

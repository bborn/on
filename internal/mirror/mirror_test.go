package mirror

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	got := RunScript(Run{Path: "~/w/mirror", Setup: "bundle install --quiet",
		Cmd: []string{"bin/rails", "test"}})

	if !strings.Contains(got, "cd ") {
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
	got := RunScript(Run{Path: "/m", Cmd: []string{"ls"}})
	if strings.Contains(got, "|| exit $?") {
		t.Errorf("no setup step should be emitted: %q", got)
	}
}

// The command now crosses two shells — ssh's, then the login shell — so assert
// the arguments actually survive rather than restating the escaping.
func TestRunScriptArgumentsSurviveBothShells(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh available")
	}
	script := RunScript(Run{Path: ".", Cmd: []string{"printf", "%s|%s", "a b", "it's"}})

	out, err := exec.Command("sh", "-c", "SHELL=/bin/sh; "+script).Output()
	if err != nil {
		t.Fatalf("script rejected by the shell: %v", err)
	}
	if got, want := string(out), "a b|it's"; got != want {
		t.Errorf("arguments did not survive: got %q, want %q", got, want)
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
	got := RunScript(Run{Path: "~/w/.on-mirrors/app-abc", Cmd: []string{"ls"}})
	if strings.Contains(got, "cd '~/w") {
		t.Errorf("tilde must not be quoted literally: %q", got)
	}
	if !strings.Contains(got, `cd "$HOME"/w/.on-mirrors/app-abc`) {
		t.Errorf("tilde should expand on the remote: %q", got)
	}
}

func TestRunScriptUsesLoginShell(t *testing.T) {
	// ssh without a tty starts a non-login shell, so ~/.profile never runs and
	// version-managed tools are not on PATH. Setup fails first, with
	// "bundle: not found", which looks like a broken host rather than a missing
	// environment.
	got := RunScript(Run{Path: "/m", Setup: "bundle install",
		Cmd: []string{"bin/rails", "test"}})
	if !strings.HasPrefix(got, "${SHELL:-/bin/sh} -lc ") {
		t.Errorf("should run through a login shell: %q", got)
	}
	// The cd, setup and command must all share that shell's environment.
	for _, part := range []string{"cd /m", "bundle install", "exec bin/rails test"} {
		if !strings.Contains(got, part) {
			t.Errorf("missing %q in %q", part, got)
		}
	}
}

// The prepare step exists because its absence is silent. Assets that were never
// precompiled make every test rendering a layout raise an asset-pipeline error,
// which Minitest counts as an error rather than a failure — so a run missing 29
// of 115 tests still prints "0 failures". Measured on one real controller test:
// 266 assertions without precompiled assets, 366 with.
func TestRunScriptPreparesOnlyWhenStale(t *testing.T) {
	got := RunScript(Run{
		Path:          "/m",
		Prepare:       "bin/rails assets:precompile",
		Stamp:         "~/w/.on-mirrors/.stamps/app-abc",
		PrepareInputs: []string{"app/assets", "package.json"},
		Cmd:           []string{"bin/rails", "test"},
	})

	// Guarded, not unconditional: precompiling is far too slow to pay per run,
	// which is exactly why callers stopped doing it by hand.
	if !strings.Contains(got, "if [ ! -e ") {
		t.Errorf("prepare should be guarded by the stamp: %q", got)
	}
	if !strings.Contains(got, "find app/assets package.json -newer") {
		t.Errorf("declared inputs should decide staleness: %q", got)
	}
	// A failed precompile must stop the run. Letting it through would produce
	// precisely the false green the step exists to remove.
	if !strings.Contains(got, "bin/rails assets:precompile || exit $?") {
		t.Errorf("a failed prepare must abort: %q", got)
	}
	if !strings.Contains(got, `: > "$HOME"/w/.on-mirrors/.stamps/app-abc`) {
		t.Errorf("stamp should be written after a successful prepare: %q", got)
	}
}

func TestRunScriptPrepareWithoutInputsRunsOncePerMirror(t *testing.T) {
	got := RunScript(Run{Path: "/m", Prepare: "bootstrap", Stamp: "/s", Cmd: []string{"ls"}})
	if strings.Contains(got, "find ") {
		t.Errorf("no inputs means no staleness check: %q", got)
	}
	if !strings.Contains(got, "if [ ! -e /s ]; then") {
		t.Errorf("should still run once when the stamp is missing: %q", got)
	}
}

func TestRunScriptSkipsPrepareBlockEntirelyWhenUnconfigured(t *testing.T) {
	got := RunScript(Run{Path: "/m", Cmd: []string{"ls"}})
	if strings.Contains(got, "preparing mirror") {
		t.Errorf("no prepare configured should emit nothing: %q", got)
	}
}

// Two concurrent runs against one host share a test database however few
// workers each uses. Observed as walls of PG::TRDeadlockDetected — 38, 71, 119
// and 203 errors — with zero assertion failures in any of them, which reads as a
// regression that does not exist.
func TestRunScriptSerialisesUnderALock(t *testing.T) {
	got := RunScript(Run{
		Path: "/m",
		Lock: "~/w/.on-mirrors/.locks/myapp.lock",
		Cmd:  []string{"bin/rails", "test"},
	})

	if !strings.Contains(got, "command -v flock") {
		t.Errorf("should probe for flock before relying on it: %q", got)
	}
	// The descriptor survives the final exec, so the lock is held for as long as
	// the command runs and released by the kernel however it dies.
	if !strings.Contains(got, `exec 9>"$HOME"/w/.on-mirrors/.locks/myapp.lock`) {
		t.Errorf("lock should be held on a descriptor: %q", got)
	}
	if !strings.Contains(got, "flock -n 9 ||") || !strings.Contains(got, "flock 9 ||") {
		t.Errorf("should say it is waiting, then block: %q", got)
	}
	// A host without flock must not silently drop the guarantee.
	if !strings.Contains(got, "runs unserialised") {
		t.Errorf("missing flock should be announced: %q", got)
	}
}

func TestRunScriptOrdersSetupBeforeLockBeforeCommand(t *testing.T) {
	got := RunScript(Run{
		Path: "/m", Setup: "bundle install", Lock: "/l",
		Prepare: "precompile", Stamp: "/s", Cmd: []string{"go", "test"},
	})
	// Setup touches only this mirror, so it need not queue. Prepare and the
	// command are what contend for a shared database.
	setup, lock := strings.Index(got, "bundle install"), strings.Index(got, "flock")
	prep, cmd := strings.Index(got, "precompile"), strings.Index(got, "exec go test")
	if !(setup < lock && lock < prep && prep < cmd) {
		t.Errorf("wrong order (setup %d, lock %d, prepare %d, cmd %d): %q",
			setup, lock, prep, cmd, got)
	}
}

func TestRunScriptExportsEnvBeforeEverything(t *testing.T) {
	got := RunScript(Run{
		Path:  "/m",
		Env:   map[string]string{"PARALLEL_WORKERS": "1", "RAILS_ENV": "test"},
		Setup: "bundle install",
		Cmd:   []string{"bin/rails", "test"},
	})
	// Sorted, so the script is byte-identical between runs and diffable.
	if !strings.Contains(got, "export PARALLEL_WORKERS=1\nexport RAILS_ENV=test\n") {
		t.Errorf("env should be exported in sorted order: %q", got)
	}
	if strings.Index(got, "export PARALLEL_WORKERS") > strings.Index(got, "bundle install") {
		t.Errorf("setup should see the environment too: %q", got)
	}
}

// Values come from a config file, so they carry whatever the user wrote, and
// they cross two shells to get there. Assert against a real one rather than
// restating the escaping.
func TestRunScriptQuotesEnvValues(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh available")
	}
	value := "a b; echo pwned"
	script := RunScript(Run{
		Path: ".",
		Env:  map[string]string{"OPTS": value},
		Cmd:  []string{"sh", "-c", `printf %s "$OPTS"`},
	})

	out, err := exec.Command("sh", "-c", "SHELL=/bin/sh; "+script).Output()
	if err != nil {
		t.Fatalf("script rejected by the shell: %v", err)
	}
	if got := string(out); got != value {
		t.Errorf("value did not survive: got %q, want %q", got, value)
	}
}

// The stamp cannot live inside the mirror: --delete would remove it on every
// sync, and a once-per-mirror step would silently become a per-run one.
func TestStampAndLockLiveOutsideTheMirror(t *testing.T) {
	m := Path("~/w", "/a/app")
	stamp := StampPath("~/w", "/a/app")
	if strings.HasPrefix(stamp, m+"/") {
		t.Errorf("stamp %q is inside the synced tree %q", stamp, m)
	}
	if !strings.Contains(stamp, StampRoot) {
		t.Errorf("stamp should live under %s: %q", StampRoot, stamp)
	}
	// One stamp per mirror, since staleness is a property of the synced tree.
	if StampPath("~/w", "/a/app") == StampPath("~/w", "/b/app") {
		t.Error("different trees should not share a stamp")
	}
}

// Lock names are config values, and a lock file is a path.
func TestLockPathCannotEscapeItsDirectory(t *testing.T) {
	got := LockPath("~/w", "../../etc/passwd")
	if !strings.HasPrefix(got, "~/w/"+LockRoot+"/") {
		t.Fatalf("lock should stay under %s: %q", LockRoot, got)
	}
	if segment := strings.TrimPrefix(got, "~/w/"+LockRoot+"/"); strings.Contains(segment, "/") {
		t.Errorf("name should be reduced to one path segment: %q", segment)
	}
}

// The prepare guard is shell, and the interesting failures are shell failures:
// a stamp compared wrongly runs an expensive step every time, or — far worse —
// never runs it again. Exercise it against a real shell and a real clock.
func TestPrepareRunsOnceThenAgainWhenInputsChange(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh available")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "assets", "app.css")
	if err := os.WriteFile(source, []byte("a{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	counter := filepath.Join(dir, "prepared")
	script := RunScript(Run{
		Path:          dir,
		Prepare:       "printf x >> " + counter,
		Stamp:         filepath.Join(dir, "stamp"),
		PrepareInputs: []string{"assets"},
		Cmd:           []string{"true"},
	})

	times := func() int {
		t.Helper()
		out, err := exec.Command("sh", "-c", "SHELL=/bin/sh; "+script).CombinedOutput()
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		b, err := os.ReadFile(counter)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}

	if got := times(); got != 1 {
		t.Fatalf("first run should prepare: prepared %d times", got)
	}
	// The whole point: precompiling costs minutes, so an unchanged tree must not
	// pay it again.
	if got := times(); got != 1 {
		t.Errorf("unchanged tree should not re-prepare: prepared %d times", got)
	}

	// rsync -a preserves modification times, so an edited file arrives newer
	// than the stamp of the last prepare. Skipping here is the dangerous
	// direction: stale assets are silent, and silence reads as a pass.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(source, later, later); err != nil {
		t.Fatal(err)
	}
	if got := times(); got != 2 {
		t.Errorf("touched input should re-prepare: prepared %d times", got)
	}
}

// A prepare that fails must stop the run. Continuing is how the false green
// appears: the command runs, the missing artefacts raise errors rather than
// failures, and the summary line still says "0 failures".
func TestFailedPrepareAbortsBeforeTheCommand(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh available")
	}
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	script := RunScript(Run{
		Path:    dir,
		Prepare: "exit 3",
		Stamp:   filepath.Join(dir, "stamp"),
		Cmd:     []string{"touch", ran},
	})

	err := exec.Command("sh", "-c", "SHELL=/bin/sh; "+script).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Errorf("prepare's exit status should be the run's: %v", err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("command ran despite a failed prepare")
	}
	if _, err := os.Stat(filepath.Join(dir, "stamp")); err == nil {
		t.Error("a failed prepare must not stamp itself as done")
	}
}

// Two concurrent runs against one host share a test database, so they must not
// overlap. Assert that against real flock rather than against the string that
// asks for it.
func TestLockSerialisesConcurrentRuns(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock available (macOS does not ship one)")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "log")

	script := RunScript(Run{
		Path: dir,
		Lock: filepath.Join(dir, "locks", "shared.lock"),
		// Long enough that unserialised runs would certainly interleave.
		Cmd: []string{"sh", "-c", "printf in >> " + log + "; sleep 0.3; printf out >> " + log},
	})

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := exec.Command("sh", "-c", "SHELL=/bin/sh; "+script).CombinedOutput(); err != nil {
				t.Errorf("script failed: %v\n%s", err, out)
			}
		}()
	}
	wg.Wait()

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "inoutinoutinout"; got != want {
		t.Errorf("runs overlapped: got %q, want %q", got, want)
	}
}

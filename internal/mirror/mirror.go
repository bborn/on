// Package mirror copies a local working tree to a remote host so a command can
// run against it.
//
// This exists because the interesting case is a tree with uncommitted work: an
// agent has just edited files and wants the tests run somewhere with spare CPU.
// Committing first is not an option, and a network filesystem is far too slow
// for a test suite's access pattern.
//
// Sync happens at invocation rather than continuously. A test run is a discrete
// event, so a watcher daemon buys nothing over one rsync at the moment you ask.
package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bborn/on/internal/remote"
)

// DefaultExcludes are paths that must not cross machines even if git tracks
// them, or that git does not know about.
//
// The first group holds native extensions and compiled output built for the
// local architecture. A macOS arm64 gem or node module copied to a Linux x86
// host fails in ways that look like broken code rather than a broken
// environment, so the remote must build its own.
//
// This list is a safety net, not the main defence: GitignoreFilter does the real
// work. A hand-maintained list cannot keep up with what accumulates in a working
// tree — the first run of this synced 623MB of compiled binaries and history
// before the filter was added.
var DefaultExcludes = []string{
	"node_modules/",
	"vendor/bundle/",
	".bundle/",
	"target/",
	".venv/",
	"__pycache__/",

	"tmp/",
	"log/",
	"coverage/",
	".DS_Store",
}

// GitignoreFilter makes rsync honour .gitignore, including nested ones.
//
// The tree already declares what is derived rather than source; reusing that is
// both more accurate and less maintenance than a fixed list. It also keeps
// gitignored secrets — .env, master.key — on the machine that owns them, which
// matches the rule that this tool never moves credentials.
const GitignoreFilter = ":- .gitignore"

// MirrorRoot is where mirrors live under a host's workdir.
const MirrorRoot = ".on-mirrors"

// Path returns the remote mirror directory for a local tree.
//
// The name carries the local basename so it is recognisable when you ssh in, and
// a hash of the absolute path so two worktrees of the same repo — a common case
// when several agents run at once — never collide.
func Path(workdir, localPath string) string {
	sum := sha256.Sum256([]byte(localPath))
	name := filepath.Base(localPath) + "-" + hex.EncodeToString(sum[:])[:8]
	return strings.TrimRight(workdir, "/") + "/" + MirrorRoot + "/" + name
}

// Options control how the tree is synced.
type Options struct {
	// Excludes replaces DefaultExcludes when non-nil.
	Excludes []string

	// Extra adds to the excludes in force.
	Extra []string

	// Delete removes remote files absent locally, so a rename or deletion does
	// not leave a stale file behind that a test might still pick up.
	Delete bool

	// IncludeGit copies .git too. Off by default: it is usually the largest
	// thing in the tree and a test run rarely needs history.
	IncludeGit bool

	// NoGitignore disables .gitignore filtering, for a tree where derived files
	// genuinely need to cross.
	NoGitignore bool

	// Progress shows transfer progress, so a large first sync is not silent.
	// Ignored where the local rsync does not support it — see SupportsProgress2.
	Progress bool
}

// SupportsProgress2 reports whether an rsync --version banner belongs to a build
// with --info=progress2, i.e. GNU rsync 3.1 or newer.
//
// macOS no longer ships GNU rsync: /usr/bin/rsync is openrsync, which accepts
// --filter, --files-from and --progress but rejects --info=progress2 outright,
// failing the whole transfer. Rather than force a Homebrew dependency, detect it
// and go quiet — the caller already prints what it is syncing.
func SupportsProgress2(versionBanner string) bool {
	if strings.Contains(strings.ToLower(versionBanner), "openrsync") {
		return false
	}
	i := strings.Index(versionBanner, "version ")
	if i < 0 {
		return false
	}
	var major, minor int
	if _, err := fmt.Sscanf(versionBanner[i+len("version "):], "%d.%d", &major, &minor); err != nil {
		return false
	}
	return major > 3 || (major == 3 && minor >= 1)
}

// progress2Available caches the probe: rsync is invoked once per process at most.
var progress2Available = sync.OnceValue(func() bool {
	out, err := exec.Command("rsync", "--version").Output()
	if err != nil {
		return false
	}
	return SupportsProgress2(string(out))
})

func (o Options) excludes() []string {
	base := DefaultExcludes
	if o.Excludes != nil {
		base = o.Excludes
	}
	return append(append([]string{}, base...), o.Extra...)
}

// RsyncArgs builds the rsync invocation that pushes localPath to the host.
func RsyncArgs(target, localPath, remotePath string, o Options) []string {
	args := []string{
		"rsync",
		"-a", // preserve modes and times, so rebuilds are not triggered spuriously
		"-z", // compress: source is text, and the link may be slow
		"--partial",
	}
	if o.Delete {
		args = append(args, "--delete")
	}
	if o.Progress && progress2Available() {
		args = append(args, "--info=progress2")
	}
	if !o.NoGitignore {
		args = append(args, "--filter", GitignoreFilter)
	}
	if !o.IncludeGit {
		args = append(args, "--exclude", ".git/")
	}
	for _, e := range o.excludes() {
		args = append(args, "--exclude", e)
	}

	// The mirror directory may not exist yet. --rsync-path runs on the far side,
	// so creating it here avoids a second round trip.
	//
	// QuotePath, not Quote: rsync expands a leading ~ in the destination path
	// itself, so quoting the tilde here would have mkdir create a directory
	// literally named "~" while rsync wrote to the real home — and the mkdir
	// would appear to succeed.
	args = append(args, "--rsync-path",
		fmt.Sprintf("mkdir -p %s && rsync", remote.QuotePath(remotePath)))

	args = append(args, "-e", "ssh -o ConnectTimeout=10")

	// Trailing slashes matter: "src/" means the contents of src, not src itself.
	args = append(args,
		strings.TrimRight(localPath, "/")+"/",
		target+":"+remotePath+"/")
	return args
}

// RunScript is the remote script that runs cmd in the mirror, after an optional
// setup step.
//
// The whole thing runs through a login shell. ssh without a tty starts a
// non-login shell, so ~/.profile never runs and PATH is only what sshd
// provided — tools managed by a version manager, or installed under
// ~/.local/bin, are simply not found. Setup is the first thing that would fail,
// with "bundle: not found", which reads as a broken host rather than a missing
// environment.
//
// Setup runs before the command rather than being folded into it, so a failing
// `bundle install` reports itself instead of surfacing later as a confusing
// error from the test suite.
func RunScript(remotePath, setup string, cmd []string) string {
	var inner strings.Builder

	// QuotePath, not Quote: the mirror path carries the host's ~.
	fmt.Fprintf(&inner, "cd %s || exit 1\n", remote.QuotePath(remotePath))
	if setup != "" {
		fmt.Fprintf(&inner, "%s || exit $?\n", setup)
	}
	// exec so the command owns the exit status and signals directly.
	fmt.Fprintf(&inner, "exec %s", remote.QuoteAll(cmd))

	return "${SHELL:-/bin/sh} -lc " + remote.Quote(inner.String())
}

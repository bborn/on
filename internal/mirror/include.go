package mirror

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ValidInclude reports why an include path is unusable, or "" when it is fine.
// Includes are relative to the tree and may not climb out of it.
func ValidInclude(p string) string {
	switch {
	case p == "" || filepath.IsAbs(p):
		return "must be a relative path"
	case strings.HasPrefix(filepath.Clean(p), ".."):
		return "must stay inside the tree"
	}
	return ""
}

// MainCheckout returns the repository's main checkout for a tree that may be a
// linked worktree, or "" when dir is not in a git repository. For the main
// checkout itself it returns that checkout.
func MainCheckout(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

// StageIncludes copies the include files into a fresh directory, with the same
// relative layout, ready to be synced on top of the mirror.
//
// Includes exist for the gitignored files an app cannot run without (secrets,
// local config) — exactly the files the main sync leaves out. Each one is read
// from the tree first, following a symlink to what it points at, since a
// worktree typically links these to the main checkout's copy. When the tree
// has none, the main checkout's copy is used, which is where bin/worktree-setup
// style scripts take them from. A path found in neither is reported as missing
// rather than failing the run.
func StageIncludes(tree, mainCheckout string, includes []string) (dir string, staged, missing []string, err error) {
	dir, err = os.MkdirTemp("", "on-include-")
	if err != nil {
		return "", nil, nil, err
	}
	for _, rel := range includes {
		src := ""
		for _, root := range []string{tree, mainCheckout} {
			if root == "" {
				continue
			}
			candidate := filepath.Join(root, rel)
			if info, statErr := os.Stat(candidate); statErr == nil && info.Mode().IsRegular() {
				src = candidate
				break
			}
		}
		if src == "" {
			missing = append(missing, rel)
			continue
		}
		if err := copyFile(src, filepath.Join(dir, rel)); err != nil {
			os.RemoveAll(dir)
			return "", nil, nil, fmt.Errorf("staging %s: %w", rel, err)
		}
		staged = append(staged, rel)
	}
	return dir, staged, missing, nil
}

func copyFile(src, dst string) error {
	// Directories stay 0755: they are the app's own (config/), not secrets.
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src) // follows symlinks
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// IncludeRsyncArgs syncs a staged include directory on top of the mirror. It
// never deletes. -p carries the staged 0600 over, so the files land private:
// they are usually secrets. (Not --chmod: macOS ships rsync 2.6.9, which
// rejects the D/F form.)
func IncludeRsyncArgs(target, stagedDir, remotePath string) []string {
	return []string{
		"rsync", "-rtpz",
		strings.TrimRight(stagedDir, "/") + "/",
		target + ":" + remotePath + "/",
	}
}

package mirror

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStageIncludes(t *testing.T) {
	main, tree := t.TempDir(), t.TempDir()
	write(t, filepath.Join(main, "config/application.yml"), "main-app")
	write(t, filepath.Join(main, ".env"), "main-env")
	write(t, filepath.Join(main, "config/master.key"), "main-key")
	write(t, filepath.Join(tree, ".env"), "tree-env")
	// A worktree links its secrets to the main checkout's copy.
	if err := os.MkdirAll(filepath.Join(tree, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(main, "config/master.key"), filepath.Join(tree, "config/master.key")); err != nil {
		t.Fatal(err)
	}

	dir, staged, missing, err := StageIncludes(tree, main,
		[]string{"config/application.yml", ".env", "config/master.key", "config/absent.yml"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	if got := read(t, filepath.Join(dir, "config/application.yml")); got != "main-app" {
		t.Errorf("absent from the tree should fall back to the main checkout, got %q", got)
	}
	if got := read(t, filepath.Join(dir, ".env")); got != "tree-env" {
		t.Errorf("the tree's own copy should win, got %q", got)
	}
	info, err := os.Lstat(filepath.Join(dir, "config/master.key"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || read(t, filepath.Join(dir, "config/master.key")) != "main-key" {
		t.Errorf("a symlink should be staged as the file it points to")
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("staged files should be 0600, got %v", info.Mode().Perm())
	}
	if len(staged) != 3 || len(missing) != 1 || missing[0] != "config/absent.yml" {
		t.Errorf("staged=%v missing=%v", staged, missing)
	}
}

func TestValidInclude(t *testing.T) {
	for p, ok := range map[string]bool{
		"config/application.yml": true, ".env": true,
		"": false, "/etc/passwd": false, "../secrets": false, "config/../../x": false,
	} {
		if got := ValidInclude(p) == ""; got != ok {
			t.Errorf("ValidInclude(%q) ok=%v, want %v", p, got, ok)
		}
	}
}

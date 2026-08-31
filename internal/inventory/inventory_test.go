package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	inv, err := Load(write(t, `
hosts:
  ol-agents:
    ssh: ol-agents
    workdir: ~/worktrees
    capabilities: [agent, ruby]
    projects: [offerlab]
  ik-agents:
    ssh: ik-agents
    capabilities: [agent, postgres]
`))
	if err != nil {
		t.Fatal(err)
	}

	h, err := inv.Lookup("ol-agents")
	if err != nil {
		t.Fatal(err)
	}
	if h.SSH != "ol-agents" || h.Name != "ol-agents" {
		t.Errorf("unexpected host: %+v", h)
	}
	if !h.Has("ruby") || h.Has("postgres") {
		t.Errorf("capability lookup wrong: %+v", h.Capabilities)
	}
	if !h.Serves("offerlab") || h.Serves("influencekit") {
		t.Errorf("project lookup wrong: %+v", h.Projects)
	}

	// A host that omits workdir still needs one to root sessions in.
	ik, _ := inv.Lookup("ik-agents")
	if ik.Workdir != DefaultWorkdir {
		t.Errorf("workdir should default to %q, got %q", DefaultWorkdir, ik.Workdir)
	}
}

func TestLookupUnknownHostNamesTheAlternatives(t *testing.T) {
	inv, err := Load(write(t, "hosts:\n  rex:\n    ssh: rex\n  mona:\n    ssh: mona\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = inv.Lookup("rexx")
	if err == nil {
		t.Fatal("expected an error for an unknown host")
	}
	// A mistyped host is the most common failure; the error should be actionable
	// rather than sending the user to read the config.
	if !strings.Contains(err.Error(), "mona") || !strings.Contains(err.Error(), "rex") {
		t.Errorf("error should list available hosts, got: %v", err)
	}
}

func TestLoadRejectsHostWithoutSSHAlias(t *testing.T) {
	_, err := Load(write(t, "hosts:\n  broken:\n    workdir: ~/w\n"))
	if err == nil || !strings.Contains(err.Error(), "ssh") {
		t.Errorf("expected a missing-ssh error, got: %v", err)
	}
}

func TestLoadRejectsEmptyInventory(t *testing.T) {
	if _, err := Load(write(t, "hosts:\n")); err == nil {
		t.Error("expected an error for an inventory with no hosts")
	}
}

func TestLoadMissingFileSuggestsInit(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil || !strings.Contains(err.Error(), "on init") {
		t.Errorf("missing inventory should point at `on init`, got: %v", err)
	}
}

func TestNamesAreSorted(t *testing.T) {
	inv, err := Load(write(t, "hosts:\n  zeta:\n    ssh: z\n  alpha:\n    ssh: a\n  mid:\n    ssh: m\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := inv.Names()
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}

func TestDefaultPathHonoursEnvOverride(t *testing.T) {
	t.Setenv("ON_HOSTS", "/tmp/custom.yaml")
	if got := DefaultPath(); got != "/tmp/custom.yaml" {
		t.Errorf("DefaultPath() = %q, want the ON_HOSTS override", got)
	}
}

func TestTemplateIsValidAndEmpty(t *testing.T) {
	// The template must parse, but it declares no hosts, so Load reports the
	// empty-inventory error rather than a syntax error.
	_, err := Load(write(t, Template))
	if err == nil || !strings.Contains(err.Error(), "no hosts") {
		t.Errorf("template should parse but declare no hosts, got: %v", err)
	}
}

func TestDefaultPathUsesXDGNotMacOSApplicationSupport(t *testing.T) {
	t.Setenv("ON_HOSTS", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/someone")

	got := DefaultPath()
	if strings.Contains(got, "Application Support") {
		t.Errorf("DefaultPath() = %q; a terminal tool should not use macOS Application Support", got)
	}
	if want := "/home/someone/.config/on/hosts.yaml"; got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestDefaultPathHonoursXDGConfigHome(t *testing.T) {
	t.Setenv("ON_HOSTS", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got, want := DefaultPath(), "/xdg/on/hosts.yaml"; got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}

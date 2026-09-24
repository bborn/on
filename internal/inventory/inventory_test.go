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
  builder:
    ssh: builder
    workdir: ~/worktrees
    capabilities: [agent, ruby]
    repos:
      myapp: ~/projects/engineering
  testbox:
    ssh: testbox
    capabilities: [agent, postgres]
    repos:
      otherapp: ~/projects/otherapp
`))
	if err != nil {
		t.Fatal(err)
	}

	h, err := inv.Lookup("builder")
	if err != nil {
		t.Fatal(err)
	}
	if h.SSH != "builder" || h.Name != "builder" {
		t.Errorf("unexpected host: %+v", h)
	}
	if !h.Has("ruby") || h.Has("postgres") {
		t.Errorf("capability lookup wrong: %+v", h.Capabilities)
	}
	if !h.Serves("myapp") || h.Serves("otherapp") {
		t.Errorf("project lookup wrong: %+v", h.Repos)
	}
	// The directory name is not the project name; the mapping must be explicit.
	if got := h.RepoPath("myapp"); got != "~/projects/engineering" {
		t.Errorf("RepoPath = %q, want the configured path", got)
	}

	// A host that omits workdir still needs one to root sessions in.
	ik, _ := inv.Lookup("testbox")
	if ik.Workdir != DefaultWorkdir {
		t.Errorf("workdir should default to %q, got %q", DefaultWorkdir, ik.Workdir)
	}
}

func TestLookupUnknownHostNamesTheAlternatives(t *testing.T) {
	inv, err := Load(write(t, "hosts:\n  bigbox:\n    ssh: bigbox\n  devbox:\n    ssh: devbox\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = inv.Lookup("rexx")
	if err == nil {
		t.Fatal("expected an error for an unknown host")
	}
	// A mistyped host is the most common failure; the error should be actionable
	// rather than sending the user to read the config.
	if !strings.Contains(err.Error(), "devbox") || !strings.Contains(err.Error(), "bigbox") {
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

func TestHostsForAndCloneURL(t *testing.T) {
	inv, err := Load(write(t, `
repos:
  shared: git@example.com:me/shared.git
hosts:
  a:
    ssh: a
    repos:
      shared: ~/a/shared
  b:
    ssh: b
    repos:
      shared: ~/b/shared
      only-b: ~/b/only
`))
	if err != nil {
		t.Fatal(err)
	}

	if got := inv.HostsFor("shared"); len(got) != 2 {
		t.Errorf("both hosts serve shared, got %d", len(got))
	}
	if got := inv.HostsFor("only-b"); len(got) != 1 || got[0].Name != "b" {
		t.Errorf("only-b should resolve to host b, got %+v", got)
	}
	if got := inv.HostsFor("nope"); len(got) != 0 {
		t.Errorf("unknown project should match no hosts, got %+v", got)
	}
	if got := inv.CloneURL("shared"); got != "git@example.com:me/shared.git" {
		t.Errorf("CloneURL = %q", got)
	}
	if got := inv.CloneURL("only-b"); got != "" {
		t.Errorf("a project with no clone URL should return empty, got %q", got)
	}

	names := inv.ProjectNames()
	if len(names) != 2 || names[0] != "only-b" || names[1] != "shared" {
		t.Errorf("ProjectNames = %v, want sorted [only-b shared]", names)
	}
}

func TestLoadPools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	os.WriteFile(path, []byte(`
elastic:
  ol:
    provider: hetzner
    context: dev
    image: offerlab
    types: [cx53]
    locations: [fsn1]
    serves: [offerlab]
`), 0o600)
	inv, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := inv.PoolFor("offerlab")
	if !ok || p.Name != "ol" {
		t.Fatalf("PoolFor = %+v, %v", p, ok)
	}
	if p.User != DefaultPoolUser || p.IdleMinutes != DefaultPoolIdleMinutes || p.MaxServers != DefaultPoolMaxServers {
		t.Fatalf("defaults not applied: %+v", p)
	}
	if _, ok := inv.PoolFor("other"); ok {
		t.Fatal("pool must only serve its listed projects")
	}
}

func TestLoadPoolValidation(t *testing.T) {
	for name, body := range map[string]string{
		"unknown provider": "elastic:\n  p:\n    provider: aws\n    image: x\n    types: [a]\n    locations: [b]\n",
		"missing image":    "elastic:\n  p:\n    provider: hetzner\n    types: [a]\n    locations: [b]\n",
		"name clash":       "hosts:\n  p:\n    ssh: p\nelastic:\n  p:\n    provider: hetzner\n    image: x\n    types: [a]\n    locations: [b]\n",
	} {
		path := filepath.Join(t.TempDir(), "hosts.yaml")
		os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadPoolWithSeveralProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	os.WriteFile(path, []byte(`
elastic:
  cloud:
    image: offerlab
    min_cpus: 8
    min_memory_gb: 30
    rates: {USD: 0.86}
    serves: [offerlab]
    providers:
      hetzner: {context: dev, locations: [fsn1]}
      digitalocean: {token_file: ~/do.env, locations: [nyc3, sfo3], ssh_keys: [k]}
`), 0o600)
	inv, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := inv.Elastic["cloud"]
	if got := p.ProviderNames(); len(got) != 2 || got[0] != "digitalocean" || got[1] != "hetzner" {
		t.Fatalf("providers = %v", got)
	}
	if p.Currency != "EUR" {
		t.Fatalf("currency should default to EUR, got %q", p.Currency)
	}
	if r, ok := p.Rate("USD"); !ok || r != 0.86 {
		t.Fatalf("USD rate = %v, %v", r, ok)
	}
	if r, ok := p.Rate("EUR"); !ok || r != 1 {
		t.Fatalf("the pool's own currency converts at 1, got %v, %v", r, ok)
	}
}

func TestLoadPoolFoldsTheSingleProviderForm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	os.WriteFile(path, []byte("elastic:\n  p:\n    provider: hetzner\n    context: dev\n    image: x\n    types: [cx53]\n    locations: [fsn1]\n    ssh_keys: [k]\n"), 0o600)
	inv, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h, ok := inv.Elastic["p"].Providers["hetzner"]
	if !ok || h.Context != "dev" || h.Types[0] != "cx53" || h.Locations[0] != "fsn1" || h.SSHKeys[0] != "k" {
		t.Fatalf("single-provider fields not folded: %+v", inv.Elastic["p"].Providers)
	}
}

func TestLoadPoolNeedsARateForAForeignCurrency(t *testing.T) {
	for name, body := range map[string]string{
		"no rate for USD":    "elastic:\n  p:\n    image: x\n    min_memory_gb: 30\n    currency: EUR\n    providers:\n      digitalocean: {locations: [nyc3]}\n",
		"no locations":       "elastic:\n  p:\n    image: x\n    min_memory_gb: 30\n    providers:\n      hetzner: {context: c}\n",
		"no size or types":   "elastic:\n  p:\n    image: x\n    providers:\n      hetzner: {locations: [fsn1]}\n",
		"unknown provider":   "elastic:\n  p:\n    image: x\n    min_memory_gb: 30\n    providers:\n      aws: {locations: [us-east-1]}\n",
		"provider both ways": "elastic:\n  p:\n    provider: hetzner\n    image: x\n    types: [a]\n    locations: [b]\n    providers:\n      hetzner: {locations: [b]}\n",
	} {
		path := filepath.Join(t.TempDir(), "hosts.yaml")
		os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadPoolWithOneProviderUsesItsCurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	os.WriteFile(path, []byte("elastic:\n  p:\n    image: x\n    min_memory_gb: 30\n    providers:\n      digitalocean: {locations: [nyc3]}\n"), 0o600)
	inv, err := Load(path)
	if err != nil {
		t.Fatalf("a DigitalOcean-only pool should need no rate: %v", err)
	}
	if c := inv.Elastic["p"].Currency; c != "USD" {
		t.Fatalf("currency = %q, want USD", c)
	}
}

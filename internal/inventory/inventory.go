// Package inventory loads the host inventory that `on` resolves targets against.
//
// A host entry names an ssh_config alias rather than a hostname or IP. The alias
// already carries the user, identity file and any connection tuning, and it is the
// only representation that distinguishes two accounts on one machine — e.g. a box
// reachable as both `bigbox` (root) and `bigbox-dev` (an unprivileged account),
// which have different
// HOME, PATH, toolchains and credentials. It also keeps private addresses out of
// this file.
package inventory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Host is one target `on` can place work on.
type Host struct {
	// Name is the inventory key, filled in during load.
	Name string `yaml:"-"`

	// SSH is an ssh_config alias. Required.
	SSH string `yaml:"ssh"`

	// Workdir is where worktrees and sessions are rooted on the host.
	// Defaults to DefaultWorkdir when empty.
	Workdir string `yaml:"workdir"`

	// Capabilities are free-form tags a caller can require, e.g. "postgres".
	Capabilities []string `yaml:"capabilities"`

	// Repos maps a project name to its checkout path on this host.
	//
	// Directory names are not project names: a checkout of myapp may live at
	// ~/projects/engineering on one host and ~/Projects/myapp on another, so
	// the mapping has to be explicit rather than inferred from the path.
	Repos map[string]string `yaml:"repos"`
}

// DefaultWorkdir is used when a host does not set one.
const DefaultWorkdir = "~/worktrees"

// Inventory is the parsed host file.
type Inventory struct {
	Hosts map[string]Host `yaml:"hosts"`

	// Repos maps a project name to a clone URL, used when a host is asked for a
	// project it does not have yet.
	Repos map[string]string `yaml:"repos"`

	// Exec holds per-project settings for `on exec`.
	Exec map[string]ExecConfig `yaml:"exec"`
}

// ExecConfig is how a project prepares itself on a remote host.
type ExecConfig struct {
	// Setup runs in the mirror before the command. It must be cheap and
	// idempotent, since it runs on every invocation — `bundle install` is the
	// intended shape: near-instant once satisfied.
	//
	// It exists because dependencies are deliberately not synced: native
	// extensions built locally will not run on the remote's architecture, so the
	// remote has to build its own.
	Setup string `yaml:"setup"`

	// Exclude adds to the default rsync excludes for this project.
	Exclude []string `yaml:"exclude"`

	// Prepare runs in the mirror once, when it is first created, and again
	// whenever PrepareInputs change. It is for work that is too expensive to
	// repeat per run but whose absence is not a loud failure — the motivating
	// case is `bin/rails assets:precompile`, without which every test that
	// renders a layout raises an asset-pipeline error. Minitest counts those as
	// errors rather than failures, so a run missing a quarter of its tests
	// still prints "0 failures" and reads as a pass.
	//
	// Unlike Setup it may be slow, and unlike Setup its absence is silent, which
	// is exactly why it belongs in the tool rather than in each caller's command
	// line.
	Prepare string `yaml:"prepare"`

	// PrepareInputs are paths, relative to the mirror, whose modification times
	// decide whether Prepare is stale. Empty means Prepare runs once per mirror
	// and never again — right for a one-off bootstrap, wrong for anything
	// derived from files you are editing.
	PrepareInputs []string `yaml:"prepare_inputs"`

	// Env is exported before Setup, Prepare and the command.
	//
	// This exists so a project can state the environment its remote runs need
	// once, instead of every caller remembering to prefix it. `PARALLEL_WORKERS:
	// "1"` is the motivating case: above Rails' 50-test parallelisation
	// threshold, forked workers truncating tables against a live connection
	// deadlock in Postgres, producing a wall of errors and zero assertion
	// failures — a red suite with no regression behind it.
	Env map[string]string `yaml:"env"`

	// Lock serialises runs that share this name on a host. Empty means no lock.
	//
	// Mirrors are isolated from each other, but the things they talk to are not:
	// two worktrees of one project on one host share a test database, so two
	// concurrent runs corrupt each other's fixtures however few workers each
	// uses. The lock is per host and named rather than implicit, so cheap
	// commands are not made to queue behind test suites.
	Lock string `yaml:"lock"`
}

// ExecFor returns the whole exec configuration for a project. The zero value is
// meaningful — a project with no entry simply gets no setup, no prepare step and
// no lock — so this never reports absence.
func (inv *Inventory) ExecFor(project string) ExecConfig {
	return inv.Exec[project]
}

// Has reports whether the host declares the named capability.
func (h Host) Has(capability string) bool {
	for _, c := range h.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// Serves reports whether the host has a checkout of the named project.
func (h Host) Serves(project string) bool {
	_, ok := h.Repos[project]
	return ok
}

// RepoPath returns the project's checkout path on this host, or "" if absent.
func (h Host) RepoPath(project string) string { return h.Repos[project] }

// HostsFor returns hosts that have a checkout of the project, in stable order.
func (inv *Inventory) HostsFor(project string) []Host {
	var out []Host
	for _, n := range inv.Names() {
		if h := inv.Hosts[n]; h.Serves(project) {
			out = append(out, h)
		}
	}
	return out
}

// CloneURL returns the clone URL for a project, or "" if none is configured.
func (inv *Inventory) CloneURL(project string) string { return inv.Repos[project] }

// ProjectNames lists every project any host serves, for error messages and
// completion.
func (inv *Inventory) ProjectNames() []string {
	seen := map[string]bool{}
	for _, h := range inv.Hosts {
		for p := range h.Repos {
			seen[p] = true
		}
	}
	for p := range inv.Repos {
		seen[p] = true
	}
	names := make([]string, 0, len(seen))
	for p := range seen {
		names = append(names, p)
	}
	sort.Strings(names)
	return names
}

// DefaultPath is the inventory location, honouring ON_HOSTS for tests and
// alternate profiles.
//
// This deliberately uses ~/.config rather than os.UserConfigDir, which resolves
// to ~/Library/Application Support on macOS. `on` runs on both macOS and Linux
// and is a terminal tool, so the same XDG path on every platform is both more
// predictable and consistent with its neighbours.
func DefaultPath() string {
	if p := os.Getenv("ON_HOSTS"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.Getenv("HOME")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "on", "hosts.yaml")
}

// Load reads and validates the inventory at path.
func Load(path string) (*Inventory, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no inventory at %s — run `on init` to create one", path)
		}
		return nil, err
	}

	var inv Inventory
	if err := yaml.Unmarshal(raw, &inv); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(inv.Hosts) == 0 {
		return nil, fmt.Errorf("%s declares no hosts", path)
	}

	for name, h := range inv.Hosts {
		if h.SSH == "" {
			return nil, fmt.Errorf("host %q has no `ssh:` alias", name)
		}
		h.Name = name
		if h.Workdir == "" {
			h.Workdir = DefaultWorkdir
		}
		inv.Hosts[name] = h
	}
	return &inv, nil
}

// Names returns host names in stable order.
func (inv *Inventory) Names() []string {
	names := make([]string, 0, len(inv.Hosts))
	for n := range inv.Hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Lookup resolves a host by name. The error names the alternatives, since a
// mistyped host is the most common failure and a bare "not found" makes the user
// go read the config themselves.
func (inv *Inventory) Lookup(name string) (Host, error) {
	if h, ok := inv.Hosts[name]; ok {
		return h, nil
	}
	return Host{}, fmt.Errorf("unknown host %q — inventory has: %s",
		name, strings.Join(inv.Names(), ", "))
}

// Template is the starter inventory written by `on init`.
const Template = `# Hosts that ` + "`on`" + ` can place work on.
#
# ssh: names an ssh_config alias, NOT a hostname or IP. The alias carries the
# user, identity file and connection tuning — and it is what distinguishes two
# accounts on the same machine, which are different environments entirely.

hosts:
  # example:
  #   ssh: example              # must exist in ~/.ssh/config
  #   workdir: ~/worktrees      # where sessions and worktrees are rooted
  #   capabilities: [agent, ruby, node]
  #   repos:                    # project name -> checkout path on THIS host
  #     myapp: ~/projects/myapp

# Clone URLs, used when a host is asked for a project it does not have yet.
# repos:
#   myapp: git@github.com:me/myapp.git

# Per-project settings for ` + "`on exec`" + `.
# exec:
#   myapp:
#     setup: bundle install --quiet   # every run; must be cheap and idempotent
#     exclude: [storage/]             # extra rsync excludes
#
#     # Once per mirror, and again when prepare_inputs change. For work too slow
#     # to repeat per run whose absence is silent — un-precompiled assets make
#     # Rails tests raise errors rather than failures, so the run still prints
#     # "0 failures". Without prepare_inputs this runs once and never again.
#     prepare: bin/rails assets:precompile
#     prepare_inputs: [app/assets, app/javascript, package.json]
#
#     env:                            # exported before setup, prepare and cmd
#       PARALLEL_WORKERS: "1"
#
#     # Serialise runs sharing this name on a host. Mirrors are isolated; the
#     # test database they share is not.
#     lock: myapp
`

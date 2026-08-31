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

	// Projects names repositories this host is provisioned to serve.
	Projects []string `yaml:"projects"`
}

// DefaultWorkdir is used when a host does not set one.
const DefaultWorkdir = "~/worktrees"

// Inventory is the parsed host file.
type Inventory struct {
	Hosts map[string]Host `yaml:"hosts"`
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

// Serves reports whether the host is provisioned for the named project.
func (h Host) Serves(project string) bool {
	for _, p := range h.Projects {
		if p == project {
			return true
		}
	}
	return false
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
  #   projects: [myapp]
`

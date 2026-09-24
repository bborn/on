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

	"github.com/bborn/on/internal/mirror"
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

	// Elastic holds pools of on-demand hosts, keyed by pool name.
	Elastic map[string]Pool `yaml:"elastic"`
}

// Pool is a set of on-demand hosts `on` creates from a snapshot when the fixed
// hosts are short of memory, and deletes again once they sit idle.
//
// A pool can draw on several providers. It states the size it needs (MinCPUs,
// MinMemoryGB); `on` gathers every size and location each provider can boot the
// pool's image on, converts the prices into the pool's currency, and tries them
// cheapest first. Capacity is part of the answer: a sold-out type fails fast and
// the next cheapest is tried, so a pool need not be tuned to one provider's stock.
type Pool struct {
	// Name is the inventory key, filled in during load.
	Name string `yaml:"-"`

	// Image is the snapshot to boot, matched by its `on-image` label (tag, on
	// providers without labels); the newest wins. Each provider keeps its own.
	Image string `yaml:"image"`

	// MinCPUs and MinMemoryGB are the smallest server the pool will boot. Any
	// type at least this big qualifies, whatever it is called.
	MinCPUs     int     `yaml:"min_cpus"`
	MinMemoryGB float64 `yaml:"min_memory_gb"`

	// Providers maps a provider ("hetzner", "digitalocean") to its settings.
	Providers map[string]ProviderConfig `yaml:"providers"`

	// Currency is what DailyBudget and the spend ledger are counted in.
	// Defaults to EUR.
	Currency string `yaml:"currency"`

	// Rates converts a provider's prices into Currency: 1 unit of the key is
	// worth this much. Needed for any provider billing in another currency.
	Rates map[string]float64 `yaml:"rates"`

	// Provider, Context, Types, Locations and SSHKeys are the single-provider
	// form, from before pools had several; Load folds them into Providers.
	Provider  string   `yaml:"provider"`
	Context   string   `yaml:"context"`
	Types     []string `yaml:"types"`
	Locations []string `yaml:"locations"`
	SSHKeys   []string `yaml:"ssh_keys"`

	// User is the login baked into the image. Defaults to "dev".
	User string `yaml:"user"`

	// Workdir is where mirrors live on the server. Defaults to DefaultWorkdir.
	Workdir string `yaml:"workdir"`

	Capabilities []string `yaml:"capabilities"`

	// Serves lists the projects this pool can run through `on exec`.
	Serves []string `yaml:"serves"`

	// MinFreeMB is the memory a fixed host must have available for `on exec` to
	// stay on it. Below that on every fixed host, the run goes to this pool.
	MinFreeMB int `yaml:"min_free_mb"`

	// IdleMinutes is how long a server may go unused before `on reap` deletes it.
	IdleMinutes int `yaml:"idle_minutes"`

	// MaxHours deletes a server this old even if busy: a forgotten dev server
	// should not run all week.
	MaxHours int `yaml:"max_hours"`

	// MaxServers caps how many servers the pool runs at once, across providers.
	MaxServers int `yaml:"max_servers"`

	// DailyBudget caps what the pool may spend per UTC day, in Currency.
	// Reaching it stops new servers and deletes running ones.
	DailyBudget float64 `yaml:"daily_budget"`

	// Build is a script that provisions a fresh server into the pool's image,
	// given the server's IP as its argument; `on image build` runs it.
	Build string `yaml:"build"`
}

// ProviderConfig is one provider's part of a pool.
type ProviderConfig struct {
	// Context is the hcloud CLI context (hetzner).
	Context string `yaml:"context"`

	// TokenFile is a dotenv file holding DIGITALOCEAN_ACCESS_TOKEN
	// (digitalocean). Without it the variable is read from the environment.
	TokenFile string `yaml:"token_file"`

	// Locations are the locations (hetzner) or regions (digitalocean) servers
	// may be created in. Required.
	Locations []string `yaml:"locations"`

	// Types, when set, limits the pool to these server types on this provider.
	// Otherwise every type meeting the pool's minimum qualifies.
	Types []string `yaml:"types"`

	// SSHKeys are the provider's names for the keys installed on each server.
	SSHKeys []string `yaml:"ssh_keys"`
}

// ProviderCurrency is the currency each supported provider bills in.
var ProviderCurrency = map[string]string{
	"hetzner":      "EUR",
	"digitalocean": "USD",
}

// ProviderNames returns the pool's providers in stable order.
func (p Pool) ProviderNames() []string {
	names := make([]string, 0, len(p.Providers))
	for n := range p.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Rate is what one unit of the given currency is worth in the pool's currency,
// and whether the pool knows.
func (p Pool) Rate(currency string) (float64, bool) {
	if currency == p.Currency {
		return 1, true
	}
	r, ok := p.Rates[currency]
	return r, ok && r > 0
}

// Pool defaults, applied at load.
const (
	DefaultPoolUser        = "dev"
	DefaultPoolIdleMinutes = 20
	DefaultPoolMaxHours    = 12
	DefaultPoolMaxServers  = 2
	DefaultPoolMinFreeMB   = 6000
	DefaultPoolCurrency    = "EUR"
)

// ServesProject reports whether the pool can run the project.
func (p Pool) ServesProject(project string) bool {
	for _, s := range p.Serves {
		if s == project {
			return true
		}
	}
	return false
}

// PoolFor returns the first pool (in name order) that serves the project.
func (inv *Inventory) PoolFor(project string) (Pool, bool) {
	for _, n := range inv.PoolNames() {
		if p := inv.Elastic[n]; p.ServesProject(project) {
			return p, true
		}
	}
	return Pool{}, false
}

// PoolNames returns pool names in stable order.
func (inv *Inventory) PoolNames() []string {
	names := make([]string, 0, len(inv.Elastic))
	for n := range inv.Elastic {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
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

	// Include lists gitignored files to sync anyway, relative to the tree: the
	// secrets and local config an app will not boot without, such as
	// config/application.yml. Symlinks are followed, and a file the tree lacks is
	// taken from the repository's main checkout, the way worktree setup scripts
	// link them. They land 0600 and are left in place by later syncs.
	Include []string `yaml:"include"`

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
	if len(inv.Hosts) == 0 && len(inv.Elastic) == 0 {
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

	for project, e := range inv.Exec {
		for _, inc := range e.Include {
			if why := mirror.ValidInclude(inc); why != "" {
				return nil, fmt.Errorf("exec.%s.include: %q %s", project, inc, why)
			}
		}
	}

	for name, p := range inv.Elastic {
		if _, clash := inv.Hosts[name]; clash {
			return nil, fmt.Errorf("pool %q has the same name as a host", name)
		}
		if p.Provider != "" {
			if p.Providers == nil {
				p.Providers = map[string]ProviderConfig{}
			}
			if _, dup := p.Providers[p.Provider]; dup {
				return nil, fmt.Errorf("pool %q sets provider %q both ways; use providers:", name, p.Provider)
			}
			p.Providers[p.Provider] = ProviderConfig{Context: p.Context, Types: p.Types, Locations: p.Locations, SSHKeys: p.SSHKeys}
		}
		if p.Currency == "" {
			p.Currency = DefaultPoolCurrency
		}
		if p.Image == "" || len(p.Providers) == 0 {
			return nil, fmt.Errorf("pool %q needs an image and at least one provider", name)
		}
		for _, pn := range p.ProviderNames() {
			pc := p.Providers[pn]
			cur, known := ProviderCurrency[pn]
			if !known {
				return nil, fmt.Errorf("pool %q: unknown provider %q (supported: hetzner, digitalocean)", name, pn)
			}
			if len(pc.Locations) == 0 {
				return nil, fmt.Errorf("pool %q: provider %s needs locations", name, pn)
			}
			if len(pc.Types) == 0 && p.MinCPUs <= 0 && p.MinMemoryGB <= 0 {
				return nil, fmt.Errorf("pool %q: provider %s needs types, or the pool min_cpus/min_memory_gb", name, pn)
			}
			if _, ok := p.Rate(cur); !ok {
				return nil, fmt.Errorf("pool %q: provider %s bills in %s; add rates: {%s: <value in %s>}", name, pn, cur, cur, p.Currency)
			}
		}
		p.Name = name
		if p.User == "" {
			p.User = DefaultPoolUser
		}
		if p.Workdir == "" {
			p.Workdir = DefaultWorkdir
		}
		if p.IdleMinutes <= 0 {
			p.IdleMinutes = DefaultPoolIdleMinutes
		}
		if p.MaxHours <= 0 {
			p.MaxHours = DefaultPoolMaxHours
		}
		if p.MaxServers <= 0 {
			p.MaxServers = DefaultPoolMaxServers
		}
		if p.MinFreeMB <= 0 {
			p.MinFreeMB = DefaultPoolMinFreeMB
		}
		inv.Elastic[name] = p
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

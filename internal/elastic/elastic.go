// Package elastic runs pools of on-demand hosts: servers `on` boots from a snapshot
// when the fixed hosts are short of memory, and deletes once they sit idle.
//
// Hetzner is driven through the hcloud CLI rather than its API client, so the
// token stays in an hcloud context, and whatever `on` does can be inspected or
// undone by hand with the same tool.
//
// Every server carries labels that make the pool self-describing, so nothing here
// needs local state to find what it created: on=elastic, on-pool=<pool>, and
// on-last-used=<unix seconds>, which `on` bumps on every use and `on reap` reads.
package elastic

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bborn/on/internal/inventory"
)

// Labels `on` sets on the servers and snapshots it manages.
const (
	LabelManaged  = "on"           // "elastic" on servers `on` may delete
	LabelPool     = "on-pool"      // pool name
	LabelLastUsed = "on-last-used" // unix seconds of the last `on` use
	LabelImage    = "on-image"     // on snapshots: which pool image this is
	LabelPaused   = "on-paused"    // on the snapshot: UTC day the daily budget ran out
	managedValue  = "elastic"
)

// Server is one on-demand host.
type Server struct {
	ID       int64
	Name     string
	IP       string
	Status   string
	Type     string
	Location string
	Pool     string
	Created  time.Time
	LastUsed time.Time
}

// Hetzner manages one pool's servers.
type Hetzner struct {
	Pool inventory.Pool

	// Run executes hcloud with the given arguments. Tests replace it.
	Run func(args ...string) ([]byte, error)

	prices map[string]float64
}

// New returns a manager for the pool that shells out to hcloud.
func New(p inventory.Pool) *Hetzner {
	h := &Hetzner{Pool: p}
	h.Run = func(args ...string) ([]byte, error) {
		full := append([]string{}, args...)
		if p.Context != "" {
			full = append([]string{"--context", p.Context}, full...)
		}
		// stdout only: hcloud writes "Waiting for …" progress to stderr, which
		// would corrupt the JSON. stderr is kept for the error message.
		var stderr strings.Builder
		cmd := exec.Command("hcloud", full...)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return out, fmt.Errorf("hcloud %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
	return h
}

type apiServer struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Created   time.Time         `json:"created"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
	ServerType struct {
		Name string `json:"name"`
	} `json:"server_type"`
	// Newer hcloud releases put the location at the top level and leave
	// datacenter null; older ones only nest it under datacenter. Read both.
	Location *struct {
		Name string `json:"name"`
	} `json:"location"`
	Datacenter *struct {
		Location struct {
			Name string `json:"name"`
		} `json:"location"`
	} `json:"datacenter"`
}

func (a apiServer) location() string {
	if a.Location != nil && a.Location.Name != "" {
		return a.Location.Name
	}
	if a.Datacenter != nil {
		return a.Datacenter.Location.Name
	}
	return ""
}

func (a apiServer) server() Server {
	s := Server{
		ID:       a.ID,
		Name:     a.Name,
		IP:       a.PublicNet.IPv4.IP,
		Status:   a.Status,
		Type:     a.ServerType.Name,
		Location: a.location(),
		Pool:     a.Labels[LabelPool],
		Created:  a.Created,
	}
	if sec, err := strconv.ParseInt(a.Labels[LabelLastUsed], 10, 64); err == nil {
		s.LastUsed = time.Unix(sec, 0)
	} else {
		s.LastUsed = a.Created
	}
	return s
}

// List returns the pool's servers, oldest first.
func (h *Hetzner) List() ([]Server, error) {
	out, err := h.Run("server", "list", "-o", "json",
		"-l", fmt.Sprintf("%s=%s,%s=%s", LabelManaged, managedValue, LabelPool, h.Pool.Name))
	if err != nil {
		return nil, err
	}
	var raw []apiServer
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing hcloud server list: %w", err)
	}
	servers := make([]Server, 0, len(raw))
	for _, r := range raw {
		servers = append(servers, r.server())
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Created.Before(servers[j].Created) })
	return servers, nil
}

// Snapshot is the image a pool boots from.
type Snapshot struct {
	ID      int64
	Created time.Time
	Labels  map[string]string
}

// Snapshot returns the newest snapshot labelled for this pool's image.
func (h *Hetzner) Snapshot() (Snapshot, error) {
	out, err := h.Run("image", "list", "-t", "snapshot", "-o", "json",
		"-l", fmt.Sprintf("%s=%s", LabelImage, h.Pool.Image))
	if err != nil {
		return Snapshot{}, err
	}
	var raw []struct {
		ID      int64             `json:"id"`
		Created time.Time         `json:"created"`
		Labels  map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Snapshot{}, fmt.Errorf("parsing hcloud image list: %w", err)
	}
	if len(raw) == 0 {
		return Snapshot{}, fmt.Errorf("no snapshot labelled %s=%s in context %q — build one first",
			LabelImage, h.Pool.Image, h.Pool.Context)
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].Created.After(raw[j].Created) })
	return Snapshot{ID: raw[0].ID, Created: raw[0].Created, Labels: raw[0].Labels}, nil
}

// PausedDay reports the UTC day the pool's daily budget ran out, or "" when the
// pool is not paused. The mark lives on the snapshot so every machine running
// `on` sees it, not only the one that ran `on reap`.
func (s Snapshot) PausedDay() string { return s.Labels[LabelPaused] }

// Pause marks the snapshot so no new server starts today.
func (h *Hetzner) Pause(s Snapshot, day string) error {
	_, err := h.Run("image", "add-label", "--overwrite", strconv.FormatInt(s.ID, 10),
		fmt.Sprintf("%s=%s", LabelPaused, day))
	return err
}

// Unpause clears a pause mark from an earlier day.
func (h *Hetzner) Unpause(s Snapshot) error {
	_, err := h.Run("image", "remove-label", strconv.FormatInt(s.ID, 10), LabelPaused)
	return err
}

// ErrPaused is returned by Create while the daily budget is spent.
var ErrPaused = fmt.Errorf("daily budget reached")

// Create boots a new server from the pool's snapshot, trying each type and
// location in order until one has capacity.
func (h *Hetzner) Create(now time.Time) (Server, error) {
	snap, err := h.Snapshot()
	if err != nil {
		return Server{}, err
	}
	if day := snap.PausedDay(); day == UTCDay(now) {
		return Server{}, fmt.Errorf("pool %s: %w for %s (daily_budget %.2f) — no new servers until tomorrow UTC",
			h.Pool.Name, ErrPaused, day, h.Pool.DailyBudget)
	}

	name := fmt.Sprintf("on-%s-%s", h.Pool.Name, randomSuffix())
	var failures []string
	for _, typ := range h.Pool.Types {
		for _, loc := range h.Pool.Locations {
			args := []string{"server", "create", "-o", "json",
				"--name", name, "--type", typ, "--location", loc,
				"--image", strconv.FormatInt(snap.ID, 10),
				"--label", fmt.Sprintf("%s=%s", LabelManaged, managedValue),
				"--label", fmt.Sprintf("%s=%s", LabelPool, h.Pool.Name),
				"--label", fmt.Sprintf("%s=%d", LabelLastUsed, now.Unix()),
			}
			for _, k := range h.Pool.SSHKeys {
				args = append(args, "--ssh-key", k)
			}
			out, err := h.Run(args...)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s@%s: %s", typ, loc, lastLine(err.Error())))
				continue
			}
			var created struct {
				Server apiServer `json:"server"`
			}
			if err := json.Unmarshal(out, &created); err != nil {
				return Server{}, fmt.Errorf("parsing hcloud server create: %w", err)
			}
			return created.Server.server(), nil
		}
	}
	return Server{}, fmt.Errorf("no capacity for pool %s: %s", h.Pool.Name, strings.Join(failures, "; "))
}

// Touch records a use, which is what keeps `on reap` away.
func (h *Hetzner) Touch(s Server, now time.Time) error {
	_, err := h.Run("server", "add-label", "--overwrite", s.Name,
		fmt.Sprintf("%s=%d", LabelLastUsed, now.Unix()))
	return err
}

// Delete removes the server. Hetzner bills a powered-off server, so there is no
// cheaper "stop" to offer.
func (h *Hetzner) Delete(s Server) error {
	_, err := h.Run("server", "delete", s.Name)
	return err
}

// HourlyPrice is the gross hourly price for the server's type and location, or 0
// when hcloud does not list one.
func (h *Hetzner) HourlyPrice(typ, location string) float64 {
	key := typ + "@" + location
	if p, ok := h.prices[key]; ok {
		return p
	}
	if h.prices == nil {
		h.prices = map[string]float64{}
	}
	out, err := h.Run("server-type", "describe", typ, "-o", "json")
	if err != nil {
		return 0
	}
	var st struct {
		Prices []struct {
			Location    string `json:"location"`
			PriceHourly struct {
				Gross string `json:"gross"`
			} `json:"price_hourly"`
		} `json:"prices"`
	}
	if json.Unmarshal(out, &st) != nil {
		return 0
	}
	for _, p := range st.Prices {
		if v, err := strconv.ParseFloat(p.PriceHourly.Gross, 64); err == nil {
			h.prices[typ+"@"+p.Location] = v
		}
	}
	return h.prices[key]
}

// Host turns a server into something the rest of `on` can place work on. The
// server's name is its ssh alias, written into the pool's ssh config.
func Host(p inventory.Pool, s Server) inventory.Host {
	return inventory.Host{
		Name:         s.Name,
		SSH:          s.Name,
		Workdir:      p.Workdir,
		Capabilities: p.Capabilities,
	}
}

// Verdict is what `on reap` decided about one server.
type Verdict struct {
	Server Server
	Delete bool
	Reason string
}

// Decide says whether a server should be deleted. Busy means something is running
// on it (an `on exec`, a tmux session, an `on forward`), which protects it from
// the idle rule but not from MaxHours or the budget.
func Decide(p inventory.Pool, s Server, busy, overBudget bool, now time.Time) Verdict {
	age := now.Sub(s.Created)
	idle := now.Sub(s.LastUsed)
	switch {
	case overBudget:
		return Verdict{s, true, "daily budget reached"}
	case age >= time.Duration(p.MaxHours)*time.Hour:
		return Verdict{s, true, fmt.Sprintf("older than max_hours (%dh)", p.MaxHours)}
	case s.Status != "running" && s.Status != "initializing" && s.Status != "starting":
		return Verdict{s, true, "not running (" + s.Status + ") but still billed"}
	case busy:
		return Verdict{s, false, "busy"}
	case idle >= time.Duration(p.IdleMinutes)*time.Minute:
		return Verdict{s, true, fmt.Sprintf("idle %s", idle.Round(time.Minute))}
	default:
		return Verdict{s, false, fmt.Sprintf("idle %s of %dm", idle.Round(time.Minute), p.IdleMinutes)}
	}
}

// BusyScript prints "busy" when anything user-driven is running: a tmux session,
// or any process whose working directory is under the workdir (where `on exec`
// mirrors live and where `on forward` parks its keepalive).
func BusyScript(workdir string) string {
	dir := strings.TrimPrefix(workdir, "~/")
	return fmt.Sprintf(`if tmux ls >/dev/null 2>&1; then echo busy; exit; fi
w="$HOME/%s"
for p in /proc/[0-9]*; do
  case "$(readlink "$p/cwd" 2>/dev/null)" in "$w"|"$w"/*) echo busy; exit;; esac
done
echo idle`, dir)
}

// UTCDay formats the billing day a time falls in.
func UTCDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

func randomSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// ConfigDir is where `on` keeps the ssh config and known_hosts for pool servers.
func ConfigDir() string {
	return filepath.Dir(inventory.DefaultPath())
}

// SSHConfigPath is the per-pool ssh config `on` rewrites whenever it lists servers.
func SSHConfigPath(pool string) string {
	return filepath.Join(ConfigDir(), "ssh_config.d", pool+".conf")
}

// IncludeLine is what ~/.ssh/config needs so pool servers resolve as aliases.
func IncludeLine() string {
	return "Include " + filepath.Join(ConfigDir(), "ssh_config.d", "*.conf")
}

// RenderSSHConfig writes one Host block per server. Hetzner reuses IPs, so each
// pool keeps its own known_hosts, and entries are forgotten when a server goes.
func RenderSSHConfig(p inventory.Pool, servers []Server) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by `on` for pool %s. Regenerated on every listing; do not edit.\n", p.Name)
	for _, s := range servers {
		if s.IP == "" {
			continue
		}
		fmt.Fprintf(&b, "\nHost %s\n  HostName %s\n  User %s\n  StrictHostKeyChecking accept-new\n  UserKnownHostsFile %s\n  ServerAliveInterval 30\n",
			s.Name, s.IP, p.User, knownHostsPath(p.Name))
	}
	return b.String()
}

func knownHostsPath(pool string) string {
	return filepath.Join(ConfigDir(), "known_hosts."+pool)
}

// WriteSSHConfig rewrites the pool's ssh config atomically.
func WriteSSHConfig(p inventory.Pool, servers []Server) error {
	path := SSHConfigPath(p.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(RenderSSHConfig(p, servers)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ForgetHostKey drops a deleted server's key, so the next server on that IP is
// accepted rather than refused as an impostor.
func ForgetHostKey(p inventory.Pool, s Server) {
	if s.IP == "" {
		return
	}
	_ = exec.Command("ssh-keygen", "-R", s.IP, "-f", knownHostsPath(p.Name)).Run()
}

// IncludeMissing reports whether ~/.ssh/config lacks the Include line.
func IncludeMissing() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	raw, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return true
	}
	return !strings.Contains(string(raw), IncludeLine())
}

// Package elastic runs pools of on-demand hosts: servers `on` boots from a snapshot
// when the fixed hosts are short of memory, and deletes once they sit idle.
//
// A pool draws on one or more providers (provider.go). The Manager asks each for
// what it can boot, and boots the cheapest that has capacity.
//
// Every server carries labels (tags, where the provider has no labels) that make
// the pool self-describing, so nothing here needs local state to find what it
// created: on=elastic, on-pool=<pool>, and on-last-used=<unix seconds>, which `on`
// bumps on every use and `on reap` reads.
package elastic

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// Provider is the provider that runs it, e.g. "hetzner".
	Provider string
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

// RenderSSHConfig writes one Host block per server.
//
// Hetzner hands a deleted server's IP to the next one, so host keys are recorded
// under the server's name (HostKeyAlias), which is random and never reused,
// rather than its IP. Otherwise a machine that did not delete the old server
// still holds its key for that IP and refuses the new one as an impostor.
func RenderSSHConfig(p inventory.Pool, servers []Server) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by `on` for pool %s. Regenerated on every listing; do not edit.\n", p.Name)
	for _, s := range servers {
		if s.IP == "" {
			continue
		}
		fmt.Fprintf(&b, "\nHost %s\n  HostName %s\n  HostKeyAlias %s\n  User %s\n  StrictHostKeyChecking accept-new\n  UserKnownHostsFile %s\n  ServerAliveInterval 30\n",
			s.Name, s.IP, s.Name, p.User, knownHostsPath(p.Name))
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

// ForgetHostKey drops a deleted server's key so known_hosts does not grow
// forever. Keys are stored under the server name (see RenderSSHConfig), so a
// machine that never runs this is merely left a stale line, not locked out.
func ForgetHostKey(p inventory.Pool, s Server) {
	_ = exec.Command("ssh-keygen", "-R", s.Name, "-f", knownHostsPath(p.Name)).Run()
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

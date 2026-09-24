package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bborn/on/internal/elastic"
	"github.com/bborn/on/internal/fleet"
	"github.com/bborn/on/internal/inventory"
	"github.com/bborn/on/internal/remote"
)

// placement is where `on exec` will run, plus what to do when the run ends.
type placement struct {
	host inventory.Host
	done func()
}

// placeForExec picks a host for `on exec`: a fixed host serving the project when
// one has room, otherwise a server from the project's elastic pool.
func placeForExec(inv *inventory.Inventory, repo string) (placement, error) {
	pool, hasPool := inv.PoolFor(repo)
	if !hasPool {
		h, err := pickHostFor(inv, repo)
		return placement{host: h, done: func() {}}, err
	}

	best, bestFree := inventory.Host{}, -1
	if candidates := inv.HostsFor(repo); len(candidates) > 0 {
		for _, st := range fleet.Probe(candidates, false) {
			if st.Reachable && st.AvailMB > bestFree {
				best, bestFree = st.Host, st.AvailMB
			}
		}
	}
	if bestFree >= pool.MinFreeMB {
		return placement{host: best, done: func() {}}, nil
	}
	if bestFree >= 0 {
		fmt.Fprintf(os.Stderr, "  fixed hosts are short of memory (best: %s, %dM free < %dM) — using pool %s\n",
			best.Name, bestFree, pool.MinFreeMB, pool.Name)
	}
	return acquire(pool, false)
}

// acquire returns a pool server for a run: an idle one if the pool has it,
// otherwise a new one, otherwise (at max_servers) the least loaded busy one,
// where the project's lock queues the run.
func acquire(pool inventory.Pool, forceNew bool) (placement, error) {
	if elastic.IncludeMissing() {
		return placement{}, fmt.Errorf("pool servers resolve through an ssh config include — add this line near the top of ~/.ssh/config:\n\n  %s\n", elastic.IncludeLine())
	}
	h := elastic.New(pool)
	servers, err := listPool(h)
	if err != nil {
		return placement{}, err
	}

	if !forceNew {
		var running []elastic.Server
		for _, s := range servers {
			if s.Status == "running" {
				running = append(running, s)
			}
		}
		idle, busy := splitBusy(pool, running)
		if s, ok := roomiest(pool, idle); ok {
			return use(h, s), nil
		}
		if len(servers) >= pool.MaxServers {
			if s, ok := roomiest(pool, busy); ok {
				fmt.Fprintf(os.Stderr, "  pool %s is at max_servers (%d); sharing busy %s\n", pool.Name, pool.MaxServers, s.Name)
				return use(h, s), nil
			}
			return placement{}, fmt.Errorf("pool %s is at max_servers (%d) and none is reachable — `on pools` to see them", pool.Name, pool.MaxServers)
		}
	} else if len(servers) >= pool.MaxServers {
		return placement{}, fmt.Errorf("pool %s is at max_servers (%d)", pool.Name, pool.MaxServers)
	}

	start := time.Now()
	fmt.Fprintf(os.Stderr, "→ pool %s: starting a server from its snapshot…\n", pool.Name)
	s, err := h.Create(start)
	if err != nil {
		return placement{}, err
	}
	if _, err := listPool(h); err != nil {
		return placement{}, err
	}
	if err := waitForSSH(s.Name, 4*time.Minute); err != nil {
		return placement{}, fmt.Errorf("%s (%s) never accepted ssh: %w — delete it with `on down %s`", s.Name, s.IP, err, s.Name)
	}
	fmt.Fprintf(os.Stderr, "  %s ready in %s (%s %s @ %s, %.3f %s/h)\n", s.Name,
		time.Since(start).Round(time.Second), s.Provider, s.Type, s.Location, h.HourlyPrice(s), pool.Currency)
	return use(h, s), nil
}

func use(h *elastic.Manager, s elastic.Server) placement {
	_ = h.Touch(s, time.Now())
	return placement{
		host: elastic.Host(h.Pool, s),
		done: func() { _ = h.Touch(s, time.Now()) },
	}
}

// listPool lists a pool's servers and rewrites its ssh config to match.
func listPool(h *elastic.Manager) ([]elastic.Server, error) {
	servers, err := h.List()
	if err != nil {
		return nil, err
	}
	return servers, elastic.WriteSSHConfig(h.Pool, servers)
}

// splitBusy probes each server for running work.
func splitBusy(pool inventory.Pool, servers []elastic.Server) (idle, busy []elastic.Server) {
	for _, s := range servers {
		if b, ok := probeBusy(pool, s); ok && !b {
			idle = append(idle, s)
		} else if ok {
			busy = append(busy, s)
		}
	}
	return idle, busy
}

// probeBusy reports whether anything is running on the server; ok is false when
// it could not be reached.
func probeBusy(pool inventory.Pool, s elastic.Server) (busy, ok bool) {
	argv := remote.Command(s.Name, remote.Options{BatchMode: true, ConnectTimeout: 8},
		[]string{"sh", "-c", elastic.BusyScript(pool.Workdir)})
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(string(out)) == "busy", true
}

func roomiest(pool inventory.Pool, servers []elastic.Server) (elastic.Server, bool) {
	if len(servers) == 0 {
		return elastic.Server{}, false
	}
	hosts := make([]inventory.Host, len(servers))
	for i, s := range servers {
		hosts[i] = elastic.Host(pool, s)
	}
	best, bestFree := -1, -1
	for i, st := range fleet.Probe(hosts, false) {
		if st.Reachable && st.AvailMB > bestFree {
			best, bestFree = i, st.AvailMB
		}
	}
	if best < 0 {
		return elastic.Server{}, false
	}
	return servers[best], true
}

func waitForSSH(alias string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		argv := remote.Command(alias, remote.Options{BatchMode: true, ConnectTimeout: 5}, []string{"true"})
		err := exec.Command(argv[0], argv[1:]...).Run()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(3 * time.Second)
	}
}

func poolByName(inv *inventory.Inventory, name string) (inventory.Pool, error) {
	if p, ok := inv.Elastic[name]; ok {
		return p, nil
	}
	return inventory.Pool{}, fmt.Errorf("unknown pool %q — inventory has: %s", name, strings.Join(inv.PoolNames(), ", "))
}

// on up <pool> — start a server now, e.g. before a dev-server session.
func cmdUp(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: on up <pool>")
	}
	inv, err := load()
	if err != nil {
		return err
	}
	pool, err := poolByName(inv, args[0])
	if err != nil {
		return err
	}
	p, err := acquire(pool, true)
	if err != nil {
		return err
	}
	fmt.Println(p.host.Name)
	return nil
}

// on down <server>... | on down --pool <pool> — delete now.
func cmdDown(args []string) error {
	inv, err := load()
	if err != nil {
		return err
	}
	if len(args) == 2 && args[0] == "--pool" {
		pool, err := poolByName(inv, args[1])
		if err != nil {
			return err
		}
		h := elastic.New(pool)
		servers, err := h.List()
		if err != nil {
			return err
		}
		for _, s := range servers {
			if err := deleteServer(h, s); err != nil {
				return err
			}
		}
		_, err = listPool(h)
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: on down <server>... | on down --pool <pool>")
	}
	for _, name := range args {
		found := false
		for _, pn := range inv.PoolNames() {
			h := elastic.New(inv.Elastic[pn])
			servers, err := h.List()
			if err != nil {
				return err
			}
			for _, s := range servers {
				if s.Name == name {
					found = true
					if err := deleteServer(h, s); err != nil {
						return err
					}
					if _, err := listPool(h); err != nil {
						return err
					}
				}
			}
		}
		if !found {
			return fmt.Errorf("no pool server named %q — `on pools` lists them", name)
		}
	}
	return nil
}

func deleteServer(h *elastic.Manager, s elastic.Server) error {
	if err := h.Delete(s); err != nil {
		return err
	}
	elastic.ForgetHostKey(h.Pool, s)
	fmt.Fprintf(os.Stderr, "deleted %s (%s)\n", s.Name, s.IP)
	return nil
}

// on reap [--dry-run] — delete idle, over-age and over-budget servers, and
// update the spend ledger. Meant to run every few minutes on an always-on box.
func cmdReap(args []string) error {
	dry := len(args) > 0 && (args[0] == "--dry-run" || args[0] == "-n")
	inv, err := load()
	if err != nil {
		return err
	}
	ledger, err := elastic.LoadLedger(elastic.LedgerPath())
	if err != nil {
		return err
	}
	now := time.Now()
	today := elastic.UTCDay(now)

	for _, pn := range inv.PoolNames() {
		pool := inv.Elastic[pn]
		h := elastic.New(pool)
		servers, err := listPool(h)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", pn, err)
			continue
		}

		present := map[string]bool{}
		for _, s := range servers {
			present[s.Name] = true
			if !dry {
				ledger.Charge(pn, s, h.HourlyPrice(s), now)
			}
		}
		ledger.Forget(pn, present)
		spent := ledger.Spent(pn, today)
		over := pool.DailyBudget > 0 && spent >= pool.DailyBudget

		for _, s := range servers {
			busy, ok := probeBusy(pool, s)
			if !ok && now.Sub(s.Created) < 10*time.Minute {
				fmt.Printf("%-24s keep    booting (unreachable, %s old)\n", s.Name, now.Sub(s.Created).Round(time.Second))
				continue
			}
			v := elastic.Decide(pool, s, busy, over, now)
			action := "keep"
			if v.Delete {
				action = "delete"
				if !dry {
					if err := deleteServer(h, s); err != nil {
						fmt.Fprintf(os.Stderr, "%s: %v\n", s.Name, err)
						action = "FAILED"
					}
				}
			}
			fmt.Printf("%-24s %-7s %s\n", s.Name, action, v.Reason)
		}

		if !dry {
			switch paused := h.PausedDay(); {
			case over && paused != today:
				if err := h.Pause(today); err != nil {
					fmt.Fprintf(os.Stderr, "%s: pausing: %v\n", pn, err)
				}
				fmt.Printf("%s: daily budget %.2f %s reached (spent %.2f) — paused until tomorrow UTC\n", pn, pool.DailyBudget, pool.Currency, spent)
			case !over && paused != "" && paused != today:
				if err := h.Unpause(); err != nil {
					fmt.Fprintf(os.Stderr, "%s: unpausing: %v\n", pn, err)
				}
			}
		}
		if !dry {
			_, _ = listPool(h)
		}
	}
	if dry {
		return nil
	}
	return ledger.Save(elastic.LedgerPath())
}

// poolReport is what `on pools --json` prints, for dashboards.
type poolReport struct {
	Name        string         `json:"name"`
	Image       string         `json:"image"`
	Providers   []string       `json:"providers"`
	Currency    string         `json:"currency"`
	Paused      string         `json:"paused,omitempty"`
	MaxServers  int            `json:"max_servers"`
	DailyBudget float64        `json:"daily_budget"`
	SpentToday  float64        `json:"spent_today"`
	Servers     []serverReport `json:"servers"`
	Error       string         `json:"error,omitempty"`
}

type serverReport struct {
	Provider    string    `json:"provider"`
	Name        string    `json:"name"`
	IP          string    `json:"ip"`
	Status      string    `json:"status"`
	Type        string    `json:"type"`
	Location    string    `json:"location"`
	Created     time.Time `json:"created"`
	LastUsed    time.Time `json:"last_used"`
	HourlyPrice float64   `json:"hourly_price"`
}

// on pools [--json] — pool servers, prices and today's estimated spend.
func cmdPools(args []string) error {
	asJSON := len(args) > 0 && args[0] == "--json"
	inv, err := load()
	if err != nil {
		return err
	}
	ledger, _ := elastic.LoadLedger(elastic.LedgerPath())
	now := time.Now()
	var reports []poolReport
	for _, pn := range inv.PoolNames() {
		pool := inv.Elastic[pn]
		h := elastic.New(pool)
		r := poolReport{Name: pn, Image: pool.Image, Providers: pool.ProviderNames(), Currency: pool.Currency,
			MaxServers: pool.MaxServers, DailyBudget: pool.DailyBudget, Servers: []serverReport{}}
		if ledger != nil {
			r.SpentToday = ledger.Spent(pn, elastic.UTCDay(now))
		}
		servers, err := listPool(h)
		if err != nil {
			r.Error = err.Error()
			reports = append(reports, r)
			continue
		}
		r.Paused = h.PausedDay()
		for _, s := range servers {
			r.Servers = append(r.Servers, serverReport{s.Provider, s.Name, s.IP, s.Status, s.Type, s.Location, s.Created, s.LastUsed,
				h.HourlyPrice(s)})
		}
		reports = append(reports, r)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(reports)
	}
	for _, r := range reports {
		budget := "no daily budget"
		if r.DailyBudget > 0 {
			budget = fmt.Sprintf("spent %.2f of %.2f %s today", r.SpentToday, r.DailyBudget, r.Currency)
		}
		fmt.Printf("pool %s (%s; image %s, max %d, %s)", r.Name, strings.Join(r.Providers, "+"), r.Image, r.MaxServers, budget)
		if r.Paused != "" {
			fmt.Printf(" — PAUSED %s", r.Paused)
		}
		fmt.Println()
		if r.Error != "" {
			fmt.Printf("  error: %s\n", r.Error)
		}
		sort.Slice(r.Servers, func(i, j int) bool { return r.Servers[i].Created.Before(r.Servers[j].Created) })
		for _, s := range r.Servers {
			fmt.Printf("  %-22s %-9s %-12s %-14s %-5s up %-8s idle %-8s %.3f/h\n", s.Name, s.Status, s.Provider, s.Type, s.Location,
				now.Sub(s.Created).Round(time.Minute), now.Sub(s.LastUsed).Round(time.Minute), s.HourlyPrice)
		}
		if len(r.Servers) == 0 {
			fmt.Println("  (no servers)")
		}
	}
	return nil
}

// on forward <host|pool> <port> [local-port] — reach a server's port from here,
// e.g. a Rails dev server, at http://localhost:<local-port>.
//
// The remote end parks a loop in the workdir, which `on reap` counts as busy, so
// a server stays up for as long as the tunnel is open, and no longer.
func cmdForward(args []string) error {
	if len(args) < 2 || len(args) > 3 {
		return fmt.Errorf("usage: on forward <host|pool> <port> [local-port]")
	}
	inv, err := load()
	if err != nil {
		return err
	}
	remotePort, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("port must be a number: %q", args[1])
	}
	localPort := remotePort
	if len(args) == 3 {
		if localPort, err = strconv.Atoi(args[2]); err != nil {
			return fmt.Errorf("local port must be a number: %q", args[2])
		}
	}

	var host inventory.Host
	if h, ok := inv.Hosts[args[0]]; ok {
		host = h
	} else if pool, ok := inv.Elastic[args[0]]; ok {
		servers, err := listPool(elastic.New(pool))
		if err != nil {
			return err
		}
		if len(servers) == 0 {
			return fmt.Errorf("pool %s has no servers — `on up %s` starts one", pool.Name, pool.Name)
		}
		host = elastic.Host(pool, servers[len(servers)-1])
	} else {
		host = inventory.Host{Name: args[0], SSH: args[0], Workdir: inventory.DefaultWorkdir}
		for _, pn := range inv.PoolNames() {
			if strings.HasPrefix(args[0], "on-"+pn+"-") {
				host.Workdir = inv.Elastic[pn].Workdir
			}
		}
	}

	// Without a tty, sshd does not signal the remote command when the tunnel
	// drops, so a plain sleep would outlive it and keep the server "busy" until
	// max_hours. Watching the sshd session process ends it with the tunnel.
	keepalive := fmt.Sprintf("mkdir -p %s && cd %s && p=$PPID && while kill -0 $p 2>/dev/null; do sleep 10; done",
		remote.QuotePath(host.Workdir), remote.QuotePath(host.Workdir))
	argv := []string{"ssh", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=30",
		"-L", fmt.Sprintf("%d:localhost:%d", localPort, remotePort), host.SSH, "--", keepalive}
	fmt.Fprintf(os.Stderr, "→ http://localhost:%d  is  %s:%d  (Ctrl-C closes the tunnel)\n", localPort, host.Name, remotePort)
	// Become ssh rather than wait on it, so killing this process by pid (how an
	// agent stops a background tunnel) closes the tunnel instead of orphaning it.
	path, err := exec.LookPath("ssh")
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, os.Environ())
}

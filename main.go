// Command on runs work on a remote host and hands you the terminal.
//
//	on ol-agents claude            start or reattach an agent session there
//	on ls                          fleet health
//	on ps                          live sessions across the fleet
//
// Work runs on the remote host's CPU and RAM, in its checkouts, with its
// credentials. Nothing here moves secrets: a host's credentials are provisioned
// out of band and stay there.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bborn/on/internal/fleet"
	"github.com/bborn/on/internal/inventory"
	"github.com/bborn/on/internal/remote"
	"github.com/bborn/on/internal/session"
)

const usage = `on — run work on another machine, interactively.

usage:
  on [flags] <host> <command>...   run <command> on <host> in a tmux session, and attach
  on ls                            list hosts with load and free memory
  on ps                            list live sessions across the fleet
  on attach <host> [name]          reattach to a session
  on kill <host> <name>            end a session
  on init                          write a starter inventory

flags (before <host>):
  -C <dir>      remote working directory
  -n <name>     session name (default: derived from the command)
  -d            create the session but do not attach
  --new         always start a new session instead of reattaching

examples:
  on ol-agents claude
  on -C ~/projects/offerlab mona claude --resume
  on -d ik-agents bin/rails test

Re-running the same command reattaches to the existing session rather than
starting a second one. Sessions survive disconnection; reattach from anywhere.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "on: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return nil
	}

	switch args[0] {
	case "init":
		return cmdInit()
	case "ls", "hosts":
		return cmdLs()
	case "ps":
		return cmdPs()
	case "attach":
		return cmdAttach(args[1:])
	case "kill":
		return cmdKill(args[1:])
	}
	return cmdRun(args)
}

// opts are the flags accepted before the host name. Flags must precede the host
// so that everything after it passes through to the remote command untouched —
// `on mona claude --resume` must send --resume to claude, not to on.
type opts struct {
	dir    string
	name   string
	detach bool
	fresh  bool
}

func parseFlags(args []string) (opts, []string, error) {
	var o opts
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		switch args[i] {
		case "-C":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("-C needs a directory")
			}
			o.dir = args[i+1]
			i += 2
		case "-n":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("-n needs a name")
			}
			o.name = args[i+1]
			i += 2
		case "-d":
			o.detach = true
			i++
		case "--new":
			o.fresh = true
			i++
		default:
			return o, nil, fmt.Errorf("unknown flag %q (flags must come before the host)", args[i])
		}
	}
	return o, args[i:], nil
}

func cmdRun(args []string) error {
	o, rest, err := parseFlags(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("no host given\n\n%s", usage)
	}
	hostName, cmd := rest[0], rest[1:]

	host, err := lookup(hostName)
	if err != nil {
		return err
	}
	if len(cmd) == 0 {
		return fmt.Errorf("no command given — try `on %s claude`", hostName)
	}

	name := o.name
	if name == "" {
		name = session.Name(cmd)
	}
	if o.fresh {
		name = uniqueName(host, name)
	}

	dir := o.dir
	if dir == "" {
		dir = host.Workdir
	}

	if o.detach {
		script := session.CreateScript(name, dir, cmd)
		if out, err := runCapture(host, script); err != nil {
			return fmt.Errorf("%s: %s", host.Name, err)
		} else if s := strings.TrimSpace(out); s != "" {
			fmt.Println(s)
		}
		fmt.Printf("%s started on %s — attach with `on attach %s %s`\n",
			name, host.Name, host.Name, name)
		return nil
	}

	return attachTo(host, session.CreateOrAttachScript(name, dir, cmd))
}

func cmdAttach(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: on attach <host> [session]")
	}
	host, err := lookup(args[0])
	if err != nil {
		return err
	}

	name := ""
	if len(args) > 1 {
		name = args[1]
	} else {
		// With no name, attach when the choice is unambiguous rather than making
		// the user run `on ps` first.
		sessions := onSessions(host)
		switch len(sessions) {
		case 0:
			return fmt.Errorf("no sessions on %s", host.Name)
		case 1:
			name = sessions[0].Name
		default:
			var names []string
			for _, s := range sessions {
				names = append(names, s.Name)
			}
			return fmt.Errorf("%s has several sessions — name one: %s",
				host.Name, strings.Join(names, ", "))
		}
	}
	return attachTo(host, session.AttachScript(name))
}

func cmdKill(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: on kill <host> <session>")
	}
	host, err := lookup(args[0])
	if err != nil {
		return err
	}
	if _, err := runCapture(host, session.KillScript(args[1])); err != nil {
		return fmt.Errorf("%s: %s", host.Name, err)
	}
	fmt.Printf("killed %s on %s\n", args[1], host.Name)
	return nil
}

func cmdLs() error {
	inv, err := load()
	if err != nil {
		return err
	}
	statuses := fleet.Probe(allHosts(inv), false)

	fmt.Printf("%-14s %-18s %6s %8s %8s %7s\n", "HOST", "SSH", "CORES", "AVAIL", "TOTAL", "LOAD")
	for _, s := range statuses {
		if !s.Reachable {
			fmt.Printf("%-14s %-18s %6s %8s %8s %7s  %s\n",
				s.Host.Name, s.Host.SSH, "-", "-", "-", "-", s.Err)
			continue
		}
		fmt.Printf("%-14s %-18s %6d %7dM %7dM %7.2f  %d%% free\n",
			s.Host.Name, s.Host.SSH, s.Cores, s.AvailMB, s.TotalMB, s.Load, s.AvailPct())
	}
	return nil
}

func cmdPs() error {
	inv, err := load()
	if err != nil {
		return err
	}
	statuses := fleet.Probe(allHosts(inv), true)

	any := false
	for _, s := range statuses {
		if !s.Reachable {
			fmt.Printf("%-14s unreachable: %s\n", s.Host.Name, s.Err)
			continue
		}
		for _, sess := range s.Sessions {
			state := "detached"
			if sess.Attached {
				state = "attached"
			}
			fmt.Printf("%-14s %-24s %s  %s windows\n", s.Host.Name, sess.Name, state, sess.Windows)
			any = true
		}
	}
	if !any {
		fmt.Println("no sessions")
	}
	return nil
}

func cmdInit() error {
	path := inventory.DefaultPath()
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(inventory.Template), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s — add your hosts, then run `on ls`\n", path)
	return nil
}

// attachTo replaces this process with ssh so the terminal is handed straight to
// tmux. Proxying the streams instead would mean forwarding resize events and
// signals by hand, and getting that subtly wrong in ways that only show up in
// full-screen programs.
func attachTo(h inventory.Host, script string) error {
	argv := remote.Command(h.SSH, remote.Options{TTY: true}, []string{"sh", "-c", script})
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, os.Environ())
}

func runCapture(h inventory.Host, script string) (string, error) {
	argv := remote.Command(h.SSH, remote.Options{BatchMode: true}, []string{"sh", "-c", script})
	cmd := exec.Command(argv[0], argv[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		// Never let a connection failure look like the command having failed:
		// the two need opposite responses from the user.
		if remote.IsConnectionFailure(code, stderr.String()) {
			return "", fmt.Errorf("cannot reach host (%s)", strings.TrimSpace(stderr.String()))
		}
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return "", fmt.Errorf("%s", s)
		}
		return "", err
	}
	return string(out), nil
}

// uniqueName finds a free name by suffixing, so --new never collides.
func uniqueName(h inventory.Host, base string) string {
	taken := map[string]bool{}
	for _, s := range onSessions(h) {
		taken[s.Name] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !taken[candidate] {
			return candidate
		}
	}
}

func onSessions(h inventory.Host) []session.Info {
	out, err := runCapture(h, session.ListScript())
	if err != nil {
		return nil
	}
	var on []session.Info
	for _, s := range session.ParseList(out) {
		if s.IsOn() {
			on = append(on, s)
		}
	}
	return on
}

func load() (*inventory.Inventory, error) {
	return inventory.Load(inventory.DefaultPath())
}

func lookup(name string) (inventory.Host, error) {
	inv, err := load()
	if err != nil {
		return inventory.Host{}, err
	}
	return inv.Lookup(name)
}

func allHosts(inv *inventory.Inventory) []inventory.Host {
	var hosts []inventory.Host
	for _, n := range inv.Names() {
		hosts = append(hosts, inv.Hosts[n])
	}
	return hosts
}

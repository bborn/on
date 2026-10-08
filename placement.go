package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"

	"github.com/bborn/on/internal/fleet"
	"github.com/bborn/on/internal/inventory"
	"github.com/bborn/on/internal/mirror"
	"github.com/bborn/on/internal/remote"
)

// chooseHost picks where an exec run goes among probed fixed hosts.
//
// Hosts are tried by priority (lowest first), then by most free memory. One is
// passed over when it is unreachable, has less than minFree MB available, or busy
// reports it already running the project's locked command, so a big preferred
// host that is mid-suite sends the next run on instead of queueing it there. ok is
// false when every candidate was passed over. A nil busy treats every host as idle.
func chooseHost(sts []fleet.Status, minFree int, busy func(inventory.Host) bool) (inventory.Host, int, bool) {
	ranked := make([]fleet.Status, 0, len(sts))
	for _, st := range sts {
		if st.Reachable && st.AvailMB >= minFree {
			ranked = append(ranked, st)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Host.Priority != ranked[j].Host.Priority {
			return ranked[i].Host.Priority < ranked[j].Host.Priority
		}
		return ranked[i].AvailMB > ranked[j].AvailMB
	})
	for _, st := range ranked {
		if busy != nil && busy(st.Host) {
			fmt.Fprintf(os.Stderr, "  %s is already running this project — trying the next host\n", st.Host.Name)
			continue
		}
		return st.Host, st.AvailMB, true
	}
	return inventory.Host{}, -1, false
}

// lockBusy reports whether a host is holding the project's exec lock, i.e. is
// mid-run. Projects without a lock never make a host busy. A host that cannot be
// asked counts as idle: the run's own lock still serialises it there.
func lockBusy(cfg inventory.ExecConfig) func(inventory.Host) bool {
	if cfg.Lock == "" {
		return nil
	}
	return func(h inventory.Host) bool {
		return lockHeld(h, mirror.LockPath(h.Workdir, cfg.Lock))
	}
}

// lockHeld is a variable so tests can stand in for ssh.
var lockHeld = func(h inventory.Host, lockPath string) bool {
	// flock -E sets a distinct exit status for "held by someone else", so a
	// missing directory or a host without flock reads as idle rather than busy.
	script := fmt.Sprintf("[ -e %s ] && command -v flock >/dev/null 2>&1 && flock -n -E 75 %s true",
		remote.QuotePath(lockPath), remote.QuotePath(lockPath))
	argv := remote.Command(h.SSH, remote.Options{BatchMode: true}, []string{"sh", "-c", script})
	err := exec.Command(argv[0], argv[1:]...).Run()
	ee, ok := err.(*exec.ExitError)
	return ok && ee.ExitCode() == 75
}

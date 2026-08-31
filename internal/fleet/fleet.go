// Package fleet probes hosts concurrently for `on ls` and `on ps`.
package fleet

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/bborn/on/internal/inventory"
	"github.com/bborn/on/internal/remote"
	"github.com/bborn/on/internal/session"
)

// probeScript reports cores, total MB, available MB and 1-minute load.
//
// Every field degrades to a zero rather than failing, so one unreadable value on
// an unusual host does not blank the whole row.
const probeScript = `c=$(nproc 2>/dev/null || echo 0)
t=$(awk '/^MemTotal:/{printf "%d", $2/1024}' /proc/meminfo 2>/dev/null || echo 0)
a=$(awk '/^MemAvailable:/{printf "%d", $2/1024}' /proc/meminfo 2>/dev/null || echo 0)
l=$(cut -d" " -f1 /proc/loadavg 2>/dev/null || echo 0)
printf '%s\t%s\t%s\t%s\n' "$c" "$t" "$a" "$l"`

// Status is one host's probe result.
type Status struct {
	Host      inventory.Host
	Reachable bool
	Err       string

	Cores   int
	TotalMB int
	AvailMB int
	Load    float64

	Sessions []session.Info
}

// AvailPct is the share of memory still available, the same signal the daemon's
// memory guard uses. Returns -1 when unknown.
func (s Status) AvailPct() int {
	if s.TotalMB <= 0 {
		return -1
	}
	return s.AvailMB * 100 / s.TotalMB
}

// Probe queries every host concurrently. A slow or unreachable host delays only
// its own row, never the listing.
func Probe(hosts []inventory.Host, withSessions bool) []Status {
	out := make([]Status, len(hosts))
	var wg sync.WaitGroup

	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h inventory.Host) {
			defer wg.Done()
			out[i] = probeOne(h, withSessions)
		}(i, h)
	}
	wg.Wait()
	return out
}

func probeOne(h inventory.Host, withSessions bool) Status {
	st := Status{Host: h}

	script := probeScript
	if withSessions {
		script += "\n" + session.ListScript()
	}

	// BatchMode keeps an ssh password prompt from hanging the whole listing.
	argv := remote.Command(h.SSH, remote.Options{
		BatchMode:      true,
		ConnectTimeout: 8,
	}, []string{"sh", "-c", script})

	cmd := exec.Command(argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		st.Err = firstLine(stderr.String())
		if st.Err == "" {
			st.Err = err.Error()
		}
		return st
	}

	st.Reachable = true
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) > 0 {
		parseProbe(&st, lines[0])
	}
	if withSessions && len(lines) > 1 {
		st.Sessions = session.ParseList(strings.Join(lines[1:], "\n"))
	}
	return st
}

func parseProbe(st *Status, line string) {
	f := strings.Split(line, "\t")
	if len(f) < 4 {
		return
	}
	st.Cores, _ = strconv.Atoi(strings.TrimSpace(f[0]))
	st.TotalMB, _ = strconv.Atoi(strings.TrimSpace(f[1]))
	st.AvailMB, _ = strconv.Atoi(strings.TrimSpace(f[2]))
	st.Load, _ = strconv.ParseFloat(strings.TrimSpace(f[3]), 64)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

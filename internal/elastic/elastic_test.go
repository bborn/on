package elastic

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bborn/on/internal/inventory"
)

var pool = inventory.Pool{
	Name: "ol", Provider: "hetzner", Image: "offerlab", User: "dev", Workdir: "~/worktrees",
	Types: []string{"cx53", "cpx62"}, Locations: []string{"fsn1", "nbg1"},
	SSHKeys: []string{"k"}, IdleMinutes: 20, MaxHours: 12, MaxServers: 2,
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	fresh := Server{Name: "a", Status: "running", Created: now.Add(-time.Hour), LastUsed: now.Add(-5 * time.Minute)}
	idle := fresh
	idle.LastUsed = now.Add(-30 * time.Minute)
	old := fresh
	old.Created = now.Add(-13 * time.Hour)
	off := fresh
	off.Status = "off"

	cases := []struct {
		name       string
		s          Server
		busy, over bool
		del        bool
	}{
		{"recently used stays", fresh, false, false, false},
		{"idle past the limit goes", idle, false, false, true},
		{"busy protects an idle server", idle, true, false, false},
		{"max_hours beats busy", old, true, false, true},
		{"budget beats busy", fresh, true, true, true},
		{"a stopped server is still billed, so it goes", off, false, false, true},
	}
	for _, c := range cases {
		if got := Decide(pool, c.s, c.busy, c.over, now); got.Delete != c.del {
			t.Errorf("%s: delete=%v (%s), want %v", c.name, got.Delete, got.Reason, c.del)
		}
	}
}

func TestLedgerChargesStartedHoursOnce(t *testing.T) {
	l := &Ledger{Days: map[string]map[string]float64{}, Billed: map[string]BilledServer{}}
	created := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	s := Server{Name: "a", Created: created}

	if got := l.Charge("ol", s, 0.5, created.Add(time.Minute)); got != 0.5 {
		t.Fatalf("first sighting should bill the started hour, got %v", got)
	}
	if got := l.Charge("ol", s, 0.5, created.Add(30*time.Minute)); got != 0 {
		t.Fatalf("same hour must not bill twice, got %v", got)
	}
	if got := l.Charge("ol", s, 0.5, created.Add(2*time.Hour+time.Minute)); got != 1.0 {
		t.Fatalf("two more started hours = 1.0, got %v", got)
	}
	if spent := l.Spent("ol", "2026-09-24"); spent != 1.5 {
		t.Fatalf("spent = %v, want 1.5", spent)
	}

	l.Forget("ol", map[string]bool{})
	if _, ok := l.Billed["a"]; ok {
		t.Fatal("a server no longer present should be forgotten")
	}
}

func TestLedgerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Charge("ol", Server{Name: "a", Created: time.Now()}, 0.25, time.Now())
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Spent("ol", UTCDay(time.Now())) != 0.25 {
		t.Fatalf("ledger did not round-trip: %+v", again)
	}
}

// fake records hcloud calls and answers from a script.
type fake struct {
	calls   [][]string
	respond func(args []string) ([]byte, error)
}

func (f *fake) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	return f.respond(args)
}

const snapshotJSON = `[{"id":7,"created":"2026-09-24T10:00:00Z","labels":{"on-image":"offerlab"}}]`

func TestCreateFallsBackAcrossTypesAndLocations(t *testing.T) {
	f := &fake{respond: func(args []string) ([]byte, error) {
		switch args[0] {
		case "image":
			return []byte(snapshotJSON), nil
		case "server":
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "--type cpx62 --location nbg1") {
				return []byte(`{"server":{"id":1,"name":"on-ol-x","status":"initializing",
					"public_net":{"ipv4":{"ip":"1.2.3.4"}},"server_type":{"name":"cpx62"},
					"datacenter":{"location":{"name":"nbg1"}},"labels":{"on-pool":"ol"}}}`), nil
			}
			return []byte("resource_unavailable"), errors.New("error during placement (resource_unavailable)")
		}
		return nil, errors.New("unexpected")
	}}
	h := &Hetzner{Pool: pool, Run: f.run}
	s, err := h.Create(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != "cpx62" || s.Location != "nbg1" || s.IP != "1.2.3.4" {
		t.Fatalf("got %+v", s)
	}
	// cx53@fsn1, cx53@nbg1, cpx62@fsn1 fail before cpx62@nbg1 succeeds.
	if n := len(f.calls); n != 5 {
		t.Fatalf("expected 1 image lookup + 4 create attempts, got %d calls", n)
	}
	create := strings.Join(f.calls[4], " ")
	for _, want := range []string{"--image 7", "--label on=elastic", "--label on-pool=ol", "--ssh-key k"} {
		if !strings.Contains(create, want) {
			t.Errorf("create call missing %q: %s", want, create)
		}
	}
}

func TestCreateRefusesWhilePausedToday(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	f := &fake{respond: func(args []string) ([]byte, error) {
		return []byte(`[{"id":7,"created":"2026-09-24T10:00:00Z","labels":{"on-image":"offerlab","on-paused":"2026-09-24"}}]`), nil
	}}
	h := &Hetzner{Pool: pool, Run: f.run}
	if _, err := h.Create(now); !errors.Is(err, ErrPaused) {
		t.Fatalf("want ErrPaused, got %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("a paused pool must not attempt a create, made %d calls", len(f.calls))
	}
}

func TestListScopesToThePoolAndParses(t *testing.T) {
	f := &fake{respond: func(args []string) ([]byte, error) {
		return []byte(`[{"id":2,"name":"on-ol-b","status":"running","created":"2026-09-24T11:00:00Z",
			"labels":{"on-pool":"ol","on-last-used":"1790260000"},"public_net":{"ipv4":{"ip":"5.6.7.8"}},
			"server_type":{"name":"cx53"},"datacenter":{"location":{"name":"fsn1"}}},
			{"id":1,"name":"on-ol-a","status":"running","created":"2026-09-24T10:00:00Z",
			"labels":{"on-pool":"ol"},"public_net":{"ipv4":{"ip":"1.2.3.4"}},
			"server_type":{"name":"cx53"},"location":{"name":"nbg1"},"datacenter":null}]`), nil
	}}
	h := &Hetzner{Pool: pool, Run: f.run}
	servers, err := h.List()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls[0], " "); !strings.Contains(got, "-l on=elastic,on-pool=ol") {
		t.Fatalf("list must be scoped by labels, got: %s", got)
	}
	if servers[0].Name != "on-ol-a" || servers[1].LastUsed.Unix() != 1790260000 {
		t.Fatalf("want oldest first with parsed last-used, got %+v", servers)
	}
	if servers[0].Location != "nbg1" || servers[1].Location != "fsn1" {
		t.Fatalf("location must come from either shape, got %q and %q", servers[0].Location, servers[1].Location)
	}
	if !servers[0].LastUsed.Equal(servers[0].Created) {
		t.Fatal("a server without on-last-used counts as used when created")
	}
}

func TestRenderSSHConfig(t *testing.T) {
	out := RenderSSHConfig(pool, []Server{{Name: "on-ol-a", IP: "1.2.3.4"}, {Name: "on-ol-b"}})
	if !strings.Contains(out, "Host on-ol-a\n  HostName 1.2.3.4\n  HostKeyAlias on-ol-a\n  User dev\n") {
		t.Fatalf("missing host block:\n%s", out)
	}
	if strings.Contains(out, "on-ol-b") {
		t.Fatal("a server without an IP yet should be skipped")
	}
}
